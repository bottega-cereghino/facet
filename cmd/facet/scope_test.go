package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RiccardoCereghino/facet/internal/config"
	"github.com/RiccardoCereghino/facet/internal/manifest"
	"github.com/RiccardoCereghino/facet/internal/scope"
)

// workspaceWithRepo builds a workspace holding one repository subdirectory, and
// returns both paths. It is the shape that matters here: the work is done in the
// subdirectory, so every scope command has to find the workspace from there.
func workspaceWithRepo(t *testing.T) (ws, repoDir string) {
	t.Helper()
	ws = t.TempDir()
	m := &manifest.Manifest{Name: "example", Clones: map[string]string{"repo": "git@example.com:owner/repo.git"}}
	if err := m.Write(ws); err != nil {
		t.Fatal(err)
	}
	repoDir = filepath.Join(ws, "repo")
	if err := os.MkdirAll(repoDir, 0o777); err != nil {
		t.Fatal(err)
	}
	return ws, repoDir
}

func TestScopeListReportsTheScope(t *testing.T) {
	ws, _ := workspaceWithRepo(t)
	if err := scope.Write(ws, []scope.Ref{{Repo: "owner/repo", Number: 12}, {Repo: "acme/tools", Number: 7}}); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := runScopeList(&buf, ws); err != nil {
		t.Fatalf("runScopeList: %v", err)
	}
	for _, want := range []string{"owner/repo#12", "acme/tools#7"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("scope list output does not mention %q:\n%s", want, buf.String())
		}
	}
}

// TestScopeRemoveThenListReflectsIt is the cobra-adjacent round trip: add two,
// remove one via scope.RemoveScope (what newScopeRemoveCmd's RunE calls), and
// confirm scope list -- the documented way to check a workspace's scope --
// shows exactly what remains.
func TestScopeRemoveThenListReflectsIt(t *testing.T) {
	ws, _ := workspaceWithRepo(t)
	if err := scope.Write(ws, []scope.Ref{
		{Repo: "owner/repo", Number: 12}, {Repo: "acme/tools", Number: 7},
	}); err != nil {
		t.Fatal(err)
	}

	removed, _, err := scope.RemoveScope(ws, []scope.Ref{{Repo: "acme/tools", Number: 7}})
	if err != nil {
		t.Fatalf("RemoveScope: %v", err)
	}
	if len(removed) != 1 {
		t.Fatalf("removed = %v, want 1 entry", removed)
	}

	var buf bytes.Buffer
	if err := runScopeList(&buf, ws); err != nil {
		t.Fatalf("runScopeList: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "owner/repo#12") {
		t.Errorf("surviving entry missing from scope list:\n%s", out)
	}
	if strings.Contains(out, "acme/tools#7") {
		t.Errorf("removed entry still shows in scope list:\n%s", out)
	}
}

// A workspace with no scope recorded reads as "none recorded", not as a blank
// field and not as an error. Absent means there is nothing to check, and that is
// a state a real workspace is in -- one that covers no single issue.
func TestScopeListSaysSoWhenNothingIsRecorded(t *testing.T) {
	ws, _ := workspaceWithRepo(t)

	var buf bytes.Buffer
	if err := runScopeList(&buf, ws); err != nil {
		t.Fatalf("runScopeList: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "none recorded in "+scope.ScopeFile) {
		t.Errorf("an absent scope is not reported as none recorded:\n%s", out)
	}
}

// The work happens inside a clone, so the scope commands find the workspace by
// walking up from there, to the nearest .workspace.json.
func TestScopeFindsTheWorkspaceFromInsideAClone(t *testing.T) {
	ws, repoDir := workspaceWithRepo(t)
	if err := scope.Write(ws, []scope.Ref{{Repo: "owner/repo", Number: 12}}); err != nil {
		t.Fatal(err)
	}
	got, err := config.FindWorkspace(repoDir)
	if err != nil {
		t.Fatalf("FindWorkspace: %v", err)
	}
	if got != ws {
		t.Errorf("FindWorkspace(%s) = %s, want %s", repoDir, got, ws)
	}
}
