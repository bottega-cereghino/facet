package workspace

import (
	"fmt"
	"path/filepath"
	"strings"
)

// Blockers lists the reasons this workspace must not be deleted. An empty slice
// means it is safe to reap.
//
// The ordering matters: work that cannot be recovered or even confirmed comes
// first, an open PR is merely premature. Every check fails safe -- a state we
// could not read is reported as a blocker, never assumed clean.
func (s *IssueState) Blockers() []string {
	var out []string
	for _, c := range s.Clone {
		for _, u := range c.Unverifiable {
			out = append(out, fmt.Sprintf("%s: %s -- cannot confirm it is safe to delete", c.Dir, u))
		}
	}
	for _, c := range s.Clone {
		if c.Unpushed > 0 {
			msg := fmt.Sprintf("%s has %d unpushed commit(s) -- this workspace is their only copy", c.Dir, c.Unpushed)
			if len(c.StaleRemotes) > 0 {
				msg += fmt.Sprintf(" (could not fetch %s first -- a branch merged and deleted upstream may be miscounted here)", strings.Join(c.StaleRemotes, ", "))
			}
			if c.DefaultBranchUnresolved {
				msg += " (could not resolve origin's default branch -- work already landed there cannot be told from unpushed work here)"
			}
			out = append(out, msg)
		}
	}
	out = append(out, s.Loose...)
	for _, c := range s.Clone {
		for _, w := range c.Worktrees {
			out = append(out, fmt.Sprintf("%s has a linked worktree at %s -- reap cannot see its work; push it, then `git worktree remove` it", c.Dir, w))
		}
	}
	for _, c := range s.Clone {
		for _, r := range c.InnerRepos {
			out = append(out, fmt.Sprintf("%s has a repository at %s that reap does not judge (another VCS, a bare repository, or one in a directory it ignores) -- reap cannot see its work; push it or move it out of the clone", c.Dir, r))
		}
		for _, g := range c.StrayGitlinks {
			out = append(out, fmt.Sprintf("%s has files in %s, a submodule's directory with no repository checked out -- git status never looks there, and they are in no repository; move them out of the clone, then reap again", c.Dir, g))
		}
	}
	for _, c := range s.Clone {
		if len(c.IgnoredFiles) > 0 {
			out = append(out, fmt.Sprintf("%s has files git ignores that nothing rebuilds: %s -- the clone is their only copy; copy out what you need and delete the rest, then reap again", c.Dir, strings.Join(capList(c.IgnoredFiles, "ignored paths"), ", ")))
		}
	}
	for _, c := range s.Clone {
		if c.Stashed > 0 {
			out = append(out, fmt.Sprintf("%s has %d stash entry(ies) -- no push would carry them", c.Dir, c.Stashed))
		}
	}
	for _, c := range s.Clone {
		if c.Dirty {
			out = append(out, fmt.Sprintf("%s has uncommitted changes", c.Dir))
		}
	}
	for _, c := range s.Clone {
		for _, op := range c.InProgress {
			out = append(out, fmt.Sprintf("%s has %s in progress -- its state, an autostash of uncommitted edits among it, is only in the git directory; finish or abort it, then reap again", c.Dir, op))
		}
	}
	for _, c := range s.Clone {
		for _, h := range c.Hidden {
			out = append(out, fmt.Sprintf("%s: %s", c.Dir, h))
		}
	}
	if s.UnaccountedCount > 0 {
		out = append(out, s.unaccountedBlocker())
	}
	if s.PR != nil && strings.EqualFold(s.PR.State, "OPEN") {
		out = append(out, fmt.Sprintf("pull request #%d is still open", s.PR.Number))
	}
	if s.PRUnknown {
		out = append(out, "could not determine the pull-request state -- an open PR cannot be ruled out")
	}
	if s.SessionLive {
		out = append(out, fmt.Sprintf("tmux session %q runs in it -- end it, or --force ends it", filepath.Base(s.Dir)))
	}
	for _, r := range s.LiveRoots {
		out = append(out, fmt.Sprintf("%s -- kill it before reaping", r))
	}
	if s.LiveRootsUnknown != nil {
		out = append(out, fmt.Sprintf("could not list the panes and processes running from here (%v) -- one cannot be ruled out; check that tmux and lsof run, then reap again", s.LiveRootsUnknown))
	}
	for _, j := range s.Jobs {
		out = append(out, fmt.Sprintf("%s -- unload the job, or move it out, before reaping", j))
	}
	if s.JobsUnknown != nil {
		out = append(out, fmt.Sprintf("could not read the loaded launchd jobs (%v) -- a job that runs from here cannot be ruled out; run `launchctl list` to see why, then reap again", s.JobsUnknown))
	}
	return out
}

// StaleFetch reports whether any clone was judged against refs that could not
// be fetched first. A single reap goes ahead with a note, since refusing every
// offline reap would make it useless without a network; reap --reapable, which
// never guesses, leaves such a workspace alone.
func (s *IssueState) StaleFetch() bool {
	for _, c := range s.Clone {
		if len(c.StaleRemotes) > 0 {
			return true
		}
	}
	return false
}

// Notes lists non-blocking information about a workspace's state -- things
// worth saying so an operator does not read silence as "nothing to report"
// (facet#66). Nothing here belongs in Blockers(): every entry is something
// that does *not* hold the workspace.
func (s *IssueState) Notes() []string {
	var out []string
	for _, c := range s.Clone {
		// With unpushed commits the disclaimer rides on that blocker; with none,
		// "nothing unpushed" was still judged against the last fetch, and a
		// branch deleted upstream since then would read as pushed.
		if len(c.StaleRemotes) > 0 && c.Unpushed == 0 {
			out = append(out, fmt.Sprintf(
				"%s: could not fetch %s, so it was judged against the last fetch -- a branch deleted upstream since then reads as pushed; reap again online to be sure",
				c.Dir, strings.Join(c.StaleRemotes, ", ")))
		}
	}
	for _, c := range s.Clone {
		if c.SquashLanded > 0 {
			out = append(out, fmt.Sprintf(
				"%s: %d local commit(s) already landed upstream under a different sha (squash-merged) -- not held",
				c.Dir, c.SquashLanded))
		}
	}
	return out
}
