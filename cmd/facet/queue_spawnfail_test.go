package main

import (
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bottega-cereghino/facet/internal/ghx"
)

func TestQueueBlocksAnIssueWhoseSpawnKeepsFailing(t *testing.T) {
	dir := t.TempDir()
	notified := filepath.Join(dir, "notify")
	cfg := fixQueue(t, filepath.Join(dir, "log"))
	cfg.Fix.Notify = []string{appendScript(t), notified, "{repo}#{number}: {reason}"}
	labels := &labelCalls{}
	var calls atomic.Int32
	s := startQueue(t, cfg, queueEnv{
		list: func(string, string) ([]ghx.Issue, error) {
			return []ghx.Issue{openIssue(8, "2026-01-01T00:00:00Z")}, nil
		},
		spawn: func(string, int) (string, error) {
			calls.Add(1)
			return "", errors.New("API returned empty branch name")
		},
		addLabel: labels.add,
	})
	waitFor(t, "the notify", func() bool { return len(readLines(t, notified)) >= 1 })
	s.stop(t)
	if calls.Load() != maxAgentFailures {
		t.Fatalf("spawn calls = %d, want %d", calls.Load(), maxAgentFailures)
	}
	if got := labels.list(); len(got) != 1 || got[0] != "acme/widgets#8 blocked" {
		t.Fatalf("labels = %q", got)
	}
	if got := readLines(t, notified); len(got) != 1 || !strings.Contains(got[0], "spawn failed: API returned empty branch name") {
		t.Fatalf("notify = %q", got)
	}
}

func TestQueueDoesNotCountASpawnFailureThatCanPass(t *testing.T) {
	cfg := fixQueue(t, filepath.Join(t.TempDir(), "log"))
	labels := &labelCalls{}
	var calls atomic.Int32
	s := startQueue(t, cfg, queueEnv{
		list: func(string, string) ([]ghx.Issue, error) {
			return []ghx.Issue{openIssue(8, "2026-01-01T00:00:00Z")}, nil
		},
		spawn: func(string, int) (string, error) {
			calls.Add(1)
			return "", errors.New("Post \"https://api.github.com\": dial tcp: i/o timeout")
		},
		addLabel: labels.add,
	})
	waitFor(t, "five spawn attempts", func() bool { return calls.Load() >= maxAgentFailures+2 })
	time.Sleep(100 * time.Millisecond)
	s.stop(t)
	if got := labels.list(); len(got) != 0 {
		t.Fatalf("labels = %q, want none", got)
	}
}

func TestSpawnErrorMayPass(t *testing.T) {
	for err, want := range map[error]bool{
		&ghx.RateLimitError{Message: "x"}:               true,
		errors.New("HTTP 503: Service Unavailable"):     true,
		errors.New("dial tcp 1.2.3.4:443: i/o timeout"): true,
		errors.New("API returned empty branch name"):    false,
		errors.New("mirror missing"):                    false,
	} {
		if got := spawnErrorMayPass(err); got != want {
			t.Errorf("spawnErrorMayPass(%q) = %v, want %v", err, got, want)
		}
	}
}
