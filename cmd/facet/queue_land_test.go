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

// An update that fails for a reason other than a conflict keeps the
// listing and waits, with the merge backoff, before it is tried again.
func TestQueueBacksOffAFailedUpdate(t *testing.T) {
	cfg := fixQueue(t, filepath.Join(t.TempDir(), "log"))
	seedState(t, cfg.State, []int{1}, nil)
	land := &landCalls{update: func(int) error { return errors.New("HTTP 502") }}
	ticks := &countCalls{}
	listings := &countCalls{}
	start := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	var now atomic.Value
	now.Store(start)
	s := startQueue(t, cfg, land.env(queueEnv{
		list: func(repo, _ string) ([]ghx.Issue, error) { ticks.add(repo); return nil, nil },
		prs: func(repo string) ([]ghx.OpenPR, error) {
			listings.add(repo)
			return []ghx.OpenPR{landPR(11, "feature/1-bell", "BEHIND")}, nil
		},
		now: func() time.Time { return now.Load().(time.Time) },
	}))
	waitFor(t, "ten rounds", func() bool { return ticks.get("acme/widgets") >= 10 })
	if got := len(land.list()); got != 1 {
		t.Fatalf("updates = %d in ten rounds, want 1", got)
	}
	if got := listings.get("acme/widgets"); got != 1 {
		t.Fatalf("listings = %d in ten rounds, want the cached one", got)
	}
	if n := strings.Count(s.buf.String(), "HTTP 502; trying again after"); n != 1 {
		t.Fatalf("logged %d times, want once:\n%s", n, s.buf.String())
	}
	now.Store(start.Add(prListTTL + time.Second))
	waitFor(t, "the second update", func() bool { return len(land.list()) >= 2 })
	before := ticks.get("acme/widgets")
	waitFor(t, "ten more rounds", func() bool { return ticks.get("acme/widgets") >= before+10 })
	s.stop(t)
	if got := len(land.list()); got != 2 {
		t.Fatalf("updates = %d, want 2", got)
	}
	if !strings.Contains(s.buf.String(), "trying again after "+(2*cfg.Wait).String()) {
		t.Fatalf("the second wait did not double:\n%s", s.buf.String())
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

// An issue whose pull request waits to land is seen by new work's pick
// every round. The pick reuses the cached listing, as the fix stage does.
func TestQueueReusesTheListingForAnIssueThatWaitsToLand(t *testing.T) {
	cfg := fixQueue(t, filepath.Join(t.TempDir(), "log"))
	seedState(t, cfg.State, []int{1}, nil)
	ticks := &countCalls{}
	listings := &countCalls{}
	pr := landPR(11, "feature/1-bell", "CLEAN")
	pr.Checks[0].Status = "IN_PROGRESS"
	start := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	s := startQueue(t, cfg, (&landCalls{}).env(queueEnv{
		list: func(repo, _ string) ([]ghx.Issue, error) {
			ticks.add(repo)
			return []ghx.Issue{openIssue(1, "2026-01-01T00:00:00Z")}, nil
		},
		prs: func(repo string) ([]ghx.OpenPR, error) {
			listings.add(repo)
			return []ghx.OpenPR{pr}, nil
		},
		now: func() time.Time { return start },
	}))
	waitFor(t, "ten rounds", func() bool { return ticks.get("acme/widgets") >= 10 })
	s.stop(t)
	if got := listings.get("acme/widgets"); got != 1 {
		t.Fatalf("listings = %d in ten rounds, want 1", got)
	}
}

func TestQueueTriesARefusedMergeOncePerListing(t *testing.T) {
	cfg := fixQueue(t, filepath.Join(t.TempDir(), "log"))
	seedState(t, cfg.State, []int{1}, nil)
	land := &landCalls{merge: func(int) error { return errors.New("the base branch policy prohibits the merge") }}
	ticks := &countCalls{}
	start := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	s := startQueue(t, cfg, land.env(queueEnv{
		list: func(repo, _ string) ([]ghx.Issue, error) { ticks.add(repo); return nil, nil },
		prs:  staticPRs(landPR(11, "feature/1-bell", "BLOCKED")),
		now:  func() time.Time { return start },
	}))
	waitFor(t, "ten rounds", func() bool { return ticks.get("acme/widgets") >= 10 })
	s.stop(t)
	if got := land.list(); len(got) != 1 {
		t.Fatalf("landed = %q, want one try while the wait has not elapsed", got)
	}
	if !strings.Contains(s.buf.String(), "prohibits the merge") || !strings.Contains(s.buf.String(), "trying again after") {
		t.Fatalf("log =\n%s", s.buf.String())
	}
	st, err := loadQueueState(cfg.State)
	if err != nil {
		t.Fatal(err)
	}
	h := st.Merges["acme/widgets#11"]
	if h == nil || h.Count != 1 || !h.Next.Equal(start.Add(cfg.Wait)) {
		t.Fatalf("hold = %+v, want one refusal waiting %s", h, cfg.Wait)
	}
}

func TestQueueRetriesARefusedMergeAfterTheWait(t *testing.T) {
	cfg := fixQueue(t, filepath.Join(t.TempDir(), "log"))
	seedState(t, cfg.State, []int{1}, nil)
	var tries atomic.Int32
	land := &landCalls{merge: func(int) error {
		if tries.Add(1) < 3 {
			return errors.New("the base branch policy prohibits the merge")
		}
		return nil
	}}
	start := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	var now atomic.Value
	now.Store(start)
	s := startQueue(t, cfg, land.env(queueEnv{
		list: noIssues,
		prs:  staticPRs(landPR(11, "feature/1-bell", "CLEAN")),
		now:  func() time.Time { return now.Load().(time.Time) },
	}))
	waitFor(t, "the first refusal", func() bool { return mergeHoldCount(t, cfg.State) >= 1 })
	if tries.Load() != 1 {
		t.Fatalf("tries = %d before the wait", tries.Load())
	}
	now.Store(mergeHoldNext(t, cfg.State))
	waitFor(t, "the second refusal", func() bool { return mergeHoldCount(t, cfg.State) >= 2 })
	if tries.Load() != 2 {
		t.Fatalf("tries = %d after one wait, want 2 (twice the wait has not elapsed)", tries.Load())
	}
	now.Store(mergeHoldNext(t, cfg.State))
	waitFor(t, "the merge", func() bool { return tries.Load() >= 3 })
	s.stop(t)
	if got := land.list(); len(got) != 3 || got[0] != "merge acme/widgets#11 squash head11" || got[1] != got[0] || got[2] != got[0] {
		t.Fatalf("landed = %q", got)
	}
}

func TestQueueStopsMergingAfterFiveRefusals(t *testing.T) {
	dir := t.TempDir()
	notified := filepath.Join(dir, "notify")
	cfg := fixQueue(t, filepath.Join(dir, "log"))
	cfg.Fix.Notify = []string{appendScript(t), notified, "{repo}#{number} PR {pr}: {reason}"}
	seedState(t, cfg.State, []int{1}, nil)
	var tries atomic.Int32
	land := &landCalls{merge: func(int) error {
		tries.Add(1)
		return errors.New("the base branch policy prohibits the merge")
	}}
	labels := &labelCalls{}
	start := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	var now atomic.Value
	now.Store(start)
	s := startQueue(t, cfg, land.env(queueEnv{
		list:     noIssues,
		prs:      staticPRs(landPR(11, "feature/1-bell", "CLEAN")),
		addLabel: labels.add,
		now:      func() time.Time { return now.Load().(time.Time) },
	}))
	for n := 1; n <= maxMergeRefusals; n++ {
		want := n
		waitFor(t, fmt.Sprintf("refusal %d", want), func() bool { return mergeHoldCount(t, cfg.State) >= want })
		if want < maxMergeRefusals {
			now.Store(mergeHoldNext(t, cfg.State))
		}
	}
	waitFor(t, "the label", func() bool { return len(labels.list()) >= 1 })
	now.Store(start.Add(24 * time.Hour))
	time.Sleep(150 * time.Millisecond)
	s.stop(t)
	if tries.Load() != maxMergeRefusals {
		t.Fatalf("tries = %d, want %d", tries.Load(), maxMergeRefusals)
	}
	if labels.list()[0] != "acme/widgets#1 blocked" {
		t.Fatalf("labels = %q", labels.list())
	}
	note := readLines(t, notified)
	if len(note) != 1 || !strings.Contains(note[0], "prohibits the merge") {
		t.Fatalf("notify = %q", note)
	}
	log := s.buf.String()
	if strings.Count(log, "refusal 1 of 5") != 1 || !strings.Contains(log, "not trying again") {
		t.Fatalf("log =\n%s", log)
	}
}

func TestQueueResetsAMergeRefusalWhenTheHeadChanges(t *testing.T) {
	cfg := fixQueue(t, filepath.Join(t.TempDir(), "log"))
	seedState(t, cfg.State, []int{1}, nil)
	var head atomic.Value
	head.Store("head11")
	var tries atomic.Int32
	land := &landCalls{merge: func(int) error {
		tries.Add(1)
		return errors.New("the base branch policy prohibits the merge")
	}}
	start := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	var now atomic.Value
	now.Store(start)
	s := startQueue(t, cfg, land.env(queueEnv{
		list: func(string, string) ([]ghx.Issue, error) {
			return []ghx.Issue{openIssue(7, "2026-01-01T00:00:00Z")}, nil
		},
		prs: func(string) ([]ghx.OpenPR, error) {
			pr := landPR(11, "feature/1-bell", "CLEAN")
			pr.HeadRefOid = head.Load().(string)
			return []ghx.OpenPR{pr, redPR(17, "feature/7-other")}, nil
		},
		now: func() time.Time { return now.Load().(time.Time) },
	}))
	waitFor(t, "the first refusal", func() bool { return mergeHoldCount(t, cfg.State) >= 1 })
	now.Store(mergeHoldNext(t, cfg.State))
	waitFor(t, "two refusals", func() bool { return mergeHoldCount(t, cfg.State) >= 2 })
	// A new head is seen on the next listing, once the cached one is
	// older than prListTTL. It is tried and the count starts again.
	head.Store("head11b")
	now.Store(now.Load().(time.Time).Add(prListTTL))
	waitFor(t, "the reset try", func() bool { return tries.Load() >= 3 })
	waitFor(t, "the reset count", func() bool {
		st, err := loadQueueState(cfg.State)
		return err == nil && st.Merges["acme/widgets#11"] != nil && st.Merges["acme/widgets#11"].Count == 1 && st.Merges["acme/widgets#11"].Head == "head11b"
	})
	s.stop(t)
}

func TestQueueKeepsAMergeWaitAcrossARestart(t *testing.T) {
	cfg := fixQueue(t, filepath.Join(t.TempDir(), "log"))
	seedState(t, cfg.State, []int{1}, nil)
	land := &landCalls{merge: func(int) error { return errors.New("the base branch policy prohibits the merge") }}
	start := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	var now atomic.Value
	now.Store(start)
	env := land.env(queueEnv{
		list: noIssues,
		prs:  staticPRs(landPR(11, "feature/1-bell", "CLEAN")),
		now:  func() time.Time { return now.Load().(time.Time) },
	})
	s := startQueue(t, cfg, env)
	waitFor(t, "the first refusal", func() bool { return mergeHoldCount(t, cfg.State) >= 1 })
	s.stop(t)
	if len(land.list()) != 1 {
		t.Fatalf("landed = %q", land.list())
	}
	ticks := &countCalls{}
	env.list = func(repo, _ string) ([]ghx.Issue, error) { ticks.add(repo); return nil, nil }
	s = startQueue(t, cfg, env)
	waitFor(t, "rounds after restart", func() bool { return ticks.get("acme/widgets") >= 5 })
	if len(land.list()) != 1 {
		t.Fatalf("restart tried the merge again: %q", land.list())
	}
	now.Store(mergeHoldNext(t, cfg.State))
	waitFor(t, "the second refusal", func() bool { return mergeHoldCount(t, cfg.State) >= 2 })
	s.stop(t)
	if len(land.list()) != 2 {
		t.Fatalf("landed = %q, want the try after the saved wait", land.list())
	}
}

func TestQueueDoesNotLandACancelledRequiredCheck(t *testing.T) {
	cfg := fixQueue(t, filepath.Join(t.TempDir(), "log"))
	seedState(t, cfg.State, []int{1}, nil)
	pr := landPR(11, "feature/1-bell", "CLEAN")
	pr.Checks[0].Conclusion = "CANCELLED"
	land := &landCalls{}
	ticks := &countCalls{}
	s := startQueue(t, cfg, land.env(queueEnv{
		list: func(repo, _ string) ([]ghx.Issue, error) { ticks.add(repo); return nil, nil },
		prs:  staticPRs(pr),
		requiredChecks: func(string, string) ([]string, bool, error) {
			return []string{"ci"}, true, nil
		},
	}))
	waitFor(t, "the refusal", func() bool {
		return ticks.get("acme/widgets") >= 3 && strings.Contains(s.buf.String(), "not landing: ci is CANCELLED")
	})
	s.stop(t)
	if len(land.list()) != 0 {
		t.Fatalf("landed = %q", land.list())
	}
}

func TestQueueDoesNotLandACancelledCheckThatIsNotRequired(t *testing.T) {
	cfg := fixQueue(t, filepath.Join(t.TempDir(), "log"))
	seedState(t, cfg.State, []int{1}, nil)
	pr := landPR(11, "feature/1-bell", "CLEAN")
	pr.Checks = append(pr.Checks, ghx.CheckRun{Typename: "CheckRun", Name: "lint", Status: "COMPLETED", Conclusion: "CANCELLED", StartedAt: "2026-01-01T00:00:00Z"})
	land := &landCalls{}
	ticks := &countCalls{}
	s := startQueue(t, cfg, land.env(queueEnv{
		list: func(repo, _ string) ([]ghx.Issue, error) { ticks.add(repo); return nil, nil },
		prs:  staticPRs(pr),
		requiredChecks: func(string, string) ([]string, bool, error) {
			return []string{"ci"}, true, nil
		},
	}))
	waitFor(t, "the refusal", func() bool {
		return ticks.get("acme/widgets") >= 3 && strings.Contains(s.buf.String(), "not landing: lint is CANCELLED")
	})
	s.stop(t)
	if len(land.list()) != 0 {
		t.Fatalf("landed = %q", land.list())
	}
}

// A branch with neither a ruleset nor classic protection requires nothing,
// and that is known. A cancelled check there still does not land.
func TestQueueDoesNotLandACancelledCheckWhenNothingIsRequired(t *testing.T) {
	cfg := fixQueue(t, filepath.Join(t.TempDir(), "log"))
	seedState(t, cfg.State, []int{1}, nil)
	pr := landPR(11, "feature/1-bell", "CLEAN")
	pr.Checks[0].Conclusion = "CANCELLED"
	land := &landCalls{}
	ticks := &countCalls{}
	s := startQueue(t, cfg, land.env(queueEnv{
		list: func(repo, _ string) ([]ghx.Issue, error) { ticks.add(repo); return nil, nil },
		prs:  staticPRs(pr),
		requiredChecks: func(string, string) ([]string, bool, error) {
			return nil, true, nil
		},
	}))
	waitFor(t, "the refusal", func() bool {
		return ticks.get("acme/widgets") >= 3 && strings.Contains(s.buf.String(), "not landing: ci is CANCELLED")
	})
	s.stop(t)
	if len(land.list()) != 0 {
		t.Fatalf("landed = %q", land.list())
	}
}

func TestLandableChecks(t *testing.T) {
	run := func(name, conclusion string) ghx.CheckRun {
		return ghx.CheckRun{Typename: "CheckRun", Name: name, Status: "COMPLETED", Conclusion: conclusion, StartedAt: "2026-01-01T00:00:00Z"}
	}
	cancelled := ghx.OpenPR{Checks: []ghx.CheckRun{run("ci", "CANCELLED")}}
	if ok, why := landableChecks(cancelled, []string{"ci"}, true); ok || why != "ci is CANCELLED" {
		t.Fatalf("required cancelled = %v %q", ok, why)
	}
	if ok, why := landableChecks(cancelled, nil, false); ok || why != "ci is CANCELLED" {
		t.Fatalf("unreadable cancelled = %v %q", ok, why)
	}
	skipped := ghx.OpenPR{Checks: []ghx.CheckRun{run("ci", "SKIPPED")}}
	if ok, _ := landableChecks(skipped, nil, false); !ok {
		t.Fatal("an unreadable skipped check should pass")
	}
	if ok, why := landableChecks(skipped, []string{"ci"}, true); ok || why != "ci is SKIPPED" {
		t.Fatalf("required skipped = %v %q", ok, why)
	}
	missing := ghx.OpenPR{Checks: []ghx.CheckRun{run("ci", "SUCCESS")}}
	if ok, why := landableChecks(missing, []string{"ci", "lint"}, true); ok || why != "lint is missing" {
		t.Fatalf("missing = %v %q", ok, why)
	}
	neutral := ghx.OpenPR{Checks: []ghx.CheckRun{run("ci", "SUCCESS"), run("extra", "NEUTRAL")}}
	if ok, why := landableChecks(neutral, nil, false); !ok {
		t.Fatalf("neutral = %v %q", ok, why)
	}
	if ok, why := landableChecks(neutral, []string{"ci"}, true); !ok {
		t.Fatalf("neutral, not required = %v %q", ok, why)
	}
	for _, res := range []string{"CANCELLED", "STALE", "FAILURE", ""} {
		other := ghx.OpenPR{Checks: []ghx.CheckRun{run("ci", "SUCCESS"), run("extra", res)}}
		want := "extra is " + res
		if res == "" {
			want = "extra is UNSET"
		}
		if ok, why := landableChecks(other, []string{"ci"}, true); ok || why != want {
			t.Fatalf("%s, not required = %v %q", res, ok, why)
		}
		if ok, why := landableChecks(other, nil, true); ok || why != want {
			t.Fatalf("%s, nothing required = %v %q", res, ok, why)
		}
	}
}

func TestMergeBackoffDoublesUntilAnHour(t *testing.T) {
	base := 30 * time.Second
	if mergeBackoff(base, 1) != base || mergeBackoff(base, 2) != 2*base || mergeBackoff(base, 3) != 4*base {
		t.Fatalf("backoff = %s %s %s", mergeBackoff(base, 1), mergeBackoff(base, 2), mergeBackoff(base, 3))
	}
	if mergeBackoff(base, 20) != maxMergeBackoff {
		t.Fatalf("cap = %s", mergeBackoff(base, 20))
	}
}

func mergeHoldCount(t *testing.T, path string) int {
	t.Helper()
	st, err := loadQueueState(path)
	if err != nil {
		t.Fatal(err)
	}
	h := st.Merges["acme/widgets#11"]
	if h == nil {
		return 0
	}
	return h.Count
}

func mergeHoldNext(t *testing.T, path string) time.Time {
	t.Helper()
	st, err := loadQueueState(path)
	if err != nil {
		t.Fatal(err)
	}
	h := st.Merges["acme/widgets#11"]
	if h == nil {
		t.Fatal("no merge hold")
	}
	return h.Next
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
	if !strings.HasPrefix(got, "release: Never take a shortcut.") || !strings.Contains(got, "merge origin/release into it") ||
		!strings.Contains(got, "Never clear a ratchet") || strings.Contains(got, "<base>") {
		t.Fatalf("fix run = %q", got)
	}
}

func TestUpdateBackoffDoublesAndResetsOnANewHead(t *testing.T) {
	cfg := oneAgentConfig(nil)
	cfg.Wait = time.Minute
	start := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	now := start
	r, err := newQueueRunner(cfg, queueEnv{now: func() time.Time { return now }, out: &bytes.Buffer{}}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !r.mayRetry(r.updateHolds, "k", "h1") {
		t.Fatal("a pull request with no failure may not be updated")
	}
	if wait := r.noteFailure(r.updateHolds, "k", "h1"); wait != time.Minute {
		t.Fatalf("first wait = %s", wait)
	}
	if r.mayRetry(r.updateHolds, "k", "h1") {
		t.Fatal("updated again before the wait")
	}
	if !r.mayRetry(r.updateHolds, "k", "h2") {
		t.Fatal("a new head is held by the old head's failure")
	}
	now = start.Add(time.Minute)
	if !r.mayRetry(r.updateHolds, "k", "h1") {
		t.Fatal("held after the wait")
	}
	if wait := r.noteFailure(r.updateHolds, "k", "h1"); wait != 2*time.Minute {
		t.Fatalf("second wait = %s", wait)
	}
}

// A draft is not landed, and neither is a pull request with no checks,
// however clean GitHub says its merge is.
func TestQueueLandsNeitherADraftNorAPRWithNoChecks(t *testing.T) {
	for _, tc := range []struct {
		name string
		pr   func() ghx.OpenPR
	}{
		{"draft", func() ghx.OpenPR { pr := landPR(11, "feature/1-bell", "CLEAN"); pr.IsDraft = true; return pr }},
		{"no checks", func() ghx.OpenPR { pr := landPR(11, "feature/1-bell", "CLEAN"); pr.Checks = nil; return pr }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := fixQueue(t, filepath.Join(t.TempDir(), "log"))
			seedState(t, cfg.State, []int{1}, nil)
			land := &landCalls{}
			ticks := &countCalls{}
			s := startQueue(t, cfg, land.env(queueEnv{
				list: func(repo, _ string) ([]ghx.Issue, error) { ticks.add(repo); return nil, nil },
				prs:  staticPRs(tc.pr()),
			}))
			waitFor(t, "five rounds", func() bool { return ticks.get("acme/widgets") >= 5 })
			s.stop(t)
			if got := land.list(); len(got) != 0 {
				t.Fatalf("landed = %q", got)
			}
		})
	}
}

// A recorded pull request that now comes from a fork is not the queue's:
// it is neither fixed nor landed.
func TestQueueDoesNotTouchARecordedPRFromAFork(t *testing.T) {
	log := filepath.Join(t.TempDir(), "log")
	cfg := fixQueue(t, log)
	seedState(t, cfg.State, []int{1, 2}, nil)
	recordPR(t, cfg.State, 1, 11)
	recordPR(t, cfg.State, 2, 12)
	red := redPR(11, "feature/1-bell")
	red.IsCrossRepository = true
	green := landPR(12, "feature/2-horn", "CLEAN")
	green.IsCrossRepository = true
	land := &landCalls{}
	ticks := &countCalls{}
	s := startQueue(t, cfg, land.env(queueEnv{
		list: func(repo, _ string) ([]ghx.Issue, error) { ticks.add(repo); return nil, nil },
		prs:  staticPRs(red, green),
	}))
	waitFor(t, "five rounds", func() bool { return ticks.get("acme/widgets") >= 5 })
	s.stop(t)
	if got := land.list(); len(got) != 0 {
		t.Fatalf("landed = %q", got)
	}
	if got := countLines(log); got != 0 {
		t.Fatalf("runs = %q", readLines(t, log))
	}
}
