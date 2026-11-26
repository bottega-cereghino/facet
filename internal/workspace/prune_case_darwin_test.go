package workspace

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/bottega-cereghino/facet/internal/gitx"
	"github.com/bottega-cereghino/facet/internal/manifest"
)

// On a case-insensitive filesystem a link `docs` on disk is the declared
// `Docs`: --prune deletes only links absent from the manifest.
func TestSyncPruneKeepsADeclaredLinkOfAnotherCase(t *testing.T) {
	t.Parallel()
	roots, ws, _ := setup(t)
	if err := os.MkdirAll(filepath.Join(roots.Projects, "proj"), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(roots.Projects, "proj"), filepath.Join(ws, "docs")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(ws, "Docs")); err != nil {
		t.Skip("filesystem is case-sensitive")
	}
	m := &manifest.Manifest{Name: "w", Links: map[string]string{"Docs": "proj"}}
	if err := m.Write(ws); err != nil {
		t.Fatal(err)
	}
	if err := Sync(roots, ws, gitx.Git{}, quiet(), SyncOptions{Prune: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(ws, "Docs")); err != nil {
		t.Errorf("prune deleted a declared link: %v", err)
	}
}
