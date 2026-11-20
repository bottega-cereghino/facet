package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RiccardoCereghino/facet/internal/config"
	"github.com/RiccardoCereghino/facet/internal/gitx"
	"github.com/RiccardoCereghino/facet/internal/manifest"
)

// reapableRoot makes a workspaces root with two long-lived workspaces: one that
// holds nothing of its own, and one holding the only copy of a file. It points
// facet at that root and tmux at a server of its own, because reap kills the
// session named after each workspace it deletes.
func reapableRoot(t *testing.T) (root, free, held string) {
	t.Helper()
	t.Setenv("TMUX_TMPDIR", t.TempDir())
	t.Setenv("TMUX", "")
	root = t.TempDir()
	free = filepath.Join(root, "facet-test-reapable-free")
	held = filepath.Join(root, "facet-test-reapable-held")
	for _, dir := range []string{free, held} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := (&manifest.Manifest{Name: filepath.Base(dir)}).Write(dir); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(held, "plan.md"), []byte("the only copy\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	savedRoots, savedGit := roots, git
	t.Cleanup(func() { roots, git = savedRoots, savedGit })
	roots = config.Roots{Workspaces: root}
	git = gitx.Git{}
	return root, free, held
}

func exists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Stat(path)
	return err == nil
}

func TestReapReapableDeletesOnlyWhatNothingHolds(t *testing.T) {
	_, free, held := reapableRoot(t)
	var out bytes.Buffer
	var asked string
	err := reapReapable(&out, []string{free, held}, false, func(q string) bool { asked = q; return true })
	if err != nil {
		t.Fatalf("reapReapable: %v\n%s", err, out.String())
	}
	if asked != "Delete this workspace?" {
		t.Errorf("asked %q, want one question naming the count", asked)
	}
	if exists(t, free) {
		t.Errorf("%s holds nothing and should be gone:\n%s", free, out.String())
	}
	if !exists(t, filepath.Join(held, "plan.md")) {
		t.Errorf("%s holds the only copy of a file and must be left alone:\n%s", held, out.String())
	}
	for _, want := range []string{"1 reapable", "1 held left alone", "removed " + free, "shared mirror was not touched"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}

func TestReapReapableDeletesNothingWhenTheAnswerIsNo(t *testing.T) {
	_, free, held := reapableRoot(t)
	var out bytes.Buffer
	if err := reapReapable(&out, []string{free, held}, false, func(string) bool { return false }); err != nil {
		t.Fatal(err)
	}
	if !exists(t, free) || !exists(t, held) {
		t.Errorf("answering no deleted something:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "aborted.") {
		t.Errorf("output should say it aborted:\n%s", out.String())
	}
}

// Work can start in a workspace while the prompt waits. The check before each
// deletion must see it.
func TestReapReapableChecksAgainBeforeDeleting(t *testing.T) {
	_, free, held := reapableRoot(t)
	var out bytes.Buffer
	startWork := func(string) bool {
		if err := os.WriteFile(filepath.Join(free, "draft.md"), []byte("new work\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return true
	}
	if err := reapReapable(&out, []string{free, held}, false, startWork); err != nil {
		t.Fatal(err)
	}
	if !exists(t, filepath.Join(free, "draft.md")) {
		t.Fatalf("a workspace that became held during the prompt was deleted:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "skipped facet-test-reapable-free") {
		t.Errorf("output should say it skipped the workspace:\n%s", out.String())
	}
}

func TestReapReapableSaysSoWhenNothingIsReapable(t *testing.T) {
	_, _, held := reapableRoot(t)
	var out bytes.Buffer
	ask := func(string) bool { t.Fatal("asked although there is nothing to delete"); return false }
	if err := reapReapable(&out, []string{held}, false, ask); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "nothing to reap: 1 held") {
		t.Errorf("output:\n%s", out.String())
	}
}

func TestReapReapableRefusesForceAndAWorkspace(t *testing.T) {
	_, free, _ := reapableRoot(t)
	for _, args := range [][]string{
		{"--reapable", "--force"},
		{"--reapable", "facet-test-reapable-free"},
		{"--reapable", "--path", free},
	} {
		cmd := newReapCmd()
		cmd.SetArgs(args)
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		if err := cmd.Execute(); err == nil {
			t.Errorf("reap %v: no error", args)
		}
		if !exists(t, free) {
			t.Fatalf("reap %v deleted %s", args, free)
		}
	}
}
