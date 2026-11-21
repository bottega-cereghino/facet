package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bottega-cereghino/facet/internal/config"
	"github.com/bottega-cereghino/facet/internal/gitx"
	"github.com/bottega-cereghino/facet/internal/manifest"
)

// TestLsAtWorkspacesRootListsEveryWorkspace is the regression test for #31:
// `facet ls` run against a directory with no manifest of its own, but which
// contains manifest-bearing subdirectories, must list every one of them
// instead of erroring with "no .workspace.json in <dir>".
func TestLsAtWorkspacesRootListsEveryWorkspace(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"alpha", "beta"} {
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		m := &manifest.Manifest{Name: name}
		if err := m.Write(dir); err != nil {
			t.Fatal(err)
		}
	}

	savedRoots, savedGit := roots, git
	defer func() { roots, git = savedRoots, savedGit }()
	roots = config.Roots{Workspaces: root}
	git = gitx.Git{}

	cmd := newLsCmd()
	cmd.SetArgs([]string{"--path", root})

	out := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("ls at workspaces root: %v", err)
		}
	})

	if !strings.Contains(out, "alpha") || !strings.Contains(out, "beta") {
		t.Errorf("expected ls to list both workspaces, got:\n%s", out)
	}
}

// TestLsWithNoManifestAnywhereStillErrors makes sure the workspaces-root
// fallback doesn't swallow the genuinely-wrong-directory case: a directory
// with no manifest and no manifest-bearing subdirectories must still get the
// original, helpful error.
func TestLsWithNoManifestAnywhereStillErrors(t *testing.T) {
	root := t.TempDir()

	savedRoots, savedGit := roots, git
	defer func() { roots, git = savedRoots, savedGit }()
	roots = config.Roots{Workspaces: root}
	git = gitx.Git{}

	cmd := newLsCmd()
	cmd.SetArgs([]string{"--path", root})
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true

	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "facet new") {
		t.Errorf("want a helpful error pointing at `facet new`, got %v", err)
	}
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = saved }()

	fn()

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

// TestLsRootSizeSaysWhatReapWouldDo holds the --size listing to reap's own
// checks: a workspace with nothing of its own is reapable, and one holding a
// file that exists nowhere else is held, exactly as `facet reap` would say.
func TestLsRootSizeSaysWhatReapWouldDo(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"empty", "notes"} {
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := (&manifest.Manifest{Name: name}).Write(dir); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "notes", "plan.md"), []byte("the only copy\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	savedRoots, savedGit := roots, git
	defer func() { roots, git = savedRoots, savedGit }()
	roots = config.Roots{Workspaces: root}
	git = gitx.Git{}

	var out bytes.Buffer
	dirs := []string{filepath.Join(root, "empty"), filepath.Join(root, "notes")}
	if err := lsRoot(&out, root, dirs, true); err != nil {
		t.Fatal(err)
	}
	rows := map[string]string{}
	for _, line := range strings.Split(out.String(), "\n") {
		if f := strings.Fields(line); len(f) > 0 {
			rows[f[0]] = line
		}
	}
	if r := rows["empty"]; !strings.HasSuffix(strings.TrimSpace(r), "reapable") {
		t.Errorf("a workspace holding nothing of its own should be reapable; row %q in:\n%s", r, out.String())
	}
	if r := rows["notes"]; !strings.Contains(r, "held (1)") {
		t.Errorf("a workspace holding the only copy of a file should be held; row %q in:\n%s", r, out.String())
	}
	if !strings.Contains(out.String(), "1 reapable now") {
		t.Errorf("the summary should count one reapable workspace:\n%s", out.String())
	}
}

// Without --size, ls stays the fast listing it was: no network, no new columns.
func TestLsRootWithoutSizeKeepsItsColumns(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "alpha")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := (&manifest.Manifest{Name: "alpha"}).Write(dir); err != nil {
		t.Fatal(err)
	}
	savedRoots, savedGit := roots, git
	defer func() { roots, git = savedRoots, savedGit }()
	roots = config.Roots{Workspaces: root}
	git = gitx.Git{}

	var out bytes.Buffer
	if err := lsRoot(&out, root, []string{dir}, false); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "REAP") || strings.Contains(out.String(), "FREES") {
		t.Errorf("plain ls grew the --size columns:\n%s", out.String())
	}
}
