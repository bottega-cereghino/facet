package workspace

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/bottega-cereghino/facet/internal/gitx"
)

// ignoredRepos finds the git repositories inside the directories clone
// ignores: a fork cloned into vendor/ to patch, a scratch clone in tmp/, an
// editable install in a .venv. Each has commits of its own that no check of
// the clone sees, and deleting the clone deletes them. The worktrees in known
// are left out, being judged already. A tree that cannot be walked is an
// error, never "none found".
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
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.IsDir() {
				return nil
			}
			if _, err := os.Lstat(filepath.Join(path, ".git")); err != nil {
				return nil
			}
			if !skip[realPath(path)] {
				out = append(out, path)
			}
			return filepath.SkipDir
		})
		if err != nil {
			return out, fmt.Errorf("walk %s: %w", root, err)
		}
	}
	return out, nil
}
