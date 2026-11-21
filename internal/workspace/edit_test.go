package workspace

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/bottega-cereghino/facet/internal/gitx"
	"github.com/bottega-cereghino/facet/internal/manifest"
)

func TestNewAddRemove(t *testing.T) {
	roots, _, origin := setup(t)
	rep := quiet()

	ws, err := New(roots, gitx.Git{}, rep, NewOptions{Name: "demo", Description: "d"}, SyncOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := AddClone(roots, ws, gitx.Git{}, rep, "repo", origin, nil, nil, false, SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(ws, "repo", "README")); err != nil {
		t.Fatalf("clone not created: %v", err)
	}

	// A clone's checkout survives removal from the manifest: it may hold the only
	// copy of unpushed work.
	res, err := Remove(ws, "repo")
	if err != nil {
		t.Fatal(err)
	}
	if !res.WasClone || res.CheckoutLeft == "" {
		t.Errorf("Remove(clone) = %+v; the checkout must be reported as left behind", res)
	}
	if _, err := os.Stat(filepath.Join(ws, "repo", "README")); err != nil {
		t.Error("!!! Remove deleted a clone's checkout")
	}
	m, err := manifest.Read(ws)
	if err != nil {
		t.Fatal(err)
	}
	if _, still := m.Clones["repo"]; still {
		t.Error("manifest entry survived")
	}
}

func TestNewRefusesExisting(t *testing.T) {
	roots, _, _ := setup(t)
	if _, err := New(roots, gitx.Git{}, quiet(), NewOptions{Name: "demo"}, SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := New(roots, gitx.Git{}, quiet(), NewOptions{Name: "demo"}, SyncOptions{}); err == nil {
		t.Error("New overwrote an existing workspace")
	}
}

func TestAddCloneRefusesNameUsedByALink(t *testing.T) {
	roots, ws, origin := setup(t)
	m := &manifest.Manifest{Name: "w", Links: map[string]string{"x": "proj"}}
	if err := m.Write(ws); err != nil {
		t.Fatal(err)
	}
	err := AddClone(roots, ws, gitx.Git{}, quiet(), "x", origin, nil, nil, false, SyncOptions{})
	if err == nil {
		t.Error("AddClone accepted a name already used by a link")
	}
}

func TestRemoveUnknownEntry(t *testing.T) {
	_, ws, _ := setup(t)
	m := &manifest.Manifest{Name: "w"}
	if err := m.Write(ws); err != nil {
		t.Fatal(err)
	}
	if _, err := Remove(ws, "nope"); err == nil {
		t.Error("Remove accepted an unknown entry")
	}
}

// `facet rm app` leaves the checkout on disk; `facet add link app ...` must not
// then declare a link over it, or the checkout, unpushed work and all, is
// judged by nothing. The refusal comes before the manifest is written.
func TestAddLinkRefusesARealDirectoryAtItsName(t *testing.T) {
	roots, ws, origin := setup(t)
	if err := os.MkdirAll(filepath.Join(roots.Projects, "app"), 0o777); err != nil {
		t.Fatal(err)
	}
	m := &manifest.Manifest{Name: "w", Clones: map[string]string{"app": origin}}
	if err := m.Write(ws); err != nil {
		t.Fatal(err)
	}
	if err := Sync(roots, ws, gitx.Git{}, quiet(), SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := Remove(ws, "app"); err != nil {
		t.Fatal(err)
	}
	if err := AddLink(roots, ws, gitx.Git{}, quiet(), "app", "app", "", false, SyncOptions{}); err == nil {
		t.Fatal("AddLink accepted a name a real directory holds")
	}
	m, err := manifest.Read(ws)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Links["app"]; ok {
		t.Error("the refused link was written to the manifest")
	}
}

// A symlink at a name is a link; AddClone must not declare a clone there.
func TestAddCloneRefusesASymlinkAtItsName(t *testing.T) {
	roots, ws, origin := setup(t)
	m := &manifest.Manifest{Name: "w"}
	if err := m.Write(ws); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(ws, "x")); err != nil {
		t.Fatal(err)
	}
	if err := AddClone(roots, ws, gitx.Git{}, quiet(), "x", origin, nil, nil, false, SyncOptions{}); err == nil {
		t.Fatal("AddClone accepted a name a symlink holds")
	}
	m, err := manifest.Read(ws)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Clones["x"]; ok {
		t.Error("the refused clone was written to the manifest")
	}
}

// A hand edit can still declare a link where a real directory sits. Reap
// judges that directory like any file beside the clones, since only a symlink
// is a link.
func TestReapHoldsARealDirectoryAtALinkName(t *testing.T) {
	_, ws, _ := setup(t)
	m := &manifest.Manifest{Name: "w", Links: map[string]string{"app": "app"}}
	if err := m.Write(ws); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(ws, "app"), 0o777); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(ws, "app", "notes.txt"), "only copy\n")
	st, err := Inspect(ws, gitx.Git{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !hasBlocker(st, "app") {
		t.Errorf("reap would delete app/notes.txt, a real directory where a link was declared: %v", st.Blockers())
	}
}
