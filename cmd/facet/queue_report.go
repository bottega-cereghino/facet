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
	// Census is the latest census row in the window and the starved minutes,
	// read from census.jsonl. It is null when the config sets no census.
	Census *censusReport `json:"census"`
}

// censusReport is the census part of the report. Rows is how many census.jsonl
// rows fall in the window; with none, the counts are zero and At is empty, so
// a window without data is told apart from a quiet queue.
type censusReport struct {
	Rows              int            `json:"rows"`
	At                string         `json:"at"`
	Startable         int            `json:"startable"`
	Running           int            `json:"running"`
	FreeSlots         int            `json:"free_slots"`
	ByReason          map[string]int `json:"by_reason"`
	StarvedMinutes24h int            `json:"starved_minutes_24h"`
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
			"reason; then the holds and the last error. When the config sets census, it\n" +
			"also reads census.jsonl: the latest counts, and the minutes in the last 24\n" +
			"hours with a free slot and nothing startable. It makes no GitHub call.\n" +
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
	if cfg.Census != "" {
		c, err := buildCensusReport(cfg.Census, now)
		if err != nil {
			return rep, err
		}
		rep.Census = &c
	}
	return rep, nil
}

// buildCensusReport reads census.jsonl. Each row stands for the time until the
// next row, at most censusLogEvery, because the queue writes one row an hour
// and a longer gap means it was not running. The last row stands until now,
// at most censusLogEvery. A minute is starved when a slot was free, nothing
// was startable and labelled issues existed. The window is always the last
// 24 hours, whatever --since says, as the field name says.
func buildCensusReport(census string, now time.Time) (censusReport, error) {
	path := censusLogPath(census)
	rep := censusReport{ByReason: map[string]int{}}
	f, err := os.Open(path)
	if err != nil {
		return rep, fmt.Errorf("read census log %s: %w\nfix: the queue writes it once an hour when census is set; check the path in the config", path, err)
	}
	defer func() { _ = f.Close() }()
	var rows []censusRow
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), maxDecisionLineBytes)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var row censusRow
		if err := json.Unmarshal([]byte(line), &row); err != nil || row.At.IsZero() {
			continue
		}
		rows = append(rows, row)
	}
	if err := sc.Err(); err != nil {
		return rep, fmt.Errorf("read census log %s: %w", path, err)
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].At.Before(rows[j].At) })
	from := now.Add(-24 * time.Hour)
	var starved time.Duration
	for i, row := range rows {
		end := now
		if i+1 < len(rows) {
			end = rows[i+1].At
		}
		if end.After(row.At.Add(censusLogEvery)) {
			end = row.At.Add(censusLogEvery)
		}
		start := row.At
		if start.Before(from) {
			start = from
		}
		if end.After(now) {
			end = now
		}
		if !end.After(start) {
			continue
		}
		if row.FreeSlots > 0 && row.Startable == 0 && row.Total > 0 {
			starved += end.Sub(start)
		}
		if !row.At.After(now) {
			rep.Rows++
			rep.At = row.At.UTC().Format(time.RFC3339)
			rep.Startable, rep.Running, rep.FreeSlots = row.Startable, row.Running, row.FreeSlots
			rep.ByReason = row.ByReason
		}
	}
	if rep.ByReason == nil {
		rep.ByReason = map[string]int{}
	}
	rep.StarvedMinutes24h = int(starved / time.Minute)
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
	if c := rep.Census; c != nil {
		_, _ = fmt.Fprintf(out, "census: %d startable, %d running, %d free slots, %d starved minutes in 24h\n", c.Startable, c.Running, c.FreeSlots, c.StarvedMinutes24h)
	}
	if rep.TornLines > 0 {
		_, _ = fmt.Fprintf(out, "torn lines skipped: %d\n", rep.TornLines)
	}
}
