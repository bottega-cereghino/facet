package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bottega-cereghino/facet/internal/ghx"
)

func readDecisionLines(t *testing.T, path string) ([]queueDecision, []map[string]any) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read decision log %s: %v", path, err)
	}
	var (
		rows []queueDecision
		raws []map[string]any
	)
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var row queueDecision
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatalf("unmarshal decision line %q: %v", line, err)
		}
		var raw map[string]any
		if err := json.Unmarshal([]byte(line), &raw); err != nil {
			t.Fatalf("unmarshal raw decision line %q: %v", line, err)
		}
		rows = append(rows, row)
		raws = append(raws, raw)
	}
	return rows, raws
}

func TestQueueDecisionsHeldAndReleasedOncePerChange(t *testing.T) {
	dir := t.TempDir()
	holdA := filepath.Join(dir, "hold.json")
	holdB := filepath.Join(dir, "queue.hold")
	logFile := filepath.Join(dir, "queue-gate.jsonl")

	if err := os.WriteFile(holdA, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	fixedNow := time.Date(2026, 11, 23, 15, 0, 0, 0, time.UTC)
	cfg := oneAgentConfig(nil)
	cfg.Hold = []string{holdA, holdB}
	cfg.Decisions = logFile

	env := queueEnv{
		out:  &bytes.Buffer{},
		now:  func() time.Time { return fixedNow },
		list: func(string, string) ([]ghx.Issue, error) { return nil, nil },
	}

	// First poll while holdA exists: emits "held".
	if err := runQueue(context.Background(), cfg, env); err != nil {
		t.Fatal(err)
	}
	// Second poll while holdA still exists (new process / runner): emits nothing.
	if err := runQueue(context.Background(), cfg, env); err != nil {
		t.Fatal(err)
	}
	rows, raws := readDecisionLines(t, logFile)
	if len(rows) != 1 {
		t.Fatalf("got %d lines after two held polls, want 1: %+v", len(rows), rows)
	}
	if rows[0].Decision != "held" || rows[0].Why != holdA || rows[0].Source != "queue" || rows[0].At != "2026-11-23T15:00:00Z" {
		t.Fatalf("held row = %+v", rows[0])
	}
	for _, key := range []string{"at", "repo", "issue", "lane", "decision", "why", "source"} {
		if _, ok := raws[0][key]; !ok {
			t.Errorf("missing key %q in %v", key, raws[0])
		}
	}

	// Switch from holdA to holdB: emits "held" with holdB.
	if err := os.Remove(holdA); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(holdB, []byte("stop"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runQueue(context.Background(), cfg, env); err != nil {
		t.Fatal(err)
	}
	rows, _ = readDecisionLines(t, logFile)
	if len(rows) != 2 || rows[1].Decision != "held" || rows[1].Why != holdB {
		t.Fatalf("after switch to holdB: %+v", rows)
	}

	// Remove holdB: emits "released" once, and a second tick emits nothing more.
	if err := os.Remove(holdB); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var polls atomic.Int32
	env.list = func(string, string) ([]ghx.Issue, error) {
		if polls.Add(1) >= 2 {
			cancel()
		}
		return nil, nil
	}
	if err := runQueue(ctx, cfg, env); err != nil {
		t.Fatal(err)
	}
	rows, _ = readDecisionLines(t, logFile)
	if len(rows) != 3 {
		t.Fatalf("got %d lines after release, want 3: %+v", len(rows), rows)
	}
	if rows[2].Decision != "released" || rows[2].Why != holdB || rows[2].Source != "queue" {
		t.Fatalf("released row = %+v", rows[2])
	}
}

func TestQueueDecisionsStartAndFixStart(t *testing.T) {
	dir := t.TempDir()
	logFile := filepath.Join(dir, "queue-gate.jsonl")
	statePath := filepath.Join(dir, "state.json")

	// 1. Normal start with a labelled agent.
	cfg := queueConfig{
		Max:       1,
		PerRepo:   defaultQueuePerRepo,
		Wait:      10 * time.Millisecond,
		Label:     "next",
		Repos:     []string{"acme/widgets"},
		Prompt:    "SECRET_PROMPT_TEXT {issues}",
		Decisions: logFile,
		Agents: []queueAgent{
			{Label: "lane:gemini", Bundle: 1, Command: []string{"run-gemini", "--effort", "{effort}", "{prompt}"}},
			{Label: "", Bundle: 1, Command: []string{"run-default", "{prompt}"}},
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	var ran atomic.Int32
	err := runQueue(ctx, cfg, queueEnv{
		out: &bytes.Buffer{},
		list: func(string, string) ([]ghx.Issue, error) {
			if ran.Load() > 0 {
				cancel()
				return nil, nil
			}
			return []ghx.Issue{{
				Number: 42,
				Title:  "add jsonl",
				State:  "OPEN",
				Labels: []ghx.Label{{Name: "next"}, {Name: "lane:gemini"}},
			}}, nil
		},
		spawn: func(string, int) (string, error) { return dir, nil },
		run: func(context.Context, string, []string) error {
			ran.Add(1)
			cancel()
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// 2. Red-first fix run.
	st := newQueueState()
	st.Started["acme/widgets#7"] = startedIssue{
		Repo:   "acme/widgets",
		Number: 7,
		At:     time.Now().Add(-time.Hour),
		PR:     77,
		Marker: testMarker(7),
	}
	if err := st.save(statePath); err != nil {
		t.Fatal(err)
	}
	fixCfg := queueConfig{
		Max:       1,
		PerRepo:   defaultQueuePerRepo,
		Wait:      10 * time.Millisecond,
		Label:     "next",
		Repos:     []string{"acme/widgets"},
		Prompt:    "Do {issues}",
		State:     statePath,
		Decisions: logFile,
		Agents:    []queueAgent{{Label: "", Bundle: 1, Command: []string{"agent", "{prompt}"}}},
		Fix: &queueFix{
			Command:    []string{"run-fix", "--pr", "{pr}", "{prompt}"},
			Prompt:     "FIX_PROMPT_TEXT",
			Tries:      2,
			BlockLabel: "blocked",
		},
	}
	ctx2, cancel2 := context.WithCancel(context.Background())
	err = runQueue(ctx2, fixCfg, ownerSeams(queueEnv{
		out:  &bytes.Buffer{},
		list: func(string, string) ([]ghx.Issue, error) { return nil, nil },
		prs: func(string) ([]ghx.OpenPR, error) {
			return []ghx.OpenPR{{
				Number:      77,
				HeadRefName: "feature/7-bug",
				BaseRefName: "main",
				Mergeable:   "MERGEABLE",
				Author:      ghx.User{Login: testViewer},
				Body:        markerLine(testMarker(7)),
				Checks: []ghx.CheckRun{{
					Typename:   "CheckRun",
					Name:       "ci",
					Status:     "COMPLETED",
					Conclusion: "FAILURE",
					StartedAt:  "2026-11-23T12:00:00Z",
				}},
			}}, nil
		},
		spawn: func(string, int) (string, error) { return dir, nil },
		run: func(context.Context, string, []string) error {
			cancel2()
			return nil
		},
	}))
	if err != nil {
		t.Fatal(err)
	}

	rows, _ := readDecisionLines(t, logFile)
	if len(rows) != 4 {
		t.Fatalf("got %d rows, want 4 (start, exit_ok, fix_start, exit_ok): %+v", len(rows), rows)
	}
	if rows[1].Decision != decisionExitOK || rows[1].Lane != "lane:gemini" || rows[3].Decision != decisionExitOK || rows[3].Lane != "fix" {
		t.Fatalf("exit rows = %+v, %+v", rows[1], rows[3])
	}
	startRow := rows[0]
	if startRow.Repo != "acme/widgets" || startRow.Issue != "42" || startRow.Lane != "lane:gemini" || startRow.Decision != "start" || startRow.Source != "queue" {
		t.Fatalf("start row = %+v", startRow)
	}
	if !strings.Contains(startRow.Why, "run-gemini --effort medium") || strings.Contains(startRow.Why, "SECRET_PROMPT_TEXT") {
		t.Fatalf("start why = %q", startRow.Why)
	}
	fixRow := rows[2]
	if fixRow.Repo != "acme/widgets" || fixRow.Issue != "7" || fixRow.Lane != "fix" || fixRow.Decision != "fix_start" || fixRow.Source != "queue" {
		t.Fatalf("fix_start row = %+v", fixRow)
	}
	if !strings.Contains(fixRow.Why, "PR #77") || !strings.Contains(fixRow.Why, "run-fix --pr 77") || strings.Contains(fixRow.Why, "FIX_PROMPT_TEXT") {
		t.Fatalf("fix_start why = %q", fixRow.Why)
	}
}

func TestQueueDecisionsSkipOpenPRAndDoneAndEffortConflict(t *testing.T) {
	dir := t.TempDir()
	logFile := filepath.Join(dir, "queue-gate.jsonl")

	cfg := queueConfig{
		Max:       1,
		PerRepo:   defaultQueuePerRepo,
		Wait:      10 * time.Millisecond,
		Label:     "next",
		Repos:     []string{"acme/widgets"},
		Prompt:    "Do {issues}",
		Effort:    "medium",
		Decisions: logFile,
		effortLabels: map[string]string{
			"fix":  "low",
			"feat": "high",
		},
		Agents: []queueAgent{{Label: "", Bundle: 1, Command: []string{"agent", "{prompt}"}}},
	}
	ctx, cancel := context.WithCancel(context.Background())
	var ticks atomic.Int32
	err := runQueue(ctx, cfg, queueEnv{
		out: &bytes.Buffer{},
		list: func(string, string) ([]ghx.Issue, error) {
			if ticks.Add(1) >= 3 {
				cancel()
			}
			return []ghx.Issue{
				{Number: 10, Title: "has pr", State: "OPEN", CreatedAt: "2026-01-01T00:00:00Z", Labels: []ghx.Label{{Name: "next"}}},
				{Number: 11, Title: "will run", State: "OPEN", CreatedAt: "2026-01-02T00:00:00Z", Labels: []ghx.Label{{Name: "next"}}},
				{Number: 12, Title: "effort clash", State: "OPEN", CreatedAt: "2026-01-03T00:00:00Z", Labels: []ghx.Label{{Name: "next"}, {Name: "fix"}, {Name: "feat"}}},
			}, nil
		},
		prs: func(string) ([]ghx.OpenPR, error) {
			return []ghx.OpenPR{{Number: 99, HeadRefName: "feature/10-has-pr"}}, nil
		},
		spawn: func(string, int) (string, error) { return dir, nil },
		run:   func(context.Context, string, []string) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}

	rows, _ := readDecisionLines(t, logFile)
	var sawOpenPR, sawStart11, sawDone11, sawEffort12 bool
	for _, row := range rows {
		if row.Source != "queue" {
			t.Errorf("unexpected source in %+v", row)
		}
		switch {
		case row.Issue == "10" && row.Decision == "skip" && strings.Contains(row.Why, "PR #99") || (row.Issue == "10" && row.Decision == "skip" && strings.Contains(row.Why, "#99") && strings.Contains(row.Why, "feature/10-has-pr")):
			sawOpenPR = true
		case row.Issue == "11" && row.Decision == "start":
			sawStart11 = true
		case row.Issue == "11" && row.Decision == "skip" && strings.Contains(row.Why, "already started"):
			sawDone11 = true
		case row.Issue == "12" && row.Decision == "skip" && strings.Contains(row.Why, "effort"):
			sawEffort12 = true
		}
	}
	if !sawOpenPR || !sawStart11 || !sawDone11 || !sawEffort12 {
		t.Fatalf("missing expected decisions (openPR=%v start11=%v done11=%v effort12=%v) in %+v",
			sawOpenPR, sawStart11, sawDone11, sawEffort12, rows)
	}
}

func TestQueueDecisionsFailedWriteNeverStopsQueue(t *testing.T) {
	dir := t.TempDir()
	// Make parent a regular file so MkdirAll/OpenFile fails.
	blocker := filepath.Join(dir, "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := oneAgentConfig(nil)
	cfg.Decisions = filepath.Join(blocker, "queue-gate.jsonl")

	var buf bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	var ran atomic.Int32
	err := runQueue(ctx, cfg, queueEnv{
		out: &buf,
		list: func(string, string) ([]ghx.Issue, error) {
			return []ghx.Issue{{Number: 5, Title: "still runs", State: "OPEN", Labels: []ghx.Label{{Name: "next"}}}}, nil
		},
		spawn: func(string, int) (string, error) { return dir, nil },
		run: func(context.Context, string, []string) error {
			ran.Add(1)
			cancel()
			return nil
		},
	})
	if err != nil {
		t.Fatalf("failed decision write stopped queue: %v", err)
	}
	if ran.Load() != 1 {
		t.Fatalf("agent ran %d time(s), want 1", ran.Load())
	}
	if !strings.Contains(buf.String(), "write decision log") {
		t.Fatalf("expected write warning in log, got %q", buf.String())
	}
}

func TestQueueDecisionsDryRunWritesNothing(t *testing.T) {
	dir := t.TempDir()
	logFile := filepath.Join(dir, "queue-gate.jsonl")
	cfg := oneAgentConfig(nil)
	cfg.Decisions = logFile
	err := dryQueue(context.Background(), cfg, queueEnv{
		out: &bytes.Buffer{},
		list: func(string, string) ([]ghx.Issue, error) {
			return []ghx.Issue{{Number: 1, Title: "dry", State: "OPEN", Labels: []ghx.Label{{Name: "next"}}}}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(logFile); !os.IsNotExist(err) {
		t.Fatalf("dry-run created decision log: %v", err)
	}
}

func TestQueueDecisionDefaultPathAndConfigRelative(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv(queueDecisionEnv, "")
	wantDefault := filepath.Join(dir, queueDecisionRelativePath)
	if got := defaultQueueDecisionPath(); got != wantDefault {
		t.Fatalf("default path = %q, want %q", got, wantDefault)
	}
	custom := filepath.Join(dir, "custom.jsonl")
	t.Setenv(queueDecisionEnv, custom)
	if got := defaultQueueDecisionPath(); got != custom {
		t.Fatalf("env path = %q, want %q", got, custom)
	}
	cfg, err := loadTempConfig(t, `{
		"decisions": "decisions.jsonl",
		"repos": ["acme/widgets"],
		"agents": [{"command": ["agent", "{prompt}"]}]
	}`)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(cfg.Decisions) != "decisions.jsonl" || !filepath.IsAbs(cfg.Decisions) {
		t.Fatalf("relative decisions = %q", cfg.Decisions)
	}
}

func TestQueueDecisionsSkipPerRepoCap(t *testing.T) {
	dir := t.TempDir()
	logFile := filepath.Join(dir, "queue-gate.jsonl")

	cfg := queueConfig{
		Max:       2,
		PerRepo:   1,
		Wait:      10 * time.Millisecond,
		Label:     "next",
		Repos:     []string{"acme/widgets"},
		Prompt:    "Do {issues}",
		Decisions: logFile,
		Agents:    []queueAgent{{Label: "", Bundle: 1, Command: []string{"agent", "{prompt}"}}},
	}
	release := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	var started atomic.Int32
	var ticks atomic.Int32
	err := runQueue(ctx, cfg, queueEnv{
		out: &bytes.Buffer{},
		list: func(string, string) ([]ghx.Issue, error) {
			if started.Load() > 0 && ticks.Add(1) >= 2 {
				close(release)
				cancel()
			}
			return []ghx.Issue{
				{Number: 1, Title: "first", State: "OPEN", CreatedAt: "2026-01-01T00:00:00Z", Labels: []ghx.Label{{Name: "next"}}},
				{Number: 2, Title: "second", State: "OPEN", CreatedAt: "2026-01-02T00:00:00Z", Labels: []ghx.Label{{Name: "next"}}},
			}, nil
		},
		spawn: func(string, int) (string, error) { return dir, nil },
		run: func(context.Context, string, []string) error {
			started.Add(1)
			<-release
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	rows, _ := readDecisionLines(t, logFile)
	var sawCapSkip bool
	for _, row := range rows {
		if row.Issue == "2" && row.Decision == "skip" && strings.Contains(row.Why, "per-repo cap") {
			sawCapSkip = true
		}
	}
	if !sawCapSkip {
		t.Fatalf("expected per-repo cap skip for issue 2 in %+v", rows)
	}
}

func TestQueueDecisionsSkipOncePerChangeOfReason(t *testing.T) {
	logFile := filepath.Join(t.TempDir(), "queue-gate.jsonl")
	clock := time.Date(2026, 11, 27, 9, 0, 0, 0, time.UTC)
	r := &queueRunner{
		cfg: queueConfig{Decisions: logFile},
		env: queueEnv{out: &bytes.Buffer{}, now: func() time.Time { return clock }},
	}
	skip := func(issue, why string) {
		clock = clock.Add(time.Minute)
		r.recordDecision("acme/widgets", issue, "default", "skip", why)
	}

	for range 10 {
		skip("10", "open pull request #99")
	}
	skip("11", "open pull request #99") // another issue, same reason: its own row
	skip("10", "effort conflict")       // changed reason writes again
	skip("10", "effort conflict")
	clock = clock.Add(time.Minute)
	r.recordDecision("acme/widgets", "10", "default", "start", "agent")
	skip("10", "effort conflict") // a skip after the issue ran writes again
	skip("10", "effort conflict")

	rows, _ := readDecisionLines(t, logFile)
	var got []string
	for _, row := range rows {
		got = append(got, row.Issue+" "+row.Decision+" "+row.Why)
	}
	want := []string{
		"10 skip open pull request #99",
		"11 skip open pull request #99",
		"10 skip effort conflict",
		"10 start agent",
		"10 skip effort conflict",
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("rows = %q, want %q", got, want)
	}
	if rows[0].At != "2026-11-27T09:01:00Z" {
		t.Fatalf("first row at %s, want the first tick's time", rows[0].At)
	}
}
