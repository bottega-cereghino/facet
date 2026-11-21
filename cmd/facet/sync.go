package main

import (
	"fmt"
	"os"

	"github.com/bottega-cereghino/facet/internal/config"
	"github.com/bottega-cereghino/facet/internal/mirror"
	"github.com/bottega-cereghino/facet/internal/workspace"
	"github.com/spf13/cobra"
)

func newSyncCmd() *cobra.Command {
	var (
		path      string
		prune     bool
		bootstrap bool
		viaMirror bool
		noWT      bool
	)
	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Make the workspace directory match its manifest",
		Long: "Creates any missing link or clone declared in .workspace.json.\n\n" +
			"An existing clone is never touched -- no pull, no reset, no clean -- because\n" +
			"it may hold the only copy of unpushed work. --prune deletes only links.\n\n" +
			"A missing clone whose repository is already checked out under the projects\n" +
			"root, as <projects>/<name> with the same origin, is made a git worktree of\n" +
			"that checkout, on a detached HEAD at origin's default branch. The checkout's\n" +
			"own branch and files are not touched. --no-worktree clones instead.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runSync(path, prune, bootstrap, viaMirror, noWT)
		},
	}
	cmd.Flags().StringVar(&path, "path", "", "workspace directory (default: working directory)")
	cmd.Flags().BoolVar(&prune, "prune", false, "remove links present on disk but absent from the manifest")
	cmd.Flags().BoolVar(&bootstrap, "bootstrap", false, "clone a link's missing target from its recorded origin")
	cmd.Flags().BoolVar(&viaMirror, "via-mirror", false, "clone from a local bare mirror, hardlinking the object store")
	cmd.Flags().BoolVar(&noWT, noWorktreeFlag, false, noWorktreeHelp)
	deprecateUnsoundFlag(cmd.Flags())
	return cmd
}

// noWorktreeFlag keeps every clone a repository of its own, for sync, restore
// and spawn.
const (
	noWorktreeFlag = "no-worktree"
	noWorktreeHelp = "clone every repository, even one already checked out under the projects root"
)

// runSync is sync's body, split from the cobra wiring so it can be driven
// directly in tests.
func runSync(path string, prune, bootstrap, viaMirror, noWorktree bool) error {
	ws, err := config.ResolveWorkspace(path)
	if err != nil {
		return err
	}
	rep := workspace.Reporter{W: os.Stdout}
	return workspace.Sync(roots, ws, git, rep, workspace.SyncOptions{
		Prune:     prune,
		Bootstrap: bootstrap,
		Source:    sourceFor(viaMirror, rep),
		Worktrees: !noWorktree,
	})
}

// sourceFor picks where clones come from: straight off the forge, or hardlinked
// out of a local bare mirror.
func sourceFor(viaMirror bool, rep workspace.Reporter) workspace.SourceResolver {
	if !viaMirror {
		return workspace.DirectSource{}
	}
	return &mirror.Store{
		Root:   roots.Mirrors,
		Git:    git,
		Report: func(f string, a ...any) { rep.Working(f, a...) },
		Warn:   func(f string, a ...any) { rep.Warn(f, a...) },
	}
}

func newRestoreCmd() *cobra.Command {
	var noWT bool
	cmd := &cobra.Command{
		Use:   "restore",
		Short: "Bootstrap and sync every workspace (new-machine entry point)",
		Long: "Runs `sync --bootstrap` over every workspace under the workspaces root.\n" +
			"Ephemeral issue workspaces are skipped: they are gitignored by design.\n" +
			"A clone whose repository is checked out under the projects root becomes a\n" +
			"worktree of it, as in sync.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runRestore(noWT)
		},
	}
	cmd.Flags().BoolVar(&noWT, noWorktreeFlag, false, noWorktreeHelp)
	deprecateUnsoundFlag(cmd.Flags())
	return cmd
}

// runRestore is restore's body: sync --bootstrap over every workspace.
func runRestore(noWorktree bool) error {
	dirs, err := workspace.Dirs(roots.Workspaces, false)
	if err != nil {
		return err
	}
	rep := workspace.Reporter{W: os.Stdout}
	for _, dir := range dirs {
		if err := workspace.Sync(roots, dir, git, rep, workspace.SyncOptions{Bootstrap: true, Worktrees: !noWorktree}); err != nil {
			return fmt.Errorf("%s: %w", dir, err)
		}
	}
	return nil
}
