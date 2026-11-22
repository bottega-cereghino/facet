package workspace

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/bottega-cereghino/facet/internal/gitx"
)

// pushedRefs is what counts as pushed for one repository: the refs under
// refs/remotes/<name>/ of each configured remote whose repository lies outside
// the directory reap deletes. fetchRemotes refreshes and prunes exactly those
// refs, whatever the remote's own refspec says. A ref under refs/remotes that
// no configured remote owns (its config section removed by hand) is refreshed
// by nothing, so it proves nothing; nor does a ref of a remote that reap
// deletes with the workspace (the clone itself, a sibling clone, a bare backup
// inside it).
type pushedRefs struct {
	// globs are the rev-list arguments naming those refs.
	globs []string
	// origin is set when origin is one of the remotes counted, so its default
	// branch can prove a squash-merge landed.
	origin bool
}

// not is the rev-list tail that excludes what pushed counts: "--not" and the
// globs. With no remote counted, every commit is someone's only copy.
func (pushed pushedRefs) not() []string {
	return append([]string{"--not"}, pushed.globs...)
}

// countedRemotes reads the remotes of the repository git runs at in dir, the
// directory its fetches ran in, and keeps those whose repository lies outside
// gone, the directory reap deletes (remoteCounts). On an error it returns no remote, so
// every commit counts as unpushed, and the caller records the error.
func countedRemotes(git gitx.Runner, dir, gone string) (pushedRefs, error) {
	out, err := git.Run(dir, nil, "remote")
	if err != nil {
		return pushedRefs{}, err
	}
	names := strings.Fields(out)
	counted := map[string]bool{}
	for _, name := range names {
		url, err := git.Run(dir, nil, "ls-remote", "--get-url", "--", name)
		if err != nil {
			return pushedRefs{}, fmt.Errorf("read the URL of remote %s: %w", name, err)
		}
		counted[name] = remoteCounts(git, strings.TrimSpace(url), dir, gone)
	}
	// A remote named a/b fetches into refs/remotes/a/b/, inside a's
	// namespace: when a counts and a/b does not, a's glob would take a/b's
	// refs, so a is not counted either.
	for _, name := range names {
		for _, other := range names {
			if counted[name] && !counted[other] && strings.HasPrefix(other, name+"/") {
				counted[name] = false
			}
		}
	}
	var pushed pushedRefs
	for _, name := range names {
		if counted[name] {
			pushed.globs = append(pushed.globs, "--glob=refs/remotes/"+name+"/*")
			pushed.origin = pushed.origin || name == "origin"
		}
	}
	return pushed, nil
}

// remoteCounts reports whether the remote at url is a copy that outlives
// gone, the directory reap deletes. A remote on another host is. A local one
// is when the repository git reads there lies outside gone, resolved as git
// resolves it (#364): the path itself, its git directory through a gitfile or
// a symlinked .git, and the common directory a worktree's git directory names;
// and when none of the object directories it borrows through alternates lies
// inside gone. A local remote git cannot read as a repository proves nothing.
// A bundle file outside gone is a copy.
func remoteCounts(git gitx.Runner, url, dir, gone string) bool {
	path, local := localPath(url, dir)
	if !local {
		return true
	}
	root := realPath(gone)
	if isUnder(root, resolvePath(path)) {
		return false
	}
	// git tries the path, then the path with .git appended.
	fi, err := os.Stat(path)
	if err != nil {
		path += ".git"
		if fi, err = os.Stat(path); err != nil {
			return false
		}
		if isUnder(root, resolvePath(path)) {
			return false
		}
	}
	if fi.Mode().IsRegular() {
		return isBundle(path)
	}
	out, err := git.Run(path, nil, "rev-parse", "--path-format=absolute", "--git-dir", "--git-common-dir")
	if err != nil {
		return false
	}
	dirs := strings.Fields(out)
	if len(dirs) != 2 {
		return false
	}
	for _, d := range dirs {
		if isUnder(root, realPath(d)) {
			return false
		}
	}
	// The objects directory itself may be a link into the workspace (#382),
	// as an alternate may.
	objects := filepath.Join(realPath(dirs[1]), "objects")
	if isUnder(root, realPath(objects)) {
		return false
	}
	inside, err := alternateInside(objects, gone)
	return err == nil && inside == ""
}

// localPath returns the path url names when it names a repository on this
// disk. As git reads a URL: one with "://" is local only as file://; one with
// a colon before any slash is scp-like (host:path); anything else is a path,
// relative to dir, where the fetch ran.
func localPath(url, dir string) (string, bool) {
	path, isFile := strings.CutPrefix(url, "file://")
	switch {
	case isFile:
	case strings.Contains(url, "://"):
		return "", false
	default:
		if colon := strings.Index(url, ":"); colon >= 0 && !strings.Contains(url[:colon], "/") {
			return "", false
		}
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(dir, path)
	}
	return path, true
}

// resolvePath is path with the symlinks of its deepest existing ancestor
// resolved, so a path that does not exist yet compares with real paths.
func resolvePath(path string) string {
	path = filepath.Clean(path)
	if r, err := filepath.EvalSymlinks(path); err == nil {
		return r
	}
	parent := filepath.Dir(path)
	if parent == path {
		return path
	}
	return filepath.Join(resolvePath(parent), filepath.Base(path))
}

// isBundle reports whether the file at path starts with a git bundle's
// header. git fetches from a bundle file, but upload-pack also follows a
// gitfile (`gitdir: <path>`), so a remote URL naming a file can be the clone
// itself (#380): only a bundle is a copy of its own.
func isBundle(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	head := make([]byte, len("# v2 git bundle\n"))
	if _, err := io.ReadFull(f, head); err != nil {
		return false
	}
	return string(head) == "# v2 git bundle\n" || string(head) == "# v3 git bundle\n"
}
