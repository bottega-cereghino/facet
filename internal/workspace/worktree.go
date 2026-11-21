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
	var env []string
	if skipLFS {
		env = append(env, "GIT_LFS_SKIP_SMUDGE=1")
	}
	if _, err := git.Run(checkout, env, "worktree", "add", "--detach", "--quiet", "--", dir, base); err != nil {
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

// removeWorktree removes the linked worktree at dir through its checkout, so the
// checkout forgets it and its branch is free to be checked out again. reap has
// decided by now, so --force is given twice: once for its changes, once for a
// lock. When git still refuses (a worktree holding submodules cannot be
// removed), the directory is deleted and the checkout pruned instead.
func removeWorktree(git gitx.Runner, checkout, dir string) error {
	if _, err := git.Run(checkout, nil, "worktree", "remove", "--force", "--force", "--", dir); err == nil {
		return nil
	}
	// Delete by hand only what the checkout names as one of its worktrees,
	// never a folder that merely sits inside one.
	list, err := git.Run(checkout, nil, "worktree", "list", "--porcelain")
	if err != nil {
		return fmt.Errorf("could not remove the worktree %s, and could not list %s's worktrees to check it: %w", dir, checkout, err)
	}
	if !listed(linkedWorktrees(list), dir) {
		return fmt.Errorf("%s is not a worktree %s lists; left alone\nfix: `git -C %s worktree list` shows its worktrees", dir, checkout, checkout)
	}
	if err := removeAllForce(dir); err != nil {
		return err
	}
	if _, err := git.Run(checkout, nil, "worktree", "prune"); err != nil {
		return fmt.Errorf("deleted the worktree %s, but %s still lists it: %w\nfix: run `git -C %s worktree prune`", dir, checkout, err, checkout)
	}
	if _, err := os.Lstat(dir); err == nil {
		return fmt.Errorf("could not delete the worktree %s", dir)
	}
	return nil
}

// InspectWorktree gathers the state of a plain linked worktree, one made by
// `git worktree add` by hand or by an agent (Claude Code keeps its own under
// .claude/worktrees), for reap. It is judged as a worktree clone is: by its
// uncommitted files and the commits only its HEAD reaches. Its branch's pull
// request is looked up too, since a worktree is usually one pull request's
// work. A directory that is a repository of its own is refused: reap never
// deletes a checkout.
func InspectWorktree(dir string, git gitx.Runner, pr PRLookup, mux LiveChecker) (*IssueState, error) {
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
	repo, clonePR := githubRepo(gitx.Origin(git, dir)), pr
	if repo == "" {
		clonePR = nil
	}
	c := inspectClone(git, filepath.Base(dir), dir, repo, clonePR)
	// The worktree comes first: Reap removes it through Clone[0].WorktreeOf.
	clones := append([]CloneState{c}, submoduleStates(git, filepath.Base(dir), dir)...)
	st := &IssueState{Dir: dir, Name: filepath.Base(dir), Worktree: true, Clone: clones}
	if clonePR != nil && c.Branch != "" && c.Branch != "HEAD" {
		// As for an issue workspace: a lookup that fails is not "no PR".
		if found, err := clonePR.ViewPR(repo, c.Branch); err == nil {
			st.PR = found
		} else {
			st.PRUnknown = true
		}
	}
	st.checkRunning(dir, mux)
	st.SizeBytes = freedBytes(dir)
	return st, nil
}

// ProjectWorktrees lists the linked worktrees of the repositories directly
// under projects, each once, leaving out those under skip (a workspace's
// worktree clones are reaped with their workspace) and those whose directory
// is gone (`git worktree prune` forgets them). A checkout git cannot list is
// named in unchecked.
func ProjectWorktrees(git gitx.Runner, projects, skip string) (out, unchecked []string, err error) {
	entries, err := os.ReadDir(projects)
	if err != nil {
		return nil, nil, err
	}
	skipReal := realPath(skip)
	seen := map[string]bool{}
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
		for _, wt := range linkedWorktrees(list) {
			key := realPath(wt)
			if seen[key] || (skipReal != "" && isUnder(skipReal, key)) {
				continue
			}
			seen[key] = true
			if fi, err := os.Stat(wt); err != nil || !fi.IsDir() {
				continue
			}
			out = append(out, wt)
		}
	}
	return out, unchecked, nil
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
