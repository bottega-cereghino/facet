package workspace

import (
	"io/fs"
	"path/filepath"
	"strings"

	"github.com/bottega-cereghino/facet/internal/gitx"
)

// rebuiltIgnored are the names of ignored files and directories that reap
// lets go with a clone: dependency folders and build output something
// rebuilds from the code, prune's list (regenNames) and a few more, and
// Finder's .DS_Store. A path with any of these as one of its steps does not
// hold.
var rebuiltIgnored = map[string]bool{
	"target":      true, // cargo, maven
	"dist":        true, // a build's output
	"__pycache__": true, // Python's compiled bytecode
	".DS_Store":   true, // Finder's view settings
}

// rebuilt reports whether the slash-separated path, relative to a clone, lies
// in a directory something rebuilds (rebuiltIgnored, regenNames).
func rebuilt(rel string) bool {
	for _, step := range strings.Split(strings.TrimSuffix(rel, "/"), "/") {
		if _, ok := regenNames[step]; ok || rebuiltIgnored[step] {
			return true
		}
	}
	return false
}

// unrebuiltIgnored lists what the clone at p ignores that nothing rebuilds:
// a .env, a local config, notes kept out of git. Each is the clone's only
// copy (#364, part 7). What lies in a repository or worktree inside the clone is
// left out, being held already (skip).
func unrebuiltIgnored(git gitx.Runner, p string, skip []string) ([]string, error) {
	out, err := git.Run(p, nil, "ls-files", "-z", "--others", "--ignored", "--exclude-standard", "--directory", "--no-empty-directory")
	if err != nil {
		return nil, err
	}
	skip = realPaths(skip)
	var held []string
	for _, rel := range strings.Split(out, "\x00") {
		if rel == "" || rebuilt(rel) {
			continue
		}
		abs := realPath(filepath.Join(p, filepath.FromSlash(strings.TrimSuffix(rel, "/"))))
		switch {
		case under(abs, skip):
		case strings.HasSuffix(rel, "/"):
			// git names a wholly ignored directory once, but it may hold only
			// what is rebuilt (pkg/__pycache__) or a nested worktree (Claude
			// Code's .claude/worktrees): its own files are named instead.
			files, err := filesOutside(p, abs, skip)
			if err != nil {
				return held, err
			}
			held = append(held, files...)
		default:
			held = append(held, rel)
		}
	}
	return held, nil
}

// filesOutside lists the files under dir, relative to the clone p, that lie
// outside every one of skip and outside what is rebuilt. The walk never
// follows a symlink.
func filesOutside(p, dir string, skip []string) ([]string, error) {
	root := realPath(p)
	var out []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if under(path, skip) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel = filepath.ToSlash(rel); !rebuilt(rel) {
			out = append(out, rel)
		}
		return nil
	})
	return out, err
}

// realPaths is paths, each resolved.
func realPaths(paths []string) []string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		out = append(out, realPath(p))
	}
	return out
}
