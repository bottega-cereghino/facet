package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/bottega-cereghino/facet/internal/ghx"
)

// TestWriteAuthorWarning: spawn warns whenever it cannot show that the issue
// is the signed-in account's own, because its body goes into CLAUDE.md and an
// agent reads it there. Not knowing either side counts as a stranger.
func TestWriteAuthorWarning(t *testing.T) {
	me := &ghx.AuthStatus{Account: "Alice"}
	tests := []struct {
		name   string
		author string
		st     *ghx.AuthStatus
		want   string // "" means no warning
	}{
		{"own issue, login case differs", "alice", me, ""},
		{"someone else's issue", "mallory", me, "written by @mallory, not by you (@Alice)"},
		{"author unknown", "", me, "GitHub did not say who wrote it"},
		{"account unknown", "alice", nil, "the signed-in account is unknown"},
		{"account empty", "alice", &ghx.AuthStatus{}, "the signed-in account is unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			iss := &ghx.Issue{Number: 67, URL: "https://github.com/acme/platform/issues/67", Author: ghx.User{Login: tt.author}}
			var buf bytes.Buffer
			writeAuthorWarning(&buf, "acme/platform", iss, tt.st)
			out := buf.String()
			if tt.want == "" {
				if out != "" {
					t.Errorf("warned about the user's own issue:\n%s", out)
				}
				return
			}
			for _, s := range []string{tt.want, "acme/platform#67", "CLAUDE.md", iss.URL} {
				if !strings.Contains(out, s) {
					t.Errorf("warning does not say %q:\n%s", s, out)
				}
			}
		})
	}
}

// TestWarnPreflightReturnsTheStatus: spawn compares the issue's author with
// the account warnPreflight already read, rather than asking gh again.
func TestWarnPreflightReturnsTheStatus(t *testing.T) {
	withFakes(t, "")
	var buf bytes.Buffer
	st := warnPreflight(&buf, "spawn")
	if st == nil || st.Account == "" {
		t.Fatalf("warnPreflight returned %+v, want the status it read", st)
	}
}

// strangerGH returns an open issue someone else filed.
type strangerGH struct{ fakeGH }

func (strangerGH) ViewIssue(_ string, n int) (*ghx.Issue, error) {
	return &ghx.Issue{Number: n, State: "OPEN", Title: "t", Author: ghx.User{Login: "mallory"},
		URL: "https://github.com/acme/gateway/issues/1"}, nil
}

// TestSpawnWarnsAboutAStrangersIssueInThePlan drives runSpawn itself, so the
// warning is proven to be wired in, and shown even on --dry-run, before
// anything is created.
func TestSpawnWarnsAboutAStrangersIssueInThePlan(t *testing.T) {
	withSpawnableRouting(t)
	prevGH := gh
	gh = &strangerGH{fakeGH{auth: &ghx.AuthStatus{
		Host: "github.com", State: ghx.StateConfirmed, Account: "alice", Active: true,
		Scopes: []string{"read:org", "repo", "workflow"}, GitProtocol: "ssh",
	}}}
	t.Cleanup(func() { gh = prevGH })

	out := captureStdout(t, func() {
		if err := runSpawn(spawnOpts{Repo: "acme/gateway", Number: 1, DryRun: true}); err != nil {
			t.Errorf("dry run failed: %v", err)
		}
	})
	if !strings.Contains(out, "written by @mallory, not by you (@alice)") {
		t.Errorf("the plan does not warn about the issue's author:\n%s", out)
	}
}
