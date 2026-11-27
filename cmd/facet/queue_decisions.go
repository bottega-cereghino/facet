package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	queueDecisionEnv          = "FACET_QUEUE_GATE_LOG"
	queueDecisionRelativePath = ".local/state/fleet/queue-gate.jsonl"
	queueDecisionSource       = "queue"
	maxDecisionLineBytes      = 1024 * 1024
)

// Decisions the report counts. The others (start, skip, fix_start, held,
// released) are spelled where they are written.
const (
	decisionExitOK   = "exit_ok"
	decisionExitFail = "exit_fail"
	decisionLanded   = "landed"
	decisionBlocked  = "blocked"
	decisionError    = "error"
)

// queueDecision is one JSONL record written to the shared dispatch decision log.
// Every field is always serialized so lines match the shape written by gate scripts.
type queueDecision struct {
	At       string `json:"at"`
	Repo     string `json:"repo"`
	Issue    string `json:"issue"`
	Lane     string `json:"lane"`
	Decision string `json:"decision"`
	Why      string `json:"why"`
	Source   string `json:"source"`
}

// defaultQueueDecisionPath resolves the decision log path when the config
// leaves decisions empty: FACET_QUEUE_GATE_LOG first, then the default under HOME.
func defaultQueueDecisionPath() string {
	if v := strings.TrimSpace(os.Getenv(queueDecisionEnv)); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, queueDecisionRelativePath)
}

// lastHoldPath reads the last queue-sourced held/released transition in path
// so repeated polls or process restarts emit one line per state change.
// Any read or parse failure returns "" and never stops the queue.
func lastHoldPath(path string) string {
	if path == "" {
		return ""
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()

	var held string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), maxDecisionLineBytes)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var row queueDecision
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			continue
		}
		if row.Source != queueDecisionSource {
			continue
		}
		switch row.Decision {
		case "held":
			held = row.Why
		case "released":
			held = ""
		}
	}
	return held
}

// recordHold emits "held" when a hold file appears (or changes) and "released"
// when the previous hold file is gone. Unchanged polls emit nothing.
func (r *queueRunner) recordHold(held string) {
	if r.dry || r.cfg.Decisions == "" {
		return
	}
	r.mu.Lock()
	prev := r.lastHeld
	if held == prev {
		r.mu.Unlock()
		return
	}
	r.lastHeld = held
	r.mu.Unlock()
	if held != "" {
		r.recordDecision("", "", "", "held", held)
		return
	}
	r.recordDecision("", "", "", "released", prev)
}

// recordDecision appends one JSON object line to the decision log. A skip row
// for an issue is written once per change of reason: the same reason again
// writes nothing until another row for the issue, or a new reason, comes.
// A failed write logs a warning and never stops the queue.
func (r *queueRunner) recordDecision(repo, issue, lane, decision, why string) {
	if r.dry || r.cfg.Decisions == "" {
		return
	}
	row := queueDecision{
		At:       r.env.now().Format(time.RFC3339),
		Repo:     repo,
		Issue:    issue,
		Lane:     lane,
		Decision: decision,
		Why:      why,
		Source:   queueDecisionSource,
	}
	b, err := json.Marshal(row)
	if err != nil {
		r.logf("queue: marshal decision %s: %v", decision, err)
		return
	}
	b = append(b, '\n')

	r.decisionMu.Lock()
	key := repo + "#" + issue
	if issue != "" {
		if decision == "skip" {
			if prev, ok := r.lastSkip[key]; ok && prev == why {
				r.decisionMu.Unlock()
				return
			}
		} else {
			delete(r.lastSkip, key)
		}
	}
	err = appendDecisionLine(r.cfg.Decisions, b)
	if err == nil && decision == "skip" && issue != "" {
		if r.lastSkip == nil {
			r.lastSkip = map[string]string{}
		}
		r.lastSkip[key] = why
	}
	r.decisionMu.Unlock()
	if err != nil {
		r.logf("queue: write decision log %s: %v", r.cfg.Decisions, err)
	}
}

func appendDecisionLine(path string, line []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create directory: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	_, werr := f.Write(line)
	cerr := f.Close()
	if werr != nil {
		return werr
	}
	return cerr
}

// laneFor returns the matched agent label for iss, or "default" when the
// fallback agent matches.
func (r *queueRunner) laneFor(iss queuedIssue) string {
	label, _, err := r.cfg.commandFor(iss.Issue.LabelNames())
	if err != nil {
		return ""
	}
	if label == "" {
		return "default"
	}
	return label
}

// commandSummary formats an expanded command for the decision log, omitting any
// argument whose template contained {prompt} so multi-kilobyte prompts stay out
// of the JSONL file.
func commandSummary(template, argv []string) string {
	parts := make([]string, 0, len(argv))
	for i, arg := range argv {
		if i < len(template) && strings.Contains(template[i], "{prompt}") {
			continue
		}
		parts = append(parts, arg)
	}
	if len(parts) == 0 && len(argv) > 0 {
		return argv[0]
	}
	return strings.Join(parts, " ")
}

func firstErrorLine(s string) string {
	if line, _, ok := strings.Cut(s, "\n"); ok {
		return line
	}
	return s
}
