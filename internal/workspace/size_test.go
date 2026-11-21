package workspace

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

const mib = 1 << 20

func writeMiB(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	// Not zeros: a filesystem may store a zero-filled file sparsely, and the
	// point is to measure blocks that deleting would really free.
	if err := os.WriteFile(path, bytes.Repeat([]byte("facet\n"), mib/6+1), 0o644); err != nil {
		t.Fatal(err)
	}
}

// freedBytes is what deleting the tree gives back, so a file is counted by its
// disk blocks and only when every name it has is inside the tree. A clone's
// objects hardlinked from the mirror stay on disk after a reap, and counting
// them made a workspace look as big as a full copy.
func TestFreedBytesCountsOnlyWhatDeletingFrees(t *testing.T) {
	base := t.TempDir()
	ws := filepath.Join(base, "ws")
	writeMiB(t, filepath.Join(ws, "own", "data"))

	own := freedBytes(ws)
	if own < mib {
		t.Fatalf("a 1 MiB file inside the workspace counts %d bytes; want at least %d", own, mib)
	}

	// A second name for the same file, also inside: still one file on disk.
	if err := os.Link(filepath.Join(ws, "own", "data"), filepath.Join(ws, "own", "again")); err != nil {
		t.Fatal(err)
	}
	if got := freedBytes(ws); got != own {
		t.Errorf("two names inside the workspace for one file: %d bytes, want %d (counted once)", got, own)
	}

	// A file whose other name lives outside, like an object shared with the
	// mirror: deleting the workspace frees nothing of it.
	writeMiB(t, filepath.Join(base, "mirror", "pack"))
	if err := os.MkdirAll(filepath.Join(ws, "clone"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(base, "mirror", "pack"), filepath.Join(ws, "clone", "pack")); err != nil {
		t.Fatal(err)
	}
	// A symlink to something big outside: reap removes the link, never the target.
	if err := os.Symlink(filepath.Join(base, "mirror", "pack"), filepath.Join(ws, "link")); err != nil {
		t.Fatal(err)
	}
	if got := freedBytes(ws); got-own >= mib/2 {
		t.Errorf("a hardlink shared with outside and a symlink added %d bytes; deleting frees neither", got-own)
	}
}

func TestFreedBytesOfAMissingDirectoryIsZero(t *testing.T) {
	if got := freedBytes(filepath.Join(t.TempDir(), "gone")); got != 0 {
		t.Errorf("freedBytes(missing) = %d, want 0", got)
	}
}
