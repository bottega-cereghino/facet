package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bottega-cereghino/facet/internal/ghx"
)

// noIssues is a list seam with nothing queued.
func noIssues(string, string) ([]ghx.Issue, error) { return nil, nil }

func TestQueueUpdatesAGreenPRBehindItsBaseWithoutAnAgent(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "log")
	cfg := fixQueue(t, log)
	cfg.Gate = []string{appendScript(t), log, "gate"}
	seedState(t, cfg.State, []int{1}, nil)
	land := &landCalls{}
	var updated atomic.Bool
	land.update = func(int) error { updated.Store(true); return nil }
	// After the update GitHub runs the checks again.
	prs := func(string) ([]ghx.OpenPR, error) {
		if updated.Load() {
			pr := landPR(11, "feature/1-bell", "BEHIND")
			pr.Checks[0].Status = "IN_PROGRESS"
			return []ghx.OpenPR{pr}, nil
		}
		return []ghx.OpenPR{landPR(11, "feature/1-bell", "BEHIND")}, nil
	}
	s := startQueue(t, cfg, land.env(queueEnv{list: noIssues, prs: prs, addLabel: (&labelCalls{}).add}))
	waitFor(t, "the update", func() bool { return len(land.list()) >= 1 })
	time.Sleep(150 * time.Millisecond)
	s.stop(t)
	if got := land.list(); strings.Join(got, "|") != "update acme/widgets#11" {
		t.Fatalf("landed = %q, want one update", got)
	}
	if got := readLines(t, log); len(got) != 0 {
		t.Fatalf("an agent or the gate ran: %q", got)
	}
}

func TestQueueFixesAPRWhoseUpdateConflicts(t *testing.T) {
	log := filepath.Join(t.TempDir(), "log")
	cfg := fixQueue(t, log)
	seedState(t, cfg.State, []int{1}, nil)
	land := &landCalls{update: func(int) error { return fmt.Errorf("%w: gh: merge conflict", ghx.ErrUpdateConflict) }}
	s := startQueue(t, cfg, land.env(queueEnv{
		list:     noIssues,
		prs:      staticPRs(landPR(11, "feature/1-bell", "BEHIND")),
		addLabel: (&labelCalls{}).add,
	}))
	waitFor(t, "the fix", func() bool { return len(readLines(t, log)) >= 1 })
	s.stop(t)
	if got := readLines(t, log); got[0] != "fix 1 PR 11 try 1: conflicts with main" {
		t.Fatalf("runs = %q", got)
	}
}

func TestQueueMergesAGreenUpToDatePR(t *testing.T) {
	cfg := fixQueue(t, filepath.Join(t.TempDir(), "log"))
	seedState(t, cfg.State, []int{1}, map[int]int{1: 1})
	land := &landCalls{}
	var merged atomic.Bool
	land.merge = func(int) error { merged.Store(true); return nil }
	prs := func(string) ([]ghx.OpenPR, error) {
		if merged.Load() {
			return nil, nil
		}
		return []ghx.OpenPR{landPR(11, "feature/1-bell", "CLEAN")}, nil
	}
	s := startQueue(t, cfg, land.env(queueEnv{list: noIssues, prs: prs}))
	waitFor(t, "the merge", func() bool { return merged.Load() })
	waitFor(t, "the forget", func() bool {
		st, err := loadQueueState(cfg.State)
		return err == nil && len(st.Started) == 0
	})
	s.stop(t)
	if got := land.list(); strings.Join(got, "|") != "merge acme/widgets#11 squash head11" {
		t.Fatalf("landed = %q", got)
	}
	st, err := loadQueueState(cfg.State)
	if err != nil || len(st.Fixes) != 0 {
		t.Fatalf("state = %+v, %v", st, err)
	}
}

func TestQueueTriesARefusedMergeOncePerListing(t *testing.T) {
	cfg := fixQueue(t, filepath.Join(t.TempDir(), "log"))
	seedState(t, cfg.State, []int{1}, nil)
	land := &landCalls{merge: func(int) error { return errors.New("the base branch policy prohibits the merge") }}
	ticks := &countCalls{}
	s := startQueue(t, cfg, land.env(queueEnv{
		list: func(repo, _ string) ([]ghx.Issue, error) { ticks.add(repo); return nil, nil },
		prs:  staticPRs(landPR(11, "feature/1-bell", "BLOCKED")),
	}))
	waitFor(t, "ten rounds", func() bool { return ticks.get("acme/widgets") >= 10 })
	s.stop(t)
	if got := land.list(); len(got) != 1 {
		t.Fatalf("landed = %q, want one try on one listing", got)
	}
	if !strings.Contains(s.buf.String(), "prohibits the merge; trying again on the next listing") {
		t.Fatalf("log =\n%s", s.buf.String())
	}
}

func TestQueueRetriesARefusedMergeOnTheNextListing(t *testing.T) {
	cfg := fixQueue(t, filepath.Join(t.TempDir(), "log"))
	seedState(t, cfg.State, []int{1}, nil)
	var tries atomic.Int32
	land := &landCalls{merge: func(int) error {
		if tries.Add(1) == 1 {
			return errors.New("required status check is expected")
		}
		return nil
	}}
	// #7 is queued and has its own open pull request, so new work lists
	// the repository afresh every round.
	s := startQueue(t, cfg, land.env(queueEnv{
		list: func(string, string) ([]ghx.Issue, error) {
			return []ghx.Issue{openIssue(7, "2026-01-01T00:00:00Z")}, nil
		},
		prs: staticPRs(landPR(11, "feature/1-bell", "CLEAN"), redPR(17, "feature/7-other")),
	}))
	waitFor(t, "the second merge", func() bool { return tries.Load() >= 2 })
	s.stop(t)
	if got := land.list(); len(got) < 2 || got[0] != "merge acme/widgets#11 squash head11" || got[1] != got[0] {
		t.Fatalf("landed = %q", got)
	}
}

func TestQueueLandsNothingWithLandOff(t *testing.T) {
	cfg := fixQueue(t, filepath.Join(t.TempDir(), "log"))
	cfg.Fix.Land = false
	seedState(t, cfg.State, []int{1, 2}, nil)
	land := &landCalls{}
	ticks := &countCalls{}
	s := startQueue(t, cfg, land.env(queueEnv{
		list: func(repo, _ string) ([]ghx.Issue, error) { ticks.add(repo); return nil, nil },
		prs:  staticPRs(landPR(11, "feature/1-bell", "BEHIND"), landPR(12, "feature/2-horn", "CLEAN")),
	}))
	waitFor(t, "five rounds", func() bool { return ticks.get("acme/widgets") >= 5 })
	s.stop(t)
	if got := land.list(); len(got) != 0 {
		t.Fatalf("landed = %q", got)
	}
}

func TestQueueDryRunSaysWhatItWouldLand(t *testing.T) {
	cfg := fixQueue(t, "unused")
	seedState(t, cfg.State, []int{1, 2}, nil)
	before := readFile(t, cfg.State)
	land := &landCalls{}
	var buf bytes.Buffer
	err := dryQueue(context.Background(), cfg, land.env(queueEnv{
		list: noIssues,
		prs:  staticPRs(landPR(11, "feature/1-bell", "BEHIND"), landPR(12, "feature/2-horn", "CLEAN")),
		out:  &buf,
	}))
	got := buf.String()
	if err != nil || !strings.Contains(got, "dry-run: would update acme/widgets#1 PR #11: merge main into it") ||
		!strings.Contains(got, "dry-run: would merge acme/widgets#2 PR #12") {
		t.Fatalf("err %v, dry-run =\n%s", err, got)
	}
	if len(land.list()) != 0 || readFile(t, cfg.State) != before {
		t.Fatalf("dry run landed %q or wrote the state", land.list())
	}
}

func TestQueueDoesNotCountAFixStoppedWithTheQueue(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "log")
	cfg := fixQueue(t, log)
	cfg.Fix.Command = []string{blockScript(t), log, "fix {number} {prompt}"}
	seedState(t, cfg.State, []int{1}, nil)
	s := startQueue(t, cfg, queueEnv{list: noIssues, prs: staticPRs(redPR(11, "feature/1-bell"))})
	waitFor(t, "the fix", func() bool { return len(readLines(t, log)) >= 1 })
	s.stop(t)
	st, err := loadQueueState(cfg.State)
	if err != nil {
		t.Fatal(err)
	}
	if rec := st.Fixes["acme/widgets#1"]; rec != nil && rec.Runs != 0 {
		t.Fatalf("record = %+v, want no try counted", rec)
	}
}

func TestQueueListsARepositoryOnlyWhenItsIssuesCanAct(t *testing.T) {
	cfg := fixQueue(t, "unused")
	cfg.Repos = []string{"acme/widgets", "acme/other"}
	seedState(t, cfg.State, []int{1}, nil)
	// acme/other's only started issue is blocked.
	st, err := loadQueueState(cfg.State)
	if err != nil {
		t.Fatal(err)
	}
	st.Started["acme/other#2"] = startedIssue{Repo: "acme/other", Number: 2, At: time.Now().Add(-48 * time.Hour)}
	st.Fixes["acme/other#2"] = &fixRecord{PR: 12, Runs: 2, Blocked: true}
	if err := st.save(cfg.State); err != nil {
		t.Fatal(err)
	}
	pending := redPR(11, "feature/1-bell")
	pending.Checks[0].Status = "IN_PROGRESS"
	listed, ticks := &countCalls{}, &countCalls{}
	s := startQueue(t, cfg, queueEnv{
		list: func(repo, _ string) ([]ghx.Issue, error) { ticks.add(repo); return nil, nil },
		prs: func(repo string) ([]ghx.OpenPR, error) {
			listed.add(repo)
			if repo == "acme/widgets" {
				return []ghx.OpenPR{pending}, nil
			}
			return []ghx.OpenPR{redPR(12, "feature/2-horn")}, nil
		},
	})
	waitFor(t, "ten rounds", func() bool { return ticks.get("acme/widgets") >= 10 })
	s.stop(t)
	if w, o := listed.get("acme/widgets"), listed.get("acme/other"); w != 1 || o != 0 {
		t.Fatalf("listings: acme/widgets %d, acme/other %d; want 1 and 0 over ten rounds", w, o)
	}
}

func TestQueueActsOnlyInTheConfiguredRepositories(t *testing.T) {
	cfg := fixQueue(t, "unused")
	seedState(t, cfg.State, nil, nil)
	st, err := loadQueueState(cfg.State)
	if err != nil {
		t.Fatal(err)
	}
	st.Started["acme/gone#1"] = startedIssue{Repo: "acme/gone", Number: 1, At: time.Now().Add(-48 * time.Hour)}
	if err := st.save(cfg.State); err != nil {
		t.Fatal(err)
	}
	listed := &countCalls{}
	var buf bytes.Buffer
	err = dryQueue(context.Background(), cfg, queueEnv{
		list: noIssues,
		prs: func(repo string) ([]ghx.OpenPR, error) {
			listed.add(repo)
			return []ghx.OpenPR{redPR(11, "feature/1-bell")}, nil
		},
		out: &buf,
	})
	if err != nil || strings.Contains(buf.String(), "would fix") || listed.get("acme/gone") != 0 {
		t.Fatalf("err %v, listed %d, dry-run =\n%s", err, listed.get("acme/gone"), buf.String())
	}
}

func TestQueueTriesAMissingBlockLabelOncePerWait(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "log")
	cfg := fixQueue(t, log)
	cfg.Wait = time.Minute
	seedState(t, cfg.State, []int{1}, map[int]int{1: 2})
	// The record's last run is an hour old, past a minute's settling.
	var tries atomic.Int32
	var issues []ghx.Issue
	for n := 5; n < 40; n++ {
		issues = append(issues, openIssue(n, fmt.Sprintf("2026-01-01T00:00:%02dZ", n)))
	}
	s := startQueue(t, cfg, queueEnv{
		list: func(string, string) ([]ghx.Issue, error) { return issues, nil },
		prs:  staticPRs(redPR(11, "feature/1-bell")),
		addLabel: func(string, int, string) error {
			tries.Add(1)
			return errors.New("could not add label: 'blocked' not found")
		},
	})
	// Each agent exits at once and wakes the queue for another round.
	waitFor(t, "five rounds", func() bool { return len(readLines(t, log)) >= 5 })
	s.stop(t)
	if n := tries.Load(); n != 1 {
		t.Fatalf("label tried %d times within one wait, want 1", n)
	}
	if !strings.Contains(s.buf.String(), "fix: create the label blocked in acme/widgets") {
		t.Fatalf("log =\n%s", s.buf.String())
	}
}

func TestQueueKeepsABlockedIssueBlocked(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "log")
	cfg := fixQueue(t, log)
	seedState(t, cfg.State, []int{1}, nil)
	st, err := loadQueueState(cfg.State)
	if err != nil {
		t.Fatal(err)
	}
	st.Fixes["acme/widgets#1"] = &fixRecord{PR: 11, Runs: 2, Last: time.Now().Add(-time.Hour), Blocked: true}
	if err := st.save(cfg.State); err != nil {
		t.Fatal(err)
	}
	// Green once, then red again.
	var calls atomic.Int32
	prs := func(string) ([]ghx.OpenPR, error) {
		if calls.Add(1) == 1 {
			return []ghx.OpenPR{landPR(11, "feature/1-bell", "CLEAN")}, nil
		}
		return []ghx.OpenPR{redPR(11, "feature/1-bell")}, nil
	}
	land := &landCalls{}
	s := startQueue(t, cfg, land.env(queueEnv{
		list: func(string, string) ([]ghx.Issue, error) {
			return []ghx.Issue{openIssue(5, "2026-01-01T00:00:00Z")}, nil
		},
		prs:      prs,
		addLabel: (&labelCalls{}).add,
	}))
	waitFor(t, "the start", func() bool { return len(readLines(t, log)) >= 1 })
	time.Sleep(200 * time.Millisecond)
	s.stop(t)
	if got := readLines(t, log); strings.Join(got, "|") != "start 5" || len(land.list()) != 0 {
		t.Fatalf("runs %q, landed %q", got, land.list())
	}
	st, err = loadQueueState(cfg.State)
	if err != nil {
		t.Fatal(err)
	}
	if rec := st.Fixes["acme/widgets#1"]; rec == nil || !rec.Blocked {
		t.Fatalf("record = %+v, want it still blocked", rec)
	}
}

func TestQueueDryRunSaysItWouldClearARecord(t *testing.T) {
	cfg := fixQueue(t, "unused")
	seedState(t, cfg.State, []int{1}, map[int]int{1: 1})
	before := readFile(t, cfg.State)
	var buf bytes.Buffer
	err := dryQueue(context.Background(), cfg, queueEnv{list: noIssues, prs: staticPRs(greenPR(11, "feature/1-bell")), out: &buf})
	got := buf.String()
	if err != nil || !strings.Contains(got, "dry-run: would clear the fix record of acme/widgets#1: PR #11 is green") ||
		strings.Contains(got, "clearing") {
		t.Fatalf("err %v, dry-run =\n%s", err, got)
	}
	if readFile(t, cfg.State) != before {
		t.Fatal("the dry run wrote the state")
	}
}

func TestQueueStateIsSyncedBeforeTheRenameAndAfter(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	var synced []string
	orig := syncFile
	syncFile = func(f *os.File) error {
		_, err := os.Stat(path)
		synced = append(synced, fmt.Sprintf("dir=%v exists=%v", filepath.Base(f.Name()) == filepath.Base(dir), err == nil))
		return orig(f)
	}
	t.Cleanup(func() { syncFile = orig })
	if err := newQueueState().save(path); err != nil {
		t.Fatal(err)
	}
	// The temp file is synced before the state exists, the directory after.
	if got := strings.Join(synced, "|"); got != "dir=false exists=false|dir=true exists=true" {
		t.Fatalf("syncs = %q", got)
	}
}

func TestQueueFixPromptNamesTheBase(t *testing.T) {
	log := filepath.Join(t.TempDir(), "log")
	cfg := fixQueue(t, log)
	cfg.Fix.Command = []string{appendScript(t), log, "{base}: {prompt}"}
	seedState(t, cfg.State, []int{1}, nil)
	pr := redPR(11, "feature/1-bell")
	pr.BaseRefName = "release"
	s := startQueue(t, cfg, queueEnv{list: noIssues, prs: staticPRs(pr), addLabel: (&labelCalls{}).add})
	waitFor(t, "the fix", func() bool { return len(readLines(t, log)) >= 1 })
	s.stop(t)
	got := readLines(t, log)[0]
	if !strings.HasPrefix(got, "release: ") || !strings.Contains(got, "merge origin/release into it") || strings.Contains(got, "<base>") {
		t.Fatalf("fix run = %q", got)
	}
}
