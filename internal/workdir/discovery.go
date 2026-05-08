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

// FindBareRepo walks root recursively (depth <= 4) for *.git directories.
// It returns the path of the single match. Multi-repo migrations are rejected.
func FindBareRepo(root string) (string, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("failed to get absolute root path: %w", err)
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
		return "", fmt.Errorf("failed to walk %s: %w", absRoot, err)
	}

	switch len(matches) {
	case 0:
		return "", ErrNoBareRepo
	case 1:
		return matches[0], nil
	default:
		return "", fmt.Errorf("%w: %v", ErrMultipleBareRepos, matches)
	}
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
