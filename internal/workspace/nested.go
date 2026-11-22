package workspace

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	pathpkg "path"
	"path/filepath"
	"strings"

	"github.com/bottega-cereghino/facet/internal/gitx"
)

// unjudgedRepos walks the whole work tree of clone once, ignored directories
// included, and returns every place a repository sits that no check judges,
// and every submodule directory that holds files with no repository (#364).
//
// A repository is any entry marksRepository names (git, any other VCS, a bare
// repository), checked as the walk passes it, so the tree is read once. One
// at a gitlink or in a directory the clone tracks files in, with a .git, is
// judged as a clone of its own (submoduleStates) and not walked; any other
// holds: a fork cloned into an ignored vendor/, a Mercurial .hg a global
// gitignore hides, Jujutsu colocated at the clone's root, a bare backup a
// remote points at, an editable install in a .venv. A gitlink directory with
// no .git is a submodule not checked out: empty, it holds nothing; with files
// in it, those are in no repository (stray).
//
// A marker git tracks, or holds tracked files in, is committed content (a
// test fixture), not a repository, and is walked as any directory.
//
// The walk never follows a symlink and never enters a repository it met. The
// worktrees in known are left out, being judged already. A tree that cannot
// be walked is an error, never "none found".
func unjudgedRepos(git gitx.Runner, clone string, known []string) (repos, stray []string, err error) {
	idx, err := readIndex(git, clone)
	if err != nil {
		return nil, nil, err
	}
	skip := map[string]bool{}
	for _, w := range known {
		skip[realPath(w)] = true
	}
	var found []string // repositories met; nothing under one is walked
	err = filepath.WalkDir(clone, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if under(path, found) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if path != clone && d.IsDir() {
			if skip[realPath(path)] {
				return filepath.SkipDir
			}
			rel, err := filepath.Rel(clone, path)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			if judged, err := judgedRepo(path, rel, idx); err != nil {
				return err
			} else if judged != "" {
				if judged != path {
					repos = append(repos, judged)
				}
				return filepath.SkipDir
			}
			if idx.gitlinks[rel] {
				if empty, err := isEmptyDir(path); err != nil {
					return err
				} else if !empty {
					stray = append(stray, rel)
				}
				return filepath.SkipDir
			}
		}
		parent := filepath.Dir(path)
		if path == clone || !marksRepository(parent, d) || idx.tracks(clone, path) {
			return nil
		}
		if parent == clone {
			// The clone's own .git is judged, and its git directory is not
			// walked. Any other marker at the root is another repository
			// sharing the work tree, such as Jujutsu colocated with git.
			if d.Name() != ".git" {
				repos = append(repos, path)
			}
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		found = append(found, parent)
		repos = append(repos, parent)
		// A directory marker skips itself; a file marker, the rest of parent.
		// Siblings walked later are caught by under.
		return filepath.SkipDir
	})
	if err != nil {
		return repos, stray, fmt.Errorf("walk %s: %w", clone, err)
	}
	return repos, stray, nil
}

// judgedRepo reports whether the directory path, at rel in the work tree, is
// a repository submoduleStates judges as a clone: one with a .git at a gitlink
// or in a tracked directory. It returns path when it is, or the first other
// repository marker beside that .git, which no check judges; "" when it is
// not judged.
func judgedRepo(path, rel string, idx index) (string, error) {
	if !idx.gitlinks[rel] && !idx.dirs[rel] {
		return "", nil
	}
	if _, err := os.Lstat(filepath.Join(path, ".git")); err != nil {
		return "", nil
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		if e.Name() != ".git" && marksRepository(path, e) {
			return filepath.Join(path, e.Name()), nil
		}
	}
	return path, nil
}

// index is what a work tree's index names, by slash-separated paths relative
// to it: the files and gitlinks, and every directory holding one.
type index struct {
	files, gitlinks, dirs map[string]bool
}

// readIndex reads the stage entries of the index of the work tree p.
func readIndex(git gitx.Runner, p string) (index, error) {
	out, err := git.Run(p, nil, "ls-files", "--stage", "-z")
	if err != nil {
		return index{}, err
	}
	idx := index{files: map[string]bool{}, gitlinks: map[string]bool{}, dirs: map[string]bool{}}
	for _, entry := range strings.Split(out, "\x00") {
		meta, file, ok := strings.Cut(entry, "\t")
		if !ok {
			continue
		}
		if strings.HasPrefix(meta, "160000 ") {
			idx.gitlinks[file] = true
		} else {
			idx.files[file] = true
		}
		for d := pathpkg.Dir(file); d != "." && !idx.dirs[d]; d = pathpkg.Dir(d) {
			idx.dirs[d] = true
		}
	}
	return idx, nil
}

// tracks reports whether the index names path, in the work tree p, as a file
// or as a directory holding one.
func (idx index) tracks(p, path string) bool {
	rel, err := filepath.Rel(p, path)
	if err != nil {
		return false
	}
	rel = filepath.ToSlash(rel)
	return idx.files[rel] || idx.dirs[rel]
}

// isEmptyDir reports whether the directory path has no entry at all.
func isEmptyDir(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Readdirnames(1); err == io.EOF {
		return true, nil
	} else if err != nil {
		return false, err
	}
	return false, nil
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
