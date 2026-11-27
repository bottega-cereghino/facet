package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var reportNow = time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)

func writeReportLog(t *testing.T, lines ...string) queueConfig {
	t.Helper()
	p := filepath.Join(t.TempDir(), "gate.jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return queueConfig{Decisions: p}
}

func row(at, lane, decision, why string) string {
	b, _ := json.Marshal(queueDecision{At: at, Repo: "o/r", Issue: "1", Lane: lane, Decision: decision, Why: why, Source: queueDecisionSource})
	return string(b)
}

func reportJSON(t *testing.T, cfg queueConfig) queueReport {
	t.Helper()
	var out bytes.Buffer
	if err := runQueueReport(cfg, reportNow, 24*time.Hour, true, &out); err != nil {
		t.Fatal(err)
	}
	var rep queueReport
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out.String())
	}
	return rep
}

func TestQueueReportCountsAFixtureLog(t *testing.T) {
	in := "2026-11-24T10:00:00Z"
	cfg := writeReportLog(t,
		row("2026-11-20T10:00:00Z", "codex", "start", "too old"),
		row(in, "codex", "start", "a"),
		row(in, "codex", "start", "a"),
		row(in, "codex", decisionExitOK, "exit 0"),
		row(in, "codex", decisionExitFail, "exit status 1"),
		row(in, "codex", "skip", "cap"),
		row(in, "codex", "skip", "cap"),
		row(in, "codex", "skip", "gate"),
		row(in, "fix", "fix_start", "PR #2"),
		row(in, "fix", decisionBlocked, "PR #2 still red"),
		row(in, "land", decisionLanded, "PR #3 (squash)"),
		row(in, "", "held", "queue.hold"),
		row(in, "", "released", "queue.hold"),
		row(in, "", "held", "queue.hold"),
		row("2026-11-24T11:00:00Z", "", decisionError, "spawn failed"),
	)
	rep := reportJSON(t, cfg)
	c := rep.Agents["codex"]
	if c.Started != 2 || c.ExitedOK != 1 || c.ExitedFail != 1 || c.Skips["cap"] != 2 || c.Skips["gate"] != 1 {
		t.Errorf("codex = %+v", c)
	}
	if f := rep.Agents["fix"]; f.FixRuns != 1 || f.Blocked != 1 {
		t.Errorf("fix = %+v", f)
	}
	if l := rep.Agents["land"]; l.Landed != 1 {
		t.Errorf("land = %+v", l)
	}
	if rep.Holds.Held != 2 || rep.Holds.Released != 1 || !rep.Holds.HeldNow {
		t.Errorf("holds = %+v", rep.Holds)
	}
	if rep.LastError == nil || rep.LastError.Why != "spawn failed" {
		t.Errorf("last error = %+v", rep.LastError)
	}
	var out bytes.Buffer
	if err := runQueueReport(cfg, reportNow, 24*time.Hour, false, &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"codex", "skip codex: 2 x cap", "held now: true", "last error: 2026-11-24T11:00:00Z spawn failed"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("table lacks %q:\n%s", want, out.String())
		}
	}
}

func TestQueueReportEmptyLogIsZeros(t *testing.T) {
	p := filepath.Join(t.TempDir(), "gate.jsonl")
	if err := os.WriteFile(p, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	rep := reportJSON(t, queueConfig{Decisions: p})
	if len(rep.Agents) != 0 || rep.Holds != (holdReport{}) || rep.LastError != nil || rep.TornLines != 0 {
		t.Errorf("empty log gave %+v", rep)
	}
}

func TestQueueReportMissingLogIsRefused(t *testing.T) {
	var out bytes.Buffer
	err := runQueueReport(queueConfig{Decisions: filepath.Join(t.TempDir(), "none.jsonl")}, reportNow, time.Hour, true, &out)
	if err == nil || !strings.Contains(err.Error(), "fix:") {
		t.Fatalf("want a refusal with a fix, got %v", err)
	}
}

func TestQueueReportTornLineIsReportedAndSkipped(t *testing.T) {
	in := "2026-11-24T10:00:00Z"
	cfg := writeReportLog(t,
		row(in, "codex", "start", "a"),
		`{"at":"2026-11-24T10:00:00Z","repo":"o/r","lane":"co`,
		row(in, "codex", "start", "a"),
	)
	rep := reportJSON(t, cfg)
	if rep.TornLines != 1 || rep.Agents["codex"].Started != 2 {
		t.Errorf("torn = %d, codex = %+v", rep.TornLines, rep.Agents["codex"])
	}
	var out bytes.Buffer
	if err := runQueueReport(cfg, reportNow, 24*time.Hour, false, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "torn lines skipped: 1") {
		t.Errorf("table does not report the torn line:\n%s", out.String())
	}
}

func TestQueueReportBlockedNowFromState(t *testing.T) {
	cfg := writeReportLog(t, row("2026-11-24T10:00:00Z", "codex", "start", "a"))
	cfg.State = filepath.Join(t.TempDir(), "state.json")
	st := newQueueState()
	st.Fixes["o/r#1"] = &fixRecord{PR: 2, Runs: 2, Blocked: true}
	st.Fixes["o/r#9"] = &fixRecord{PR: 3, Runs: 1}
	if err := st.save(cfg.State); err != nil {
		t.Fatal(err)
	}
	rep := reportJSON(t, cfg)
	if len(rep.BlockedNow) != 1 || rep.BlockedNow[0] != "o/r#1" {
		t.Errorf("blocked now = %v", rep.BlockedNow)
	}
}

func censusLine(at string, total, startable, running, free int, by map[string]int) string {
	ts, _ := time.Parse(time.RFC3339, at)
	b, _ := json.Marshal(censusRow{At: ts, Total: total, Startable: startable, Running: running, FreeSlots: free, ByReason: by})
	return string(b)
}

func writeCensusLog(t *testing.T, cfg queueConfig, lines ...string) queueConfig {
	t.Helper()
	cfg.Census = filepath.Join(t.TempDir(), "census.json")
	if err := os.WriteFile(censusLogPath(cfg.Census), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return cfg
}

// The golden file is the report's JSON for a fixed log and clock. Compare by
// bytes; to change it on purpose, edit the file and say why in the PR.
func TestQueueReportCarriesTheCensusGolden(t *testing.T) {
	cfg := writeReportLog(t, row("2026-11-24T10:00:00Z", "codex", "start", "a"))
	cfg = writeCensusLog(t, cfg,
		"not json",
		// Before the window: starved, but only the part after 12:00 the day
		// before counts, and this row ends at 11:00 that day.
		censusLine("2026-11-23T10:00:00Z", 5, 0, 0, 2, map[string]int{"pr_pending": 5}),
		// 11:30 to 12:00 the day before is inside the window: 30 starved minutes.
		censusLine("2026-11-23T11:30:00Z", 5, 0, 0, 2, map[string]int{"pr_pending": 5}),
		// Startable work: not starved.
		censusLine("2026-11-24T08:00:00Z", 4, 2, 1, 1, map[string]int{"startable": 2, "running": 1, "pr_pending": 1}),
		// No free slot: not starved.
		censusLine("2026-11-24T09:00:00Z", 3, 0, 2, 0, map[string]int{"running": 2, "blocked_label": 1}),
		// Free slot, nothing startable: starved 60 minutes, then the queue
		// was down, which is not counted.
		censusLine("2026-11-24T10:00:00Z", 3, 0, 0, 2, map[string]int{"pr_pending": 2, "blocked_label": 1}),
		// Nothing labelled: not starved. Stands from 11:30 to 12:00 now.
		censusLine("2026-11-24T11:30:00Z", 0, 0, 0, 2, map[string]int{}),
	)
	var out bytes.Buffer
	if err := runQueueReport(cfg, reportNow, 24*time.Hour, true, &out); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Census json.RawMessage `json:"census"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, got.Census, "", "  "); err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(filepath.Join("..", "..", "testdata", "queue_report_census.golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(pretty.String()) != strings.TrimSpace(string(want)) {
		t.Errorf("census differs from the golden file\ngot:\n%s\nwant:\n%s", pretty.String(), want)
	}
}

func TestQueueReportCensusIsNullWithoutCensusInTheConfig(t *testing.T) {
	rep := reportJSON(t, writeReportLog(t, row("2026-11-24T10:00:00Z", "codex", "start", "a")))
	if rep.Census != nil {
		t.Errorf("census = %+v, want null", rep.Census)
	}
}

func TestQueueReportRefusesAMissingCensusLog(t *testing.T) {
	cfg := writeReportLog(t, row("2026-11-24T10:00:00Z", "codex", "start", "a"))
	cfg.Census = filepath.Join(t.TempDir(), "census.json")
	var out bytes.Buffer
	err := runQueueReport(cfg, reportNow, 24*time.Hour, true, &out)
	if err == nil || !strings.Contains(err.Error(), "fix:") {
		t.Fatalf("want a refusal with a fix, got %v", err)
	}
}

func TestQueueReportCensusWithNoRowsIsToldApartFromQuiet(t *testing.T) {
	cfg := writeCensusLog(t, writeReportLog(t, row("2026-11-24T10:00:00Z", "codex", "start", "a")),
		censusLine("2026-11-01T00:00:00Z", 3, 0, 0, 2, nil))
	rep := reportJSON(t, cfg)
	if rep.Census == nil || rep.Census.Rows != 0 || rep.Census.At != "" || rep.Census.StarvedMinutes24h != 0 {
		t.Errorf("census = %+v", rep.Census)
	}
}
