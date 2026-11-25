package main

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bottega-cereghino/facet/internal/ghx"
)

// ignoredRun is a queue with a heartbeat over started issue 1, whose open
// pull requests are whatever set last gave it. skew moves the queue's clock
// past the listing cache, so a change shows in the next listing.
type ignoredRun struct {
	cfg  queueConfig
	mu   sync.Mutex
	prs  []ghx.OpenPR
	skew time.Duration
}

func (g *ignoredRun) now() time.Time {
	g.mu.Lock()
	defer g.mu.Unlock()
	return time.Now().Add(g.skew)
}

// change sets the open pull requests and ages the listing cache.
func (g *ignoredRun) change(prs ...ghx.OpenPR) {
	g.mu.Lock()
	g.prs = prs
	g.skew += 2 * prListTTL
	g.mu.Unlock()
}

func (g *ignoredRun) list(string) ([]ghx.OpenPR, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]ghx.OpenPR(nil), g.prs...), nil
}

// startIgnoredRun starts the queue with issue 1 started age ago.
func startIgnoredRun(t *testing.T, age time.Duration, prs ...ghx.OpenPR) *ignoredRun {
	t.Helper()
	cfg := fixQueue(t, filepath.Join(t.TempDir(), "log"))
	cfg.Heartbeat = filepath.Join(t.TempDir(), "hb.json")
	cfg.Wait = 10 * time.Millisecond
	cfg.Fix.Apps = []string{testApp}
	seedState(t, cfg.State, []int{1}, nil)
	st, err := loadQueueState(cfg.State)
	if err != nil {
		t.Fatal(err)
	}
	s := st.Started["acme/widgets#1"]
	s.At = time.Now().Add(-age)
	st.Started["acme/widgets#1"] = s
	if err := st.save(cfg.State); err != nil {
		t.Fatal(err)
	}
	g := &ignoredRun{cfg: cfg}
	g.prs = prs
	startQueue(t, cfg, queueEnv{
		list: func(string, string) ([]ghx.Issue, error) { return nil, nil },
		prs:  g.list,
		now:  g.now,
	})
	return g
}

// waitFor reads the heartbeat until ok holds for its ignored list.
func (g *ignoredRun) waitFor(t *testing.T, what string, ok func([]ignoredPR) bool) []ignoredPR {
	t.Helper()
	var got []ignoredPR
	waitFor(t, what, func() bool {
		hb, found, err := readHeartbeatFile(g.cfg.Heartbeat)
		if err != nil || !found {
			return false
		}
		got = hb.IgnoredPRs
		return ok(got)
	})
	return got
}

func ownedBy(pr ghx.OpenPR, login string) ghx.OpenPR {
	pr.Author = ghx.User{Login: login}
	return pr
}

func TestQueueHeartbeatListsAMarkedPRTheQueueIgnores(t *testing.T) {
	unnamed := ownedBy(redPR(11, "feature/1-bell"), "app/rogue")
	g := startIgnoredRun(t, time.Hour, unnamed)
	got := g.waitFor(t, "the ignored pull request", func(l []ignoredPR) bool { return len(l) == 1 })
	want := ignoredPR{Repo: "acme/widgets", Number: 11, Author: "app/rogue", Reason: ignoredAuthor, Issue: "acme/widgets#1"}
	if got[0] != want {
		t.Fatalf("entry = %+v, want %+v", got[0], want)
	}
	line := got[0].warning()
	for _, frag := range []string{"acme/widgets#11", "app/rogue", `fix.apps: add "rogue"`} {
		if !strings.Contains(line, frag) {
			t.Fatalf("warning %q lacks %q", line, frag)
		}
	}

	// queue status prints it, makes no change to the exit code, and --json
	// carries the same list.
	hb, _, err := readHeartbeatFile(g.cfg.Heartbeat)
	if err != nil {
		t.Fatal(err)
	}
	now := hb.At.Add(time.Second)
	g.cfg.Wait = time.Minute
	var text, js bytes.Buffer
	if err := runQueueStatus(g.cfg, now, false, &text); err != nil {
		t.Fatalf("status exit: %v", err)
	}
	if !strings.Contains(text.String(), "warning: acme/widgets#11 is ignored by the queue") {
		t.Fatalf("status output:\n%s", text.String())
	}
	if err := runQueueStatus(g.cfg, now, true, &js); err != nil {
		t.Fatalf("status --json exit: %v", err)
	}
	var st queueStatusJSON
	if err := json.Unmarshal(js.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if st.State != "ok" || len(st.IgnoredPRs) != 1 || st.IgnoredPRs[0].Number != 11 {
		t.Fatalf("status json: %+v", st)
	}

	// Closed: the entry goes.
	g.change()
	g.waitFor(t, "the entry to go", func(l []ignoredPR) bool { return len(l) == 0 })
}

func TestQueueHeartbeatListsAForkAndDropsAPRThatPassesTheCheck(t *testing.T) {
	fork := redPR(11, "feature/1-bell")
	fork.IsCrossRepository = true
	g := startIgnoredRun(t, time.Hour, fork)
	got := g.waitFor(t, "the fork", func(l []ignoredPR) bool { return len(l) == 1 })
	if got[0].Reason != ignoredFork || !strings.Contains(got[0].warning(), "never the queue's") {
		t.Fatalf("entry %+v, warning %q", got[0], got[0].warning())
	}
	// The same number now passes the check.
	g.change(redPR(11, "feature/1-bell"))
	g.waitFor(t, "the entry to go", func(l []ignoredPR) bool { return len(l) == 0 })
}

func TestQueueHeartbeatDoesNotListPRsItShouldNot(t *testing.T) {
	named := ownedBy(redPR(11, "feature/1-bell"), "app/"+testApp)
	persons := redPR(12, "feature/1-bell-own")
	persons.Author = ghx.User{Login: "someone"}
	persons.Body = "My own pull request."
	g := startIgnoredRun(t, time.Hour, named, persons)
	// Control: a marked pull request by an unnamed author joins, so the
	// heartbeat shows the scan ran and the two above were judged.
	g.change(named, persons, ownedBy(redPR(13, "feature/1-horn"), "someone"))
	got := g.waitFor(t, "the control", func(l []ignoredPR) bool { return len(l) > 0 })
	if len(got) != 1 || got[0].Number != 13 {
		t.Fatalf("ignored = %+v, want only #13", got)
	}
}

// The queue forgets a start older than a day whose pull request it does not
// own. The warning stays until the pull request is closed.
func TestQueueHeartbeatKeepsTheEntryOfAForgottenStartUntilThePRCloses(t *testing.T) {
	g := startIgnoredRun(t, 48*time.Hour, ownedBy(redPR(11, "feature/1-bell"), "app/rogue"))
	g.waitFor(t, "the entry", func(l []ignoredPR) bool { return len(l) == 1 })
	// Several ticks later it is still there: the start is gone.
	time.Sleep(100 * time.Millisecond)
	g.waitFor(t, "the entry still", func(l []ignoredPR) bool { return len(l) == 1 })
	g.change()
	g.waitFor(t, "the entry to go", func(l []ignoredPR) bool { return len(l) == 0 })
}
