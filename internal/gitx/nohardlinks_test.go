package gitx

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// sharedObjectFiles counts the files under dir/.git/objects that another path
// also links to.
func sharedObjectFiles(t *testing.T, dir string) int {
	t.Helper()
	n := 0
	err := filepath.WalkDir(filepath.Join(dir, ".git", "objects"), func(_ string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		if st, ok := fi.Sys().(*syscall.Stat_t); ok && st.Nlink > 1 {
			n++
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// A clone from a local path hardlinks its objects unless NoHardlinks is set.
// The first case is the control: it must show shared files, or the second
// proves nothing.
func TestCloneNoHardlinksSharesNoFileWithTheSource(t *testing.T) {
	g := Git{}
	src := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "one"},
	} {
		if _, err := g.Run(src, nil, args...); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}
	for _, tc := range []struct {
		name        string
		noHardlinks bool
		wantShared  bool
	}{
		{"default hardlinks", false, true},
		{"no hardlinks", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dst := filepath.Join(t.TempDir(), "dst")
			if err := Clone(g, src, dst, CloneOptions{NoHardlinks: tc.noHardlinks}); err != nil {
				t.Fatal(err)
			}
			if got := sharedObjectFiles(t, dst) > 0; got != tc.wantShared {
				t.Errorf("shares object files with the source = %v, want %v", got, tc.wantShared)
			}
		})
	}
}
