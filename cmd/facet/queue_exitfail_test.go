package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bottega-cereghino/facet/internal/ghx"
)

// exitQueue is a queue whose agent logs a line and exits with status code.
// Its list drops the issue once the block label has been added, as gh does.
func exitQueue(t *testing.T, code string) (queueConfig, queueEnv, *labelCalls, string) {
	t.Helper()
	log := filepath.Join(t.TempDir(), "log")
	cfg := fixQueue(t, log)
	cfg.Agents = []queueAgent{{Command: []string{
		writeExe(t, "agent", "#!/bin/sh\nprintf 'start\\n' >> \"$1\"\nexit "+code+"\n"), log,
	}}}
	labels := &labelCalls{}
	env := queueEnv{
		list: func(string, string) ([]ghx.Issue, error) {
			if len(labels.list()) > 0 {
				return nil, nil
			}
			return []ghx.Issue{openIssue(1, "2026-01-01T00:00:00Z")}, nil
		},
		prs:      staticPRs(),
		addLabel: labels.add,
	}
	return cfg, env, labels, log
}

func TestQueueStartsANonZeroExitAgainAfterWait(t *testing.T) {
	cfg, env, _, log := exitQueue(t, "1")
	cfg.Fix.Notify = nil
	s := startQueue(t, cfg, env)
	waitFor(t, "two starts", func() bool { return len(readLines(t, log)) >= 2 })
	s.stop(t)
}

func TestQueueBlocksAndNotifiesAfterThreeNonZeroExits(t *testing.T) {
	cfg, env, labels, log := exitQueue(t, "1")
	notified := filepath.Join(t.TempDir(), "notify")
	cfg.Fix.Notify = []string{appendScript(t), notified, "{repo}#{number}"}
	s := startQueue(t, cfg, env)
	waitFor(t, "the notify", func() bool { return len(readLines(t, notified)) >= 1 })
	time.Sleep(200 * time.Millisecond)
	s.stop(t)
	if got := readLines(t, log); len(got) != 3 {
		t.Fatalf("starts = %q", got)
	}
	if got := labels.list(); len(got) != 1 || got[0] != "acme/widgets#1 blocked" {
		t.Fatalf("labels = %q", got)
	}
	if got := readLines(t, notified); len(got) != 1 || got[0] != "acme/widgets#1" {
		t.Fatalf("notify = %q", got)
	}
}

func TestQueueDoesNotStartAZeroExitAgain(t *testing.T) {
	cfg, env, labels, log := exitQueue(t, "0")
	s := startQueue(t, cfg, env)
	waitFor(t, "one start", func() bool { return len(readLines(t, log)) >= 1 })
	time.Sleep(300 * time.Millisecond)
	s.stop(t)
	if got := readLines(t, log); len(got) != 1 || len(labels.list()) != 0 {
		t.Fatalf("starts = %q, labels = %q", got, labels.list())
	}
	st, err := loadQueueState(cfg.State)
	if err != nil || st.Done["acme/widgets#1"].IsZero() {
		t.Fatalf("done = %v, %v", st.Done, err)
	}
}

func TestQueueKeepsANonZeroExitOutOfStateDone(t *testing.T) {
	cfg, env, _, log := exitQueue(t, "1")
	s := startQueue(t, cfg, env)
	waitFor(t, "two starts", func() bool { return len(readLines(t, log)) >= 2 })
	s.stop(t)
	st, err := loadQueueState(cfg.State)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Done) != 0 {
		t.Fatalf("done = %v", st.Done)
	}
	// A restart starts the issue again.
	before := len(readLines(t, log))
	s2 := startQueue(t, cfg, env)
	waitFor(t, "a start after the restart", func() bool { return len(readLines(t, log)) > before })
	s2.stop(t)
	if !strings.Contains(strings.Join(readLines(t, log), ""), "start") {
		t.Fatal("no start logged")
	}
}
