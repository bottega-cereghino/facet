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
	var configPath string
	cmd := &cobra.Command{
		Use:   "queue",
		Short: "Run a configured agent on each queued issue",
		Long: "Takes the next open issue carrying the config's label, spawns a workspace for\n" +
			"it, and starts the agent command the config names for the issue's labels. When\n" +
			"that agent exits, it takes the next issue. At most max agents run at once,\n" +
			"across every repository in the config.\n\n" +
			"The commands, the cap, the gate, the hold file and the order all come from the\n" +
			"JSON config (--config, or FACET_QUEUE). docs/queue.example.json is an example.\n\n" +
			"Before each start it runs the gate command, when one is set. A non-zero exit\n" +
			"waits and tries later. When the hold file exists, it starts nothing and stops;\n" +
			"an agent already running is left to finish.\n\n" +
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
			return runQueue(ctx, cfg, realQueueEnv())
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "queue config JSON file (default: FACET_QUEUE)")
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
