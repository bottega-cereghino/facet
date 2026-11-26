package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/bottega-cereghino/facet/internal/config"
	"github.com/bottega-cereghino/facet/internal/ghx"
	"github.com/bottega-cereghino/facet/internal/gitx"
	"github.com/bottega-cereghino/facet/internal/launchd"
	"github.com/bottega-cereghino/facet/internal/manifest"
	"github.com/bottega-cereghino/facet/internal/mux"
	"github.com/bottega-cereghino/facet/internal/workspace"
	"github.com/spf13/cobra"
)

func newIssuesCmd() *cobra.Command {
	var offline bool
	cmd := &cobra.Command{
		Use:   "issues",
		Short: "List every ephemeral issue workspace and whether it can be reaped",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			var pr workspace.PRLookup
			if !offline {
				pr = gh
			}
			states, unchecked, err := workspace.ListIssues(roots, git, pr, muxLive())
			if err != nil {
				return err
			}
			for _, u := range unchecked {
				fmt.Fprintf(os.Stderr, "! could not check %s -- left out of the list\n", u)
			}
			if len(states) == 0 {
				fmt.Println("no issue workspaces")
				return nil
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			_, _ = fmt.Fprintln(w, "WORKSPACE\tISSUE\tBRANCH\tSTATE\tPR\tSIZE")
			for _, s := range states {
				state := reapVerdict(s, offline)
				pr := "-"
				if s.PR != nil {
					pr = fmt.Sprintf("#%d %s", s.PR.Number, strings.ToLower(s.PR.State))
				}
				branch := s.Issue.Branch
				if branch == "" {
					branch = "-"
				}
				_, _ = fmt.Fprintf(w, "%s\t%s#%d\t%s\t%s\t%s\t%s\n",
					s.Name, s.Issue.Repo, s.Issue.Number, branch, state, pr, humanBytes(s.SizeBytes))
			}
			if err := w.Flush(); err != nil {
				return err
			}
			for _, s := range states {
				if b := s.Blockers(); len(b) > 0 {
					fmt.Printf("\n%s:\n", s.Name)
					for _, r := range b {
						fmt.Printf("  - %s\n", r)
					}
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&offline, "offline", false, "skip the pull-request lookup")
	return cmd
}

func newReapCmd() *cobra.Command {
	var (
		path     string
		force    bool
		yes      bool
		reapable bool
		dryRun   bool
		closed   bool
		safe     bool
		merged   string
	)
	cmd := &cobra.Command{
		Use:   "reap",
		Short: "Delete a workspace once its work has landed",
		Long: "Deletes an issue workspace or a long-lived one, or a plain git worktree. Refuses\n" +
			"while there are\n" +
			"unpushed commits, uncommitted changes, stashes, an open pull request, a live\n" +
			"multiplexer session, a tmux pane or process still rooted in the workspace, or\n" +
			"a loaded launchd job whose program or working directory is in it.\n\n" +
			"A long-lived workspace may also hold files of its own beside its clones. Any\n" +
			"that are not committed in an enclosing repository (~/Workspaces is usually\n" +
			"one) hold it too, since the workspace is their only copy. Links are removed,\n" +
			"never followed: their targets are untouched.\n\n" +
			"The shared mirror is never touched: a clone's objects are hardlinks, so\n" +
			"deleting the workspace drops those names and leaves the mirror's own intact.\n\n" +
			"Given a linked worktree that is not a workspace (`git worktree add`, Claude\n" +
			"Code's .claude/worktrees), it checks its uncommitted files, the commits only\n" +
			"its HEAD reaches, its branch's pull request and what runs in it, then removes\n" +
			"it with git. Its branches stay in the checkout.\n\n" +
			"--reapable deletes every workspace at the root that reap would delete now, and\n" +
			"every such worktree inside a checkout under the projects root (not one\n" +
			"elsewhere, nor any of a bare repository's; name those to reap them):\n" +
			"it lists them with the disk each frees, asks once, and checks each again just\n" +
			"before deleting it. It never forces; held workspaces are left alone, and so\n" +
			"is one with a remote it could not fetch, since its pushed work cannot be\n" +
			"confirmed. With\n" +
			"--dry-run it prints the list and deletes nothing.\n\n" +
			"--closed narrows --reapable to issue workspaces whose branch has a merged or\n" +
			"closed pull request and no open one. Every other workspace is left alone and\n" +
			"listed with its reason: not an issue workspace, no pull request, pull request\n" +
			"open, or pull request lookup failed.\n\n" +
			"--safe is --reapable --yes for a run nobody watches: it asks nothing, deletes\n" +
			"only what --reapable would, and prints each path it removed with its size.\n" +
			"It refuses --force and a named workspace or --path, so nothing widens it;\n" +
			"--closed and --dry-run still apply.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if safe {
				if force || path != "" || len(args) > 0 {
					return fmt.Errorf("--safe sweeps only what --reapable would delete; it takes no --force, workspace name or --path\nfix: drop them, or name the workspace without --safe: facet reap <name>")
				}
				reapable = true
				yes = true
			}
			if dryRun && !reapable {
				return fmt.Errorf("--dry-run lists what --reapable would delete; add --reapable")
			}
			if closed && !reapable {
				return fmt.Errorf("--closed narrows what --reapable would delete; add --reapable")
			}
			if reapable {
				if force {
					return fmt.Errorf("--reapable never forces; to delete a held workspace, name it: facet reap <name> --force")
				}
				if path != "" || len(args) > 0 {
					return fmt.Errorf("--reapable takes no workspace: it reaps every reapable one under %s; drop the name, or drop --reapable", roots.Workspaces)
				}
				// Absolute, as reap's own target is: the live-pane check compares
				// against the absolute paths tmux reports.
				root, err := filepath.Abs(roots.Workspaces)
				if err != nil {
					return err
				}
				dirs, err := workspace.Dirs(root, true)
				if err != nil {
					return err
				}
				// The worktrees inside the checkouts under the projects root,
				// Claude Code's among them; a workspace's own are reaped with it.
				wts, unchecked, err := workspace.ProjectWorktrees(git, roots.Projects, root)
				if err != nil && !os.IsNotExist(err) {
					return err
				}
				for _, u := range unchecked {
					fmt.Printf("could not list worktrees, left alone: %s\n", u)
				}
				var narrow func(*workspace.IssueState) string
				if closed {
					narrow = closedReason
				}
				return reapBatch(os.Stdout, append(dirs, wts...), yes, dryRun, narrow, confirm)
			}
			ws, err := reapTarget(path, args)
			if err != nil {
				return err
			}
			pinned, err := pathOf(ws)
			if err != nil {
				return err
			}
			if merged != "" && (reapable || force) {
				return fmt.Errorf("--merged-head names the merged commit of one workspace; it takes no --reapable or --force\nfix: drop it, or name one workspace")
			}
			prs := prLookupFor(merged)
			st, err := inspectTargetWith(ws, git, prs, muxLive())
			if err != nil {
				return err
			}
			switch {
			case st.Worktree:
				fmt.Printf("%s  (worktree of %s, %s)\n", st.Dir, st.Clone[0].WorktreeOf, humanBytes(st.SizeBytes))
			case st.Issue != nil:
				fmt.Printf("%s  (%s#%d, %s)\n", st.Name, st.Issue.Repo, st.Issue.Number, humanBytes(st.SizeBytes))
			default:
				fmt.Printf("%s  (long-lived, %s)\n", st.Name, humanBytes(st.SizeBytes))
			}
			for _, n := range st.Notes() {
				fmt.Println(n)
			}

			blockers := st.Blockers()
			if len(blockers) > 0 {
				// The count is part of the claim: this is every holder reap can
				// see, so killing what is named and re-running should not turn
				// up more (facet#171).
				fmt.Printf("\nheld by %d thing(s):\n", len(blockers))
				for _, b := range blockers {
					fmt.Printf("  - %s\n", b)
				}
				if !force {
					return fmt.Errorf("refusing to reap; resolve the above, or pass --force to delete anyway")
				}
				fmt.Println("\n--force: deleting despite the above.")
			}
			if !yes {
				// --force covers the holders shown, never work that lands while
				// the prompt waits; a holder's sentence cannot tell, so the
				// files can. Taken after the inspection (whose fetch writes
				// under .git) and again before the next one.
				var before string
				if len(blockers) > 0 {
					if before, err = workspace.Fingerprint(st.Dir); err != nil {
						return fmt.Errorf("could not read %s to notice work that lands during the prompt: %w -- refusing to reap", st.Dir, err)
					}
				}
				if !reapConfirm(fmt.Sprintf("Delete %s?", st.Dir)) {
					fmt.Println("aborted.")
					return nil
				}
				// The path first: the fingerprint reads through it.
				if err := samePath(ws, pinned); err != nil {
					return err
				}
				if len(blockers) > 0 {
					after, err := workspace.Fingerprint(st.Dir)
					if err != nil || after != before {
						return fmt.Errorf("%s changed while the prompt waited; --force covers only what it showed -- refusing to reap; run reap again to see what is there now", st.Dir)
					}
				}
				// Work can start while the prompt waits: a commit, a new file,
				// an agent in a second pane. Check again, as --reapable does, and
				// refuse on anything that was not on the list just shown.
				again, err := inspectTargetWith(ws, git, prs, muxLive())
				if err != nil {
					return err
				}
				if fresh := newBlockers(blockers, again.Blockers()); len(fresh) > 0 {
					fmt.Printf("\nsince the prompt, held by %d more thing(s):\n", len(fresh))
					for _, b := range fresh {
						fmt.Printf("  - %s\n", b)
					}
					return fmt.Errorf("refusing to reap; resolve the above, then run reap again")
				}
				st = again
			}
			// Kill any lingering session first. A live session holds file
			// handles on the working tree, which on some platforms would
			// refuse the removeAll below.
			// A worktree has no session of its own: one of its name belongs to
			// something else.
			if l := mux.Pick(); l != nil && !st.Worktree {
				_ = l.Kill(filepath.Base(st.Dir), st.Dir)
			}
			if err := workspace.Reap(st, git); err != nil {
				return err
			}
			fmt.Printf("removed %s\n", st.Dir)
			if st.Worktree {
				fmt.Printf("its branches stay in %s.\n", st.Clone[0].WorktreeOf)
				return nil
			}
			fmt.Println("the shared mirror was not touched.")
			for _, c := range st.Clone {
				if c.WorktreeOf != "" {
					fmt.Printf("%s was a worktree of %s; its branches stay there.\n", c.Dir, c.WorktreeOf)
				}
			}
			if st.Enclosing != "" {
				fmt.Printf("its tracked files are now deletions in %s; commit them there.\n", st.Enclosing)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&path, "path", "", "workspace (default: working directory)")
	cmd.Flags().BoolVar(&force, "force", false, "delete even when work would be lost")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "skip the confirmation prompt")
	cmd.Flags().BoolVar(&reapable, "reapable", false, "delete every workspace at the root that reap would delete now, after one confirmation")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "with --reapable or --safe: print the list, delete nothing")
	cmd.Flags().BoolVar(&closed, "closed", false, "with --reapable or --safe: only issue workspaces whose pull request is merged or closed")
	cmd.Flags().StringVar(&merged, "merged-head", "", "the commit a merged pull request had as its head: a branch tip equal to it counts as landed (the queue passes it after a squash merge); any commit beyond it still holds the workspace")
	cmd.Flags().BoolVar(&safe, "safe", false, "--reapable without a prompt, for unattended runs; refuses --force and a named workspace")
	return cmd
}

// reapConfirm asks before reap deletes; a package var so a test can act while
// the prompt waits.
var reapConfirm = confirm

// newBlockers returns the blockers in now that were not in shown.
func newBlockers(shown, now []string) []string {
	seen := map[string]bool{}
	for _, b := range shown {
		seen[b] = true
	}
	var out []string
	for _, b := range now {
		if !seen[b] {
			out = append(out, b)
		}
	}
	return out
}

// reapTarget resolves the workspace to reap from --path and an optional
// positional argument, which name the same thing.
//
// `facet reap <name>` used to answer `unknown command "<name>" for "facet reap"`:
// the command took --path only, and cobra.NoArgs rejected the positional. That is
// the first shape every operator reaches for, and the error names the wrong
// problem -- it reads as "reap is broken", not as "reap wants a flag". It has cost
// time more than once and is documented as a trap in prose, which is where a CLI
// puts things it has decided not to fix (facet#83).
//
// A positional is taken as a path, and falls back to <workspaces root>/<name>
// when it is a bare name and no such path exists: a workspace is referred to by
// name far more often than by path, and that fallback is what makes
// `facet reap iss-73` mean what it looks like it means.
//
// Both forms at once is an error, never a precedence rule. Two arguments that
// disagree about which directory to delete must not have a silent winner.
func reapTarget(path string, args []string) (string, error) {
	if len(args) == 0 {
		return config.ResolveWorkspace(path)
	}
	name := args[0]
	if path != "" {
		return "", fmt.Errorf("name the workspace once: --path %s and %q disagree; drop one", path, name)
	}
	if _, err := os.Stat(name); err != nil && !strings.ContainsRune(name, filepath.Separator) {
		if byName := filepath.Join(roots.Workspaces, name); byName != name {
			if _, err := os.Stat(byName); err == nil {
				return config.ResolveWorkspace(byName)
			}
		}
	}
	return config.ResolveWorkspace(name)
}

// inspectTarget inspects what reap was pointed at: a workspace when dir holds a
// manifest, a plain linked worktree otherwise. A directory inside a workspace
// (a clone, or a folder of one) is refused, since reaping it alone would leave
// that workspace short of what its manifest declares; so is the top level of a
// repository with a manifest, which is a clone that commits one, or a
// workspace someone ran `git init` in, never a workspace reap may delete. The
// enclosing workspace and the worktree are checked first, so a manifest a
// repository commits does not make its clone or worktree a workspace (#313).
// It inspects dir's real path, which is what reap deletes.
func inspectTarget(dir string, git gitx.Runner, live workspace.LiveChecker) (*workspace.IssueState, error) {
	return inspectTargetWith(dir, git, gh, live)
}

// mergedHeadLookup is the pull-request lookup with one commit known to be the
// head of a merged pull request. It is asked first; only that exact sha is
// proven by what the caller says, every other commit goes to GitHub as before.
type mergedHeadLookup struct {
	workspace.PRLookup
	head string
}

func (m mergedHeadLookup) MergedPRForSHA(repo, sha string) (*ghx.PRForCommit, error) {
	if l, ok := m.PRLookup.(workspace.CommitPRLookup); ok {
		if pr, err := l.MergedPRForSHA(repo, sha); err == nil && pr != nil {
			return pr, nil
		}
	}
	if sha == m.head {
		return &ghx.PRForCommit{}, nil
	}
	return nil, nil
}

// prLookupFor is gh, or gh told the merged head. An empty head is plain gh.
func prLookupFor(head string) workspace.PRLookup {
	head = strings.TrimSpace(head)
	if head == "" {
		return gh
	}
	return mergedHeadLookup{PRLookup: gh, head: head}
}

func inspectTargetWith(dir string, git gitx.Runner, pr workspace.PRLookup, live workspace.LiveChecker) (*workspace.IssueState, error) {
	// What reap deletes is where the path leads: a clone named through a
	// symlink outside its workspace is still inside it (#384), and a walk
	// does not descend into a symlink at its root (#381).
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return nil, fmt.Errorf("could not resolve %s: %w", dir, err)
	}
	if resolved, err = filepath.Abs(resolved); err != nil {
		return nil, err
	}
	dir = resolved
	if ws := enclosingWorkspace(git, dir); ws != "" {
		return nil, fmt.Errorf("%s is inside the workspace %s; reap the workspace instead\nfix: facet reap %s", dir, ws, ws)
	}
	top, linked := workspace.TopLevel(git, dir)
	if linked {
		return workspace.InspectWorktree(dir, git, pr, live)
	}
	if _, err := os.Stat(manifest.Path(dir)); err == nil {
		if top {
			return nil, fmt.Errorf("%s is the top level of a git repository, so it is not a workspace: its .git, with every commit, branch and stash, would go with it\n"+
				"fix: if it is a workspace someone ran `git init` in, push what that repository holds and move its .git out, then reap again", dir)
		}
		return workspace.Inspect(dir, git, pr, live)
	}
	return workspace.InspectWorktree(dir, git, pr, live)
}

// enclosingWorkspace returns the nearest workspace above dir, or "". A
// directory with a manifest that is a repository's top level (a checkout that
// commits one) is no workspace, so the search goes on above it.
func enclosingWorkspace(git gitx.Runner, dir string) string {
	for start := filepath.Dir(dir); start != dir; {
		ws, err := config.FindWorkspace(start)
		if err != nil {
			return ""
		}
		if top, _ := workspace.TopLevel(git, ws); !top {
			return ws
		}
		dir, start = ws, filepath.Dir(ws)
	}
	return ""
}

// targetPath is where a reap target's path leads: its real path, and the
// steps of the path that are symlinks. The check after the prompt compares it
// with the first, so a parent swapped for a symlink while the prompt waited
// cannot point the delete at another directory (#363).
type targetPath struct {
	real  string
	links []string
}

// pathOf reads where dir leads now.
func pathOf(dir string) (targetPath, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return targetPath{}, err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return targetPath{}, err
	}
	t := targetPath{real: resolved}
	for p := abs; ; p = filepath.Dir(p) {
		if fi, err := os.Lstat(p); err == nil && fi.Mode()&os.ModeSymlink != 0 {
			t.links = append(t.links, p)
		}
		if filepath.Dir(p) == p {
			break
		}
	}
	return t, nil
}

// samePath refuses when dir no longer leads where it did when before was read.
func samePath(dir string, before targetPath) error {
	now, err := pathOf(dir)
	if err != nil {
		return fmt.Errorf("could not resolve %s again after the prompt: %w -- refusing to reap", dir, err)
	}
	if now.real != before.real || strings.Join(now.links, "\x00") != strings.Join(before.links, "\x00") {
		return fmt.Errorf("%s led to %s before the prompt and leads to %s now, or a step of its path became or stopped being a symlink -- refusing to reap; run reap again", dir, before.real, now.real)
	}
	return nil
}

// muxLive returns a session checker, or nil when tmux is not installed.
// muxLive is every liveness check reap makes beyond git: the multiplexer's
// sessions and panes, processes through lsof, and loaded launchd jobs. The
// jobs are checked even without a multiplexer, and read anew for each call,
// so the check after a prompt sees a job loaded while it waited.
func muxLive() workspace.LiveChecker {
	c := liveChecks{jobs: jobs.Fresh()}
	if l := mux.Pick(); l != nil {
		c.mux = l
	}
	return c
}

// liveChecks joins the multiplexer's checks to the launchd one, so
// workspace.Inspect finds both through its optional interfaces.
type liveChecks struct {
	mux  workspace.LiveChecker
	jobs *launchd.Jobs
}

// fresh is the same checks with the launchd jobs read anew: a check after a
// prompt sees a job loaded while it waited.
func (c liveChecks) fresh() workspace.LiveChecker {
	c.jobs = c.jobs.Fresh()
	return c
}

func (c liveChecks) Live(session, dir string) bool {
	return c.mux != nil && c.mux.Live(session, dir)
}

func (c liveChecks) LiveRoots(dir string) ([]string, error) {
	if lr, ok := c.mux.(workspace.LiveRootChecker); ok {
		return lr.LiveRoots(dir)
	}
	// No multiplexer: a process rooted in the workspace still holds it.
	return mux.ProcessesUnder(dir)
}

func (c liveChecks) JobsUnder(dir string) ([]string, error) {
	if c.jobs == nil {
		return nil, nil
	}
	return c.jobs.Under(dir)
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.0f%c", float64(n)/float64(div), "KMGT"[exp])
}
