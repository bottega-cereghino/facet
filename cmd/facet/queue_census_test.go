package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bottega-cereghino/facet/internal/ghx"
)

// censusRig runs one pick over six issues that each have a different reason,
// and counts the GitHub listing calls the pick makes.
type censusRig struct {
	r     *queueRunner
	lists int
	prs   int
}

func newCensusRig(t *testing.T, census string) *censusRig {
	t.Helper()
	g := &censusRig{}
	cfg := fixQueue(t, "unused")
	cfg.Census = census
	cfg.PerRepo = 5
	seedState(t, cfg.State, []int{3}, nil)
	now := time.Date(2026, 11, 27, 9, 0, 0, 0, time.UTC)
	branch := func(n int) string { return issueBranchName(n, "x") }
	prs := []ghx.OpenPR{
		{Number: 13, HeadRefName: branch(3), Mergeable: "MERGEABLE", Checks: []ghx.CheckRun{{Typename: "CheckRun", Name: "ci", Status: "COMPLETED", Conclusion: "FAILURE"}}},
		{Number: 14, HeadRefName: branch(4), Mergeable: "MERGEABLE", Checks: []ghx.CheckRun{{Typename: "CheckRun", Name: "ci", Status: "IN_PROGRESS"}}},
	}
	r, err := newQueueRunner(cfg, ownerSeams(queueEnv{
		list: func(string, string) ([]ghx.Issue, error) {
			g.lists++
			return []ghx.Issue{
				openIssue(1, "2026-01-01T00:00:00Z"),
				labelled(2, "2026-01-02T00:00:00Z", "b", "blocked"),
				openIssue(3, "2026-01-03T00:00:00Z"),
				openIssue(4, "2026-01-04T00:00:00Z"),
				openIssue(5, "2026-01-05T00:00:00Z"),
				openIssue(6, "2026-01-06T00:00:00Z"),
			}, nil
		},
		prs: func(string) ([]ghx.OpenPR, error) { g.prs++; return prs, nil },
		now: func() time.Time { return now },
	}), false)
	if err != nil {
		t.Fatal(err)
	}
	r.done["acme/widgets#5"] = true
	r.retryAt["acme/widgets#6"] = now.Add(time.Minute)
	g.r = r
	return g
}

func (g *censusRig) pick(t *testing.T) {
	t.Helper()
	g.r.round = map[string]prListing{}
	run, err := g.r.candidate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(run) != 1 || run[0].Issue.Number != 1 {
		t.Fatalf("pick = %v, want issue 1", keys(run))
	}
}

func TestQueueCensusGivesEveryIssueOneReasonAndAddsNoCalls(t *testing.T) {
	without := newCensusRig(t, "")
	without.pick(t)

	path := filepath.Join(t.TempDir(), "census.json")
	with := newCensusRig(t, path)
	with.pick(t)

	if with.lists != without.lists || with.prs != without.prs {
		t.Fatalf("census made calls: lists %d vs %d, pull request listings %d vs %d", with.lists, without.lists, with.prs, without.prs)
	}
	if without.lists != 1 || without.prs != 1 {
		t.Fatalf("control: lists %d, pull request listings %d, want 1 and 1", without.lists, without.prs)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var c queueCensus
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatal(err)
	}
	want := map[int]string{1: censusStartable, 2: censusBlockedLabel, 3: censusPRRed, 4: censusPROrphan, 5: censusRanNoPR, 6: censusRetryWait}
	if c.Total != len(want) || len(c.Issues) != len(want) {
		t.Fatalf("total %d, issues %d, want %d", c.Total, len(c.Issues), len(want))
	}
	for _, iss := range c.Issues {
		if want[iss.Number] != iss.Reason {
			t.Errorf("issue %d: reason %q, want %q", iss.Number, iss.Reason, want[iss.Number])
		}
	}
	sum := 0
	for _, n := range c.ByReason {
		sum += n
	}
	if sum != c.Total {
		t.Fatalf("counts sum to %d, total is %d", sum, c.Total)
	}
	if c.Startable != 1 || c.Running != 0 {
		t.Fatalf("startable %d, running %d, want 1 and 0", c.Startable, c.Running)
	}
}

func TestQueueCensusLogsOneRowAnHour(t *testing.T) {
	path := filepath.Join(t.TempDir(), "census.json")
	g := newCensusRig(t, path)
	now := time.Date(2026, 11, 27, 9, 0, 0, 0, time.UTC)
	g.r.env.now = func() time.Time { return now }
	g.pick(t)
	now = now.Add(59 * time.Minute)
	g.pick(t)
	if got := censusRows(t, path); got != 1 {
		t.Fatalf("rows after 59 minutes = %d, want 1", got)
	}
	now = now.Add(time.Minute)
	g.pick(t)
	if got := censusRows(t, path); got != 2 {
		t.Fatalf("rows after an hour = %d, want 2", got)
	}
}

func censusRows(t *testing.T, path string) int {
	t.Helper()
	b, err := os.ReadFile(censusLogPath(path))
	if err != nil {
		t.Fatal(err)
	}
	return len(strings.Split(strings.TrimSpace(string(b)), "\n"))
}

func TestQueueCensusConfigIsRelativeToTheConfigFile(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "queue.json")
	if err := os.WriteFile(cfgPath, []byte(`{"repos":["acme/widgets"],"agents":[{"command":["agent","{prompt}"]}],"census":"census.json"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadQueueConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Census != filepath.Join(dir, "census.json") {
		t.Fatalf("census = %q", cfg.Census)
	}
}

func readCensus(t *testing.T, path string) queueCensus {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var c queueCensus
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatal(err)
	}
	return c
}

// driftRig runs one pick over two repositories. widgets is in the priorities
// file; gadgets is not; "ghost" is in the file and in no repos entry. The
// route seam refuses gadgets, as spawn does for a repository with no key.
func driftRig(t *testing.T, priorities string, route func(string) error) queueCensus {
	t.Helper()
	dir := t.TempDir()
	pfile := filepath.Join(dir, "priorities")
	if err := os.WriteFile(pfile, []byte(priorities), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := oneAgentConfig([]string{"acme/widgets", "acme/gadgets"})
	cfg.Census = filepath.Join(dir, "census.json")
	cfg.Priorities = pfile
	r, err := newQueueRunner(cfg, queueEnv{
		list: func(repo, _ string) ([]ghx.Issue, error) {
			if repo == "acme/widgets" {
				return []ghx.Issue{openIssue(1, "2026-01-01T00:00:00Z")}, nil
			}
			return nil, nil
		},
		route: route,
		now:   func() time.Time { return time.Date(2026, 11, 27, 9, 0, 0, 0, time.UTC) },
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.candidate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return readCensus(t, cfg.Census)
}

func TestQueueCensusReportsConfigurationDrift(t *testing.T) {
	route := func(repo string) error {
		if repo == "acme/gadgets" {
			return os.ErrNotExist
		}
		return nil
	}
	c := driftRig(t, "widgets\nghost\n", route)
	got := map[string]string{}
	for _, d := range c.Drift {
		got[d.Kind] = d.Subject
	}
	want := map[string]string{
		driftPriorityNoRepo: "ghost",
		driftNotInPriority:  "acme/gadgets",
		driftNoRoutingKey:   "acme/gadgets",
	}
	if len(c.Drift) != len(want) {
		t.Fatalf("drift = %+v, want %d findings", c.Drift, len(want))
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("drift %s = %q, want %q", k, got[k], v)
		}
	}
}

func TestQueueCensusWithoutDriftHasNoFindings(t *testing.T) {
	c := driftRig(t, "widgets\ngadgets\n", func(string) error { return nil })
	if len(c.Drift) != 0 {
		t.Fatalf("drift = %+v, want none", c.Drift)
	}
}
