package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// exitCodeError exits main with a specific status without printing an extra
// error line, for commands whose own output already explained the result.
type exitCodeError struct {
	code int
}

func (e exitCodeError) Error() string {
	return fmt.Sprintf("exit status %d", e.code)
}

// heartbeatRun is one agent run currently in progress, as recorded in the
// heartbeat file.
type heartbeatRun struct {
	Issue string    `json:"issue"`
	Agent string    `json:"agent"`
	Since time.Time `json:"since"`
}

// queueHeartbeat is the JSON object written atomically to the heartbeat file
// on every tick and when a run starts or ends.
type queueHeartbeat struct {
	At        time.Time      `json:"at"`
	PID       int            `json:"pid"`
	Version   string         `json:"version"`
	Running   []heartbeatRun `json:"running"`
	Held      []string       `json:"held"`
	LastError string         `json:"last_error"`
	// ListPausedUntil is set while a GitHub rate limit keeps the queue from
	// listing issues; it is the reset time.
	ListPausedUntil *time.Time `json:"list_paused_until,omitempty"`
	// IgnoredPRs are open pull requests that carry a start's marker line
	// and fail the ownership check, so the queue leaves them alone.
	IgnoredPRs []ignoredPR `json:"ignored_prs,omitempty"`
	// StalledPRs are queue pull requests reported as unlanded for stallAfter.
	StalledPRs []stalledPR `json:"stalled_prs,omitempty"`
}

// queueStatusJSON is the --json output of `facet queue status`: the heartbeat
// object plus age_seconds and state.
type queueStatusJSON struct {
	At              *time.Time     `json:"at,omitempty"`
	PID             int            `json:"pid"`
	Version         string         `json:"version"`
	Running         []heartbeatRun `json:"running"`
	Held            []string       `json:"held"`
	LastError       string         `json:"last_error"`
	ListPausedUntil *time.Time     `json:"list_paused_until,omitempty"`
	IgnoredPRs      []ignoredPR    `json:"ignored_prs,omitempty"`
	StalledPRs      []stalledPR    `json:"stalled_prs,omitempty"`
	AgeSeconds      *float64       `json:"age_seconds"`
	State           string         `json:"state"`
}

// existingHoldFiles returns every hold file in paths that currently exists.
func existingHoldFiles(paths []string) ([]string, error) {
	held := []string{}
	for _, path := range paths {
		_, err := os.Stat(path)
		if err == nil {
			held = append(held, path)
			continue
		}
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("hold file %s: %w\nfix: the queue stops when it cannot tell whether the hold file exists", path, err)
		}
	}
	return held, nil
}

func (r *queueRunner) setLastError(msg string) {
	r.mu.Lock()
	r.lastError = msg
	r.mu.Unlock()
	if msg != "" {
		r.recordDecision("", "", "", decisionError, firstErrorLine(msg))
	}
}

func (r *queueRunner) heartbeatSnapshot() queueHeartbeat {
	held, err := existingHoldFiles(r.cfg.Hold)
	r.mu.Lock()
	now := r.env.now().UTC()
	lastErr := r.lastError
	var paused *time.Time
	if now.Before(r.listWaitUntil) {
		until := r.listWaitUntil.UTC()
		paused = &until
	}
	if err != nil {
		lastErr = err.Error()
		held = []string{}
	}
	running := make([]heartbeatRun, 0, len(r.active))
	for _, run := range r.active {
		running = append(running, run)
	}
	ignored := r.ignoredListLocked()
	stalled := r.stalledListLocked()
	r.mu.Unlock()
	sort.Slice(running, func(i, j int) bool {
		if !running[i].Since.Equal(running[j].Since) {
			return running[i].Since.Before(running[j].Since)
		}
		return running[i].Issue < running[j].Issue
	})
	return queueHeartbeat{
		At:              now,
		PID:             os.Getpid(),
		Version:         buildVersion(),
		Running:         running,
		Held:            held,
		LastError:       lastErr,
		ListPausedUntil: paused,
		IgnoredPRs:      ignored,
		StalledPRs:      stalled,
	}
}

// writeHeartbeat writes the heartbeat file atomically. A dry run or a config
// without heartbeat does nothing. Writes are serialized by heartbeatMu so an
// older snapshot cannot overwrite a newer one.
func (r *queueRunner) writeHeartbeat() {
	if r.dry || r.cfg.Heartbeat == "" {
		return
	}
	r.heartbeatMu.Lock()
	defer r.heartbeatMu.Unlock()
	hb := r.heartbeatSnapshot()
	if err := writeHeartbeatFile(r.cfg.Heartbeat, hb); err != nil {
		r.logf("queue: %v", err)
	}
}

// writeHeartbeatFile writes hb as one JSON object atomically: temp file in
// the same directory, flushed to disk, renamed into place, directory flushed.
func writeHeartbeatFile(path string, hb queueHeartbeat) error {
	if hb.Running == nil {
		hb.Running = []heartbeatRun{}
	}
	if hb.Held == nil {
		hb.Held = []string{}
	}
	b, err := json.MarshalIndent(hb, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("write queue heartbeat: %w\nfix: the directory of heartbeat must be writable", err)
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp*")
	if err != nil {
		return fmt.Errorf("write queue heartbeat: %w\nfix: the directory of heartbeat must exist and be writable", err)
	}
	name := tmp.Name()
	_, werr := tmp.Write(append(b, '\n'))
	serr := syncFile(tmp)
	cerr := tmp.Close()
	if err := firstErr(werr, serr, cerr); err != nil {
		_ = os.Remove(name)
		return fmt.Errorf("write queue heartbeat %s: %w", path, err)
	}
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return fmt.Errorf("write queue heartbeat %s: %w", path, err)
	}
	return syncDir(dir)
}

// pulseHeartbeat refreshes the heartbeat on every wait interval while a
// long-running gate is executing, so a live gate does not look stuck.
func (r *queueRunner) pulseHeartbeat(ctx context.Context) func() {
	if r.dry || r.cfg.Heartbeat == "" || r.cfg.Wait <= 0 {
		return func() {}
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(r.cfg.Wait)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				r.writeHeartbeat()
			}
		}
	}()
	return func() {
		close(stop)
		<-done
	}
}

func newQueueStatusCmd() *cobra.Command {
	var (
		configPath string
		asJSON     bool
	)
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Check the queue heartbeat and exit with its health",
		Long: "Reads the heartbeat file named in the queue config and prints one line\n" +
			"per fact (or JSON with --json). Makes no network calls and runs no gate.\n\n" +
			"Exit codes: 0 when the heartbeat is younger than 3 * wait, 1 when it is\n" +
			"older or missing (down or stuck), 2 when a hold file exists.",
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
			return runQueueStatus(cfg, time.Now(), asJSON, cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "queue config JSON file (default: FACET_QUEUE)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the heartbeat object with age_seconds and state as JSON")
	return cmd
}

func readHeartbeatFile(path string) (queueHeartbeat, bool, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return queueHeartbeat{}, false, nil
		}
		return queueHeartbeat{}, false, fmt.Errorf("read queue heartbeat %s: %w", path, err)
	}
	var hb queueHeartbeat
	if err := json.Unmarshal(b, &hb); err != nil {
		return queueHeartbeat{}, false, fmt.Errorf("parse queue heartbeat %s: %w", path, err)
	}
	if hb.Running == nil {
		hb.Running = []heartbeatRun{}
	}
	if hb.Held == nil {
		hb.Held = []string{}
	}
	return hb, true, nil
}

func runQueueStatus(cfg queueConfig, now time.Time, asJSON bool, out io.Writer) error {
	if cfg.Heartbeat == "" {
		return fmt.Errorf("queue config has no heartbeat path\nfix: set heartbeat to a file path in the queue config")
	}
	held, err := existingHoldFiles(cfg.Hold)
	if err != nil {
		return err
	}
	hb, found, readErr := readHeartbeatFile(cfg.Heartbeat)
	st, code := buildQueueStatus(cfg, hb, found, readErr, held, now)
	if asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(st); err != nil {
			return err
		}
	} else {
		printQueueStatusText(out, cfg.Heartbeat, st, !found)
	}
	if code != 0 {
		return exitCodeError{code: code}
	}
	return nil
}

func buildQueueStatus(cfg queueConfig, hb queueHeartbeat, found bool, readErr error, held []string, now time.Time) (queueStatusJSON, int) {
	st := queueStatusJSON{
		PID:             hb.PID,
		Version:         hb.Version,
		Running:         hb.Running,
		Held:            held,
		LastError:       hb.LastError,
		ListPausedUntil: hb.ListPausedUntil,
		IgnoredPRs:      hb.IgnoredPRs,
		StalledPRs:      hb.StalledPRs,
	}
	if st.Running == nil {
		st.Running = []heartbeatRun{}
	}
	if st.Held == nil {
		st.Held = []string{}
	}
	if found && !hb.At.IsZero() {
		at := hb.At.UTC()
		st.At = &at
		age := now.Sub(at)
		secs := math.Round(age.Seconds()*1000) / 1000
		st.AgeSeconds = &secs
	}
	if readErr != nil && st.LastError == "" {
		st.LastError = readErr.Error()
	}
	if len(held) > 0 {
		st.State = "held"
		return st, 2
	}
	if readErr != nil {
		st.State = "error"
		return st, 1
	}
	if !found || hb.At.IsZero() {
		st.State = "missing"
		return st, 1
	}
	if now.Sub(hb.At) < 3*cfg.Wait {
		st.State = "ok"
		return st, 0
	}
	st.State = "stale"
	return st, 1
}

func printQueueStatusText(out io.Writer, path string, st queueStatusJSON, missing bool) {
	_, _ = fmt.Fprintf(out, "state: %s\n", st.State)
	if missing {
		_, _ = fmt.Fprintf(out, "heartbeat: %s (not found)\n", path)
	} else if st.At != nil {
		_, _ = fmt.Fprintf(out, "at: %s\n", st.At.Format(time.RFC3339))
		if st.AgeSeconds != nil {
			_, _ = fmt.Fprintf(out, "age: %s\n", (time.Duration(*st.AgeSeconds * float64(time.Second))).Truncate(time.Second))
		}
		_, _ = fmt.Fprintf(out, "pid: %d\n", st.PID)
		if st.Version != "" {
			_, _ = fmt.Fprintf(out, "version: %s\n", st.Version)
		}
	}
	if len(st.Running) == 0 {
		_, _ = fmt.Fprintln(out, "running: 0")
	} else {
		_, _ = fmt.Fprintf(out, "running: %d\n", len(st.Running))
		for _, r := range st.Running {
			_, _ = fmt.Fprintf(out, "  %s (%s, since %s)\n", r.Issue, r.Agent, r.Since.UTC().Format(time.RFC3339))
		}
	}
	if len(st.Held) == 0 {
		_, _ = fmt.Fprintln(out, "held: none")
	} else {
		_, _ = fmt.Fprintf(out, "held: %s\n", strings.Join(st.Held, ", "))
	}
	if st.LastError == "" {
		_, _ = fmt.Fprintln(out, "last_error: none")
	} else {
		_, _ = fmt.Fprintf(out, "last_error: %s\n", st.LastError)
	}
	if st.ListPausedUntil != nil {
		_, _ = fmt.Fprintf(out, "listing paused until: %s (GitHub rate limit)\n", st.ListPausedUntil.Format(time.RFC3339))
	}
	for _, p := range st.IgnoredPRs {
		_, _ = fmt.Fprintln(out, p.warning())
	}
	for _, p := range st.StalledPRs {
		_, _ = fmt.Fprintln(out, p.warning())
	}
}
