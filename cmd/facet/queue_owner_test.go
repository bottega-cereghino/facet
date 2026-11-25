package main

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
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

// testApp is the App slug fix.apps names in the tests below.
const testApp = "queue-app"

// A pull request a configured App opened, with the start's trailer line, is
// the queue's: it is merged, in the form gh prints an App's login and in
// the form the REST API does.
func TestQueueLandsAPRAConfiguredAppOpened(t *testing.T) {
	for _, login := range []string{"app/" + testApp, testApp + "[bot]", "App/Queue-App"} {
		t.Run(login, func(t *testing.T) {
			cfg := fixQueue(t, filepath.Join(t.TempDir(), "log"))
			cfg.Fix.Apps = []string{testApp}
			seedState(t, cfg.State, []int{1}, nil)
			land := &landCalls{}
			var merged atomic.Bool
			land.merge = func(int) error { merged.Store(true); return nil }
			pr := landPR(11, "feature/1-bell", "CLEAN")
			pr.Author = ghx.User{Login: login}
			prs := func(string) ([]ghx.OpenPR, error) {
				if merged.Load() {
					return nil, nil
				}
				return []ghx.OpenPR{pr}, nil
			}
			s := startQueue(t, cfg, land.env(queueEnv{list: noIssues, prs: prs}))
			waitFor(t, "the merge", func() bool { return merged.Load() })
			s.stop(t)
			if got := land.list(); strings.Join(got, "|") != "merge acme/widgets#11 squash head11" {
				t.Fatalf("landed = %q", got)
			}
		})
	}
}

// The App's login alone, or the trailer alone, is never enough. An App
// pull request without the start's line, or with another start's, is a
// person's; so is one from an App fix.apps does not name, one from a person
// whose login is the App's bare slug, one from a fork, and any App pull
// request when fix.apps is not set.
func TestQueueNeverAdoptsAnAppPRWithoutEverythingElse(t *testing.T) {
	for _, tc := range []struct {
		name string
		apps []string
		edit func(*ghx.OpenPR)
	}{
		{"no trailer", []string{testApp}, func(pr *ghx.OpenPR) { pr.Body = "My own fix." }},
		{"another run's trailer", []string{testApp}, func(pr *ghx.OpenPR) { pr.Body = markerLine("m99") }},
		{"another app", []string{testApp}, func(pr *ghx.OpenPR) { pr.Author = ghx.User{Login: "app/other-app"} }},
		{"a person named like the app", []string{testApp}, func(pr *ghx.OpenPR) { pr.Author = ghx.User{Login: testApp} }},
		{"a fork", []string{testApp}, func(pr *ghx.OpenPR) { pr.IsCrossRepository = true }},
		{"apps not set", nil, func(*ghx.OpenPR) {}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log := filepath.Join(t.TempDir(), "log")
			cfg := fixQueue(t, log)
			cfg.Fix.Apps = tc.apps
			seedState(t, cfg.State, []int{1, 2}, nil)
			green := landPR(11, "feature/1-bell", "CLEAN")
			red := redPR(12, "feature/2-horn")
			for _, pr := range []*ghx.OpenPR{&green, &red} {
				pr.Author = ghx.User{Login: "app/" + testApp}
				tc.edit(pr)
			}
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
		})
	}
}

func TestAuthorIsApp(t *testing.T) {
	apps := []string{testApp}
	for _, tc := range []struct {
		login string
		want  bool
	}{
		{"app/queue-app", true},
		{"queue-app[bot]", true},
		{"APP/QUEUE-APP", true},
		{"queue-app", false},
		{"app/", false},
		{"[bot]", false},
		{"app/other", false},
		{"queue-app[bot]x", false},
		{"xapp/queue-app", false},
		{"", false},
	} {
		if got := authorIsApp(tc.login, apps); got != tc.want {
			t.Errorf("authorIsApp(%q) = %v, want %v", tc.login, got, tc.want)
		}
	}
	if authorIsApp("app/queue-app", nil) {
		t.Error("no apps matched app/queue-app")
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
