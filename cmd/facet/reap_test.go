package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bottega-cereghino/facet/internal/config"
)

// facet#83's invocation trap: `facet reap <name>` answered
//
//	facet: unknown command "iss-73" for "facet reap"
//
// because reap took --path only and cobra.NoArgs rejected the positional. The
// error names the wrong problem -- it reads as "reap is broken" rather than
// "reap wants a flag" -- and the trap was documented in prose instead of fixed,
// which is where a CLI puts what it has decided to live with.
func TestReapResolvesAPositionalWorkspaceName(t *testing.T) {
	root := t.TempDir()
	ws := filepath.Join(root, "iss-repo-1-x")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	saved := roots
	defer func() { roots = saved }()
	roots = config.Roots{Workspaces: root}

	got, err := reapTarget("", []string{"iss-repo-1-x"})
	if err != nil {
		t.Fatalf("a bare workspace name must resolve under the workspaces root: %v", err)
	}
	if got != ws {
		t.Errorf("reapTarget = %q, want %q", got, ws)
	}
}

// A positional that is a real path still means that path, wherever it is --
// the name fallback is a convenience, not a redirection.
func TestReapResolvesAPositionalPath(t *testing.T) {
	root := t.TempDir()
	elsewhere := filepath.Join(root, "elsewhere")
	if err := os.MkdirAll(elsewhere, 0o755); err != nil {
		t.Fatal(err)
	}
	saved := roots
	defer func() { roots = saved }()
	roots = config.Roots{Workspaces: filepath.Join(root, "workspaces")}

	got, err := reapTarget("", []string{elsewhere})
	if err != nil {
		t.Fatalf("a positional path must resolve to itself: %v", err)
	}
	if got != elsewhere {
		t.Errorf("reapTarget = %q, want %q", got, elsewhere)
	}
}

// --path and a positional naming different directories must not have a silent
// winner. reap deletes what it resolves; a precedence rule between two arguments
// that disagree is how the wrong workspace goes.
func TestReapRefusesBothPathAndPositional(t *testing.T) {
	root := t.TempDir()
	for _, n := range []string{"a", "b"} {
		if err := os.MkdirAll(filepath.Join(root, n), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	saved := roots
	defer func() { roots = saved }()
	roots = config.Roots{Workspaces: root}

	_, err := reapTarget(filepath.Join(root, "a"), []string{"b"})
	if err == nil {
		t.Fatal("--path and a positional naming different workspaces must refuse, not pick one")
	}
	if !strings.Contains(err.Error(), "name the workspace once") {
		t.Errorf("the refusal must say which two things disagree, got %v", err)
	}
}

// The unchanged default: no positional and no flag means the working directory,
// which is how reap is run from inside the workspace it deletes.
func TestReapWithNoArgumentsUsesTheWorkingDirectory(t *testing.T) {
	saved := roots
	defer func() { roots = saved }()
	roots = config.Roots{Workspaces: t.TempDir()}

	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	got, err := reapTarget("", nil)
	if err != nil {
		t.Fatalf("no arguments must resolve the working directory: %v", err)
	}
	if got != wd {
		t.Errorf("reapTarget = %q, want the working directory %q", got, wd)
	}
}

// runReap runs `facet reap args...` with reapConfirm answering through answer.
func runReap(t *testing.T, answer func(string) bool, args ...string) error {
	t.Helper()
	saved := reapConfirm
	t.Cleanup(func() { reapConfirm = saved })
	reapConfirm = answer
	cmd := newReapCmd()
	cmd.SetArgs(args)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	return cmd.Execute()
}

// Work that starts while reap's prompt waits holds the workspace: reap checks
// again after the answer, as --reapable does (facet#266).
func TestReapChecksAgainAfterThePrompt(t *testing.T) {
	_, free, _ := reapableRoot(t)
	startWork := func(string) bool {
		if err := os.WriteFile(filepath.Join(free, "draft.md"), []byte("new work\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return true
	}
	if err := runReap(t, startWork, filepath.Base(free)); err == nil {
		t.Error("reap returned no error although work started during the prompt")
	}
	if !exists(t, filepath.Join(free, "draft.md")) {
		t.Fatal("reap deleted work that started while its prompt waited")
	}

	// Nothing new: the answer is acted on.
	if err := os.Remove(filepath.Join(free, "draft.md")); err != nil {
		t.Fatal(err)
	}
	if err := runReap(t, func(string) bool { return true }, filepath.Base(free)); err != nil {
		t.Fatal(err)
	}
	if exists(t, free) {
		t.Errorf("%s holds nothing and should be gone", free)
	}
}

// --force deletes despite the holders it showed, never despite new ones.
func TestReapForceRefusesWhatStartedDuringThePrompt(t *testing.T) {
	_, _, held := reapableRoot(t)
	startWork := func(string) bool {
		if err := os.WriteFile(filepath.Join(held, "draft.md"), []byte("new work\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return true
	}
	if err := runReap(t, startWork, filepath.Base(held), "--force"); err == nil {
		t.Error("reap --force returned no error although work started during the prompt")
	}
	if !exists(t, filepath.Join(held, "draft.md")) {
		t.Fatal("reap --force deleted work that started while its prompt waited")
	}
}

// reap pointed at a folder anywhere inside a workspace (run from a clone's
// src/, say) refuses and names the workspace, instead of reaping the folder
// alone (facet#274).
func TestReapRefusesAFolderInsideAWorkspace(t *testing.T) {
	_, free, _ := reapableRoot(t)
	src := filepath.Join(free, "repo", "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := inspectTarget(src, git, nil)
	if err == nil || !strings.Contains(err.Error(), "inside the workspace "+free) || !strings.Contains(err.Error(), "fix: facet reap "+free) {
		t.Fatalf("reap %s = %v; want a refusal naming the workspace", src, err)
	}
}
