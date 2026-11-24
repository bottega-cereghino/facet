package workspace

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/bottega-cereghino/facet/internal/config"
	"github.com/bottega-cereghino/facet/internal/gitx"
	"github.com/bottega-cereghino/facet/internal/manifest"
)

// InspectIssue gathers the state of one issue workspace, refusing any other.
// Network and multiplexer lookups are optional: pass nil to skip them.
func InspectIssue(ws string, git gitx.Runner, pr PRLookup, mux LiveChecker) (*IssueState, error) {
	m, err := manifest.Read(ws)
	if err != nil {
		return nil, err
	}
	if !m.IsIssueWorkspace() {
		return nil, fmt.Errorf("%s is not an issue workspace", ws)
	}
	return inspect(ws, m, git, pr, mux), nil
}

// Inspect gathers the state of any workspace, issue or long-lived, for reap.
// A long-lived workspace has no issue branch, so no pull request is looked up
// for it; in exchange its loose files are checked, because unlike an issue
// workspace it can hold files of its own beside its clones.
func Inspect(ws string, git gitx.Runner, pr PRLookup, mux LiveChecker) (*IssueState, error) {
	m, err := manifest.Read(ws)
	if err != nil {
		return nil, err
	}
	return inspect(ws, m, git, pr, mux), nil
}

func inspect(ws string, m *manifest.Manifest, git gitx.Runner, pr PRLookup, mux LiveChecker) *IssueState {
	st := &IssueState{Dir: ws, Name: m.Name, Issue: m.Issue}
	for _, dir := range sortedKeys(m.Clones) {
		p := filepath.Join(ws, dir)
		if !gitx.IsRepo(p) {
			// A dir that never got cloned holds nothing to lose. But a dir that
			// exists and simply is not a valid repo -- an interrupted clone, a
			// damaged .git -- may still carry a working tree full of edits, so it
			// must block rather than vanish from the accounting.
			if _, err := os.Stat(p); err == nil {
				st.Clone = append(st.Clone, CloneState{
					Dir: dir, Unverifiable: []string{"present but not a git repository"},
				})
			}
			continue
		}
		// The squash-merge landing proof asks GitHub about the clone's own
		// repo, read from its URL, falling back to the issue's repo. A clone
		// with neither gets the ancestor check alone, which fails in the safe
		// direction: a squash-merged commit there still counts as unpushed.
		repo, clonePR := githubRepo(m.Clones[dir]), pr
		if repo == "" && m.Issue != nil {
			repo = m.Issue.Repo
		}
		if repo == "" {
			clonePR = nil
		}
		st.Clone = append(st.Clone, inspectClone(git, dir, p, ws, repo, clonePR))
		st.Clone = append(st.Clone, submoduleStates(git, dir, p, ws)...)
	}

	st.Enclosing, st.Loose = looseFiles(git, ws, m)
	st.Unaccounted, st.UnaccountedCount = census(git, ws, ws, st.Enclosing, cloneDirs(ws, st.Clone), topAccounted(ws, m))
	if pr != nil && m.Issue != nil && m.Issue.Branch != "" {
		// A lookup that errors is not "no PR": auth expiry, a network blip, or a
		// rate limit must not silently drop the open-PR guard. Record that the
		// state is unknown so Blockers refuses.
		if found, err := pr.ViewPR(m.Issue.Repo, m.Issue.Branch); err == nil {
			st.PR = found
		} else {
			st.PRUnknown = true
		}
	}
	if mux != nil {
		st.SessionLive = mux.Live(filepath.Base(ws), ws)
	}
	st.checkRunning(ws, mux)
	st.SizeBytes = freedBytes(ws)
	return st
}

// checkRunning records the panes, processes and launchd jobs running from dir,
// asked through mux's optional interfaces.
func (s *IssueState) checkRunning(dir string, mux LiveChecker) {
	if lr, ok := mux.(LiveRootChecker); ok {
		s.LiveRoots, s.LiveRootsUnknown = lr.LiveRoots(dir)
	}
	if jc, ok := mux.(JobChecker); ok {
		s.Jobs, s.JobsUnknown = jc.JobsUnder(dir)
	}
}

// linkedWorktrees reads `git worktree list --porcelain`: one "worktree <path>"
// line per worktree, the main one first. It returns the others.
func linkedWorktrees(porcelain string) []string {
	var out []string
	first := true
	for _, line := range strings.Split(porcelain, "\n") {
		path, ok := strings.CutPrefix(line, "worktree ")
		if !ok {
			continue
		}
		if first {
			first = false
			continue
		}
		out = append(out, path)
	}
	return out
}

// nestedWorktrees keeps the worktrees whose directory exists strictly inside
// dir. One whose directory is gone loses nothing when dir is deleted: its
// entry and HEAD live in the checkout's git directory, which stays.
func nestedWorktrees(worktrees []string, dir string) []string {
	root := realPath(dir)
	var out []string
	for _, w := range worktrees {
		resolved := realPath(w)
		if resolved == root || !isUnder(root, resolved) {
			continue
		}
		if fi, err := os.Stat(w); err != nil || !fi.IsDir() {
			continue
		}
		out = append(out, w)
	}
	return out
}

// githubRepo returns owner/name for a GitHub clone URL, in the SSH, scp-like
// or HTTPS form, and "" for anything else.
func githubRepo(url string) string {
	var rest string
	url = strings.TrimRight(url, "/")
	// A user or token before the host (https://user@github.com/...) is not part
	// of the repository's name.
	if after, ok := strings.CutPrefix(url, "https://"); ok {
		if _, host, found := strings.Cut(after, "@"); found && strings.HasPrefix(host, "github.com/") {
			url = "https://" + host
		}
	}
	for _, prefix := range []string{"git@github.com:", "ssh://git@github.com/", "https://github.com/"} {
		if r, ok := strings.CutPrefix(url, prefix); ok {
			rest = r
			break
		}
	}
	owner, name, ok := strings.Cut(strings.TrimSuffix(rest, ".git"), "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return ""
	}
	return owner + "/" + name
}

// inspectClone reads one clone's state. Every git probe that fails is recorded
// as an Unverifiable reason rather than dropped: a git error must block the reap,
// not masquerade as a clean, empty tree. repo is the issue's home repo
// ("owner/name"), used for the squash-merge landing proof's PR lookup; pr may
// be nil to skip that lookup (the ancestor check alone still runs).
// inspectClone judges the repository whose work tree is p, named dir. gone is
// the directory reap deletes: a remote whose repository lies inside it is no
// copy at all.
func inspectClone(git gitx.Runner, dir, p, gone, repo string, pr PRLookup) CloneState {
	c := CloneState{Dir: dir}

	// A probe that fails leaves WorktreeOf empty, so the clone is judged as a
	// repository of its own: the stricter reading, which holds on every
	// branch and stash the repository has.
	if of, err := worktreeOf(git, p); err != nil {
		c.Unverifiable = append(c.Unverifiable, fmt.Sprintf("could not tell whether it is a worktree: %v", err))
	} else {
		c.WorktreeOf = of
	}
	own := c.WorktreeOf == ""

	// core.worktree can point git at another directory, and every check of
	// files below would then read that one, never the files deleted here.
	if top, err := git.Run(p, nil, "rev-parse", "--show-toplevel"); err != nil {
		c.Unverifiable = append(c.Unverifiable, fmt.Sprintf("could not find its work tree: %v", err))
	} else if top = strings.TrimSpace(top); realPath(top) != realPath(p) {
		c.Unverifiable = append(c.Unverifiable, fmt.Sprintf("its work tree is %s (core.worktree), so git never looks at the files here; run `git config --unset core.worktree` in it, then reap again", top))
	}

	// The flags are explicit so no config can hide a file from the check:
	// status.showUntrackedFiles=no drops untracked files, and
	// submodule.<name>.ignore or diff.ignoreSubmodules drop a dirty submodule.
	// honestStatus turns off the caches that answer for the disk.
	statusArgs := append(append([]string{}, honestStatus...), "status", "--porcelain", "--untracked-files=all", "--ignore-submodules=none")
	if out, err := git.Run(p, nil, statusArgs...); err != nil {
		c.Unverifiable = append(c.Unverifiable, fmt.Sprintf("git status failed: %v", err))
	} else {
		c.Dirty = strings.TrimSpace(out) != ""
	}
	if ops, err := opsInProgress(git, p); err != nil {
		c.Unverifiable = append(c.Unverifiable, fmt.Sprintf("could not tell whether a rebase or merge is in progress: %v", err))
	} else {
		c.InProgress = ops
	}
	if hidden, err := hiddenEdits(git, p, nil); err != nil {
		c.Unverifiable = append(c.Unverifiable, fmt.Sprintf("could not look for edits git status does not show: %v", err))
	} else {
		for _, h := range hidden {
			c.Hidden = append(c.Hidden, fmt.Sprintf("%s differs from what is committed -- %s", h.path, h.why))
		}
		c.Hidden = capList(c.Hidden, "files git status does not show as changed")
	}

	// A clone with its own repository is held by every linked worktree: each
	// one's HEAD and index live in the clone's git directory. A worktree's
	// siblings, the checkout's own included, belong to the checkout's
	// repository, which removing this one leaves in place; but a worktree that
	// sits inside this one's directory (Claude Code's .claude/worktrees, made
	// from inside the clone) is deleted with it, so it holds too.
	if out, err := git.Run(p, nil, "worktree", "list", "--porcelain"); err != nil {
		c.Unverifiable = append(c.Unverifiable, fmt.Sprintf("git worktree list failed: %v", err))
	} else if own {
		c.Worktrees = linkedWorktrees(out)
	} else {
		c.Worktrees = nestedWorktrees(linkedWorktrees(out), p)
	}

	if repos, stray, err := unjudgedRepos(git, p, c.Worktrees); err != nil {
		c.Unverifiable = append(c.Unverifiable, fmt.Sprintf("could not look for repositories inside it: %v", err))
	} else {
		c.InnerRepos, c.StrayGitlinks = repos, stray
	}
	if files, err := unrebuiltIgnored(git, p, append(append([]string{}, c.Worktrees...), c.InnerRepos...)); err != nil {
		c.Unverifiable = append(c.Unverifiable, fmt.Sprintf("could not list the files it ignores: %v", err))
	} else {
		c.IgnoredFiles = files
	}

	// The branch name is informational only, so a failure here is not a safety
	// signal and is not recorded as a blocker.
	if out, err := git.Run(p, nil, "rev-parse", "--abbrev-ref", "HEAD"); err == nil {
		c.Branch = out
	}

	// Refresh remote-tracking refs before judging unpushed-ness: a clone that
	// hasn't talked to a remote since a PR branch was merged and deleted there
	// would otherwise judge those commits against a stale ref and count them as
	// pushed, or as this workspace's only copy. Every remote is fetched, since
	// every remote's refs count as pushed below. A fetch that fails (no network,
	// an unreachable remote) or hangs is not fatal -- the count below still runs
	// against whatever refs are on disk -- but it is recorded so Blockers and
	// Notes can disclose it.
	if stale, err := fetchRemotes(git, p); err != nil {
		c.Unverifiable = append(c.Unverifiable, fmt.Sprintf("could not list its remotes: %v", err))
	} else {
		c.StaleRemotes = stale
	}
	pushed, err := countedRemotes(git, p, gone)
	if err != nil {
		c.Unverifiable = append(c.Unverifiable, fmt.Sprintf("could not tell which remotes hold its commits: %v", err))
	}
	if holds, err := gitDirHolds(git, p, gone); err != nil {
		c.Unverifiable = append(c.Unverifiable, fmt.Sprintf("could not read its git directory: %v", err))
	} else {
		c.Unverifiable = append(c.Unverifiable, holds...)
	}

	// Commits reachable from a local ref but from no counted remote. Every ref
	// is local except refs/remotes, refs/tags, refs/notes and the stash, which
	// is counted below (walkedRefs): HEAD, so a commit on a detached HEAD
	// counts, the branches, the per-worktree refs, refs/replace, and any
	// namespace git or a tool adds later. This is a cheap fast-path check: the
	// common case is 0, and only then is the more expensive per-ref landing
	// proof run.
	//
	// In a worktree the shared refs are the checkout's, which outlive the
	// worktree, so only HEAD and the per-worktree refs are counted there: they
	// live in the worktree's own admin directory and go with it.
	refs, err := walkedRefs(git, p, own)
	if err != nil {
		c.Unverifiable = append(c.Unverifiable, fmt.Sprintf("could not list its refs: %v", err))
	}
	c.Unverifiable = append(c.Unverifiable, refs.odd...)
	countArgs := append([]string{"rev-list", "--count"}, refs.countArgs...)
	countArgs = append(countArgs, pushed.not()...)
	if out, err := git.Run(p, nil, countArgs...); err != nil {
		c.Unverifiable = append(c.Unverifiable, fmt.Sprintf("git rev-list failed: %v", err))
	} else if _, err := strconv.Atoi(strings.TrimSpace(out)); err != nil {
		c.Unverifiable = append(c.Unverifiable, fmt.Sprintf("could not read the unpushed-commit count: %v", err))
	} else if strings.TrimSpace(out) != "0" {
		// A sha not reachable from any remote is not the same question as
		// "is this content already upstream": squash-merge rewrites the sha,
		// and GitHub's auto-delete-on-merge then removes the only ref that
		// could have proven it by name. Prove landing per ref instead.
		var commitPR CommitPRLookup
		if pr != nil {
			commitPR, _ = pr.(CommitPRLookup)
		}
		if unpushed, landed, defUnresolved, err := unpushedLanding(git, p, repo, commitPR, refs.names, pushed); err != nil {
			c.Unverifiable = append(c.Unverifiable, fmt.Sprintf("could not verify unpushed commits: %v", err))
		} else {
			c.Unpushed = unpushed
			c.SquashLanded = landed
			c.DefaultBranchUnresolved = defUnresolved
		}
	}

	// A worktree's stash belongs to the checkout's repository and stays with it.
	if !own {
		return c
	}
	if count, err := gitDirStash(git, p); err != nil {
		c.Unverifiable = append(c.Unverifiable, fmt.Sprintf("could not count its stash: %v", err))
	} else {
		c.Stashed = count
	}

	return c
}

// ListIssues inspects every issue workspace under the workspaces root. One that
// cannot be inspected (a broken manifest) is not listed, but named in unchecked
// with the reason, so a listing never reads as complete when it is not.
func ListIssues(roots config.Roots, git gitx.Runner, pr PRLookup, mux LiveChecker) (out []*IssueState, unchecked []string, err error) {
	entries, err := os.ReadDir(roots.Workspaces)
	if err != nil {
		return nil, nil, err
	}
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), config.IssuePrefix) {
			continue
		}
		dir := filepath.Join(roots.Workspaces, e.Name())
		if _, err := os.Stat(manifest.Path(dir)); err != nil {
			continue
		}
		st, err := InspectIssue(dir, git, pr, mux)
		if err != nil {
			unchecked = append(unchecked, fmt.Sprintf("%s: %v", e.Name(), err))
			continue
		}
		out = append(out, st)
	}
	return out, unchecked, nil
}
