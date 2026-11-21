package workspace

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bottega-cereghino/facet/internal/gitx"
	"golang.org/x/sys/unix"
)

// A node_modules that pnpm cloned from its store frees nothing when deleted,
// and deleting it would only cost a reinstall, so prune does not name it.
func TestInspectPruneLeavesOutADirectoryClonedFromAStore(t *testing.T) {
	ws, clone := prunableWorkspace(t)
	store := filepath.Join(t.TempDir(), "store")
	writeMiB(t, filepath.Join(store, "pkg"))
	deps := filepath.Join(clone, "node_modules")
	if err := os.RemoveAll(deps); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(deps, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := unix.Clonefile(filepath.Join(store, "pkg"), filepath.Join(deps, "pkg"), 0); err != nil {
		t.Fatalf("clonefile: %v", err)
	}
	st, err := InspectPrune(ws, gitx.Git{}, nil, time.Now(), 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range st.Found {
		if r.Rel == "node_modules" {
			t.Fatalf("named a node_modules that frees %d bytes", r.Bytes)
		}
	}
}
