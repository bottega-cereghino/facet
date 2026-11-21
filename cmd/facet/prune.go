package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"text/tabwriter"
	"time"

	"github.com/bottega-cereghino/facet/internal/gitx"
	"github.com/bottega-cereghino/facet/internal/workspace"
	"github.com/spf13/cobra"
)

func newPruneCmd() *cobra.Command {
	var (
		apply    bool
		yes      bool
		idleDays int
	)
	cmd := &cobra.Command{
		Use:   "prune [workspace]",
		Short: "Free the dependency and cache directories of workspaces nobody is using",
		Long: "Lists, and with --apply deletes, the directories in a workspace's clones that\n" +
			"one command rebuilds: node_modules, .venv and the tool caches (.mypy_cache,\n" +
			".pytest_cache, .ruff_cache, .next, .turbo, .parcel-cache). Only a directory\n" +
			"git ignores and that holds no tracked file is named, so no commit or change\n" +
			"can be among them; one holding a git repository of its own (an editable pip\n" +
			"install, a fork cloned into node_modules) is kept. The workspace stays, and\n" +
			"its manifest, clones and history are untouched. .terraform is never named:\n" +
			"it keeps the selected terraform workspace.\n\n" +
			"The size is what deleting frees. On a Mac, pnpm and uv clone packages from\n" +
			"their store, so an installed node_modules often frees almost nothing; one\n" +
			"that frees under 1 MiB is not named, since deleting it only costs a reinstall.\n\n" +
			"A workspace is held while any clone was used within --idle-days (git's index,\n" +
			"HEAD or reflog, or an install), or while a session, pane, process or launchd\n" +
			"job runs from it, as reap asks. With no workspace named it looks at every one\n" +
			"under the root. --apply lists, asks once, and checks each workspace again\n" +
			"just before deleting.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if idleDays < 0 {
				return fmt.Errorf("--idle-days %d is negative; pass 0 to prune regardless of age", idleDays)
			}
			var dirs []string
			if len(args) == 1 {
				ws, err := reapTarget("", args)
				if err != nil {
					return err
				}
				dirs = []string{ws}
			} else {
				root, err := filepath.Abs(roots.Workspaces)
				if err != nil {
					return err
				}
				if dirs, err = workspace.Dirs(root, true); err != nil {
					return err
				}
			}
			idle := time.Duration(idleDays) * 24 * time.Hour
			ask := confirm
			if yes {
				ask = func(string) bool { return true }
			}
			return runPrune(os.Stdout, dirs, git, muxLive(), time.Now(), idle, apply, ask)
		},
	}
	cmd.Flags().BoolVar(&apply, "apply", false, "delete what is listed, after one confirmation; without it prune only lists")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "skip the confirmation prompt")
	cmd.Flags().IntVar(&idleDays, "idle-days", 14, "hold a workspace used within this many days; 0 holds none for age")
	return cmd
}

// runPrune lists what each workspace would free and, with apply, deletes it
// from the ones nothing holds. Each workspace is inspected again just before
// its deletion, so a session started after the listing still holds it.
func runPrune(w io.Writer, dirs []string, git gitx.Runner, live workspace.LiveChecker, now time.Time, idle time.Duration, apply bool, ask func(string) bool) error {
	var states []*workspace.PruneState
	for _, d := range dirs {
		st, err := workspace.InspectPrune(d, git, live, now, idle)
		if err != nil {
			_, _ = fmt.Fprintf(w, "could not check %s, left alone: %v\n", filepath.Base(d), err)
			continue
		}
		states = append(states, st)
	}
	free, held, err := listPrune(w, states)
	if err != nil {
		return err
	}
	if !apply {
		_, _ = fmt.Fprintf(w, "\n%s can be freed now, %s more is held. `facet prune --apply` deletes what is not held.\n",
			humanBytes(free), humanBytes(held))
		return nil
	}
	if free == 0 {
		_, _ = fmt.Fprintln(w, "\nnothing to free now.")
		return nil
	}
	if !ask(fmt.Sprintf("Delete what frees %s?", humanBytes(free))) {
		_, _ = fmt.Fprintln(w, "aborted.")
		return nil
	}
	var freed int64
	var errs []error
	for _, st := range states {
		if len(st.Found) == 0 || len(st.Held) > 0 {
			continue
		}
		again, err := workspace.InspectPrune(st.Dir, git, live, now, idle)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if len(again.Held) > 0 {
			_, _ = fmt.Fprintf(w, "%s: held since the listing, left alone\n", filepath.Base(st.Dir))
			continue
		}
		if err := workspace.Prune(again); err != nil {
			errs = append(errs, err)
			continue
		}
		freed += again.Frees()
	}
	_, _ = fmt.Fprintf(w, "\nfreed %s.\n", humanBytes(freed))
	return errors.Join(errs...)
}

// listPrune prints a line per workspace, then what each would free and what
// holds it, and returns the bytes free to delete now and the bytes held.
func listPrune(w io.Writer, states []*workspace.PruneState) (free, held int64, err error) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "WORKSPACE\tFREES\tSTATE")
	for _, st := range states {
		state := "prunable"
		switch {
		case len(st.Found) == 0:
			state = "nothing to free"
		case len(st.Held) > 0:
			state = fmt.Sprintf("held (%d)", len(st.Held))
			held += st.Frees()
		default:
			free += st.Frees()
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\n", filepath.Base(st.Dir), humanBytes(st.Frees()), state)
	}
	if err := tw.Flush(); err != nil {
		return 0, 0, err
	}
	for _, st := range states {
		if len(st.Found) == 0 {
			continue
		}
		_, _ = fmt.Fprintf(w, "\n%s:\n", filepath.Base(st.Dir))
		for _, h := range st.Held {
			_, _ = fmt.Fprintf(w, "  held: %s\n", h)
		}
		for _, k := range st.Kept {
			_, _ = fmt.Fprintf(w, "  kept: %s\n", k)
		}
		for _, r := range st.Found {
			_, _ = fmt.Fprintf(w, "  %s/%s  %s  (rebuild: %s)\n", r.Clone, r.Rel, humanBytes(r.Bytes), r.Rebuild)
		}
	}
	return free, held, nil
}
