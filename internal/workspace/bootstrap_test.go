package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bottega-cereghino/facet/internal/gitx"
	"github.com/bottega-cereghino/facet/internal/manifest"
)

// A link target is checked lexically, so "victim/escape/new" is a local path
// even when escape, committed in a checkout under the projects root, is a
// symlink to somewhere else. Bootstrap must refuse that clone.
func TestSyncBootstrapRefusesACloneThroughASymlinkOutOfTheProjectsRoot(t *testing.T) {
	t.Parallel()
	roots, ws, origin := setup(t)
	outside := filepath.Join(filepath.Dir(roots.Projects), "outside")
	if err := os.MkdirAll(outside, 0o777); err != nil {
		t.Fatal(err)
	}
	victim := originRepo(t, filepath.Join(roots.Projects, "victim"))
	if err := os.Symlink(outside, filepath.Join(victim, "escape")); err != nil {
		t.Fatal(err)
	}
	g := gitx.Git{}
	if _, err := g.Run(victim, nil, "add", "escape"); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Run(victim, nil, "commit", "-qm", "a symlink"); err != nil {
		t.Fatal(err)
	}

	for _, target := range []string{"victim/escape/new", "victim/escape/a/new"} {
		t.Run(target, func(t *testing.T) {
			m := &manifest.Manifest{
				Name:    "w",
				Links:   map[string]string{"l": target},
				Origins: map[string]string{target: origin},
			}
			if err := m.Write(ws); err != nil {
				t.Fatal(err)
			}
			err := Sync(roots, ws, gitx.Git{}, quiet(), SyncOptions{Bootstrap: true})
			if err == nil || !strings.Contains(err.Error(), "outside the projects root") || !strings.Contains(err.Error(), "fix:") {
				t.Fatalf("bootstrap accepted a clone through a symlink out of the projects root: %v", err)
			}
			if _, statErr := os.Stat(filepath.Join(outside, "new", ".git")); statErr == nil {
				t.Fatalf("bootstrap cloned into %s", filepath.Join(outside, "new"))
			}
			if _, statErr := os.Stat(filepath.Join(outside, "a", "new", ".git")); statErr == nil {
				t.Fatalf("bootstrap cloned into %s", filepath.Join(outside, "a", "new"))
			}
			if _, statErr := os.Lstat(filepath.Join(ws, "l")); statErr == nil {
				t.Fatal("bootstrap still created the workspace link")
			}
		})
	}
}

// A missing folder under the projects root is where bootstrap is supposed to
// write, including one whose parent directory does not exist yet and one
// reached through a symlink that stays inside the root.
func TestSyncBootstrapClonesUnderTheProjectsRoot(t *testing.T) {
	t.Parallel()
	roots, ws, origin := setup(t)
	kept := filepath.Join(roots.Projects, "real")
	if err := os.MkdirAll(kept, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(kept, filepath.Join(roots.Projects, "alias")); err != nil {
		t.Fatal(err)
	}
	m := &manifest.Manifest{
		Name: "w",
		Links: map[string]string{
			"fresh": "group/repo",
			"alias": "alias/nested",
		},
		Origins: map[string]string{
			"group/repo":   origin,
			"alias/nested": origin,
		},
	}
	if err := m.Write(ws); err != nil {
		t.Fatal(err)
	}
	if err := Sync(roots, ws, gitx.Git{}, quiet(), SyncOptions{Bootstrap: true}); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"group/repo/README", "real/nested/README"} {
		if _, err := os.Stat(filepath.Join(roots.Projects, rel)); err != nil {
			t.Errorf("bootstrap did not clone %s: %v", rel, err)
		}
	}
}
