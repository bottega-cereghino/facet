package workspace

import (
	"fmt"
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
// directory its fetches ran in, and keeps those whose URL resolves outside
// gone, the directory reap deletes. On an error it returns no remote, so
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
		counted[name] = !localInside(strings.TrimSpace(url), dir, gone)
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

// localInside reports whether url names a repository on this disk inside
// gone. As git reads a URL: one with "://" is local only as file://; one
// with a colon before any slash is scp-like (host:path); anything else is a
// path, relative to dir, where the fetch ran.
func localInside(url, dir, gone string) bool {
	path, isFile := strings.CutPrefix(url, "file://")
	switch {
	case isFile:
	case strings.Contains(url, "://"):
		return false
	default:
		if colon := strings.Index(url, ":"); colon >= 0 && !strings.Contains(url[:colon], "/") {
			return false
		}
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(dir, path)
	}
	return isUnder(realPath(gone), resolvePath(path))
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
