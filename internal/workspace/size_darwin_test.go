package workspace

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// A file cloned from one outside the workspace shares every extent with it, so
// deleting the workspace frees none of it. st_blocks counts it in full: a pnpm
// node_modules, cloned from the store, read as hundreds of megabytes that
// deleting it would never give back.
func TestFreedBytesLeavesOutExtentsAnAPFSCloneShares(t *testing.T) {
	base := t.TempDir()
	ws := filepath.Join(base, "ws")
	writeMiB(t, filepath.Join(base, "store", "pkg"))
	if err := os.MkdirAll(filepath.Join(ws, "node_modules"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := unix.Clonefile(filepath.Join(base, "store", "pkg"), filepath.Join(ws, "node_modules", "pkg"), 0); err != nil {
		// The temp dir is always APFS on a Mac; a clone that fails there is a
		// failure of the premise, not a reason to pass quietly.
		t.Fatalf("clonefile: %v", err)
	}
	if got := freedBytes(ws); got >= mib/2 {
		t.Errorf("a 1 MiB clone of a file outside counts %d bytes; deleting it frees almost none", got)
	}

	// A file of its own still counts in full.
	writeMiB(t, filepath.Join(ws, "own"))
	if got := freedBytes(ws); got < mib {
		t.Errorf("with a 1 MiB file of its own the workspace counts %d bytes; want at least %d", got, mib)
	}
}
