package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

// agentReport is one agent label's counts over the window. The fix lane
// is "fix" and the merge lane is "land", as the decision log names them.
type agentReport struct {
	Started    int            `json:"started"`
	ExitedOK   int            `json:"exited_ok"`
	ExitedFail int            `json:"exited_nonzero"`
	FixRuns    int            `json:"fix_runs"`
	Landed     int            `json:"landed"`
	Blocked    int            `json:"blocked"`
	Skips      map[string]int `json:"skips"`
}

// queueReport is what `facet queue report` prints.
type queueReport struct {
	Since     time.Time              `json:"since"`
	Until     time.Time              `json:"until"`
	Agents    map[string]agentReport `json:"agents"`
	Holds     holdReport             `json:"holds"`
	LastError *reportError           `json:"last_error"`
	// BlockedNow is every issue the state file records as blocked, whenever
	// it was blocked. Empty when the config has no state file.
	BlockedNow []string `json:"blocked_now"`
	// TornLines is how many lines of the decision log could not be read.
	// They are skipped, never counted.
	TornLines int `json:"torn_lines"`
}

type holdReport struct {
	Held     int  `json:"held"`
	Released int  `json:"released"`
	HeldNow  bool `json:"held_now"`
}

type reportError struct {
	At  string `json:"at"`
	Why string `json:"why"`
}

func newQueueReportCmd() *cobra.Command {
	var (
		configPath string
		since      time.Duration
		asJSON     bool
	)
	cmd := &cobra.Command{
		Use:   "report",
		Short: "Report what the queue did in a recent window, from its own files",
		Long: "Reads the decision log and the state file named in the queue config, and\n" +
			"prints, per agent label, the runs started, the exits with status 0 and not 0,\n" +
			"the fix runs, the pull requests landed, the issues blocked and the skips by\n" +
			"reason; then the holds and the last error. It makes no GitHub call.\n" +
			"Without --json it prints the same as a short table. A line of the log that\n" +
			"cannot be read is counted in torn_lines and skipped.\n\n" +
			"The log has no exit, landed or blocked rows from before this command\n" +
			"existed, so a window that reaches back past that shows zeros for them.",
		Args:              cobra.NoArgs,
		PersistentPreRunE: func(*cobra.Command, []string) error { return nil },
		RunE: func(cmd *cobra.Command, _ []string) error {
			path, err := queueConfigPath(configPath)
			if err != nil {
				return err
			}
			cfg, err := loadQueueConfig(path)
			if err != nil {
				return err
			}
			if cfg.Decisions == "" {
				cfg.Decisions = defaultQueueDecisionPath()
			}
			return runQueueReport(cfg, time.Now(), since, asJSON, cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "queue config JSON file (default: FACET_QUEUE)")
	cmd.Flags().DurationVar(&since, "since", 24*time.Hour, "how far back to count, such as 24h")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the report as JSON")
	return cmd
}

func runQueueReport(cfg queueConfig, now time.Time, since time.Duration, asJSON bool, out io.Writer) error {
	if since <= 0 {
		return fmt.Errorf("--since must be a positive duration, such as 24h")
	}
	if cfg.Decisions == "" {
		return fmt.Errorf("no decision log path\nfix: set decisions in the queue config, or FACET_QUEUE_GATE_LOG")
	}
	rep, err := buildQueueReport(cfg, now, since)
	if err != nil {
		return err
	}
	if asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}
	printQueueReport(out, rep)
	return nil
}

func buildQueueReport(cfg queueConfig, now time.Time, since time.Duration) (queueReport, error) {
	rep := queueReport{
		Since:      now.Add(-since).UTC(),
		Until:      now.UTC(),
		Agents:     map[string]agentReport{},
		BlockedNow: []string{},
	}
	f, err := os.Open(cfg.Decisions)
	if err != nil {
		return rep, fmt.Errorf("read decision log %s: %w\nfix: the queue writes the log when it runs; check the path in the config", cfg.Decisions, err)
	}
	defer func() { _ = f.Close() }()
	var lastHold string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), maxDecisionLineBytes)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var row queueDecision
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			rep.TornLines++
			continue
		}
		at, err := time.Parse(time.RFC3339, row.At)
		if err != nil {
			rep.TornLines++
			continue
		}
		if row.Source != queueDecisionSource {
			continue
		}
		switch row.Decision {
		case "held":
			lastHold = "held"
		case "released":
			lastHold = ""
		}
		if at.Before(rep.Since) || at.After(now) {
			continue
		}
		rep.add(row, at)
	}
	if err := sc.Err(); err != nil {
		return rep, fmt.Errorf("read decision log %s: %w", cfg.Decisions, err)
	}
	rep.Holds.HeldNow = lastHold != ""
	if cfg.State != "" {
		st, err := loadQueueState(cfg.State)
		if err != nil {
			return rep, err
		}
		for k, rec := range st.Fixes {
			if rec != nil && rec.Blocked {
				rep.BlockedNow = append(rep.BlockedNow, k)
			}
		}
		sort.Strings(rep.BlockedNow)
	}
	return rep, nil
}

func (rep *queueReport) add(row queueDecision, at time.Time) {
	switch row.Decision {
	case "held":
		rep.Holds.Held++
		return
	case "released":
		rep.Holds.Released++
		return
	case decisionError:
		rep.LastError = &reportError{At: at.UTC().Format(time.RFC3339), Why: row.Why}
		return
	}
	lane := row.Lane
	if lane == "" {
		lane = "(none)"
	}
	a, ok := rep.Agents[lane]
	if !ok {
		a.Skips = map[string]int{}
	}
	switch row.Decision {
	case "start":
		a.Started++
	case decisionExitOK:
		a.ExitedOK++
	case decisionExitFail:
		a.ExitedFail++
	case "fix_start":
		a.FixRuns++
	case decisionLanded:
		a.Landed++
	case decisionBlocked:
		a.Blocked++
	case "skip":
		a.Skips[row.Why]++
	default:
		return
	}
	rep.Agents[lane] = a
}

func printQueueReport(out io.Writer, rep queueReport) {
	_, _ = fmt.Fprintf(out, "queue report, %s to %s\n", rep.Since.Format(time.RFC3339), rep.Until.Format(time.RFC3339))
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "AGENT\tSTARTED\tEXIT 0\tEXIT !0\tFIX\tLANDED\tBLOCKED\tSKIPS")
	labels := make([]string, 0, len(rep.Agents))
	for l := range rep.Agents {
		labels = append(labels, l)
	}
	sort.Strings(labels)
	for _, l := range labels {
		a := rep.Agents[l]
		skips := 0
		for _, n := range a.Skips {
			skips += n
		}
		_, _ = fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%d\t%d\t%d\t%d\n", l, a.Started, a.ExitedOK, a.ExitedFail, a.FixRuns, a.Landed, a.Blocked, skips)
	}
	_ = tw.Flush()
	for _, l := range labels {
		reasons := make([]string, 0, len(rep.Agents[l].Skips))
		for why := range rep.Agents[l].Skips {
			reasons = append(reasons, why)
		}
		sort.Strings(reasons)
		for _, why := range reasons {
			_, _ = fmt.Fprintf(out, "skip %s: %d x %s\n", l, rep.Agents[l].Skips[why], why)
		}
	}
	_, _ = fmt.Fprintf(out, "holds: %d held, %d released, held now: %t\n", rep.Holds.Held, rep.Holds.Released, rep.Holds.HeldNow)
	if rep.LastError != nil {
		_, _ = fmt.Fprintf(out, "last error: %s %s\n", rep.LastError.At, rep.LastError.Why)
	} else {
		_, _ = fmt.Fprintln(out, "last error: none")
	}
	if len(rep.BlockedNow) > 0 {
		_, _ = fmt.Fprintf(out, "blocked now: %s\n", strings.Join(rep.BlockedNow, ", "))
	}
	if rep.TornLines > 0 {
		_, _ = fmt.Fprintf(out, "torn lines skipped: %d\n", rep.TornLines)
	}
}
