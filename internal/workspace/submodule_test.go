package workspace

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/bottega-cereghino/facet/internal/gitx"
	"github.com/bottega-cereghino/facet/internal/manifest"
)

// bareRemote makes a bare repository with one commit on main, seeded through a
// throwaway clone, and returns its path.
func bareRemote(t *testing.T, dir string) string {
	t.Helper()
	gitOut(t, "", "init", "-q", "--bare", "-b", "main", dir)
	seed := filepath.Join(t.TempDir(), "seed")
	gitOut(t, "", "clone", "-q", dir, seed)
	writeTestFile(t, filepath.Join(seed, "README"), "hello\n")
	gitOut(t, seed, "add", "README")
	gitOut(t, seed, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-qm", "init")
	gitOut(t, seed, "push", "-q", "origin", "HEAD:main")
	return dir
}

// A submodule's commits, branches and stash live in the clone's git directory,
// and go with it. Commit in the submodule, commit the bump in the
// superproject, push only the superproject: the clone reads clean and pushed,
// and the submodule commit is the only copy. Reap judges each initialised
// submodule as it judges a clone (facet#269), in an own clone and in a
// worktree clone alike.
func TestReapHoldsAnUnpushedSubmoduleCommit(t *testing.T) {
	for _, worktree := range []bool{false, true} {
		t.Run(map[bool]string{false: "own clone", true: "worktree clone"}[worktree], func(t *testing.T) {
			roots, ws, _ := setup(t)
			remotes := t.TempDir()
			sub := bareRemote(t, filepath.Join(remotes, "sub.git"))
			super := bareRemote(t, filepath.Join(remotes, "super.git"))
			seed := filepath.Join(t.TempDir(), "seed")
			gitOut(t, "", "clone", "-q", super, seed)
			gitOut(t, seed, "-c", "protocol.file.allow=always", "submodule", "add", "-q", sub, "sub")
			gitOut(t, seed, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-qm", "add sub")
			gitOut(t, seed, "push", "-q", "origin", "HEAD:main")
			if worktree {
				gitOut(t, "", "clone", "-q", super, filepath.Join(roots.Projects, "super"))
			}
			m := &manifest.Manifest{Name: "w", Clones: map[string]string{"super": super}}
			if err := m.Write(ws); err != nil {
				t.Fatal(err)
			}
			if err := Sync(roots, ws, gitx.Git{}, quiet(), SyncOptions{Worktrees: worktree}); err != nil {
				t.Fatal(err)
			}
			clone := filepath.Join(ws, "super")
			if worktree != isGitFile(t, clone) {
				t.Fatalf("clone %s: worktree = %v, want %v", clone, isGitFile(t, clone), worktree)
			}
			gitOut(t, clone, "-c", "protocol.file.allow=always", "submodule", "update", "-q", "--init")
			inSub := filepath.Join(clone, "sub")
			writeTestFile(t, filepath.Join(inSub, "work.txt"), "precious\n")
			gitOut(t, inSub, "add", "work.txt")
			gitOut(t, inSub, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-qm", "submodule work")
			gitOut(t, clone, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-qam", "bump sub")
			gitOut(t, clone, "push", "-q", "origin", "HEAD:main")

			st, err := Inspect(ws, gitx.Git{}, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			if !hasBlocker(st, filepath.Join("super", "sub")+" has 1 unpushed commit") {
				t.Fatalf("reap would delete the only copy of the submodule commit: %v", st.Blockers())
			}

			// Pushed, the submodule holds nothing.
			gitOut(t, inSub, "push", "-q", "origin", "HEAD:main")
			st, err = Inspect(ws, gitx.Git{}, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			if b := st.Blockers(); len(b) != 0 {
				t.Fatalf("a pushed submodule still holds: %v", b)
			}
		})
	}
}

// An uninitialised submodule is an empty directory: nothing to judge, nothing
// held.
func TestReapIgnoresAnUninitialisedSubmodule(t *testing.T) {
	roots, ws, _ := setup(t)
	remotes := t.TempDir()
	sub := bareRemote(t, filepath.Join(remotes, "sub.git"))
	super := bareRemote(t, filepath.Join(remotes, "super.git"))
	seed := filepath.Join(t.TempDir(), "seed")
	gitOut(t, "", "clone", "-q", super, seed)
	gitOut(t, seed, "-c", "protocol.file.allow=always", "submodule", "add", "-q", sub, "sub")
	gitOut(t, seed, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-qm", "add sub")
	gitOut(t, seed, "push", "-q", "origin", "HEAD:main")
	m := &manifest.Manifest{Name: "w", Clones: map[string]string{"super": super}}
	if err := m.Write(ws); err != nil {
		t.Fatal(err)
	}
	if err := Sync(roots, ws, gitx.Git{}, quiet(), SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(ws, "super", "sub")); err != nil {
		t.Fatal(err)
	}
	st, err := Inspect(ws, gitx.Git{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if b := st.Blockers(); len(b) != 0 {
		t.Fatalf("an uninitialised submodule holds: %v", b)
	}
}
