package workdir

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

var ErrNoBareRepo = errors.New("no bare repository found")
var ErrMultipleBareRepos = errors.New("multiple bare repositories found")
var ErrWikiOnly = fmt.Errorf("%w: only a wiki repository found", ErrNoBareRepo)

// RepoPaths holds the bare repositories found in an extracted archive.
type RepoPaths struct {
	Main string
	// Wiki is the companion <name>.wiki.git of Main, or "" when there is none.
	Wiki string
}

// FindBareRepo walks root recursively (depth <= 4) for *.git directories.
// It returns the single main bare repository and, when present, its companion
// wiki. A *.wiki.git directory is a companion only when the same parent
// directory contains the matching *.git repository. Multi-repo migrations are
// rejected.
func FindBareRepo(root string) (RepoPaths, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return RepoPaths{}, fmt.Errorf("failed to get absolute root path: %w", err)
	}

	var matches []string
	err = filepath.WalkDir(absRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}

		depth, err := relativeDepth(absRoot, path)
		if err != nil {
			return err
		}
		if depth > 4 {
			return filepath.SkipDir
		}

		if strings.HasSuffix(d.Name(), ".git") {
			matches = append(matches, path)
			return filepath.SkipDir
		}
		return nil
	})
	if err != nil {
		return RepoPaths{}, fmt.Errorf("failed to walk %s: %w", absRoot, err)
	}

	if len(matches) == 0 {
		return RepoPaths{}, ErrNoBareRepo
	}

	if len(matches) == 1 && strings.HasSuffix(matches[0], ".wiki.git") {
		return RepoPaths{}, fmt.Errorf("%w: %v", ErrWikiOnly, matches)
	}

	found := make(map[string]bool, len(matches))
	for _, match := range matches {
		found[match] = true
	}

	// A wiki is a companion only when its main repository was also found.
	companions := make(map[string]bool)
	for _, match := range matches {
		if wiki := wikiFor(match); found[wiki] {
			companions[wiki] = true
		}
	}

	var mains []string
	for _, match := range matches {
		if !companions[match] {
			mains = append(mains, match)
		}
	}
	if len(mains) > 1 {
		return RepoPaths{}, fmt.Errorf("%w: %v", ErrMultipleBareRepos, matches)
	}

	repos := RepoPaths{Main: mains[0]}
	if wiki := wikiFor(repos.Main); found[wiki] {
		repos.Wiki = wiki
	}
	return repos, nil
}

// FindMetadataDirs walks root recursively (depth <= 3) for directories containing
// files matching <prefix>_*.json for any supplied prefix. It returns distinct
// directory paths in stable order. No matches is not an error.
func FindMetadataDirs(root string, prefixes []string) ([]string, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("failed to get absolute root path: %w", err)
	}
	if len(prefixes) == 0 {
		return nil, nil
	}

	seen := make(map[string]bool)
	var matches []string
	err = filepath.WalkDir(absRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() {
			depth, err := relativeDepth(absRoot, path)
			if err != nil {
				return err
			}
			if depth > 3 {
				return filepath.SkipDir
			}
			return nil
		}

		parent := filepath.Dir(path)
		depth, err := relativeDepth(absRoot, parent)
		if err != nil {
			return err
		}
		if depth > 3 || seen[parent] || !isMetadataFile(d.Name(), prefixes) {
			return nil
		}

		seen[parent] = true
		matches = append(matches, parent)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("failed to walk %s: %w", absRoot, err)
	}

	slices.Sort(matches)
	return matches, nil
}

// DescendIntoSingleSubdir returns root's only visible subdirectory when archives
// are wrapped in one extra top-level directory. It ignores the .complete sentinel
// and never descends into repositories/, which is a meaningful archive root.
func DescendIntoSingleSubdir(root string) (string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return "", err
	}
	visible := make([]os.DirEntry, 0, len(entries))
	for _, entry := range entries {
		if entry.Name() == ".complete" {
			continue
		}
		visible = append(visible, entry)
	}
	if len(visible) == 1 && visible[0].IsDir() && visible[0].Name() != "repositories" {
		return filepath.Join(root, visible[0].Name()), nil
	}
	return root, nil
}

func isMetadataFile(name string, prefixes []string) bool {
	for _, prefix := range prefixes {
		if matched, err := filepath.Match(prefix+"_*.json", name); err == nil && matched {
			return true
		}
	}
	return false
}

func wikiFor(repo string) string {
	return strings.TrimSuffix(repo, ".git") + ".wiki.git"
}

func relativeDepth(root, path string) (int, error) {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return 0, err
	}
	if rel == "." {
		return 0, nil
	}
	return len(strings.Split(rel, string(os.PathSeparator))), nil
}
