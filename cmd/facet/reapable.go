package main

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/bottega-cereghino/facet/internal/mux"
	"github.com/bottega-cereghino/facet/internal/workspace"
)

// reapReapable is `facet reap --reapable`: every workspace or plain worktree
// in dirs that reap would delete now, after one confirmation. With dryRun it
// prints the list and stops there, without asking.
//
// It never forces. A workspace something holds is counted and left alone, and
// `facet ls --size` or `facet reap <name>` says what holds it. Each workspace is
// checked again just before it is deleted, because work can start in one while
// the prompt waits; one that is held by then is skipped, not deleted.
//
// Workspaces are named by their directory, as `facet ls` names them: a copied
// workspace keeps the manifest's name, and the list must say which directory
// goes.
func reapReapable(out io.Writer, dirs []string, yes, dryRun bool, ask func(string) bool) error {
	states, inspectErrs := inspectEach(dirs)
	var picked []*workspace.IssueState
	var held, unchecked int
	var total int64
	for i, st := range states {
		switch {
		case st == nil:
			unchecked++
			_, _ = fmt.Fprintf(out, "could not check %s, left alone: %v\n", filepath.Base(dirs[i]), inspectErrs[i])
		case len(st.Blockers()) > 0:
			held++
		case st.StaleFetch():
			held++
			_, _ = fmt.Fprintf(out, "left alone %s: %s\n", reapName(st), staleReason(st))
		default:
			picked = append(picked, st)
			total += st.SizeBytes
		}
	}
	left := fmt.Sprintf("%d held", held)
	if unchecked > 0 {
		left += fmt.Sprintf(" and %d that could not be checked", unchecked)
	}
	if len(picked) == 0 {
		_, _ = fmt.Fprintf(out, "nothing to reap: %s. `facet ls --size` says which are held.\n", left)
		return nil
	}

	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "WORKSPACE\tFREES")
	for _, st := range picked {
		_, _ = fmt.Fprintf(w, "%s\t%s\n", reapName(st), humanBytes(st.SizeBytes))
	}
	if err := w.Flush(); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "\n%d reapable, freeing %s; %s left alone.\n", len(picked), humanBytes(total), left)
	if dryRun {
		_, _ = fmt.Fprintln(out, "dry run: nothing was deleted.")
		return nil
	}
	question := fmt.Sprintf("Delete these %d workspaces?", len(picked))
	switch {
	case len(picked) == 1 && picked[0].Worktree:
		question = "Delete this worktree?"
	case len(picked) == 1:
		question = "Delete this workspace?"
	case hasWorktree(picked):
		question = fmt.Sprintf("Delete these %d workspaces and worktrees?", len(picked))
	}
	// Where each path leads, read before the prompt: a parent swapped for a
	// symlink while it waits must not point a delete elsewhere (#363).
	pins := map[string]targetPath{}
	pinErrs := map[string]error{}
	for _, st := range picked {
		pins[st.Dir], pinErrs[st.Dir] = pathOf(st.Dir)
	}
	if !yes && !ask(question) {
		_, _ = fmt.Fprintln(out, "aborted.")
		return nil
	}

	live := muxLive()
	var freed int64
	var errs []error
	enclosing := map[string]bool{}
	for _, st := range picked {
		if err := pinErrs[st.Dir]; err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", reapName(st), err))
			continue
		}
		if err := samePath(st.Dir, pins[st.Dir]); err != nil {
			_, _ = fmt.Fprintf(out, "skipped %s: %v\n", reapName(st), err)
			continue
		}
		again, err := inspectTarget(st.Dir, git, live)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", filepath.Base(st.Dir), err))
			continue
		}
		if b := again.Blockers(); len(b) > 0 {
			_, _ = fmt.Fprintf(out, "skipped %s: since the list was made it is held by %s\n", reapName(again), b[0])
			continue
		}
		if again.StaleFetch() {
			_, _ = fmt.Fprintf(out, "skipped %s: %s\n", reapName(again), staleReason(again))
			continue
		}
		// As in reap: a live session holds file handles on the tree.
		if l := mux.Pick(); l != nil && !again.Worktree {
			_ = l.Kill(filepath.Base(again.Dir))
		}
		if err := workspace.Reap(again, git); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", reapName(again), err))
			continue
		}
		_, _ = fmt.Fprintf(out, "removed %s\n", again.Dir)
		freed += again.SizeBytes
		if again.Enclosing != "" {
			enclosing[again.Enclosing] = true
		}
	}
	_, _ = fmt.Fprintf(out, "\nfreed %s. The shared mirror was not touched.\n", humanBytes(freed))
	repos := make([]string, 0, len(enclosing))
	for e := range enclosing {
		repos = append(repos, e)
	}
	sort.Strings(repos)
	for _, e := range repos {
		_, _ = fmt.Fprintf(out, "tracked files are now deletions in %s; commit them there.\n", e)
	}
	return errors.Join(errs...)
}

// reapName names a workspace by its directory, as `facet ls` does, and a
// worktree by its path: worktrees of different checkouts often share a name.
func reapName(st *workspace.IssueState) string {
	if st.Worktree {
		return st.Dir + " (worktree)"
	}
	return filepath.Base(st.Dir)
}

func hasWorktree(sts []*workspace.IssueState) bool {
	for _, st := range sts {
		if st.Worktree {
			return true
		}
	}
	return false
}

// staleReason says why a workspace judged against a failed fetch is left
// alone, and how to reap it: online again, or by name, where a note says what
// was not checked.
func staleReason(st *workspace.IssueState) string {
	var remotes []string
	for _, c := range st.Clone {
		for _, r := range c.StaleRemotes {
			remotes = append(remotes, r+" for "+c.Dir)
		}
	}
	return fmt.Sprintf("could not fetch %s, so its pushed work cannot be confirmed; run --reapable again online, or `facet reap %s` to decide yourself",
		strings.Join(remotes, ", "), reapArg(st))
}

// reapArg is what `facet reap` takes to name st: a workspace's directory name,
// a worktree's path.
func reapArg(st *workspace.IssueState) string {
	if st.Worktree {
		return st.Dir
	}
	return filepath.Base(st.Dir)
}
