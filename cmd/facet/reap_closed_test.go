package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bottega-cereghino/facet/internal/ghx"
	"github.com/bottega-cereghino/facet/internal/manifest"
)

// closedGH answers the pull-request lookup by branch as prGH does, and the
// lookup by commit from its own map.
type closedGH struct {
	*prGH
	merged    map[string]*ghx.PRForCommit
	mergedErr error
}

func (f *closedGH) MergedPRForSHA(_, sha string) (*ghx.PRForCommit, error) {
	return f.merged[sha], f.mergedErr
}

func newClosedGH() *closedGH {
	return &closedGH{
		prGH:   &prGH{fakeGH: &fakeGH{}, prs: map[string]*ghx.PR{}, errs: map[string]error{}},
		merged: map[string]*ghx.PRForCommit{},
	}
}

// homeWorkspace adds an issue workspace whose home clone "app" is a clone of
// a fresh origin, on main and pushed. It returns the workspace and the clone.
func homeWorkspace(t *testing.T, root, name string) (ws, app string) {
	t.Helper()
	origin := filepath.Join(t.TempDir(), "origin.git")
	gitIn(t, "", "init", "-q", "--bare", "-b", "main", origin)
	seed := filepath.Join(t.TempDir(), "seed")
	gitIn(t, "", "clone", "-q", origin, seed)
	gitIn(t, seed, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "--allow-empty", "-m", "init")
	gitIn(t, seed, "push", "-q", "origin", "HEAD:main")
	ws = filepath.Join(root, name)
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	m := &manifest.Manifest{
		Name:   name,
		Clones: map[string]string{"app": origin},
		Issue:  &manifest.Issue{Repo: "o/r", Number: 7, Branch: name, Home: "app"},
	}
	if err := m.Write(ws); err != nil {
		t.Fatal(err)
	}
	app = filepath.Join(ws, "app")
	gitIn(t, "", "clone", "-q", origin, app)
	return ws, app
}

func headOf(t *testing.T, dir string) string {
	t.Helper()
	out, err := git.Run(dir, nil, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(out)
}

// --closed reaps only issue workspaces whose pull request is merged or
// closed. The rest are left alone and listed with their reason, a dry run
// lists the same and deletes nothing.
func TestReapClosedReapsOnlyMergedOrClosedPullRequests(t *testing.T) {
	root, nonIssue, _ := reapableRoot(t)
	merged, mb := issueWorkspace(t, root, 1)
	closed, cb := issueWorkspace(t, root, 2)
	noPR, _ := issueWorkspace(t, root, 3)
	open, ob := issueWorkspace(t, root, 4)
	failed, fb := issueWorkspace(t, root, 5)
	f := newClosedGH()
	f.prs[mb] = &ghx.PR{Number: 10, State: "MERGED"}
	f.prs[cb] = &ghx.PR{Number: 11, State: "CLOSED"}
	f.prs[ob] = &ghx.PR{Number: 12, State: "OPEN"}
	f.errs[fb] = errors.New("HTTP 502")
	gh = f
	dirs := []string{merged, closed, noPR, open, failed, nonIssue}
	wantList := []string{
		"left out by --closed (4), by reason:",
		"  no pull request (1)\n    " + filepath.Base(noPR) + "\n",
		"  not an issue workspace (1)\n    " + filepath.Base(nonIssue) + "\n",
		"  pull request lookup failed (1)\n    " + filepath.Base(failed) + "\n",
		"  pull request open (1)\n    " + filepath.Base(open) + "\n",
		"2 reapable",
	}

	var dry bytes.Buffer
	ask := func(string) bool { t.Fatal("a dry run asked"); return true }
	if err := reapBatch(&dry, dirs, true, true, closedReason, ask); err != nil {
		t.Fatalf("dry run: %v\n%s", err, dry.String())
	}
	for _, dir := range dirs {
		if !exists(t, dir) {
			t.Fatalf("a dry run deleted %s:\n%s", dir, dry.String())
		}
	}
	for _, want := range append(wantList, "dry run: nothing was deleted.") {
		if !strings.Contains(dry.String(), want) {
			t.Errorf("dry run output lacks %q:\n%s", want, dry.String())
		}
	}

	var out bytes.Buffer
	if err := reapBatch(&out, dirs, true, false, closedReason, nil); err != nil {
		t.Fatalf("reapBatch: %v\n%s", err, out.String())
	}
	for _, dir := range []string{merged, closed} {
		if exists(t, dir) {
			t.Errorf("%s has a merged or closed PR and should be gone:\n%s", dir, out.String())
		}
	}
	for _, dir := range []string{noPR, open, failed, nonIssue} {
		if !exists(t, dir) {
			t.Errorf("--closed deleted %s:\n%s", dir, out.String())
		}
	}
	for _, want := range wantList {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}

// --closed only narrows: a merged pull request does not make unpushed work
// safe to delete.
func TestReapClosedStillHoldsUnpushedCommits(t *testing.T) {
	root, _, _ := reapableRoot(t)
	ws, app := homeWorkspace(t, root, "iss-8-unpushed")
	gitIn(t, app, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "--allow-empty", "-m", "only here")
	f := newClosedGH()
	f.prs["iss-8-unpushed"] = &ghx.PR{Number: 13, State: "MERGED"}
	gh = f
	var out bytes.Buffer
	if err := reapBatch(&out, []string{ws}, true, false, closedReason, nil); err != nil {
		t.Fatalf("reapBatch: %v\n%s", err, out.String())
	}
	if !exists(t, app) {
		t.Fatalf("a merged-PR workspace with unpushed commits was deleted:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "nothing to reap: 1 held") {
		t.Errorf("output should count it as held:\n%s", out.String())
	}
}

// When the branch lookup finds no pull request, the home clone's HEAD is
// asked for the merged pull request that had it, which finds a squash merge
// whose branch is gone. A failure of that lookup holds the workspace.
func TestReapClosedFindsAMergeByTheHomeCommit(t *testing.T) {
	root, _, _ := reapableRoot(t)
	found, foundApp := homeWorkspace(t, root, "iss-9-squashed")
	failed, _ := homeWorkspace(t, root, "iss-10-failed")
	f := newClosedGH()
	f.merged[headOf(t, foundApp)] = &ghx.PRForCommit{Number: 14, MergedAt: "2026-11-01T00:00:00Z"}
	gh = f
	var out bytes.Buffer
	if err := reapBatch(&out, []string{found}, true, false, closedReason, nil); err != nil {
		t.Fatalf("reapBatch: %v\n%s", err, out.String())
	}
	if exists(t, found) {
		t.Errorf("%s has a merged PR for its HEAD and should be gone:\n%s", found, out.String())
	}

	f.mergedErr = errors.New("HTTP 403")
	out.Reset()
	if err := reapBatch(&out, []string{failed}, true, false, closedReason, nil); err != nil {
		t.Fatalf("reapBatch: %v\n%s", err, out.String())
	}
	if !exists(t, failed) {
		t.Fatalf("a failed lookup was read as merged:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "pull request lookup failed (1)") {
		t.Errorf("output lacks the reason:\n%s", out.String())
	}
}

// --closed narrows --reapable, so alone it is refused with the fix.
func TestReapClosedNeedsReapable(t *testing.T) {
	reapableRoot(t)
	cmd := newReapCmd()
	cmd.SetArgs([]string{"--closed"})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "add --reapable") {
		t.Fatalf("reap --closed: %v, want a refusal naming --reapable", err)
	}
}
