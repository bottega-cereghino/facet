package workspace

import (
	"os"
	"path/filepath"
	"testing"
)

// A workspace or clone swapped for a symlink after the listing must not be
// followed.
func TestRemoveInCloneRefusesASwappedCloneOrWorkspace(t *testing.T) {
	for _, swap := range []string{"clone", "workspace"} {
		t.Run(swap, func(t *testing.T) {
			outside := t.TempDir()
			victim := filepath.Join(outside, "c", "node_modules")
			if swap == "clone" {
				victim = filepath.Join(outside, "node_modules")
			}
			if err := os.MkdirAll(victim, 0o777); err != nil {
				t.Fatal(err)
			}
			base := t.TempDir()
			ws := filepath.Join(base, "ws")
			if swap == "workspace" {
				if err := os.Symlink(outside, ws); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.MkdirAll(ws, 0o777); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, filepath.Join(ws, "c")); err != nil {
					t.Fatal(err)
				}
			}
			if err := removeInClone(ws, "c", "node_modules"); err == nil {
				t.Error("a symlinked " + swap + " was followed")
			}
			if _, err := os.Stat(victim); err != nil {
				t.Errorf("the target of the link was deleted: %v", err)
			}
		})
	}
}
