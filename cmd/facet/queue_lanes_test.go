package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bottega-cereghino/facet/internal/ghx"
)

func TestLanePreference(t *testing.T) {
	cases := []struct {
		label string
		want  string
	}{
		{"lane:grok", "grok"},
		{"Lane:Gemini", "gemini"},
		{"lane:codex", "codex"},
		{"model:opus", ""},
		{"codex", ""},
		{"lane:", ""},
		{"lane:grok:extra", ""},
		{"", ""},
	}
	for _, tc := range cases {
		if got := lanePreference(tc.label); got != tc.want {
			t.Errorf("lanePreference(%q) = %q, want %q", tc.label, got, tc.want)
		}
	}
}

func TestResolveAgentFallsBackOnlyWhenALaneHasNoRoom(t *testing.T) {
	cfg := queueConfig{Agents: []queueAgent{
		{Label: "lane:grok", Command: []string{"grok", "{prompt}"}},
		{Label: "model:opus", Command: []string{"opus", "{prompt}"}},
		{Label: "codex", Command: []string{"codex", "{prompt}"}},
		{Label: "", Command: []string{"default", "{prompt}"}, Bundle: 3},
	}}
	rooms := map[string]string{"grok": "no", "codex": "no", "claude": "no", "gemini": "yes"}

	a, from, err := cfg.resolveAgent([]string{"next", "lane:grok"}, rooms)
	if err != nil || from != "grok" || a.Command[0] != "default" || a.Bundle != 3 {
		t.Fatalf("grok with no room: agent %v from %q err %v", a, from, err)
	}
	a, from, err = cfg.resolveAgent([]string{"next", "Lane:Grok"}, rooms)
	if err != nil || from != "grok" || a.Command[0] != "default" {
		t.Fatalf("case: agent %v from %q err %v", a, from, err)
	}
	a, from, err = cfg.resolveAgent([]string{"next", "model:opus"}, rooms)
	if err != nil || from != "" || a.Command[0] != "opus" {
		t.Fatalf("model label: agent %v from %q err %v", a, from, err)
	}
	a, from, err = cfg.resolveAgent([]string{"next", "codex"}, rooms)
	if err != nil || from != "" || a.Command[0] != "codex" {
		t.Fatalf("plain label: agent %v from %q err %v", a, from, err)
	}
	a, from, err = cfg.resolveAgent([]string{"next", "lane:grok"}, map[string]string{"grok": "unknown"})
	if err != nil || from != "" || a.Command[0] != "grok" {
		t.Fatalf("unknown room: agent %v from %q err %v", a, from, err)
	}
	a, from, err = cfg.resolveAgent([]string{"next", "lane:grok"}, nil)
	if err != nil || from != "" || a.Command[0] != "grok" {
		t.Fatalf("no lanes answer: agent %v from %q err %v", a, from, err)
	}
}

func TestResolveAgentWithoutALaneEntryKeepsTheDefault(t *testing.T) {
	cfg := queueConfig{Agents: []queueAgent{
		{Label: "lane:grok", Command: []string{"grok", "{prompt}"}},
		{Command: []string{"default", "{prompt}"}},
	}}
	a, from, err := cfg.resolveAgent([]string{"next", "lane:gemini"}, map[string]string{"gemini": "no"})
	if err != nil || from != "" || a.Command[0] != "default" {
		t.Fatalf("unmatched lane label: agent %v from %q err %v", a, from, err)
	}
}

func TestBundleJoinsALaneThatFellBackWithTheDefault(t *testing.T) {
	cfg := queueConfig{
		Effort: "medium",
		Agents: []queueAgent{
			{Label: "lane:grok", Command: []string{"grok", "{prompt}"}, Bundle: 1},
			{Command: []string{"default", "{prompt}"}, Bundle: 3},
		},
	}
	open := func(queuedIssue) bool { return false }
	ranked := []queuedIssue{
		qIssue("acme/widgets", 1, "2026-01-01T00:00:00Z", "One", "next", "lane:grok"),
		qIssue("acme/widgets", 2, "2026-01-02T00:00:00Z", "Two", "next"),
	}
	rooms := map[string]string{"grok": "no"}
	run := bundleFor(cfg, ranked, open, rooms)
	wantRun(t, run, "acme/widgets#1", "acme/widgets#2")

	run = bundleFor(cfg, ranked, open, map[string]string{"grok": "yes"})
	wantRun(t, run, "acme/widgets#1")
}

func TestParseLaneRooms(t *testing.T) {
	rooms, err := parseLaneRooms([]byte(`{"lanes":[{"key":"gemini","lane":"gemini","room":"No"},{"lane":"API","room":"yes"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if rooms["gemini"] != "no" || rooms["api"] != "yes" {
		t.Fatalf("rooms = %#v", rooms)
	}
	if _, err := parseLaneRooms([]byte(`not json`)); err == nil || !strings.Contains(err.Error(), "fix:") {
		t.Fatalf("bad JSON: %v", err)
	}
}

func TestQueueConfigReadsLanes(t *testing.T) {
	cfg, err := loadTempConfig(t, `{
		"repos": ["acme/widgets"],
		"agents": [{"label": "lane:grok", "command": ["g", "{prompt}"]}, {"command": ["d", "{prompt}"]}],
		"lanes": {"command": ["lanes", "--json"], "log": "runs.jsonl"}
	}`)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Lanes == nil || len(cfg.Lanes.Command) != 2 || !strings.HasSuffix(cfg.Lanes.Log, "runs.jsonl") {
		t.Fatalf("lanes = %+v", cfg.Lanes)
	}
	if _, err := loadTempConfig(t, `{
		"repos": ["acme/widgets"],
		"agents": [{"command": ["d", "{prompt}"]}],
		"lanes": {}
	}`); err == nil || !strings.Contains(err.Error(), "lanes.command") {
		t.Fatalf("empty lanes: %v", err)
	}
}

func TestQueueStartsAFullLaneOnTheDefaultAgentWithinTheTick(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "out")
	logPath := filepath.Join(dir, "runs.jsonl")
	script := recordScript(t)
	cfg := oneAgentConfig([]string{"acme/widgets"})
	cfg.Lanes = &queueLanes{Command: []string{"lanes"}, Log: logPath}
	cfg.Agents = []queueAgent{
		{Label: "lane:grok", Command: []string{script, out, "grok", "{number}", "{workspace}", "{prompt}"}},
		{Label: "lane:gemini", Command: []string{script, out, "gemini", "{number}", "{workspace}", "{prompt}"}},
		{Label: "model:opus", Command: []string{script, out, "opus", "{number}", "{workspace}", "{prompt}"}},
		{Command: []string{script, out, "default", "{number}", "{workspace}", "{fallback_from}", "{prompt}"}},
	}
	var asked atomic.Int32
	fixed := time.Date(2026, 11, 23, 18, 0, 0, 0, time.UTC)
	s := startQueue(t, cfg, queueEnv{
		now: func() time.Time { return fixed },
		lanes: func(context.Context, []string) ([]byte, error) {
			asked.Add(1)
			return []byte(`{"lanes":[{"lane":"grok","room":"no"},{"lane":"gemini","room":"yes"},{"lane":"claude","room":"no"}]}`), nil
		},
		list: func(string, string) ([]ghx.Issue, error) {
			return []ghx.Issue{
				labelled(1, "2026-01-01T00:00:00Z", "Grok", "next", "lane:grok"),
				labelled(2, "2026-01-02T00:00:00Z", "Opus", "next", "model:opus"),
				labelled(3, "2026-01-03T00:00:00Z", "Gemini", "next", "lane:gemini"),
			}, nil
		},
	})
	waitFor(t, "three agents", func() bool { return strings.Count(readFile(t, out), "\n---\n") >= 3 })
	s.stop(t)

	recs := parseRecords(t, readFile(t, out))
	if len(recs) != 3 {
		t.Fatalf("records = %+v", recs)
	}
	want := []string{"default", "opus", "gemini"}
	for i, rec := range recs {
		if rec.id != want[i] || rec.n != i+1 {
			t.Errorf("record %d = id %s n %d, want %s %d", i, rec.id, rec.n, want[i], i+1)
		}
	}
	if recs[0].prompt != "grok" {
		t.Errorf("fallback_from arg = %q, want grok", recs[0].prompt)
	}
	if asked.Load() < 1 {
		t.Fatal("lanes command was not run")
	}
	if !strings.Contains(s.buf.String(), "fallback_from grok") {
		t.Fatalf("log = %q", s.buf.String())
	}
	var row fallbackRow
	if err := json.Unmarshal([]byte(strings.TrimSpace(readFile(t, logPath))), &row); err != nil {
		t.Fatal(err)
	}
	if row.FallbackFrom != "grok" || row.Issue != 1 || row.Repo != "acme/widgets" || row.Agent != "default" {
		t.Fatalf("row = %+v", row)
	}
	if row.At != fixed.Format(time.RFC3339) {
		t.Fatalf("at = %s", row.At)
	}
}

func TestQueueKeepsALaneLabelWhenTheLanesCommandFails(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "out")
	script := appendScript(t)
	cfg := oneAgentConfig([]string{"acme/widgets"})
	cfg.Lanes = &queueLanes{Command: []string{"lanes"}, Log: filepath.Join(dir, "runs.jsonl")}
	cfg.Agents = []queueAgent{
		{Label: "lane:grok", Command: []string{script, out, "grok"}},
		{Command: []string{script, out, "default"}},
	}
	s := startQueue(t, cfg, queueEnv{
		lanes: func(context.Context, []string) ([]byte, error) {
			return nil, errLanes
		},
		list: func(string, string) ([]ghx.Issue, error) {
			return []ghx.Issue{labelled(7, "2026-01-01T00:00:00Z", "Grok", "next", "lane:grok")}, nil
		},
	})
	waitFor(t, "the agent", func() bool { return len(readLines(t, out)) >= 1 })
	s.stop(t)
	if got := readLines(t, out); len(got) != 1 || got[0] != "grok" {
		t.Fatalf("started %v, want the labelled agent", got)
	}
	if !strings.Contains(s.buf.String(), "keeps its agent") {
		t.Fatalf("log = %q", s.buf.String())
	}
	if readFile(t, cfg.Lanes.Log) != "" {
		t.Fatalf("ledger written on a failed lanes read: %q", readFile(t, cfg.Lanes.Log))
	}
}

// errLanes is the failure a lanes command returns in the keep-the-label test.
var errLanes = lanesFail("lanes down")

type lanesFail string

func (e lanesFail) Error() string { return string(e) }

func TestQueueDryRunNamesTheFallbackAndWritesNoLedger(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "runs.jsonl")
	cfg := oneAgentConfig([]string{"acme/widgets"})
	cfg.Lanes = &queueLanes{Command: []string{"lanes"}, Log: logPath}
	cfg.Agents = []queueAgent{
		{Label: "lane:codex", Command: []string{"codex", "{prompt}"}},
		{Command: []string{"default", "{prompt}"}},
	}
	var buf strings.Builder
	err := dryQueue(context.Background(), cfg, queueEnv{
		out: &buf,
		lanes: func(context.Context, []string) ([]byte, error) {
			return []byte(`{"lanes":[{"lane":"codex","room":"no"}]}`), nil
		},
		list: func(string, string) ([]ghx.Issue, error) {
			return []ghx.Issue{labelled(4, "2026-01-01T00:00:00Z", "Codex", "next", "lane:codex")}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	text := buf.String()
	if !strings.Contains(text, "fallback_from codex") || !strings.Contains(text, "agent default") {
		t.Fatalf("dry-run = %q", text)
	}
	if readFile(t, logPath) != "" {
		t.Fatalf("dry-run wrote %q", readFile(t, logPath))
	}
}
