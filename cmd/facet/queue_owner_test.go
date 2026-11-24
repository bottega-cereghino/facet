package main

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bottega-cereghino/facet/internal/ghx"
)

// A pull request on the issue's branch, opened after the start, is still a
// person's unless the start's own run opened it: by the account the queue
// runs as, with the start's trailer line. The queue neither fixes, updates,
// merges nor records any other.
func TestQueueNeverAdoptsAPersonsPROnTheIssueBranch(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*ghx.OpenPR)
	}{
		{"no trailer", func(pr *ghx.OpenPR) { pr.Body = "My own fix." }},
		{"another run's trailer", func(pr *ghx.OpenPR) { pr.Body = markerLine("m99") }},
		{"another author", func(pr *ghx.OpenPR) { pr.Author = ghx.User{Login: "someone"} }},
		{"a fork", func(pr *ghx.OpenPR) { pr.IsCrossRepository = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log := filepath.Join(t.TempDir(), "log")
			cfg := fixQueue(t, log)
			seedState(t, cfg.State, []int{1, 2}, nil)
			green := landPR(11, "feature/1-bell", "CLEAN")
			tc.edit(&green)
			red := redPR(12, "feature/2-horn")
			tc.edit(&red)
			land := &landCalls{}
			labels := &labelCalls{}
			ticks := &countCalls{}
			s := startQueue(t, cfg, land.env(queueEnv{
				list:     func(repo, _ string) ([]ghx.Issue, error) { ticks.add(repo); return nil, nil },
				prs:      staticPRs(green, red),
				addLabel: labels.add,
			}))
			waitFor(t, "five rounds", func() bool { return ticks.get("acme/widgets") >= 5 })
			s.stop(t)
			if got := readLines(t, log); len(got) != 0 || len(land.list()) != 0 || len(labels.list()) != 0 {
				t.Fatalf("runs %q, landed %q, labels %q", got, land.list(), labels.list())
			}
			st, err := loadQueueState(cfg.State)
			if err != nil {
				t.Fatal(err)
			}
			for k, started := range st.Started {
				if started.PR != 0 {
					t.Fatalf("%s recorded PR #%d", k, started.PR)
				}
			}
		})
	}
}

// The issue is read again before its pull request is fixed, updated or
// merged. Closed, without the queue label, or labelled blocked, the pull
// request is left alone.
func TestQueueLeavesAPRAloneOnceItsIssueIsClosedUnlabelledOrBlocked(t *testing.T) {
	for _, tc := range []struct {
		name  string
		issue ghx.Issue
		why   string
	}{
		{"closed", ghx.Issue{State: "CLOSED", Labels: []ghx.Label{{Name: "next"}}}, "the issue is closed"},
		{"label gone", ghx.Issue{State: "OPEN"}, "no longer carries the next label"},
		{"blocked", ghx.Issue{State: "OPEN", Labels: []ghx.Label{{Name: "next"}, {Name: "Blocked"}}}, "the issue is labelled blocked"},
		{"unreadable", ghx.Issue{}, "the issue cannot be read"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log := filepath.Join(t.TempDir(), "log")
			cfg := fixQueue(t, log)
			seedState(t, cfg.State, []int{1, 2}, map[int]int{2: 2})
			land := &landCalls{}
			labels := &labelCalls{}
			ticks := &countCalls{}
			reads := &countCalls{}
			s := startQueue(t, cfg, land.env(queueEnv{
				list: func(repo, _ string) ([]ghx.Issue, error) { ticks.add(repo); return nil, nil },
				// #1's is green and up to date; #2's is red with its tries used.
				prs:      staticPRs(landPR(11, "feature/1-bell", "CLEAN"), redPR(12, "feature/2-horn"), redPR(13, "feature/3-drum")),
				addLabel: labels.add,
				issue: func(repo string, n int) (*ghx.Issue, error) {
					reads.add(repo)
					if tc.name == "unreadable" {
						return nil, errors.New("HTTP 502")
					}
					iss := tc.issue
					iss.Number = n
					return &iss, nil
				},
			}))
			waitFor(t, "five rounds", func() bool { return ticks.get("acme/widgets") >= 5 })
			s.stop(t)
			if got := readLines(t, log); len(got) != 0 || len(land.list()) != 0 || len(labels.list()) != 0 {
				t.Fatalf("runs %q, landed %q, labels %q", got, land.list(), labels.list())
			}
			if reads.get("acme/widgets") == 0 || !strings.Contains(s.buf.String(), tc.why) {
				t.Fatalf("reads %d, log =\n%s", reads.get("acme/widgets"), s.buf.String())
			}
		})
	}
}

// A queue whose fix stage is on tells each run's agent the trailer line to
// end its pull request's description with, and records it. A prompt that
// places {marker} itself gets no added sentence.
func TestQueuePromptCarriesTheRunMarker(t *testing.T) {
	for _, tc := range []struct {
		name, prompt string
		added        bool
	}{
		{"added", "Do {issues}.", true},
		{"placed", "Do {issues}. Last line: {marker}", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log := filepath.Join(t.TempDir(), "log")
			cfg := fixQueue(t, log)
			cfg.Prompt = tc.prompt
			cfg.Agents = []queueAgent{{Command: []string{appendScript(t), log, "{prompt}"}}}
			s := startQueue(t, cfg, queueEnv{
				list: func(string, string) ([]ghx.Issue, error) {
					return []ghx.Issue{openIssue(5, "2026-01-01T00:00:00Z")}, nil
				},
				prs: staticPRs(),
			})
			waitFor(t, "the start", func() bool { return len(readLines(t, log)) >= 1 })
			s.stop(t)
			st, err := loadQueueState(cfg.State)
			if err != nil {
				t.Fatal(err)
			}
			marker := st.Started["acme/widgets#5"].Marker
			if len(marker) != 16 {
				t.Fatalf("marker = %q", marker)
			}
			got := strings.Join(readLines(t, log), "\n")
			if !strings.Contains(got, markerLine(marker)) || strings.Contains(got, "exactly as written") != tc.added {
				t.Fatalf("prompt = %q", got)
			}
		})
	}
}

// Without a fix block no pull request is ever the queue's, and the prompt
// is the configured one, unchanged.
func TestQueuePromptHasNoMarkerWithoutFix(t *testing.T) {
	log := filepath.Join(t.TempDir(), "log")
	cfg := oneAgentConfig(nil)
	cfg.Prompt = "Do {issues}.{marker}"
	cfg.Agents = []queueAgent{{Command: []string{appendScript(t), log, "{prompt}"}}}
	s := startQueue(t, cfg, queueEnv{
		list: func(string, string) ([]ghx.Issue, error) {
			return []ghx.Issue{openIssue(5, "2026-01-01T00:00:00Z")}, nil
		},
	})
	waitFor(t, "the start", func() bool { return len(readLines(t, log)) >= 1 })
	s.stop(t)
	if got := readLines(t, log); len(got) != 1 || got[0] != "Do acme/widgets#5: Issue 5." {
		t.Fatalf("prompt = %q", got)
	}
}

// When the account the queue runs as cannot be read, no pull request is
// taken as the queue's, and the start is not forgotten either.
func TestQueueKeepsTheStartWhenTheAccountCannotBeRead(t *testing.T) {
	cfg := fixQueue(t, filepath.Join(t.TempDir(), "log"))
	seedState(t, cfg.State, []int{1}, nil)
	var buf bytes.Buffer
	err := dryQueue(context.Background(), cfg, ownerSeams(queueEnv{
		list:   func(string, string) ([]ghx.Issue, error) { return nil, nil },
		prs:    staticPRs(redPR(11, "feature/1-bell")),
		viewer: func() (string, error) { return "", errors.New("HTTP 401") },
		out:    &buf,
	}))
	if err != nil || strings.Contains(buf.String(), "would fix") || !strings.Contains(buf.String(), "HTTP 401") {
		t.Fatalf("err %v, dry-run =\n%s", err, buf.String())
	}
	land := &landCalls{}
	s := startQueue(t, cfg, land.env(queueEnv{
		list:   func(string, string) ([]ghx.Issue, error) { return nil, nil },
		prs:    staticPRs(landPR(11, "feature/1-bell", "CLEAN")),
		viewer: func() (string, error) { return "", errors.New("HTTP 401") },
	}))
	time.Sleep(150 * time.Millisecond)
	s.stop(t)
	st, err := loadQueueState(cfg.State)
	if err != nil || len(land.list()) != 0 || st.Started["acme/widgets#1"].Marker != testMarker(1) {
		t.Fatalf("landed %q, state %+v, %v", land.list(), st, err)
	}
}
