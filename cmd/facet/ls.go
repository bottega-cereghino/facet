package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"text/tabwriter"

	"github.com/bottega-cereghino/facet/internal/config"
	"github.com/bottega-cereghino/facet/internal/manifest"
	"github.com/bottega-cereghino/facet/internal/workspace"
	"github.com/spf13/cobra"
)

func newLsCmd() *cobra.Command {
	var (
		path string
		size bool
	)
	cmd := &cobra.Command{
		Use:   "ls",
		Short: "List a workspace's entries and their state on disk, or every workspace at a workspaces root",
		Long: "Lists a workspace's entries and their state on disk. Run where there is no\n" +
			"manifest, at the workspaces root, it lists every workspace instead.\n\n" +
			"--size adds how much disk deleting each workspace would free (objects shared\n" +
			"with the mirror stay behind, so they do not count) and whether `facet reap`\n" +
			"would delete it now. It runs reap's own checks, so it is slower: it fetches\n" +
			"every clone and asks GitHub about open pull requests.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			ws, err := config.ResolveWorkspace(path)
			if err != nil {
				return err
			}
			if _, err := os.Stat(manifest.Path(ws)); err != nil {
				// ws isn't a workspace itself -- see if it's the root that holds them.
				if dirs, dirsErr := workspace.Dirs(ws, true); dirsErr == nil && len(dirs) > 0 {
					return lsRoot(os.Stdout, ws, dirs, size)
				}
			}
			m, entries, err := workspace.List(roots, ws, git)
			if err != nil {
				return err
			}
			fmt.Printf("%s -- %s\n\n", m.Name, m.Description)

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			_, _ = fmt.Fprintln(w, "ENTRY\tKIND\tSTATUS\tTARGET\tORIGIN")
			for _, e := range entries {
				name := e.Name
				if e.Transient {
					name += " *"
				}
				_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", name, e.Kind, e.Status, e.Target, e.Origin)
			}
			if err := w.Flush(); err != nil {
				return err
			}
			for _, e := range entries {
				if e.Transient {
					fmt.Println("\n* transient -- here for now, may be swapped out")
					break
				}
			}
			if size {
				st, err := workspace.Inspect(ws, git, gh, muxLive())
				if err != nil {
					return err
				}
				fmt.Printf("\nfrees %s if deleted; reap: %s\n", humanBytes(st.SizeBytes), reapVerdict(st))
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&path, "path", "", "workspace directory (default: working directory)")
	cmd.Flags().BoolVar(&size, "size", false, "also show the disk deleting frees and whether reap would delete it (slower)")
	return cmd
}

// lsRoot lists every workspace under root, each with a health summary rolled
// up from its entries' statuses -- the workspaces-root counterpart to the
// single-workspace listing above. With size, each row also says what deleting
// the workspace frees and whether reap would delete it now.
func lsRoot(out io.Writer, root string, dirs []string, size bool) error {
	_, _ = fmt.Fprintf(out, "%s -- workspaces root\n\n", root)

	var states []*workspace.IssueState
	if size {
		states = inspectAll(dirs)
	}
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	if size {
		_, _ = fmt.Fprintln(w, "WORKSPACE\tENTRIES\tHEALTH\tFREES\tREAP")
	} else {
		_, _ = fmt.Fprintln(w, "WORKSPACE\tENTRIES\tHEALTH")
	}
	var total, reapable int64
	var nReapable int
	for i, dir := range dirs {
		name := filepath.Base(dir)
		_, entries, err := workspace.List(roots, dir, git)
		if err != nil {
			_, _ = fmt.Fprintf(w, "%s\t-\terror: %v\n", name, err)
			continue
		}
		if !size {
			_, _ = fmt.Fprintf(w, "%s\t%d\t%s\n", name, len(entries), summarizeHealth(entries))
			continue
		}
		frees, verdict := "-", "-"
		if st := states[i]; st != nil {
			frees, verdict = humanBytes(st.SizeBytes), reapVerdict(st)
			total += st.SizeBytes
			if len(st.Blockers()) == 0 {
				reapable += st.SizeBytes
				nReapable++
			}
		}
		_, _ = fmt.Fprintf(w, "%s\t%d\t%s\t%s\t%s\n", name, len(entries), summarizeHealth(entries), frees, verdict)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if size {
		_, _ = fmt.Fprintf(out, "\n%s in all; %d reapable now, freeing %s. `facet reap <name>` says what holds the rest.\n",
			humanBytes(total), nReapable, humanBytes(reapable))
	}
	return nil
}

// inspectWorkers bounds how many workspaces are inspected at once. Each
// inspection fetches every clone, so running them together saves most of the
// wait, and a small bound keeps it polite to the forge.
const inspectWorkers = 4

// inspectAll runs reap's checks on every workspace, in parallel, keeping the
// order of dirs. A workspace that cannot be inspected is nil.
func inspectAll(dirs []string) []*workspace.IssueState {
	states, _ := inspectEach(dirs)
	return states
}

// inspectEach is inspectAll that also keeps why each nil state could not be
// inspected, at the same index.
func inspectEach(dirs []string) ([]*workspace.IssueState, []error) {
	states := make([]*workspace.IssueState, len(dirs))
	errs := make([]error, len(dirs))
	live := muxLive()
	// One fetch per repository: worktree clones of one checkout would
	// otherwise fetch into the same refs at once, and collide.
	shared := workspace.NewSharedFetch(git)
	var wg sync.WaitGroup
	sem := make(chan struct{}, inspectWorkers)
	for i, dir := range dirs {
		wg.Add(1)
		go func(i int, dir string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			states[i], errs[i] = inspectTarget(dir, shared, live)
		}(i, dir)
	}
	wg.Wait()
	return states, errs
}

// reapVerdict says whether reap would delete the workspace now, in the words
// `facet issues` uses.
func reapVerdict(st *workspace.IssueState) string {
	if b := st.Blockers(); len(b) > 0 {
		return fmt.Sprintf("held (%d)", len(b))
	}
	return "reapable"
}

// summarizeHealth rolls up a workspace's entries into one status word, reusing
// the ok/broken/missing vocabulary workspace.List already reports per entry.
func summarizeHealth(entries []workspace.Entry) string {
	var broken, missing int
	for _, e := range entries {
		switch e.Status {
		case "broken":
			broken++
		case "missing":
			missing++
		}
	}
	if broken == 0 && missing == 0 {
		return "ok"
	}
	var parts []string
	if broken > 0 {
		parts = append(parts, fmt.Sprintf("%d broken", broken))
	}
	if missing > 0 {
		parts = append(parts, fmt.Sprintf("%d missing", missing))
	}
	return strings.Join(parts, ", ")
}
