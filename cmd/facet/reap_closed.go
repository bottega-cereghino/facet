package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/bottega-cereghino/facet/internal/workspace"
)

// The reasons `reap --reapable --closed` leaves out a workspace reap would
// delete now.
const (
	closedNotIssue     = "not an issue workspace"
	closedNoPR         = "no pull request"
	closedPROpen       = "pull request open"
	closedLookupFailed = "pull request lookup failed"
	closedHomeGone     = "no pull request by branch, and the home clone directory is gone"
)

// closedReason is the --closed narrowing: "" when st is an issue workspace
// whose branch has a merged or closed pull request and no open one, and the
// reason to leave it out otherwise.
//
// The pull request is the one the inspection found by branch (ghx ViewPR).
// When it found none, the home clone's HEAD is asked for the merged pull
// request that had it as a commit (ghx MergedPRForSHA), which finds a squash
// merge whose branch is gone. A lookup that fails holds the workspace: it is
// never read as closed (CODE.md section 2). A home clone whose directory is
// gone has no HEAD to ask about: git is not run there, and the workspace is
// left out with a reason that says the directory is missing.
func closedReason(st *workspace.IssueState) string {
	if st.Worktree || st.Issue == nil {
		return closedNotIssue
	}
	if st.PRUnknown {
		return closedLookupFailed
	}
	if st.PR != nil {
		switch st.PR.State {
		case "MERGED", "CLOSED":
			return ""
		case "OPEN":
			return closedPROpen
		default:
			st.PRErr = fmt.Errorf("unexpected pull request state %q", st.PR.State)
			return closedLookupFailed
		}
	}
	if st.Issue.Home == "" {
		return closedNoPR
	}
	home := filepath.Join(st.Dir, st.Issue.Home)
	if _, err := os.Stat(home); errors.Is(err, fs.ErrNotExist) {
		return closedHomeGone
	}
	head, err := git.Run(home, nil, "rev-parse", "--verify", "HEAD")
	if err != nil {
		st.PRErr = err
		return closedLookupFailed
	}
	merged, err := gh.MergedPRForSHA(st.Issue.Repo, strings.TrimSpace(head))
	switch {
	case err != nil:
		st.PRErr = err
		return closedLookupFailed
	case merged == nil:
		return closedNoPR
	}
	return ""
}

// narrowReason asks narrow, when there is one, why st is left out.
func narrowReason(narrow func(*workspace.IssueState) string, st *workspace.IssueState) string {
	if narrow == nil {
		return ""
	}
	return narrow(st)
}

// printLeftOut lists the workspaces --closed left out, grouped by reason, as
// printHeld lists the held ones.
func printLeftOut(out io.Writer, leftOut map[string][]*workspace.IssueState) {
	if len(leftOut) == 0 {
		return
	}
	n := 0
	keys := make([]string, 0, len(leftOut))
	for k, sts := range leftOut {
		keys = append(keys, k)
		n += len(sts)
	}
	sort.Strings(keys)
	_, _ = fmt.Fprintf(out, "\nleft out by --closed (%d), by reason:\n", n)
	for _, k := range keys {
		sts := leftOut[k]
		sort.Slice(sts, func(i, j int) bool { return reapName(sts[i]) < reapName(sts[j]) })
		_, _ = fmt.Fprintf(out, "  %s (%d)\n", k, len(sts))
		for _, st := range sts {
			line := "    " + reapName(st)
			if k == closedLookupFailed && st.PRErr != nil {
				line += ": " + oneLine(st.PRErr.Error())
			}
			_, _ = fmt.Fprintln(out, line)
		}
	}
}

// oneLine folds s onto one line, so an error with a body stays on its
// workspace's line.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }
