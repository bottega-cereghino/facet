package main

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/bottega-cereghino/facet/internal/ghx"
)

type noPRWorld struct {
	mu       sync.Mutex
	issue    ghx.Issue
	prs      []ghx.OpenPR
	comments []string
	labels   labelCalls
	notified []string
}

func newNoPRRunner(t *testing.T, w *noPRWorld) (*queueRunner, queueConfig, queueEnv) {
	t.Helper()
	cfg := fixQueue(t, "unused")
	cfg.Fix.Notify = []string{"notify", "{repo}#{number}"}
	w.issue = openIssue(1, "2026-01-01T00:00:00Z")
	w.issue.Labels = []ghx.Label{{Name: "next"}}
	env := queueEnv{
		out:      io.Discard,
		issue:    func(string, int) (*ghx.Issue, error) { w.mu.Lock(); defer w.mu.Unlock(); i := w.issue; return &i, nil },
		prs:      func(string) ([]ghx.OpenPR, error) { w.mu.Lock(); defer w.mu.Unlock(); return w.prs, nil },
		addLabel: w.labels.add,
		comment: func(_ string, _ int, body string) error {
			w.mu.Lock()
			defer w.mu.Unlock()
			w.comments = append(w.comments, body)
			return nil
		},
		run: func(_ context.Context, _ string, argv []string) error {
			w.mu.Lock()
			defer w.mu.Unlock()
			w.notified = append(w.notified, strings.Join(argv, " "))
			return nil
		},
	}
	return mustRunner(t, cfg, env), cfg, env
}

func mustRunner(t *testing.T, cfg queueConfig, env queueEnv) *queueRunner {
	t.Helper()
	r, err := newQueueRunner(cfg, env, false)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func noPRIssue() queuedIssue {
	return queuedIssue{Repo: "acme/widgets", Issue: openIssue(1, "2026-01-01T00:00:00Z")}
}

func TestSecondExitWithoutPRBlocksCommentsAndNotifies(t *testing.T) {
	w := &noPRWorld{}
	r, cfg, env := newNoPRRunner(t, w)
	r.noteNoPR(context.Background(), noPRIssue())
	if len(w.labels.list()) != 0 {
		t.Fatalf("first exit labelled: %q", w.labels.list())
	}
	// A restart keeps the count.
	r = mustRunner(t, cfg, env)
	r.noteNoPR(context.Background(), noPRIssue())
	if got := w.labels.list(); len(got) != 1 || got[0] != "acme/widgets#1 blocked" {
		t.Fatalf("labels = %q", got)
	}
	if len(w.comments) != 1 || !strings.Contains(w.comments[0], "remove the `blocked` label") {
		t.Fatalf("comments = %q", w.comments)
	}
	if len(w.notified) != 1 {
		t.Fatalf("notified = %q", w.notified)
	}
	st, err := loadQueueState(cfg.State)
	if err != nil || st.NoPR["acme/widgets#1"] == nil || !st.NoPR["acme/widgets#1"].Blocked {
		t.Fatalf("state = %+v, %v", st.NoPR, err)
	}
}

func TestAPullRequestClearsTheNoPRCount(t *testing.T) {
	w := &noPRWorld{}
	r, cfg, _ := newNoPRRunner(t, w)
	r.noteNoPR(context.Background(), noPRIssue())
	w.prs = []ghx.OpenPR{{Number: 9, HeadRefName: issueBranchName(1, "x")}}
	r.noteNoPR(context.Background(), noPRIssue())
	w.prs = nil
	r.noteNoPR(context.Background(), noPRIssue())
	if len(w.labels.list()) != 0 {
		t.Fatalf("labelled although a pull request cleared the count: %q", w.labels.list())
	}
	st, err := loadQueueState(cfg.State)
	if err != nil || st.NoPR["acme/widgets#1"].Count != 1 {
		t.Fatalf("state = %+v, %v", st.NoPR, err)
	}
}

func TestNoPRCountSkipsAnIssueThatLostItsLabelOrIsClosed(t *testing.T) {
	w := &noPRWorld{}
	r, _, _ := newNoPRRunner(t, w)
	w.issue.Labels = nil
	r.noteNoPR(context.Background(), noPRIssue())
	w.issue.Labels = []ghx.Label{{Name: "next"}}
	w.issue.State = "CLOSED"
	r.noteNoPR(context.Background(), noPRIssue())
	if len(r.state.NoPR) != 0 {
		t.Fatalf("counted: %+v", r.state.NoPR)
	}
}

func TestStartingABlockedIssueAgainResetsTheCount(t *testing.T) {
	w := &noPRWorld{}
	r, _, _ := newNoPRRunner(t, w)
	r.noteNoPR(context.Background(), noPRIssue())
	r.noteNoPR(context.Background(), noPRIssue())
	if n := r.state.NoPR["acme/widgets#1"]; n == nil || !n.Blocked {
		t.Fatalf("not blocked: %+v", r.state.NoPR)
	}
	// The block label was removed by a person, so the issue is started.
	r.recordStarted([]queuedIssue{noPRIssue()}, "")
	if len(r.state.NoPR) != 0 {
		t.Fatalf("count kept: %+v", r.state.NoPR)
	}
}

func TestStartingAnIssueWithOneNoPRExitKeepsTheCount(t *testing.T) {
	w := &noPRWorld{}
	r, _, _ := newNoPRRunner(t, w)
	r.noteNoPR(context.Background(), noPRIssue())
	r.recordStarted([]queuedIssue{noPRIssue()}, "")
	if n := r.state.NoPR["acme/widgets#1"]; n == nil || n.Count != 1 {
		t.Fatalf("count lost: %+v", r.state.NoPR)
	}
}

// An exit 75 never reaches noteNoPR: runOne counts only a zero exit.
func TestExit75IsNotCountedAsNoPR(t *testing.T) {
	cfg, env, labels, log := exitQueue(t, "75")
	env.issue = func(string, int) (*ghx.Issue, error) {
		i := openIssue(1, "2026-01-01T00:00:00Z")
		i.Labels = []ghx.Label{{Name: "next"}}
		return &i, nil
	}
	s := startQueue(t, cfg, env)
	waitFor(t, "three starts", func() bool { return len(readLines(t, log)) >= 3 })
	s.stop(t)
	st, err := loadQueueState(cfg.State)
	if err != nil || len(st.NoPR) != 0 || len(labels.list()) != 0 {
		t.Fatalf("noPR = %+v, labels = %q, %v", st.NoPR, labels.list(), err)
	}
}
