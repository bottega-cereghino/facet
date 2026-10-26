package main

import (
	"fmt"
	"strconv"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

func newSpawnCmd() *cobra.Command {
	var (
		repo      string
		clones    []string
		addClones []string
		rmClones  []string
		scope     []string
		slug      string
		base      string
		yes       bool
		noBranch  bool
		dryRun    bool
		writeback bool
		launchF   launchFlags
	)
	cmd := &cobra.Command{
		Use:   "spawn <issue-number>",
		Short: "Create an ephemeral workspace for one GitHub issue",
		Long: "Reads the issue, works out which repositories it needs, shows you why, and\n" +
			"waits. On confirmation it creates an issue-linked branch, clones each repo\n" +
			"from the local mirror, and writes a CLAUDE.md carrying the issue body and the\n" +
			"durable hazards recorded for its areas.\n\n" +
			"It records the issues the workspace covers in .scope, one per line: the\n" +
			"spawned issue first, then any --scope. `facet scope` reads and amends it.\n\n" +
			"Labels alone cannot decide the repo set: the same topic label is used in\n" +
			"several repos, and a cross-repo dependency lives in the issue body. So the\n" +
			"inference is always shown and never silently trusted.\n\n" +
			"It sets the workspace up and stops there: it prints where to work and runs\n" +
			"nothing. To start something there, in this terminal, pass --claude (with\n" +
			"Remote Control when routing's spawn.rc says so, or --remote) or --exec\n" +
			"<command>, for example --exec 'code .' or --exec 'tmux new-window -c \"$PWD\"'.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			number, err := strconv.Atoi(args[0])
			if err != nil {
				return fmt.Errorf("issue number: %w", err)
			}
			l, err := launchF.resolve(cmd.Flags())
			if err != nil {
				return err
			}
			return runSpawn(spawnOpts{
				Number: number, Repo: repo, Clones: clones, Add: addClones, Remove: rmClones,
				Scope: scope,
				Slug:  slug, Base: base, Yes: yes, NoBranch: noBranch, DryRun: dryRun,
				Writeback: writeback, Launch: l,
			})
		},
	}
	f := cmd.Flags()
	f.StringVar(&repo, "repo", "", "the issue's home repository, as owner/name (required)")
	f.StringSliceVar(&clones, "clone", nil, "replace the inferred repo set entirely: a routing key, or key=url; repeatable. An unrecognised value refuses")
	f.StringSliceVar(&addClones, "add", nil, "add repos to the inferred set: a routing key, or key=url; repeatable. An unrecognised value refuses")
	f.StringSliceVar(&rmClones, "rm", nil, "drop repos from the inferred set: a routing key, or key=url; repeatable. An unrecognised value refuses")
	f.StringSliceVar(&scope, "scope", nil, "another issue this workspace covers, as owner/repo#n; repeatable. The spawned issue is always included")
	deprecateSeatFlags(f)
	f.StringVar(&slug, "slug", "", "override the slug derived from the issue title")
	f.StringVar(&base, "base", "main", "base branch for the issue branch")
	f.BoolVarP(&yes, "yes", "y", false, "skip the confirmation prompt")
	f.BoolVar(&noBranch, "no-branch", false, "do not create or check out an issue branch")
	f.BoolVar(&dryRun, "dry-run", false, "show the inference and exit, creating nothing")
	f.BoolVar(&writeback, "writeback", false,
		"record the confirmed repo set in the issue body (default: off -- a wrong "+
			"inference would otherwise write to someone else's issue, facet#69)")
	deprecateUnsoundFlag(f)
	launchF.register(f)
	return cmd
}

type spawnOpts struct {
	Number              int
	Repo                string
	Clones, Add, Remove []string
	// Scope lists further issues the workspace covers beyond the one being
	// spawned for.
	Scope                 []string
	Slug, Base            string
	Yes, NoBranch, DryRun bool
	// Writeback records the confirmed repo set in the issue body when true.
	// Default is false (facet#69): a wrong inference used to write to
	// someone else's issue silently, on by default. Leaving it off means the
	// confirmed set is simply re-inferred on every spawn, which is cheap.
	Writeback bool
	// Launch is what to run in the home clone once the workspace is ready.
	// The zero value runs nothing.
	Launch launch
}

func issueBranchName(number int, slug string) string {
	return fmt.Sprintf("feature/%d-%s", number, slug)
}

// deprecateSeatFlags keeps spawn's --seat and --seat-issue accepted, and
// hidden, now that facet no longer records who a workspace belongs to.
func deprecateSeatFlags(f *pflag.FlagSet) {
	f.String("seat", "", "no effect: .seat is no longer written")
	f.String("seat-issue", "", "no effect: .seat-issue is no longer written")
	_ = f.MarkDeprecated("seat", ".seat is no longer written; the flag has no effect")
	_ = f.MarkDeprecated("seat-issue", ".seat-issue is no longer written; the flag has no effect")
}
