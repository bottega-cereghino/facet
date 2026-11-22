package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bottega-cereghino/facet/internal/manifest"
)

// worktreeRoots points facet at a workspaces root and a projects root holding
// one checkout, and returns a plain worktree of that checkout on a branch of
// its own, outside both roots.
func worktreeRoots(t *testing.T) (checkout, wt string) {
	t.Helper()
	reapableRoot(t)
	checkout, _ = branchSetup(t)
	roots.Projects = filepath.Dir(checkout)
	tmp, err := filepath.EvalSymlinks(t.TempDir()) // the real path, as reap names it
	if err != nil {
		t.Fatal(err)
	}
	wt = filepath.Join(tmp, "agent-wt")
	gitIn(t, checkout, "worktree", "add", "-q", "-b", "agent", wt, "origin/main")
	return checkout, wt
}

func TestReapReapableReapsAPlainWorktree(t *testing.T) {
	checkout, wt := worktreeRoots(t)
	var out bytes.Buffer
	if err := reapReapable(&out, []string{wt}, false, false, func(string) bool { return true }); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if exists(t, wt) {
		t.Fatalf("the worktree is still there:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "(worktree)") {
		t.Errorf("the list does not say it is a worktree:\n%s", out.String())
	}
	gitIn(t, checkout, "rev-parse", "--verify", "refs/heads/agent")
}

func TestReapReapableLeavesAWorktreeWithWorkAlone(t *testing.T) {
	_, wt := worktreeRoots(t)
	if err := os.WriteFile(filepath.Join(wt, "notes.md"), []byte("only here\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := reapReapable(&out, []string{wt}, false, false, func(string) bool { return true }); err != nil {
		t.Fatal(err)
	}
	if !exists(t, filepath.Join(wt, "notes.md")) {
		t.Fatalf("a worktree with an untracked file was reaped:\n%s", out.String())
	}
}

// A worktree that is a workspace's clone goes with its workspace, never alone.
func TestReapRefusesAWorkspacesClone(t *testing.T) {
	_, wt := worktreeRoots(t)
	ws := filepath.Dir(wt)
	if err := (&manifest.Manifest{Name: "w", Clones: map[string]string{"agent-wt": "o/r"}}).Write(ws); err != nil {
		t.Fatal(err)
	}
	_, err := inspectTarget(wt, git, nil)
	if err == nil || !strings.Contains(err.Error(), "fix: facet reap "+ws) {
		t.Fatalf("inspectTarget(clone) = %v; want a refusal naming the workspace", err)
	}
}

// A clone entry that is a symlink to a worktree elsewhere is a link, not a
// clone: even --force unlinks it and leaves the worktree it points at, ignored
// files included (#323).
func TestReapForceUnlinksASymlinkedCloneAndKeepsItsTarget(t *testing.T) {
	root, _, _ := reapableRoot(t)
	checkout, wt := branchSetup(t)
	putFile(t, filepath.Join(checkout, ".git", "info", "exclude"), ".env\n")
	env := filepath.Join(wt, ".env")
	putFile(t, env, "API_KEY=only-copy\n")

	ws := filepath.Join(root, "facet-test-link")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	origin := gitIn(t, checkout, "remote", "get-url", "origin")
	if err := (&manifest.Manifest{Name: "link", Clones: map[string]string{"app": origin}}).Write(ws); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(wt, filepath.Join(ws, "app")); err != nil {
		t.Fatal(err)
	}
	if err := runReap(t, nil, filepath.Base(ws), "--yes", "--force"); err != nil {
		t.Fatal(err)
	}
	if !exists(t, env) {
		t.Fatalf("reap --force of %s deleted %s, a worktree outside it reached through a symlink", ws, wt)
	}
	if exists(t, ws) {
		t.Errorf("the workspace %s is still there", ws)
	}
	gitIn(t, wt, "status", "--porcelain")
}

// A workspace's clone named through a symlink outside the workspace is still
// inside it, so reap refuses it and names the workspace (#384).
func TestReapRefusesAWorkspacesCloneNamedThroughASymlink(t *testing.T) {
	_, wt := worktreeRoots(t)
	ws := filepath.Dir(wt)
	if err := (&manifest.Manifest{Name: "w", Clones: map[string]string{"agent-wt": "o/r"}}).Write(ws); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "shortcut")
	if err := os.Symlink(wt, alias); err != nil {
		t.Fatal(err)
	}
	resolvedWS, err := filepath.EvalSymlinks(ws)
	if err != nil {
		t.Fatal(err)
	}
	_, err = inspectTarget(alias, git, nil)
	if err == nil || !strings.Contains(err.Error(), "fix: facet reap "+resolvedWS) {
		t.Fatalf("inspectTarget(symlink to a clone) = %v; want the refusal naming %s", err, resolvedWS)
	}
}
