package workspace

import "strings"

// HoldKinds names, in a fixed order, each kind of thing that holds the
// workspace: one short label per kind, however many of it Blockers lists. It
// is read from the same state as Blockers, so a workspace with no blockers has
// no kinds. `reap --reapable` groups the held workspaces by it.
func (s *IssueState) HoldKinds() []string {
	var has [holdKindCount]bool
	if s.PRUnknown || s.LiveRootsUnknown != nil || s.JobsUnknown != nil {
		has[holdUnchecked] = true
	}
	for _, c := range s.Clone {
		has[holdUnchecked] = has[holdUnchecked] || len(c.Unverifiable) > 0 || len(c.StaleRemotes) > 0
		has[holdUnpushed] = has[holdUnpushed] || c.Unpushed > 0
		has[holdWorktree] = has[holdWorktree] || len(c.Worktrees) > 0
		has[holdInnerRepo] = has[holdInnerRepo] || len(c.InnerRepos) > 0 || len(c.StrayGitlinks) > 0
		has[holdIgnored] = has[holdIgnored] || len(c.IgnoredFiles) > 0
		has[holdStash] = has[holdStash] || c.Stashed > 0
		has[holdUncommitted] = has[holdUncommitted] || c.Dirty || len(c.Hidden) > 0 || len(c.InProgress) > 0
	}
	has[holdLoose] = len(s.Loose) > 0
	has[holdUnaccounted] = s.UnaccountedCount > 0
	has[holdOpenPR] = s.PR != nil && strings.EqualFold(s.PR.State, "OPEN")
	has[holdRunning] = s.SessionLive || len(s.LiveRoots) > 0 || len(s.Jobs) > 0
	var out []string
	for k, on := range has {
		if on {
			out = append(out, holdLabels[k])
		}
	}
	return out
}

// IgnoredPaths lists, clone by clone, the ignored files nothing rebuilds that
// hold the workspace, as paths relative to the workspace.
func (s *IssueState) IgnoredPaths() []string {
	var out []string
	for _, c := range s.Clone {
		for _, f := range c.IgnoredFiles {
			out = append(out, c.Dir+"/"+f)
		}
	}
	return out
}

const (
	holdUnchecked = iota
	holdUnpushed
	holdLoose
	holdWorktree
	holdInnerRepo
	holdIgnored
	holdStash
	holdUncommitted
	holdUnaccounted
	holdOpenPR
	holdRunning
	holdKindCount
)

var holdLabels = [holdKindCount]string{
	holdUnchecked:   "could not check",
	holdUnpushed:    "unpushed commits",
	holdLoose:       "files beside the clones",
	holdWorktree:    "linked worktrees",
	holdInnerRepo:   "repositories reap does not judge",
	holdIgnored:     "ignored files nothing rebuilds",
	holdStash:       "stashes",
	holdUncommitted: "uncommitted changes",
	holdUnaccounted: "files no remote has",
	holdOpenPR:      "open pull request",
	holdRunning:     "something running",
}
