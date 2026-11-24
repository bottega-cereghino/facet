package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bottega-cereghino/facet/internal/ghx"
)

// fixQueue is a one-slot queue with a fix block whose agent and fix
// commands append a line to log.
func fixQueue(t *testing.T, log string) queueConfig {
	t.Helper()
	cfg := oneAgentConfig([]string{"acme/widgets"})
	app := appendScript(t)
	cfg.Agents = []queueAgent{{Command: []string{app, log, "start {number}"}}}
	cfg.State = filepath.Join(t.TempDir(), "state.json")
	cfg.Fix = &queueFix{
		Command:    []string{app, log, "fix {number} PR {pr} try {try}: {reason}"},
		Prompt:     defaultFixPrompt,
		Tries:      2,
		BlockLabel: "blocked",
		Land:       true,
	}
	return cfg
}

// testViewer is the account a test queue runs as, and the author of the
// pull requests its helpers build.
const testViewer = "queue-bot"

// ownerSeams fills the seams that tell the queue's own pull requests from a
// person's, when a test left them unset: the queue runs as testViewer, and
// every issue is open and carries the queue label.
func ownerSeams(env queueEnv) queueEnv {
	if env.viewer == nil {
		env.viewer = func() (string, error) { return testViewer, nil }
	}
	if env.issue == nil {
		env.issue = func(_ string, n int) (*ghx.Issue, error) {
			return &ghx.Issue{Number: n, State: "OPEN", Labels: []ghx.Label{{Name: "next"}}}, nil
		}
	}
	return env
}

// testMarker is the trailer token seedState gives issue n's start.
func testMarker(n int) string {
	return "m" + strconv.Itoa(n)
}

// markerFor is the trailer token of the issue named by a spawn branch
// (feature/<n>-slug), or "" for any other branch.
func markerFor(branch string) string {
	rest, ok := strings.CutPrefix(branch, "feature/")
	if !ok {
		return ""
	}
	num, _, _ := strings.Cut(rest, "-")
	n, err := strconv.Atoi(num)
	if err != nil {
		return ""
	}
	return testMarker(n)
}

// recordPR writes into the state that number's queue pull request is pr.
func recordPR(t *testing.T, path string, number, pr int) {
	t.Helper()
	st, err := loadQueueState(path)
	if err != nil {
		t.Fatal(err)
	}
	k := queuedIssue{Repo: "acme/widgets", Issue: ghx.Issue{Number: number}}.key()
	s := st.Started[k]
	s.PR = pr
	st.Started[k] = s
	if err := st.save(path); err != nil {
		t.Fatal(err)
	}
}

// landCalls records the update and merge seams. update, merge, behind and
// files, when set, answer them; otherwise they succeed.
type landCalls struct {
	mu     sync.Mutex
	calls  []string
	update func(n int) error
	merge  func(n int) error
	behind func(repo, base, head string) (bool, error)
	files  func(repo string, n int) ([]string, error)
}

func (l *landCalls) env(e queueEnv) queueEnv {
	e.updateBranch = func(repo string, n int) error {
		l.record(fmt.Sprintf("update %s#%d", repo, n))
		if l.update != nil {
			return l.update(n)
		}
		return nil
	}
	e.mergeMethod = func(string, string) (string, error) { return "squash", nil }
	e.mergePR = func(repo string, n int, method, head string) error {
		l.record(fmt.Sprintf("merge %s#%d %s %s", repo, n, method, head))
		if l.merge != nil {
			return l.merge(n)
		}
		return nil
	}
	e.behindBase = func(repo, base, head string) (bool, error) {
		if l.behind != nil {
			return l.behind(repo, base, head)
		}
		return false, nil
	}
	if l.files != nil {
		e.prFiles = l.files
	}
	return e
}

func (l *landCalls) record(c string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, c)
}

func (l *landCalls) list() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.calls...)
}

// countCalls counts the calls of a seam per argument.
type countCalls struct {
	mu sync.Mutex
	n  map[string]int
}

func (c *countCalls) add(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.n == nil {
		c.n = map[string]int{}
	}
	c.n[key]++
}

func (c *countCalls) get(key string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n[key]
}

func landPR(number int, branch, status string) ghx.OpenPR {
	pr := greenPR(number, branch)
	pr.MergeStateStatus = status
	pr.HeadRefOid = "head" + strconv.Itoa(number)
	return pr
}

// seedState writes a state file in which the queue started each issue
// number of acme/widgets. runs gives a fix record to an issue.
func seedState(t *testing.T, path string, started []int, runs map[int]int) {
	t.Helper()
	st := newQueueState()
	for _, n := range started {
		k := queuedIssue{Repo: "acme/widgets", Issue: ghx.Issue{Number: n}}.key()
		st.Started[k] = startedIssue{Repo: "acme/widgets", Number: n, At: time.Now().Add(-48 * time.Hour), CreatedAt: "2026-01-01T00:00:00Z", Marker: testMarker(n)}
		if r, ok := runs[n]; ok {
			st.Fixes[k] = &fixRecord{PR: 10 + n, Runs: r, Last: time.Now().Add(-time.Hour)}
		}
	}
	if err := st.save(path); err != nil {
		t.Fatal(err)
	}
}

// redPR is a red pull request the queue's run opened: by testViewer, with
// the trailer line of the issue its branch names.
func redPR(number int, branch string) ghx.OpenPR {
	return ghx.OpenPR{Number: number, HeadRefName: branch, BaseRefName: "main", Mergeable: "MERGEABLE",
		Author: ghx.User{Login: testViewer}, Body: "Fixes the thing.\n\n" + markerLine(markerFor(branch)),
		Checks: []ghx.CheckRun{{Typename: "CheckRun", Name: "ci", Status: "COMPLETED", Conclusion: "FAILURE", StartedAt: "2026-01-01T00:00:00Z"}}}
}

func conflictingPR(number int, branch string) ghx.OpenPR {
	pr := redPR(number, branch)
	pr.Mergeable = "CONFLICTING"
	pr.Checks = nil
	return pr
}

func greenPR(number int, branch string) ghx.OpenPR {
	pr := redPR(number, branch)
	pr.Checks[0].Conclusion = "SUCCESS"
	return pr
}

func staticPRs(prs ...ghx.OpenPR) func(string) ([]ghx.OpenPR, error) {
	return func(string) ([]ghx.OpenPR, error) { return prs, nil }
}

type labelCalls struct {
	mu    sync.Mutex
	calls []string
}

func (l *labelCalls) add(repo string, n int, label string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, queuedIssue{Repo: repo, Issue: ghx.Issue{Number: n}}.key()+" "+label)
	return nil
}

func (l *labelCalls) list() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.calls...)
}

func TestJudgePR(t *testing.T) {
	run := func(name, status, conclusion, started string) ghx.CheckRun {
		return ghx.CheckRun{Typename: "CheckRun", Name: name, Status: status, Conclusion: conclusion, StartedAt: started}
	}
	for _, tc := range []struct {
		name    string
		pr      ghx.OpenPR
		verdict prVerdict
		reason  string
	}{
		{"red", redPR(1, "b"), prNeedsFix, "red: ci is FAILURE"},
		{"green", greenPR(1, "b"), prGreen, ""},
		{"a rerun that passed replaces the failure", ghx.OpenPR{Mergeable: "MERGEABLE", Checks: []ghx.CheckRun{
			run("ci", "COMPLETED", "SUCCESS", "2026-01-02T00:00:00Z"), run("ci", "COMPLETED", "FAILURE", "2026-01-01T00:00:00Z")}}, prGreen, ""},
		{"a rerun still going is pending", ghx.OpenPR{Mergeable: "MERGEABLE", Checks: []ghx.CheckRun{
			run("ci", "COMPLETED", "FAILURE", "2026-01-01T00:00:00Z"), run("ci", "IN_PROGRESS", "", "2026-01-02T00:00:00Z")}}, prPending, ""},
		{"any pending check is pending", ghx.OpenPR{Mergeable: "CONFLICTING", BaseRefName: "main", Checks: []ghx.CheckRun{
			run("lint", "COMPLETED", "FAILURE", "x"), run("test", "QUEUED", "", "x")}}, prPending, ""},
		{"unknown mergeable is pending", ghx.OpenPR{Mergeable: "UNKNOWN"}, prPending, ""},
		{"conflicting", ghx.OpenPR{Mergeable: "CONFLICTING", BaseRefName: "main"}, prNeedsFix, "conflicts with main"},
		{"a commit status in error", ghx.OpenPR{Mergeable: "MERGEABLE", Checks: []ghx.CheckRun{{Typename: "StatusContext", Context: "deploy", State: "ERROR"}}}, prNeedsFix, "red: deploy is ERROR"},
		{"a pending commit status", ghx.OpenPR{Mergeable: "MERGEABLE", Checks: []ghx.CheckRun{{Typename: "StatusContext", Context: "deploy", State: "PENDING"}}}, prPending, ""},
		{"a same-named check in another workflow does not hide a failure", ghx.OpenPR{Mergeable: "MERGEABLE", Checks: []ghx.CheckRun{
			{Typename: "CheckRun", WorkflowName: "ci", Name: "test", Status: "COMPLETED", Conclusion: "FAILURE", StartedAt: "2026-01-01T00:00:00Z"},
			{Typename: "CheckRun", WorkflowName: "nightly", Name: "test", Status: "COMPLETED", Conclusion: "SUCCESS", StartedAt: "2026-01-02T00:00:00Z"}}}, prNeedsFix, "red: test is FAILURE"},
		{"a queued rerun with a zero start is newer than a finished run", ghx.OpenPR{Mergeable: "MERGEABLE", Checks: []ghx.CheckRun{
			run("ci", "COMPLETED", "FAILURE", "2026-01-01T00:00:00Z"), run("ci", "QUEUED", "", "0001-01-01T00:00:00Z")}}, prPending, ""},
		{"cancelled is not red", ghx.OpenPR{Mergeable: "MERGEABLE", Checks: []ghx.CheckRun{run("ci", "COMPLETED", "CANCELLED", "x")}}, prGreen, ""},
	} {
		v, reason := judgePR(tc.pr)
		if v != tc.verdict || reason != tc.reason {
			t.Errorf("%s: judgePR = %d %q, want %d %q", tc.name, v, reason, tc.verdict, tc.reason)
		}
	}
}

func TestQueueFixesARedQueuePRBeforeANewIssue(t *testing.T) {
	log := filepath.Join(t.TempDir(), "log")
	cfg := fixQueue(t, log)
	seedState(t, cfg.State, []int{1}, nil)
	labels := &labelCalls{}
	s := startQueue(t, cfg, queueEnv{
		list: func(string, string) ([]ghx.Issue, error) {
			return []ghx.Issue{openIssue(5, "2025-01-01T00:00:00Z")}, nil
		},
		prs:      staticPRs(redPR(11, "feature/1-bell")),
		addLabel: labels.add,
	})
	// The PR stays red, so a second fix try may come before the new issue:
	// red first. What matters is that the first run is the fix and the new
	// issue still starts after it.
	waitFor(t, "the new issue", func() bool {
		for _, l := range readLines(t, log) {
			if l == "start 5" {
				return true
			}
		}
		return false
	})
	s.stop(t)
	got := readLines(t, log)
	if got[0] != "fix 1 PR 11 try 1: red: ci is FAILURE" {
		t.Fatalf("runs = %q, want the fix first", got)
	}
}

func TestQueueFixesAConflictingQueuePR(t *testing.T) {
	cfg := fixQueue(t, "unused")
	seedState(t, cfg.State, []int{1}, nil)
	var buf bytes.Buffer
	err := testDryQueue(context.Background(), cfg, queueEnv{
		list: func(string, string) ([]ghx.Issue, error) { return nil, nil },
		prs:  staticPRs(conflictingPR(11, "feature/1-bell")),
		out:  &buf,
	})
	if err != nil || !strings.Contains(buf.String(), "would fix acme/widgets#1 PR #11: conflicts with main, try 1 of 2") {
		t.Fatalf("err %v, dry-run =\n%s", err, buf.String())
	}
}

func TestQueueDoesNotJudgeAPendingQueuePR(t *testing.T) {
	cfg := fixQueue(t, "unused")
	seedState(t, cfg.State, []int{1, 2}, nil)
	pending := redPR(11, "feature/1-bell")
	pending.Checks = append(pending.Checks, ghx.CheckRun{Typename: "CheckRun", Name: "test", Status: "IN_PROGRESS"})
	unknown := redPR(12, "feature/2-horn")
	unknown.Mergeable = "UNKNOWN"
	var buf bytes.Buffer
	err := testDryQueue(context.Background(), cfg, queueEnv{
		list: func(string, string) ([]ghx.Issue, error) { return nil, nil },
		prs:  staticPRs(pending, unknown),
		out:  &buf,
	})
	if err != nil || strings.Contains(buf.String(), "would fix") {
		t.Fatalf("err %v, dry-run =\n%s", err, buf.String())
	}
}

func TestQueueNeverTouchesAPROfAnIssueItDidNotStart(t *testing.T) {
	cfg := fixQueue(t, "unused")
	seedState(t, cfg.State, []int{1, 3, 4}, nil)
	// #3's own pull request is 13; 23 is a person's, opened later from the
	// same branch shape.
	recordPR(t, cfg.State, 3, 13)
	fork := redPR(14, "feature/4-bell")
	fork.IsCrossRepository = true
	early := redPR(15, "feature/1-early")
	early.CreatedAt = time.Now().Add(-72 * time.Hour).UTC().Format(time.RFC3339)
	labels := &labelCalls{}
	var buf bytes.Buffer
	err := testDryQueue(context.Background(), cfg, queueEnv{
		list: func(string, string) ([]ghx.Issue, error) { return nil, nil },
		prs: staticPRs(redPR(12, "feature/2-mine"), redPR(110, "feature/10-also-mine"),
			greenPR(13, "feature/3-horn"), redPR(23, "feature/3-later"), fork, early),
		addLabel: labels.add,
		out:      &buf,
	})
	if err != nil || strings.Contains(buf.String(), "would fix") || len(labels.list()) != 0 {
		t.Fatalf("err %v, labels %q, dry-run =\n%s", err, labels.list(), buf.String())
	}
	// The loop neither fixes nor lands them, and records none of them.
	log := filepath.Join(t.TempDir(), "log")
	cfg.Fix.Command = []string{appendScript(t), log, "fix {number} PR {pr}"}
	land := &landCalls{}
	person := greenPR(23, "feature/3-later")
	person.MergeStateStatus = "CLEAN"
	gone := staticPRs(redPR(12, "feature/2-mine"), person, fork, early)
	s := startQueue(t, cfg, land.env(queueEnv{
		list:     func(string, string) ([]ghx.Issue, error) { return nil, nil },
		prs:      gone,
		addLabel: labels.add,
	}))
	time.Sleep(200 * time.Millisecond)
	s.stop(t)
	if got := readLines(t, log); len(got) != 0 || len(land.list()) != 0 || len(labels.list()) != 0 {
		t.Fatalf("runs %q, landed %q, labels %q", got, land.list(), labels.list())
	}
	// Their own pull requests are gone, so the queue forgets them; it
	// adopts none of the others.
	st, err := loadQueueState(cfg.State)
	if err != nil || len(st.Started) != 0 {
		t.Fatalf("state = %+v, %v", st, err)
	}
}

func TestQueueBlocksAfterTheLastFailedTry(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "log")
	notified := filepath.Join(dir, "notify")
	cfg := fixQueue(t, log)
	cfg.Fix.Notify = []string{appendScript(t), notified, "{repo}#{number} PR {pr}: {reason}"}
	seedState(t, cfg.State, []int{1}, nil)
	labels := &labelCalls{}
	s := startQueue(t, cfg, queueEnv{
		list:     func(string, string) ([]ghx.Issue, error) { return nil, nil },
		prs:      staticPRs(redPR(11, "feature/1-bell")),
		addLabel: labels.add,
	})
	waitFor(t, "the notify", func() bool { return len(readLines(t, notified)) >= 1 })
	time.Sleep(200 * time.Millisecond)
	s.stop(t)
	if got := readLines(t, log); strings.Join(got, "|") != "fix 1 PR 11 try 1: red: ci is FAILURE|fix 1 PR 11 try 2: red: ci is FAILURE" {
		t.Fatalf("runs = %q", got)
	}
	if got := labels.list(); len(got) != 1 || got[0] != "acme/widgets#1 blocked" {
		t.Fatalf("labels = %q", got)
	}
	if got := readLines(t, notified); len(got) != 1 || got[0] != "acme/widgets#1 PR 11: red: ci is FAILURE" {
		t.Fatalf("notify = %q", got)
	}
	st, err := loadQueueState(cfg.State)
	if err != nil {
		t.Fatal(err)
	}
	if rec := st.Fixes["acme/widgets#1"]; rec == nil || !rec.Blocked || rec.Runs != 2 || rec.PR != 11 {
		t.Fatalf("record = %+v", rec)
	}
}

func TestQueueKeepsFixTriesAcrossAGreenStreak(t *testing.T) {
	log := filepath.Join(t.TempDir(), "log")
	cfg := fixQueue(t, log)
	cfg.Fix.Land = false
	seedState(t, cfg.State, []int{1}, nil)
	labels := &labelCalls{}
	var (
		mu    sync.Mutex
		phase int // 0: red, 1: green, 2: red again
	)
	prs := func(string) ([]ghx.OpenPR, error) {
		mu.Lock()
		p := phase
		mu.Unlock()
		if p == 1 {
			return []ghx.OpenPR{greenPR(11, "feature/1-bell")}, nil
		}
		return []ghx.OpenPR{redPR(11, "feature/1-bell")}, nil
	}
	start := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	var nowMu sync.Mutex
	now := start
	advance := func(d time.Duration) {
		nowMu.Lock()
		now = now.Add(d)
		nowMu.Unlock()
	}
	s := startQueue(t, cfg, queueEnv{
		list:     func(string, string) ([]ghx.Issue, error) { return nil, nil },
		prs:      prs,
		addLabel: labels.add,
		now: func() time.Time {
			nowMu.Lock()
			defer nowMu.Unlock()
			return now
		},
	})
	waitFor(t, "first fix try", func() bool { return len(readLines(t, log)) >= 1 })
	mu.Lock()
	phase = 1
	mu.Unlock()
	advance(prListTTL + time.Second)
	time.Sleep(150 * time.Millisecond)
	st, err := loadQueueState(cfg.State)
	if err != nil {
		t.Fatal(err)
	}
	if rec := st.Fixes["acme/widgets#1"]; rec == nil || rec.Runs != 1 {
		t.Fatalf("after green, fix record = %+v; want Runs=1 kept", rec)
	}
	mu.Lock()
	phase = 2
	mu.Unlock()
	advance(prListTTL + time.Second)
	waitFor(t, "second fix try", func() bool {
		if len(readLines(t, log)) < 2 {
			return false
		}
		st, err := loadQueueState(cfg.State)
		return err == nil && st.Fixes["acme/widgets#1"] != nil && st.Fixes["acme/widgets#1"].Runs == 2 && st.Active["acme/widgets#1"] == 0
	})
	advance(prListTTL + time.Second)
	waitFor(t, "blocked after 2 total tries", func() bool {
		advance(cfg.Wait)
		return len(labels.list()) >= 1
	})
	s.stop(t)
	if got := readLines(t, log); len(got) != 2 {
		t.Fatalf("runs = %q, want exactly 2 total tries across red-green-red", got)
	}
	if labels.list()[0] != "acme/widgets#1 blocked" {
		t.Fatalf("labels = %q", labels.list())
	}
}

func TestQueueStateSurvivesARestart(t *testing.T) {
	log := filepath.Join(t.TempDir(), "log")
	cfg := fixQueue(t, log)
	// The first queue starts #5. Its pull request does not exist yet.
	first := startQueue(t, cfg, queueEnv{
		list: func(string, string) ([]ghx.Issue, error) {
			return []ghx.Issue{openIssue(5, "2026-01-01T00:00:00Z")}, nil
		},
		prs: staticPRs(),
	})
	waitFor(t, "the start", func() bool { return len(readLines(t, log)) >= 1 })
	first.stop(t)
	// The agent ended its pull request's description with the start's
	// trailer line.
	st0, err := loadQueueState(cfg.State)
	if err != nil {
		t.Fatal(err)
	}
	pr := redPR(15, "feature/5-issue-5")
	pr.Body = markerLine(st0.Started["acme/widgets#5"].Marker)
	// A new queue sees #5's pull request red, knows #5 is its own, and
	// makes one fix run. Its long wait keeps it from judging that run.
	red := staticPRs(pr)
	slow := cfg
	slow.Wait = time.Minute
	second := startQueue(t, slow, queueEnv{
		list:     func(string, string) ([]ghx.Issue, error) { return nil, nil },
		prs:      red,
		addLabel: (&labelCalls{}).add,
	})
	waitFor(t, "the first fix", func() bool { return len(readLines(t, log)) >= 2 })
	second.stop(t)
	// A third queue counts on from the second's try: try 2 is the last,
	// and then the issue is blocked.
	labels := &labelCalls{}
	third := startQueue(t, cfg, queueEnv{
		list:     func(string, string) ([]ghx.Issue, error) { return nil, nil },
		prs:      red,
		addLabel: labels.add,
	})
	waitFor(t, "the block", func() bool { return len(labels.list()) >= 1 })
	third.stop(t)
	want := "start 5|fix 5 PR 15 try 1: red: ci is FAILURE|fix 5 PR 15 try 2: red: ci is FAILURE"
	if got := readLines(t, log); strings.Join(got, "|") != want {
		t.Fatalf("runs = %q\nwant %q", got, want)
	}
	st, err := loadQueueState(cfg.State)
	if err != nil {
		t.Fatal(err)
	}
	if s := st.Started["acme/widgets#5"]; s.PR != 15 {
		t.Fatalf("started = %+v, want PR 15 recorded", s)
	}
}

func TestQueueStartsNoFixWhenThePRListingFails(t *testing.T) {
	cfg := fixQueue(t, "unused")
	seedState(t, cfg.State, []int{1}, nil)
	before := readFile(t, cfg.State)
	var buf bytes.Buffer
	err := testDryQueue(context.Background(), cfg, queueEnv{
		list: func(string, string) ([]ghx.Issue, error) { return nil, nil },
		prs:  func(string) ([]ghx.OpenPR, error) { return nil, errors.New("HTTP 502") },
		out:  &buf,
	})
	if err != nil || strings.Contains(buf.String(), "would fix") || !strings.Contains(buf.String(), "starting no fix there") {
		t.Fatalf("err %v, dry-run =\n%s", err, buf.String())
	}
	if readFile(t, cfg.State) != before {
		t.Fatal("the state changed")
	}
}

func TestQueueDryRunListsFixRunsFirstAndWritesNothing(t *testing.T) {
	cfg := fixQueue(t, "unused")
	cfg.Max = 3
	// The main gate refuses; fix.gate lets the fix through.
	cfg.Gate = []string{writeExe(t, "no", "#!/bin/sh\nexit 1\n")}
	cfg.Fix.Gate = []string{writeExe(t, "yes", "#!/bin/sh\nexit 0\n")}
	seedState(t, cfg.State, []int{1, 3}, map[int]int{1: 1, 3: 2})
	before := readFile(t, cfg.State)
	labels := &labelCalls{}
	var buf bytes.Buffer
	err := testDryQueue(context.Background(), cfg, queueEnv{
		list: func(string, string) ([]ghx.Issue, error) {
			return []ghx.Issue{openIssue(5, "2025-01-01T00:00:00Z")}, nil
		},
		prs:      staticPRs(redPR(11, "feature/1-bell"), redPR(13, "feature/3-horn")),
		addLabel: labels.add,
		out:      &buf,
	})
	if err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	if !strings.Contains(got, "would fix acme/widgets#1 PR #11: red: ci is FAILURE, try 2 of 2") ||
		!strings.Contains(got, "would label acme/widgets#3 blocked") ||
		!strings.Contains(got, "would not start acme/widgets#5: gate") {
		t.Fatalf("dry-run =\n%s", got)
	}
	if strings.Index(got, "would fix") > strings.Index(got, "would not start") {
		t.Fatalf("fix runs are not first:\n%s", got)
	}
	if readFile(t, cfg.State) != before || len(labels.list()) != 0 {
		t.Fatalf("dry run wrote: labels %q", labels.list())
	}
}

func TestQueueWithoutFixChangesNothing(t *testing.T) {
	cfg := oneAgentConfig([]string{"acme/widgets"})
	cfg.Max = 2
	issues := func(string, string) ([]ghx.Issue, error) {
		return []ghx.Issue{openIssue(1, "2026-01-01T00:00:00Z"), openIssue(5, "2026-01-02T00:00:00Z")}, nil
	}
	green := greenPR(12, "feature/2-done")
	green.MergeStateStatus = "CLEAN"
	prs := staticPRs(redPR(11, "feature/1-bell"), green)
	var buf bytes.Buffer
	err := testDryQueue(context.Background(), cfg, queueEnv{list: issues, prs: prs, out: &buf})
	got := buf.String()
	if err != nil || strings.Contains(got, "fix") || strings.Contains(got, "merge") || !strings.Contains(got, "would start acme/widgets#5") ||
		!strings.Contains(got, "acme/widgets#1 has an open pull request from feature/1-bell") {
		t.Fatalf("err %v, dry-run =\n%s", err, got)
	}
	// The loop starts #5 only, and fixes, labels and lands nothing.
	log := filepath.Join(t.TempDir(), "log")
	cfg.Agents = []queueAgent{{Command: []string{appendScript(t), log, "start {number}"}}}
	labels := &labelCalls{}
	land := &landCalls{}
	s := startQueue(t, cfg, land.env(queueEnv{list: issues, prs: prs, addLabel: labels.add}))
	waitFor(t, "the start", func() bool { return len(readLines(t, log)) >= 1 })
	time.Sleep(200 * time.Millisecond)
	s.stop(t)
	if got := readLines(t, log); strings.Join(got, "|") != "start 5" || len(labels.list()) != 0 || len(land.list()) != 0 {
		t.Fatalf("runs %q, labels %q, landed %q\n%s", got, labels.list(), land.list(), s.buf.String())
	}
}

func TestQueueRefusesAStateFileItCannotParse(t *testing.T) {
	cfg := fixQueue(t, "unused")
	if err := os.WriteFile(cfg.State, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := testDryQueue(context.Background(), cfg, queueEnv{list: func(string, string) ([]ghx.Issue, error) { return nil, nil }, out: &bytes.Buffer{}})
	if err == nil || !strings.Contains(err.Error(), "fix:") {
		t.Fatalf("err = %v", err)
	}
}

func TestQueueFixConfig(t *testing.T) {
	base := `"repos": ["acme/widgets"], "agents": [{"command": ["agent", "{prompt}"]}]`
	cfg, err := loadTempConfig(t, `{"state": "q-state.json", "fix": {"command": ["fixer", "{prompt}"]}, `+base+`}`)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Fix == nil || cfg.Fix.Tries != 2 || cfg.Fix.BlockLabel != "blocked" || cfg.Fix.Prompt != defaultFixPrompt || !cfg.Fix.Land ||
		cfg.Fix.forbidOff || len(cfg.Fix.Forbid) != len(defaultForbid) ||
		!filepath.IsAbs(cfg.State) || filepath.Base(cfg.State) != "q-state.json" {
		t.Fatalf("fix = %+v, state %q", cfg.Fix, cfg.State)
	}
	if !strings.HasPrefix(cfg.Fix.Prompt, "Never take a shortcut.") || !strings.Contains(cfg.Fix.Prompt, "Never clear a ratchet") {
		t.Fatalf("prompt = %q", cfg.Fix.Prompt)
	}
	off, err := loadTempConfig(t, `{"state": "s", "fix": {"command": ["fixer", "{prompt}"], "land": false}, `+base+`}`)
	if err != nil || off.Fix.Land {
		t.Fatalf("land false: %+v, %v", off.Fix, err)
	}
	none, err := loadTempConfig(t, `{`+base+`}`)
	if err != nil || none.Fix != nil || none.State != "" {
		t.Fatalf("no fix: %+v, %v", none, err)
	}
	offForbid, err := loadTempConfig(t, `{"state": "s", "fix": {"command": ["fixer", "{prompt}"], "forbid": []}, `+base+`}`)
	if err != nil || !offForbid.Fix.forbidOff || offForbid.Fix.Forbid != nil {
		t.Fatalf("forbid off: %+v, %v", offForbid.Fix, err)
	}
	for _, tc := range []struct{ body, want string }{
		{`{"fix": {"command": ["fixer", "{prompt}"]}, ` + base + `}`, "state"},
		{`{"state": "s", "fix": {"command": ["fixer"]}, ` + base + `}`, "{prompt}"},
		{`{"state": "s", "fix": {"command": ["fixer", "{prompt}"], "tries": 6}, ` + base + `}`, "tries"},
		{`{"state": "s", "fix": {"command": ["fixer", "{prompt}"], "tries": 0}, ` + base + `}`, "tries"},
		{`{"state": "s", "fix": {"command": ["fixer", "{prompt}"], "blockLabel": "a,b"}, ` + base + `}`, "comma"},
		{`{"state": "s", "fix": {"command": ["fixer", "{prompt}"], "forbid": ["("]}, ` + base + `}`, "forbid"},
		{`{"state": "s", "fix": {"command": ["fixer", "{prompt}"], "forbid": [""]}, ` + base + `}`, "empty pattern"},
	} {
		if _, err := loadTempConfig(t, tc.body); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", tc.body, err, tc.want)
		}
	}
}

// Two queues on one state file would start the same issue twice and lose
// each other's records. The second one refuses, with the fix, and starts
// nothing; once the first has stopped, a new one runs.
func TestQueueRefusesAStateFileAnotherQueueHolds(t *testing.T) {
	cfg := fixQueue(t, filepath.Join(t.TempDir(), "log"))
	release, err := lockQueueState(cfg.State)
	if err != nil {
		t.Fatal(err)
	}
	listed := &countCalls{}
	env := queueEnv{list: func(repo, _ string) ([]ghx.Issue, error) { listed.add(repo); return nil, nil }, out: &bytes.Buffer{}}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err = runQueue(ctx, cfg, env)
	if err == nil || !strings.Contains(err.Error(), "is in use") || !strings.Contains(err.Error(), "fix:") {
		t.Fatalf("err = %v, want a refusal with the fix", err)
	}
	if listed.get("acme/widgets") != 0 {
		t.Fatal("the second queue listed issues")
	}
	release()
	s := startQueue(t, cfg, env)
	waitFor(t, "a round", func() bool { return listed.get("acme/widgets") >= 1 })
	s.stop(t)
}

// testDryQueue is dryQueue with ownerSeams filled.
func testDryQueue(ctx context.Context, cfg queueConfig, env queueEnv) error {
	return dryQueue(ctx, cfg, ownerSeams(env))
}
