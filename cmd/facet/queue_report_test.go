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
