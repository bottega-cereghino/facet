package main

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
)

// newQueueCmd runs one configured agent per queued issue.
//
// The command lines live in the config file, not here: an adopter's agents
// are theirs, and a title from an issue is someone else's text, so the
// binary neither names an agent nor passes that text through a shell.
// The queue does not reap. Deleting a workspace is a separate, deliberate act.
func newQueueCmd() *cobra.Command {
	var (
		configPath string
		dryRun     bool
		logPath    string
	)
	cmd := &cobra.Command{
		Use:   "queue",
		Short: "Run a configured agent on each queued issue",
		Long: "Takes the next open issue carrying the config's label, spawns a workspace for\n" +
			"it, and starts the agent command the config names for the issue's labels. When\n" +
			"that agent exits, it takes the next issue. At most max agents run at once\n" +
			"across all repositories, and at most per_repo agents run in one repository.\n\n" +
			"The commands, the caps, the gate and the hold file come from the JSON config\n" +
			"(--config, or FACET_QUEUE). docs/queue.example.json is an example.\n\n" +
			"A label lane:NAME is a preference. When lanes.command says that lane's room\n" +
			"is \"no\", the same tick starts the empty-label agent instead, and lanes.log\n" +
			"gains a row with fallback_from. Any other answer, or a command that fails,\n" +
			"keeps the labelled agent. A model: label is not a preference.\n\n" +
			"When priorities names a file, each line is a repository name, highest first,\n" +
			"and a # starts a comment. The queue starts work only in the highest project\n" +
			"that has an open issue with the label. Within that project it takes p1 before\n" +
			"p2, p2 before p3, and p3 before an issue with none of those, oldest first.\n\n" +
			"effort.labels maps an issue label to an effort. The default is medium.\n" +
			"{effort} in a command is where that word goes. Two labels that name\n" +
			"different efforts are not started. Each start is logged with its effort.\n\n" +
			"bundle on an agent is how many issues one run of that command takes from\n" +
			"the same repository. Omitted means 1 and the maximum is 5. An issue\n" +
			"labelled feat or research is always a run of one. The workspace is the\n" +
			"first issue's. Each start is logged with the bundle size.\n\n" +
			"Before each start it runs the gate command, when one is set. The gate's\n" +
			"arguments are expanded with the issue about to start. {workspace} is empty,\n" +
			"because the gate runs before a workspace is spawned, and {prompt} is not\n" +
			"substituted. A non-zero exit waits and tries later. When the hold file\n" +
			"exists, it starts nothing and stops; an agent already running is left to finish.\n\n" +
			"--dry-run prints the runs the next starts would take, up to max, and does\n" +
			"not spawn a workspace or start an agent. It runs the gate, so a gate that\n" +
			"would refuse a start is reported instead of a start.\n\n" +
			"With a fix block, each tick first looks for an open pull request of an\n" +
			"issue this queue started (recorded in the state file) that is red or\n" +
			"conflicting, and starts a fix run for it instead of new work. After\n" +
			"fix.tries failed tries it labels the issue blocked, and leaves it. With\n" +
			"fix.land (the default) a green one behind its base is updated by merging\n" +
			"the base, and a green one up to date is merged, without an agent, at most\n" +
			"one per repository per round. A merge GitHub refuses waits, doubling up\n" +
			"to an hour, and stops after five. A fix that suppresses a finding is not\n" +
			"landed. Optional serialize.paths serializes open\n" +
			"queue pull requests touching a matching glob one at a time per repository.\n" +
			"A person's own pull request, or a fork's, is never touched.\n\n" +
			"It does not reap workspaces. A workspace already on disk for the issue is\n" +
			"reused, not spawned again.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			path, err := queueConfigPath(configPath)
			if err != nil {
				return err
			}
			cfg, err := loadQueueConfig(path)
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			env := realQueueEnv()
			if dryRun {
				return dryQueue(ctx, cfg, env)
			}
			if cfg.Decisions == "" {
				cfg.Decisions = defaultQueueDecisionPath()
			}
			return runQueueWithLog(ctx, cfg, env, logPath)
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "queue config JSON file (default: FACET_QUEUE)")
	cmd.Flags().StringVar(&logPath, "log", "", "write queue output to this file and reopen it on SIGHUP")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print what the next starts would take, without spawning or starting an agent")
	return cmd
}

// queueConfigPath is --config when it was passed, otherwise FACET_QUEUE.
func queueConfigPath(flag string) (string, error) {
	if flag != "" {
		return flag, nil
	}
	if v := os.Getenv("FACET_QUEUE"); v != "" {
		return v, nil
	}
	return "", fmt.Errorf("no queue config\nfix: pass --config or set FACET_QUEUE (docs/queue.example.json is an example)")
}
