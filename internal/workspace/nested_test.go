package workspace

import (
	"path/filepath"
	"testing"

	"github.com/bottega-cereghino/facet/internal/gitx"
)

// One walk of each clone's work tree finds every place a repository sits:
// judged as a clone, or held (#364).

// The walk used to start at each ignored directory and skip its root, so when
// the ignored directory IS the marker -- a Mercurial `.hg/` a global gitignore
// lists, Jujutsu's `.jj/`, which jj ignores itself with `.jj/.gitignore` --
// the repository was never seen (#357).
func TestReapHoldsARepositoryWhoseMarkerIsTheIgnoredDirectory(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		marker string
		build  func(t *testing.T, clone string)
	}{
		{"mercurial repository made in a tracked directory, .hg/ ignored", "tools", func(t *testing.T, clone string) {
			mkdirs(t, filepath.Join(clone, "tools"))
			commitFile(t, clone, filepath.Join("tools", "build.sh"), "echo build\n", "tools")
			// Pushed, so the commit itself holds nothing.
			backup := filepath.Join(t.TempDir(), "backup.git")
			gitOut(t, "", "init", "-q", "--bare", backup)
			gitOut(t, clone, "remote", "add", "backup", backup)
			gitOut(t, clone, "push", "-q", "backup", "HEAD:refs/heads/main")
			excludeIn(t, clone, ".hg/")
			mkdirs(t, filepath.Join(clone, "tools", ".hg", "store"))
			writeTestFile(t, filepath.Join(clone, "tools", ".hg", "store", "00changelog.i"), "history no remote has\n")
		}},
		{"jujutsu colocated at the clone's root", ".jj", func(t *testing.T, clone string) {
			mkdirs(t, filepath.Join(clone, ".jj", "repo", "op_store", "operations"))
			mkdirs(t, filepath.Join(clone, ".jj", "repo", "store"))
			writeTestFile(t, filepath.Join(clone, ".jj", ".gitignore"), "/*\n")
			writeTestFile(t, filepath.Join(clone, ".jj", "repo", "store", "git_target"), "../../../.git")
			writeTestFile(t, filepath.Join(clone, ".jj", "repo", "op_store", "operations", "0a1b"), "an operation no remote has\n")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws, clone := longLivedWorkspace(t, t.TempDir())
			tc.build(t, clone)
			if got := gitOut(t, clone, "status", "--porcelain", "--untracked-files=all"); got != "" {
				t.Fatalf("the clone should read clean: %q", got)
			}
			st, err := Inspect(ws, gitx.Git{}, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			if !hasBlocker(st, tc.marker) {
				t.Fatalf("nothing names the repository at %s, and reap would delete it: %v", tc.marker, st.Blockers())
			}
		})
	}
}

// A submodule that is not initialised (sync clones without
// --recurse-submodules; `submodule deinit` leaves the same) is an empty
// directory git status never looks into: the path is a gitlink in the index,
// so the untracked walk skips it, and an unpopulated submodule has no changes.
// submoduleStates skips it too, finding no .git there. A library copied in by
// hand and patched, or notes written there, were deleted unseen (#356).
func TestReapHoldsFilesInASubmoduleDirectoryThatIsNotInitialised(t *testing.T) {
	t.Parallel()
	for _, worktree := range []bool{false, true} {
		t.Run(map[bool]string{false: "own clone", true: "worktree clone"}[worktree], func(t *testing.T) {
			ws, clone := superWithSubmodule(t, worktree)
			writeTestFile(t, filepath.Join(clone, "sub", "patch.diff"), "a fix that exists nowhere else\n")
			if got := gitOut(t, clone, "status", "--porcelain", "--untracked-files=all", "--ignore-submodules=none"); got != "" {
				t.Fatalf("expected git status to read clean: %q", got)
			}
			st, err := Inspect(ws, gitx.Git{}, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			requireHeld(t, st, "sub/patch.diff is in no repository")
		})
	}
}

// An empty submodule directory holds nothing, and a directory git tracks is
// committed content even when its name marks a repository: a test fixture
// named .hg or CVS does not hold.
func TestReapDoesNotHoldOnAnEmptySubmoduleOrATrackedMarker(t *testing.T) {
	t.Parallel()
	ws, clone := superWithSubmodule(t, false)
	mkdirs(t, filepath.Join(clone, "testdata", "CVS"))
	commitFile(t, clone, filepath.Join("testdata", "CVS", "Entries"), "a fixture\n", "fixture")
	gitOut(t, clone, "push", "-q", "origin", "HEAD:main")
	st, err := Inspect(ws, gitx.Git{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if b := st.Blockers(); len(b) != 0 {
		t.Fatalf("an empty submodule directory and a tracked fixture hold: %v", b)
	}
}

// A repository judged as a clone (a checked-out submodule) is judged by its
// .git only; another VCS beside that .git holds.
func TestReapHoldsAnotherVCSBesideASubmodulesGit(t *testing.T) {
	t.Parallel()
	ws, clone := superWithSubmodule(t, false)
	gitOut(t, clone, "-c", "protocol.file.allow=always", "submodule", "update", "-q", "--init")
	mkdirs(t, filepath.Join(clone, "sub", ".hg", "store"))
	writeTestFile(t, filepath.Join(clone, "sub", ".hg", "store", "00changelog.i"), "history no remote has\n")
	st, err := Inspect(ws, gitx.Git{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !hasBlocker(st, filepath.Join("sub", ".hg")) {
		t.Fatalf("a Mercurial repository beside the submodule's .git does not hold: %v", st.Blockers())
	}
}
