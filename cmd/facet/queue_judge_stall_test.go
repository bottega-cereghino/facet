package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bottega-cereghino/facet/internal/ghx"
)

// stallRig is a runner whose clock a test moves and whose PR listing it sets.
type stallRig struct {
	r      *queueRunner
	now    time.Time
	prs    []ghx.OpenPR
	out    *bytes.Buffer
	notify string
	rows   string
}

func newStallRig(t *testing.T, prs ...ghx.OpenPR) *stallRig {
	t.Helper()
	dir := t.TempDir()
	rig := &stallRig{now: time.Date(2026, 11, 26, 8, 0, 0, 0, time.UTC), prs: prs, out: &bytes.Buffer{}}
	rig.notify = filepath.Join(dir, "notify")
	rig.rows = filepath.Join(dir, "decisions.jsonl")
	cfg := fixQueue(t, "unused")
	cfg.Decisions = rig.rows
	cfg.Fix.Notify = []string{appendScript(t), rig.notify, "PR {pr}: {reason}"}
	seedState(t, cfg.State, []int{1}, nil)
	r, err := newQueueRunner(cfg, ownerSeams(queueEnv{
		list: func(string, string) ([]ghx.Issue, error) { return nil, nil },
		prs:  func(string) ([]ghx.OpenPR, error) { return rig.prs, nil },
		now:  func() time.Time { return rig.now },
		out:  rig.out,
	}), false)
	if err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	for k, s := range r.state.Started {
		s.At = rig.now
		r.state.Started[k] = s
	}
	r.mu.Unlock()
	rig.r = r
	return rig
}

// scan runs one fix scan after the clock moved by d.
func (g *stallRig) scan(d time.Duration) {
	g.now = g.now.Add(d)
	g.r.mu.Lock()
	g.r.issueRound = map[string]string{}
	g.r.round = map[string]prListing{}
	g.r.mu.Unlock()
	g.r.scanQueuePRs(context.Background())
}

func (g *stallRig) count(path string) int {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	return strings.Count(string(b), "\n")
}

func noChecksPR() ghx.OpenPR {
	pr := greenPR(11, "feature/1-bell")
	pr.Checks = nil
	pr.HeadRefOid = "h1"
	return pr
}

func TestQueueReportsAPRWithNoChecksOnceAfterTwoHours(t *testing.T) {
	g := newStallRig(t, noChecksPR())
	g.scan(0)
	g.scan(stallAfter - time.Minute)
	if g.count(g.notify) != 0 || g.count(g.rows) != 0 || strings.Contains(g.out.String(), "has not landed") {
		t.Fatalf("reported before two hours:\n%s", g.out.String())
	}
	g.scan(2 * time.Minute)
	for i := 0; i < 3; i++ {
		g.scan(prListTTL + time.Second)
	}
	if g.count(g.notify) != 1 {
		t.Fatalf("notify rows = %d, want 1", g.count(g.notify))
	}
	b, _ := os.ReadFile(g.rows)
	if !strings.Contains(string(b), `"decision":"stalled"`) || !strings.Contains(string(b), "no checks") || g.count(g.rows) != 1 {
		t.Fatalf("decision rows = %q", b)
	}
	if got := strings.Count(g.out.String(), "has not landed"); got != 1 {
		t.Fatalf("log lines = %d, want 1:\n%s", got, g.out.String())
	}
	hb := g.r.heartbeatSnapshot()
	if len(hb.StalledPRs) != 1 || hb.StalledPRs[0].Number != 11 || !strings.Contains(hb.StalledPRs[0].Reason, "no checks") {
		t.Fatalf("heartbeat stalled = %+v", hb.StalledPRs)
	}
}

func TestQueueStallClockRestartsOnANewHeadAndANewReason(t *testing.T) {
	g := newStallRig(t, noChecksPR())
	g.scan(0)
	g.scan(stallAfter - time.Minute)
	pr := noChecksPR()
	pr.HeadRefOid = "h2"
	g.prs = []ghx.OpenPR{pr}
	g.scan(prListTTL + time.Second)
	g.scan(stallAfter - time.Minute)
	if g.count(g.notify) != 0 {
		t.Fatalf("a new head did not restart the clock:\n%s", g.out.String())
	}
	pr.IsDraft = true
	g.prs = []ghx.OpenPR{pr}
	g.scan(prListTTL + time.Second)
	g.scan(stallAfter - time.Minute)
	if g.count(g.notify) != 0 {
		t.Fatalf("a new reason did not restart the clock:\n%s", g.out.String())
	}
	g.scan(2 * time.Minute)
	b, _ := os.ReadFile(g.notify)
	if !strings.Contains(string(b), "draft") {
		t.Fatalf("notify = %q, want the draft reason", b)
	}
}

func TestQueueNamesAMissingLabelAsTheStallReason(t *testing.T) {
	g := newStallRig(t, noChecksPR())
	g.r.env.issue = func(_ string, n int) (*ghx.Issue, error) {
		return &ghx.Issue{Number: n, State: "OPEN"}, nil
	}
	g.scan(0)
	g.scan(stallAfter + time.Minute)
	b, _ := os.ReadFile(g.notify)
	if !strings.Contains(string(b), "no longer carries the next label") {
		t.Fatalf("notify = %q", b)
	}
}

func TestQueueReportsAPRWithoutTheRunMarker(t *testing.T) {
	pr := noChecksPR()
	pr.Body = "no marker here"
	g := newStallRig(t, pr)
	g.scan(0)
	g.scan(stallAfter + time.Minute)
	b, _ := os.ReadFile(g.notify)
	if !strings.Contains(string(b), "not the queue's") {
		t.Fatalf("notify = %q", b)
	}
}

func TestQueueDoesNotReportAMergedOrGreenLandedPR(t *testing.T) {
	g := newStallRig(t, noChecksPR())
	g.scan(0)
	g.prs = nil
	g.scan(stallAfter + time.Minute)
	if g.count(g.notify) != 0 || len(g.r.stalls) != 0 {
		t.Fatalf("reported a PR that is gone: %d, %v", g.count(g.notify), g.r.stalls)
	}
}

func TestStallReason(t *testing.T) {
	g := newStallRig(t)
	draft := greenPR(1, "feature/1-bell")
	draft.IsDraft = true
	cancelled := greenPR(2, "feature/1-bell")
	cancelled.Checks[0].Conclusion = "CANCELLED"
	for name, tc := range map[string]struct {
		pr      ghx.OpenPR
		blocked bool
		want    string
	}{
		"draft":     {draft, false, "draft"},
		"no checks": {noChecksPR(), false, "no checks"},
		"cancelled": {cancelled, false, "CANCELLED"},
		"blocked":   {noChecksPR(), true, ""},
	} {
		got := g.r.stallReason("acme/widgets", tc.pr, tc.blocked)
		if tc.want == "" && got != "" || !strings.Contains(got, tc.want) {
			t.Errorf("%s: reason = %q, want %q", name, got, tc.want)
		}
	}
}
