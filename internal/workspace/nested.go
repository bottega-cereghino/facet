package workspace

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"

	"github.com/bottega-cereghino/facet/internal/gitx"
)

// ignoredRepos finds the repositories inside the directories clone ignores:
// a fork cloned into vendor/ to patch, a scratch clone in tmp/, an editable
// install in a .venv, a bare backup a remote points at. Each has commits of
// its own that no check of the clone sees, and deleting the clone deletes
// them. A repository is any entry marksRepository names (git, any other VCS,
// a bare repository), checked as the walk passes it, so the tree is read once.
// The worktrees in known are left out, being judged already. A tree that
// cannot be walked is an error, never "none found".
func ignoredRepos(git gitx.Runner, clone string, known []string) ([]string, error) {
	dirs, err := ignoredDirs(git, clone)
	if err != nil {
		return nil, err
	}
	skip := map[string]bool{}
	for _, w := range known {
		skip[realPath(w)] = true
	}
	var out []string
	for _, rel := range sortedKeys(dirs) {
		root := filepath.Join(clone, filepath.FromSlash(rel))
		var found []string // repositories met in this walk; nothing under one is walked
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if under(path, found) {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			parent := filepath.Dir(path)
			if path == root || !marksRepository(parent, d) {
				return nil
			}
			found = append(found, parent)
			if !skip[realPath(parent)] {
				out = append(out, parent)
			}
			// A directory marker skips itself; a file marker, the rest of
			// parent. Siblings walked later are caught by under.
			return filepath.SkipDir
		})
		if err != nil {
			return out, fmt.Errorf("walk %s: %w", root, err)
		}
	}
	return out, nil
}

// under reports whether path is one of roots or inside one.
func under(path string, roots []string) bool {
	for _, r := range roots {
		if path == r || strings.HasPrefix(path, r+string(filepath.Separator)) {
			return true
		}
	}
	return false
}
