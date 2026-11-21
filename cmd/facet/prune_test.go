package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bottega-cereghino/facet/internal/gitx"
	"github.com/bottega-cereghino/facet/internal/manifest"
)

// pruneWorkspace makes a workspace whose one clone ignores its node_modules,
// and returns the workspace and that node_modules.
func pruneWorkspace(t *testing.T, root, name string) (ws, deps string) {
	t.Helper()
	ws = filepath.Join(root, name)
	clone := filepath.Join(ws, "repo")
	deps = filepath.Join(clone, "node_modules")
	if err := os.MkdirAll(filepath.Join(deps, "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := (&manifest.Manifest{Name: name, Clones: map[string]string{"repo": "git@github.com:acme/repo.git"}}).Write(ws); err != nil {
		t.Fatal(err)
	}
	if _, err := (gitx.Git{}).Run(clone, nil, "init", "-q"); err != nil {
		t.Fatal(err)
	}
	for path, body := range map[string]string{
		filepath.Join(clone, ".gitignore"):            "node_modules/\n",
		filepath.Join(deps, "pkg", "index.js"):        strings.Repeat("facet\n", 2<<20/6),
		filepath.Join(clone, "only-copy-of-notes.md"): "untracked, not ignored\n",
	} {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return ws, deps
}

// busyLive reports a session live for one workspace name.
type busyLive struct{ name string }

func (b busyLive) Live(session string) bool { return session == b.name }

func TestPruneListsWithoutApplyAndDeletesOnlyWhatNothingHolds(t *testing.T) {
	root := t.TempDir()
	idleWS, idleDeps := pruneWorkspace(t, root, "idle")
	busyWS, busyDeps := pruneWorkspace(t, root, "busy")
	dirs := []string{busyWS, idleWS}
	live := busyLive{name: "busy"}
	later := time.Now().Add(60 * 24 * time.Hour)

	var out bytes.Buffer
	if err := runPrune(&out, dirs, gitx.Git{}, live, later, 14*24*time.Hour, false); err != nil {
		t.Fatal(err)
	}
	if !exists(t, idleDeps) || !exists(t, busyDeps) {
		t.Fatal("prune without --apply deleted something")
	}
	for _, want := range []string{"idle", "prunable", "busy", "held (1)", "repo/node_modules", "--apply"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("listing lacks %q:\n%s", want, out.String())
		}
	}

	out.Reset()
	if err := runPrune(&out, dirs, gitx.Git{}, live, later, 14*24*time.Hour, true); err != nil {
		t.Fatal(err)
	}
	if exists(t, idleDeps) {
		t.Error("the idle workspace's node_modules survived --apply")
	}
	if !exists(t, busyDeps) {
		t.Error("--apply deleted from a workspace a session runs in")
	}
	if !exists(t, filepath.Join(idleWS, "repo", "only-copy-of-notes.md")) {
		t.Error("--apply deleted an untracked file that git does not ignore")
	}
}

// Used today, a workspace keeps its dependencies under the default window.
func TestPruneHoldsAWorkspaceUsedToday(t *testing.T) {
	ws, deps := pruneWorkspace(t, t.TempDir(), "today")
	var out bytes.Buffer
	if err := runPrune(&out, []string{ws}, gitx.Git{}, nil, time.Now(), 14*24*time.Hour, true); err != nil {
		t.Fatal(err)
	}
	if !exists(t, deps) {
		t.Fatalf("a workspace used today lost its node_modules:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "idle window") {
		t.Errorf("output does not say what holds it:\n%s", out.String())
	}
}
