package main

import (
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bottega-cereghino/facet/internal/ghx"
)

// One repository that cannot be listed no longer stops the tick: the issue of
// the other repository starts, and the log names the repository left unseen.
func TestQueueStartsAnIssueFromAnotherRepositoryWhenOneCannotBeListed(t *testing.T) {
	var mu sync.Mutex
	var spawned []string
	cfg := oneAgentConfig([]string{"acme/other", "acme/widgets"})
	s := startQueue(t, cfg, queueEnv{
		list: func(repo, _ string) ([]ghx.Issue, error) {
			if repo == "acme/other" {
				return nil, errors.New("temporary failure")
			}
			return []ghx.Issue{openIssue(1, "2026-01-01T00:00:00Z")}, nil
		},
		spawn: func(repo string, _ int) (string, error) {
			mu.Lock()
			spawned = append(spawned, repo)
			mu.Unlock()
			return "", errors.New("stop here")
		},
	})
	waitFor(t, "a spawn from the repository that listed", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(spawned) > 0
	})
	s.stop(t)
	mu.Lock()
	defer mu.Unlock()
	if spawned[0] != "acme/widgets" {
		t.Fatalf("spawned %v, want acme/widgets", spawned)
	}
	if !strings.Contains(s.buf.String(), "acme/other cannot be listed this tick") {
		t.Fatalf("log = %q, want the unseen repository named", s.buf.String())
	}
}

// Control for the test above: when every repository fails, nothing starts.
func TestQueueDoesNotStartWhenNoRepositoryCanBeListed(t *testing.T) {
	var spawned atomic.Int32
	cfg := oneAgentConfig([]string{"acme/widgets", "acme/other"})
	s := startQueue(t, cfg, queueEnv{
		list: func(string, string) ([]ghx.Issue, error) { return nil, errors.New("temporary failure") },
		spawn: func(string, int) (string, error) {
			spawned.Add(1)
			return "", errors.New("spawned")
		},
	})
	waitFor(t, "the error in the log", func() bool { return strings.Contains(s.buf.String(), "no repository can be listed") })
	s.stop(t)
	if spawned.Load() != 0 {
		t.Fatalf("spawned %d time(s) while nothing could be listed", spawned.Load())
	}
}

// A rate-limit answer pauses all listing until its reset: no call before it,
// and a call after. The heartbeat says until when.
func TestQueueMakesNoListingCallBeforeTheRateLimitReset(t *testing.T) {
	var mu sync.Mutex
	now := time.Unix(1_000_000, 0)
	reset := now.Add(10 * time.Minute)
	var calls int
	clock := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	cfg := oneAgentConfig(nil)
	dir := t.TempDir()
	cfg.Heartbeat = dir + "/hb.json"
	s := startQueue(t, cfg, queueEnv{
		now: clock,
		list: func(string, string) ([]ghx.Issue, error) {
			mu.Lock()
			defer mu.Unlock()
			calls++
			if calls == 1 {
				return nil, &ghx.RateLimitError{Reset: reset, Message: "API rate limit exceeded"}
			}
			return nil, nil
		},
	})
	waitFor(t, "the pause in the log", func() bool { return strings.Contains(s.buf.String(), "paused until") })
	// Several ticks pass (Wait is 20ms) with the clock before the reset.
	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	before := calls
	mu.Unlock()
	if before != 1 {
		t.Fatalf("%d listing calls before the reset, want 1", before)
	}
	hb, _, err := readHeartbeatFile(cfg.Heartbeat)
	if err != nil || hb.ListPausedUntil == nil || !hb.ListPausedUntil.Equal(reset) {
		t.Fatalf("heartbeat = %+v, %v; want list_paused_until %v", hb, err, reset)
	}
	mu.Lock()
	now = reset.Add(time.Second)
	mu.Unlock()
	waitFor(t, "a listing call after the reset", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return calls >= 2
	})
	s.stop(t)
}

// A repository failing for an hour sets last_error; a recovered one clears it.
func TestStaleListErrorNamesARepositoryFailingForAnHour(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	r, err := newQueueRunner(oneAgentConfig(nil), queueEnv{now: func() time.Time { return now }, out: &syncBuffer{}}, true)
	if err != nil {
		t.Fatal(err)
	}
	r.noteListFailure("acme/widgets")
	if got := r.staleListError(); got != "" {
		t.Fatalf("staleListError = %q right after the first failure, want none", got)
	}
	now = now.Add(listFailureAfter)
	if got := r.staleListError(); !strings.Contains(got, "acme/widgets") {
		t.Fatalf("staleListError = %q after an hour, want the repository named", got)
	}
	r.noteListed("acme/widgets")
	if got := r.staleListError(); got != "" {
		t.Fatalf("staleListError = %q after it listed again, want none", got)
	}
}
