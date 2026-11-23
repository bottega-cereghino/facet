package workspace

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/bottega-cereghino/facet/internal/gitx"
	"github.com/bottega-cereghino/facet/internal/mirror"
)

// A clone can be a linked worktree of the checkout of the same repository under
// the projects root, instead of a clone of its own. It then costs its working
// tree and nothing else: no second .git, and no object sharing with a mirror to
// fade once either side repacks.
//
// The checkout is someone's working copy, so facet does three things there and
// nothing more: it fetches origin, adds the worktree, and, at reap, removes it.
// The checkout's own branch, index and files are never touched.

// projectCheckout returns the checkout under projects that the clone name of
// url can be a worktree of, or "" when there is none. It looks at
// projects/<name> and projects/<repository name>, and takes the first that is a
// non-bare repository whose origin is url: a directory of the right name that
// tracks some other repository is never used.
func projectCheckout(git gitx.Runner, projects, name, url string) string {
	if projects == "" {
		return ""
	}
	seen := map[string]bool{}
	for _, cand := range []string{name, repoName(url)} {
		if cand == "" || seen[cand] || cand == "." || cand == ".." || strings.ContainsAny(cand, `/\`) {
			continue
		}
		seen[cand] = true
		dir := filepath.Join(projects, cand)
		if !gitx.IsRepo(dir) || !sameRepoURL(gitx.Origin(git, dir), url) {
			continue
		}
		if out, err := git.Run(dir, nil, "rev-parse", "--is-bare-repository"); err != nil || strings.TrimSpace(out) != "false" {
			continue
		}
		return dir
	}
	return ""
}

// repoName is the last segment of a repository URL or path, without ".git":
// "platform" for git@github.com:acme/platform.git.
func repoName(url string) string {
	s := strings.TrimRight(strings.ReplaceAll(url, `\`, "/"), "/")
	if i := strings.LastIndexAny(s, "/:"); i >= 0 {
		s = s[i+1:]
	}
	return strings.TrimSuffix(s, ".git")
}

// sameRepoURL reports whether two origins name the same repository. Remote URLs
// are compared by host and path, so the SSH and HTTPS forms of one repository
// match; local paths are compared as cleaned paths.
func sameRepoURL(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	if a == b {
		return true
	}
	pa, errA := mirror.PathFor("/", a)
	pb, errB := mirror.PathFor("/", b)
	switch {
	case errA == nil && errB == nil:
		return pa == pb
	case errors.Is(errA, mirror.ErrNotRemote) && errors.Is(errB, mirror.ErrNotRemote):
		return filepath.Clean(a) == filepath.Clean(b)
	}
	return false
}

// addWorktree adds dir as a linked worktree of checkout, on a detached HEAD at
// origin's default branch. It fetches origin first; a fetch that fails is a
// warning, and the worktree starts from what the checkout already has.
//
// ok is false, with no error, when origin's default branch cannot be named: the
// caller then clones as before.
func addWorktree(git gitx.Runner, checkout, dir string, skipLFS bool, rep Reporter) (ok bool, err error) {
	if tr, isTR := git.(timeoutRunner); isTR {
		_, err = tr.RunTimeout(checkout, nil, fetchTimeout, "fetch", "origin")
	} else {
		_, err = git.Run(checkout, nil, "fetch", "origin")
	}
	if err != nil {
		rep.Warn("%s: fetch failed, the worktree starts from the last fetch (%v)", checkout, err)
	}
	base := remoteDefaultBranch(git, checkout)
	if base == "" {
		rep.Note("%s: origin's default branch is unknown, so a clone is made instead of a worktree", checkout)
		return false, nil
	}
	tmpRoot, err := os.MkdirTemp(filepath.Dir(dir), ".facet-clone-*")
	if err != nil {
		return false, err
	}
	defer func() { _ = os.RemoveAll(tmpRoot) }()
	stage := filepath.Join(tmpRoot, filepath.Base(dir))

	var env []string
	if skipLFS {
		env = append(env, "GIT_LFS_SKIP_SMUDGE=1")
	}
	if _, err := git.Run(checkout, env, "worktree", "add", "--detach", "--quiet", "--", stage, base); err != nil {
		return false, err
	}
	if err := placeStaged(stage, dir); err != nil {
		_ = removeWorktree(git, checkout, stage)
		return false, err
	}
	if _, err := git.Run(checkout, nil, "worktree", "repair", "--", dir); err != nil {
		return false, err
	}
	return true, nil
}

// worktreeOf returns the checkout whose repository dir is a linked worktree
// of, or "" when dir is a repository of its own. The checkout is the parent of
// the common git directory, as `git worktree add` lays it out.
func worktreeOf(git gitx.Runner, dir string) (string, error) {
	out, err := git.Run(dir, nil, "rev-parse", "--path-format=absolute", "--git-dir", "--git-common-dir")
	if err != nil {
		return "", err
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 {
		return "", fmt.Errorf("git rev-parse: unexpected output %q", out)
	}
	gitDir, common := filepath.Clean(lines[0]), filepath.Clean(lines[1])
	if gitDir == common {
		return "", nil
	}
	if path.Base(filepath.ToSlash(common)) == ".git" {
		return filepath.Dir(common), nil
	}
	return common, nil
}

// TopLevel reports whether dir is the top level of a git work tree, and
// whether that work tree is a linked worktree. A directory git reads as no
// repository, or as a folder inside one (a workspace inside a versioned
// ~/Workspaces), is neither.
func TopLevel(git gitx.Runner, dir string) (top, linked bool) {
	out, err := git.Run(dir, nil, "rev-parse", "--show-toplevel")
	if err != nil || realPath(strings.TrimSpace(out)) != realPath(dir) {
		return false, false
	}
	of, err := worktreeOf(git, dir)
	return true, err == nil && of != ""
}

// removeWorktree removes the linked worktree at dir through its checkout, so the
// checkout forgets it and its branch is free to be checked out again. reap has
// decided by now, so --force is given twice: once for its changes, once for a
// lock.
//
// Only what the checkout lists as one of its worktrees is removed, never a
// folder that merely sits inside one, and the listing is read first: git goes
// on after a delete that fails partway (a directory without write permission)
// and drops the worktree's admin directory, so afterwards the checkout no
// longer lists a worktree whose HEAD and index are gone (#316). Directories are
// made writable before git runs, so it can empty them. When git still fails (a
// worktree holding submodules cannot be removed), the rest is deleted by hand,
// and the checkout's entry for this worktree alone is dropped.
func removeWorktree(git gitx.Runner, checkout, dir string) error {
	list, err := git.Run(checkout, nil, "worktree", "list", "--porcelain")
	if err != nil {
		return fmt.Errorf("could not list %s's worktrees to check %s: %w", checkout, dir, err)
	}
	if !listed(linkedWorktrees(list), dir) {
		return fmt.Errorf("%s is not a worktree %s lists; left alone\nfix: `git -C %s worktree list` shows its worktrees", dir, checkout, checkout)
	}
	// Resolved now, while it exists: once deleted, /var no longer resolves to
	// the /private/var git lists.
	resolved := realPath(dir)
	makeDirsWritable(dir)
	if _, err := git.Run(checkout, nil, "worktree", "remove", "--force", "--force", "--", dir); err == nil {
		return nil
	}
	if err := removeAllForce(dir); err != nil {
		return fmt.Errorf("git removed part of the worktree %s, and the rest could not be deleted: %w\nfix: delete %s by hand, then run `git -C %s worktree remove --force --force %s`", dir, err, dir, checkout, dir)
	}
	if _, err := os.Lstat(dir); err == nil {
		return fmt.Errorf("could not delete the worktree %s", dir)
	}
	// `git worktree prune` would also drop every other prunable worktree, one
	// on a disk that is not mounted included. remove of a directory that is
	// gone drops its entry alone.
	if list, err := git.Run(checkout, nil, "worktree", "list", "--porcelain"); err != nil || listed(linkedWorktrees(list), resolved) {
		if _, err := git.Run(checkout, nil, "worktree", "remove", "--force", "--force", "--", resolved); err != nil {
			return fmt.Errorf("deleted the worktree %s, but %s still lists it: %w\nfix: run `git -C %s worktree remove --force --force %s`", dir, checkout, err, checkout, dir)
		}
	}
	return nil
}

// InspectWorktree gathers the state of a plain linked worktree, one made by
// `git worktree add` by hand or by an agent (Claude Code keeps its own under
// .claude/worktrees), for reap. It is judged as a worktree clone is: by its
// uncommitted files and the commits only its HEAD reaches. Its branch's pull
// request is looked up too, since a worktree is usually one pull request's
// work. A directory that is a repository of its own is refused: reap never
// deletes a checkout. It is inspected, and deleted, at its real path.
func InspectWorktree(dir string, git gitx.Runner, pr PRLookup, mux LiveChecker) (*IssueState, error) {
	// The real path, so a worktree named through a symlink is walked: a walk
	// does not descend into a symlink at its root (#381).
	dir = realPath(dir)
	of, err := worktreeOf(git, dir)
	if err != nil {
		return nil, fmt.Errorf("%s: cannot tell whether it is a worktree: %w", dir, err)
	}
	if of == "" {
		return nil, fmt.Errorf("%s is neither a workspace nor a linked worktree: it has a repository of its own, and reap never deletes a checkout\n"+
			"fix: name a workspace, or a worktree that `git worktree list` shows", dir)
	}
	// A folder inside a worktree resolves to the same repository; reaping it
	// would delete that folder alone and leave the worktree.
	top, err := git.Run(dir, nil, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, fmt.Errorf("%s: cannot find the worktree's top level: %w", dir, err)
	}
	if top = strings.TrimSpace(top); realPath(top) != realPath(dir) {
		return nil, fmt.Errorf("%s is a folder inside the worktree %s, not a worktree\nfix: facet reap %s", dir, top, top)
	}
	origin := gitx.Origin(git, dir)
	repo, clonePR := githubRepo(origin), pr
	if repo == "" {
		clonePR = nil
	}
	c := inspectClone(git, filepath.Base(dir), dir, dir, repo, clonePR)
	// The worktree comes first: Reap removes it through Clone[0].WorktreeOf.
	clones := append([]CloneState{c}, submoduleStates(git, filepath.Base(dir), dir, dir)...)
	st := &IssueState{Dir: dir, Name: filepath.Base(dir), Worktree: true, Clone: clones}
	st.Unaccounted, st.UnaccountedCount = census(git, dir, dir, "", cloneDirs(filepath.Dir(dir), clones), nil)
	if clonePR != nil && c.Branch != "" && c.Branch != "HEAD" {
		// As for an issue workspace: a lookup that fails is not "no PR".
		if found, err := clonePR.ViewPR(repo, c.Branch); err == nil {
			st.PR = found
		} else {
			st.PRUnknown = true
		}
	} else if pr != nil && networkRemote(origin) && repo == "" && c.Branch != "" && c.Branch != "HEAD" {
		// An origin on a host that is not plain github.com (an SSH host alias,
		// an Enterprise host) cannot be asked about its pull requests: an open
		// one cannot be ruled out, so the state is unknown, not "none" (#325).
		// A local path or file:// origin has no pull requests to ask about.
		st.PRUnknown = true
	}
	st.checkRunning(dir, mux)
	st.SizeBytes = freedBytes(dir)
	return st, nil
}

// ProjectWorktrees lists, for reap --reapable, the linked worktrees of the
// checkouts directly under projects. It offers only a worktree inside its
// checkout's own directory, as Claude Code's .claude/worktrees are: one
// elsewhere, beside the checkout or in a folder of its own, may be a long-lived
// working copy, a service run from a tag with its ignored data beside the code.
// It offers none of a bare repository's: there every working copy is a linked
// worktree, the everyday one included. Those are still reaped by path, one at
// a time. It leaves out those under skip (a workspace's
// worktree clones are reaped with their workspace) and those whose directory
// is gone (`git worktree prune` forgets them). A checkout git cannot list is
// named in unchecked.
func ProjectWorktrees(git gitx.Runner, projects, skip string) (out, unchecked []string, err error) {
	entries, err := os.ReadDir(projects)
	if err != nil {
		return nil, nil, err
	}
	skipReal := realPath(skip)
	for _, e := range entries {
		dir := filepath.Join(projects, e.Name())
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") || !gitx.IsRepo(dir) {
			continue
		}
		list, err := git.Run(dir, nil, "worktree", "list", "--porcelain")
		if err != nil {
			unchecked = append(unchecked, fmt.Sprintf("%s: git worktree list failed: %v", dir, err))
			continue
		}
		if mainIsBare(list) {
			continue
		}
		// A worktree directly under projects lists its checkout's worktrees
		// too, but they are offered only through the checkout's own entry,
		// as the ones inside it.
		of, err := worktreeOf(git, dir)
		if err != nil {
			unchecked = append(unchecked, fmt.Sprintf("%s: cannot tell whether it is a worktree: %v", dir, err))
			continue
		}
		if of != "" {
			continue
		}
		root := realPath(dir)
		for _, wt := range linkedWorktrees(list) {
			key := realPath(wt)
			if !isUnder(root, key) || (skipReal != "" && isUnder(skipReal, key)) {
				continue
			}
			if fi, err := os.Stat(wt); err != nil || !fi.IsDir() {
				continue
			}
			out = append(out, wt)
		}
	}
	return out, unchecked, nil
}

// mainIsBare reads `git worktree list --porcelain` for whether the repository
// is bare: its main worktree, the first entry, is then marked "bare", and every
// working copy is a linked worktree.
func mainIsBare(porcelain string) bool {
	for _, line := range strings.Split(porcelain, "\n") {
		if line == "" {
			return false
		}
		if line == "bare" {
			return true
		}
	}
	return false
}

// listed reports whether dir is one of worktrees, comparing resolved paths.
func listed(worktrees []string, dir string) bool {
	want := realPath(dir)
	for _, w := range worktrees {
		if realPath(w) == want {
			return true
		}
	}
	return false
}

// realPath resolves symlinks in p, so two spellings of one directory compare
// equal (/var and /private/var on macOS); p itself when it cannot.
func realPath(p string) string {
	if p == "" {
		return ""
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}

// networkRemote reports whether a remote URL names another host: a URL with a
// scheme other than file://, or scp-like user@host:path. A local path, a
// file:// URL and "" are not.
func networkRemote(url string) bool {
	switch {
	case url == "", strings.HasPrefix(url, "file://"):
		return false
	case strings.Contains(url, "://"):
		return true
	}
	// scp-like syntax: a colon before any slash, as git reads it.
	colon := strings.IndexByte(url, ':')
	slash := strings.IndexByte(url, '/')
	return colon > 0 && (slash < 0 || colon < slash)
}
