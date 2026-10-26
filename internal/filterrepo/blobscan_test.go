package filterrepo

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const blobScanFixture = `commit 250 1111111111111111111111111111111111111111
tree 120 2222222222222222222222222222222222222222
blob 100 aaaa000000000000000000000000000000000000
blob 101 bbbb000000000000000000000000000000000000
tag 5000 3333333333333333333333333333333333333333
blob 5000 cccc000000000000000000000000000000000000
blob 5000 1234000000000000000000000000000000000000
blob 7 dddd000000000000000000000000000000000000
`

func TestScanBlobSizes_FiltersSortsAndCounts(t *testing.T) {
	exec := &fakeExecer{stdoutData: blobScanFixture}
	rec := &recordingLogger{}
	r := New(exec, rec)

	res, err := r.ScanBlobSizes(context.Background(), "/repo.git", 100)
	require.NoError(t, err)

	require.Len(t, exec.calls, 1)
	assert.Equal(t, []string{
		"git", "-c", "safe.bareRepository=all", "cat-file", "--batch-all-objects",
		"--batch-check=%(objecttype) %(objectsize) %(objectname)",
	}, exec.calls[0])
	assert.Contains(t, rec.msgs, "scanning blob sizes (git cat-file --batch-all-objects)")

	// commit/tree/tag ignored even when large; exactly-threshold (100) not
	// flagged; threshold+1 (101) flagged; Size desc then SHA asc.
	assert.Equal(t, 5, res.TotalBlobs)
	assert.Equal(t, []BlobInfo{
		{SHA: "1234000000000000000000000000000000000000", Size: 5000},
		{SHA: "cccc000000000000000000000000000000000000", Size: 5000},
		{SHA: "bbbb000000000000000000000000000000000000", Size: 101},
	}, res.Oversized)
}

func TestScanBlobSizes_PartialLineWrites(t *testing.T) {
	// No trailing newline exercises the post-Run flush of a partial line.
	data := strings.TrimRight(blobScanFixture, "\n")
	for _, chunk := range []int{1, 2, 3, 7, 13, 41} {
		exec := &fakeExecer{stdoutData: data, stdoutChunk: chunk}
		res, err := New(exec, nil).ScanBlobSizes(context.Background(), "/repo.git", 100)
		require.NoError(t, err, "chunk=%d", chunk)
		assert.Equal(t, 5, res.TotalBlobs, "chunk=%d", chunk)
		require.Len(t, res.Oversized, 3, "chunk=%d", chunk)
		assert.EqualValues(t, 101, res.Oversized[2].Size, "chunk=%d", chunk)
	}
}

func TestScanBlobSizes_TrailingPartialLineFlagged(t *testing.T) {
	exec := &fakeExecer{stdoutData: "blob 10 aaaa\nblob 999 ffff", stdoutChunk: 5}
	res, err := New(exec, nil).ScanBlobSizes(context.Background(), "/repo.git", 100)
	require.NoError(t, err)
	assert.Equal(t, 2, res.TotalBlobs)
	assert.Equal(t, []BlobInfo{{SHA: "ffff", Size: 999}}, res.Oversized)
}

func TestScanBlobSizes_EmptyOutput(t *testing.T) {
	res, err := New(&fakeExecer{}, nil).ScanBlobSizes(context.Background(), "/repo.git", 100)
	require.NoError(t, err)
	assert.Zero(t, res.TotalBlobs)
	assert.Empty(t, res.Oversized)
}

func TestScanBlobSizes_ExecErrorPropagated(t *testing.T) {
	exec := &fakeExecer{
		stdoutData: "blob 5000 aaaa\n",
		stderrData: "fatal: not a git repository",
		runErr:     errors.New("exit status 128"),
	}
	res, err := New(exec, nil).ScanBlobSizes(context.Background(), "/repo.git", 100)
	require.Error(t, err)
	assert.Nil(t, res, "a failed scan must never look like zero oversized blobs")
	assert.Contains(t, err.Error(), "exit status 128")
	assert.Contains(t, err.Error(), "not a git repository")
}

func TestScanBlobSizes_MalformedLine(t *testing.T) {
	cases := map[string]string{
		"too few fields":  "blob 100\n",
		"too many fields": "blob 100 aaaa extra\n",
		"bad size":        "blob big aaaa\n",
		"negative size":   "blob -5 aaaa\n",
		"trailing bad":    "blob 1 aaaa\nnonsense",
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := New(&fakeExecer{stdoutData: data}, nil).ScanBlobSizes(context.Background(), "/repo.git", 100)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "malformed")
		})
	}
}

func TestScanBlobSizes_RejectsNonPositiveThreshold(t *testing.T) {
	for _, th := range []int64{0, -1} {
		exec := &fakeExecer{}
		_, err := New(exec, nil).ScanBlobSizes(context.Background(), "/repo.git", th)
		require.Error(t, err)
		assert.Empty(t, exec.calls)
	}
}

func TestScanBlobSizes_RequiresBareRepoPath(t *testing.T) {
	_, err := New(&fakeExecer{}, nil).ScanBlobSizes(context.Background(), "", 100)
	require.Error(t, err)
}
