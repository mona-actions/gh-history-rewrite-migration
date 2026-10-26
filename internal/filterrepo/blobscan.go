package filterrepo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
)

// catFileBatchCheckFormat is passed as a single argv token so the spaces in
// the format string are never split.
const catFileBatchCheckFormat = "--batch-check=%(objecttype) %(objectsize) %(objectname)"

// BlobInfo is one blob found by ScanBlobSizes.
type BlobInfo struct {
	SHA  string
	Size int64
}

// BlobScanResult summarizes a cat-file scan.
type BlobScanResult struct {
	TotalBlobs int        // every blob object seen
	Oversized  []BlobInfo // Size > threshold, sorted by Size desc then SHA asc
}

// ScanBlobSizes lists every object in bareRepoPath with
// `git cat-file --batch-all-objects --batch-check` and returns the blobs
// strictly larger than threshold bytes (matching filter-repo's
// --strip-blobs-bigger-than semantics).
//
// The scan includes unreachable blobs, so Oversized is an upper bound on what
// filter-repo will actually strip. Output is parsed as it streams, so memory
// is proportional to the oversized set, not the object count.
func (r *Runner) ScanBlobSizes(ctx context.Context, bareRepoPath string, threshold int64) (*BlobScanResult, error) {
	if bareRepoPath == "" {
		return nil, errors.New("bareRepoPath is required")
	}
	if threshold <= 0 {
		return nil, fmt.Errorf("threshold must be > 0, got %d", threshold)
	}
	r.info("scanning blob sizes (git cat-file --batch-all-objects)")

	parser := &blobLineParser{threshold: threshold, result: &BlobScanResult{}}
	var stderr cappedBuffer
	// safe.bareRepository=all mirrors the pre-rewrite pipeline: hosts with
	// safe.bareRepository=explicit otherwise refuse to run in a bare repo.
	args := []string{"-c", "safe.bareRepository=all", "cat-file", "--batch-all-objects", catFileBatchCheckFormat}
	runErr := r.execer.Run(ctx, bareRepoPath, r.bin, args, parser, &stderr)
	// A parse error aborts the stream, so it is the root cause of any
	// resulting exec failure (e.g. SIGPIPE); report it first.
	if parser.err != nil {
		return nil, parser.err
	}
	if runErr != nil {
		return nil, fmt.Errorf("git cat-file --batch-all-objects failed: %w (stderr=%q)", runErr, stderr.String())
	}
	if err := parser.flush(); err != nil {
		return nil, err
	}

	res := parser.result
	sort.Slice(res.Oversized, func(i, j int) bool {
		if res.Oversized[i].Size != res.Oversized[j].Size {
			return res.Oversized[i].Size > res.Oversized[j].Size
		}
		return res.Oversized[i].SHA < res.Oversized[j].SHA
	})
	return res, nil
}

// blobLineParser is a streaming io.Writer that parses complete
// "<type> <size> <sha>" lines as they arrive, buffering only a trailing
// partial line between Write calls.
type blobLineParser struct {
	threshold int64
	result    *BlobScanResult
	partial   []byte
	err       error
}

// Write consumes one chunk of cat-file stdout. Chunk boundaries are arbitrary,
// so a line may span two calls; io.Writer requires reporting the full chunk as written.
func (p *blobLineParser) Write(chunk []byte) (int, error) {
	if p.err != nil {
		return 0, p.err
	}
	chunkLen := len(chunk)
	remaining := chunk
	for len(remaining) > 0 {
		newlineIdx := bytes.IndexByte(remaining, '\n')
		if newlineIdx < 0 {
			p.partial = append(p.partial, remaining...)
			break
		}
		line := remaining[:newlineIdx]
		if len(p.partial) > 0 {
			p.partial = append(p.partial, line...)
			line = p.partial
		}
		if err := p.parseLine(line); err != nil {
			p.err = err
			return 0, err
		}
		p.partial = p.partial[:0]
		remaining = remaining[newlineIdx+1:]
	}
	return chunkLen, nil
}

// flush parses any trailing line not terminated by a newline.
func (p *blobLineParser) flush() error {
	if len(p.partial) == 0 {
		return nil
	}
	err := p.parseLine(p.partial)
	p.partial = p.partial[:0]
	return err
}

func (p *blobLineParser) parseLine(line []byte) error {
	line = bytes.TrimRight(line, "\r")
	if len(bytes.TrimSpace(line)) == 0 {
		return nil
	}
	fields := bytes.Fields(line)
	if len(fields) != 3 {
		return fmt.Errorf("malformed git cat-file output line: %q", line)
	}
	size, err := strconv.ParseInt(string(fields[1]), 10, 64)
	if err != nil || size < 0 {
		return fmt.Errorf("malformed git cat-file output line (bad size): %q", line)
	}
	if !bytes.Equal(fields[0], []byte("blob")) {
		return nil
	}
	p.result.TotalBlobs++
	if size > p.threshold {
		p.result.Oversized = append(p.result.Oversized, BlobInfo{SHA: string(fields[2]), Size: size})
	}
	return nil
}
