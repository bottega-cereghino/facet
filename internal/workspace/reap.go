package workspace

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/bottega-cereghino/facet/internal/gitx"
)

// Reap deletes an issue workspace. It never touches the shared mirror: removing a
// hardlinked object file only drops that name, and the mirror keeps its own.
//
// Callers must check Blockers() first; Reap does not, so that --force has
// somewhere to go.
//
// A clone that is a worktree of a checkout is removed through that checkout
// first, so the checkout forgets it; its branch stays there.
func Reap(st *IssueState, git gitx.Runner) error {
	// reap is meant to be run from inside the workspace it deletes. Step out
	// first, so no git this process runs afterwards starts in a directory that
	// no longer exists.
	if wd, err := os.Getwd(); err == nil && isUnder(st.Dir, wd) {
		if err := os.Chdir(filepath.Dir(st.Dir)); err != nil {
			return fmt.Errorf("step out of %s before deleting it: %w", st.Dir, err)
		}
	}
	if st.Worktree {
		return removeWorktree(git, st.Clone[0].WorktreeOf, st.Dir)
	}
	for _, c := range st.Clone {
		if c.WorktreeOf == "" {
			continue
		}
		p := filepath.Join(st.Dir, c.Dir)
		// A clone entry that is a symlink is a link, not a clone: git would
		// follow it and remove the worktree it points at, outside the
		// workspace. removeAllForce below unlinks it and follows nothing.
		if fi, err := os.Lstat(p); err == nil && fi.Mode()&os.ModeSymlink != 0 {
			continue
		}
		if !isUnder(realPath(st.Dir), realPath(p)) {
			return fmt.Errorf("the clone %s leads to %s, outside the workspace; left alone\nfix: remove the link by hand, then reap again", p, realPath(p))
		}
		if err := removeWorktree(git, c.WorktreeOf, p); err != nil {
			return err
		}
	}
	return removeAllForce(st.Dir)
}

// isUnder reports whether path is root or lies beneath it.
func isUnder(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	// A name that only starts with ".." (`..wt`) is inside: only ".." itself,
	// or a path through it, leads out.
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// removeAllForce deletes a tree that may hold directories without write
// permission (a Go module cache, a tool that makes its output read-only), which
// plain os.RemoveAll cannot empty.
//
// DIRECTORIES ONLY. Deleting a file needs write permission on its directory,
// never on the file, so a file's own mode is left alone -- and it must be: a
// clone's objects are hardlinks to the mirror's, so a chmod on a pack here is a
// chmod on the mirror's pack (it once made every mirror pack writable on each
// reap). A directory is never hardlinked, so making one writable touches
// nothing outside the tree.
//
// WalkDir stats with Lstat, so a symlink to a directory arrives with IsDir() ==
// false and is skipped; os.Chmod follows symlinks and would otherwise change the
// target, outside the tree (facet#87).
func removeAllForce(root string) error {
	makeDirsWritable(root)
	return os.RemoveAll(root)
}

// makeDirsWritable gives the owner full permission on every directory in the
// tree at root, and changes nothing else; see removeAllForce for why only
// directories, and why a symlink is never followed. What it cannot read or
// change is left for the delete that follows to report.
func makeDirsWritable(root string) {
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if info, err := d.Info(); err == nil && info.Mode().Perm()&0o700 != 0o700 {
				_ = os.Chmod(path, info.Mode().Perm()|0o700)
			}
		}
		return nil
	})
}
