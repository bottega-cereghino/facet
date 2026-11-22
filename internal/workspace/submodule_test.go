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

// superWithSubmodule makes a workspace whose clone "super" records a
// submodule "sub", both on bare remotes, and returns the workspace and the
// clone. The submodule is not initialised. worktree makes the clone a
// worktree of a checkout under the projects root.
func superWithSubmodule(t *testing.T, worktree bool) (ws, clone string) {
	t.Helper()
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
	clone = filepath.Join(ws, "super")
	if worktree != isGitFile(t, clone) {
		t.Fatalf("clone %s: worktree = %v, want %v", clone, isGitFile(t, clone), worktree)
	}
	return ws, clone
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
			ws, clone := superWithSubmodule(t, worktree)
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

// A submodule's repository stays in the clone's git directory when its work
// tree goes: switched to a commit without it, or deinitialised. Its commits go
// with the clone, so reap judges each repository under modules, checked out or
// not, and holds while one has work no remote has (facet#311). Pushed from
// there, it holds nothing.
func TestReapHoldsASubmoduleThatIsNotCheckedOut(t *testing.T) {
	away := map[string]func(t *testing.T, clone string){
		"switched away": func(t *testing.T, clone string) {
			// Back to the commit before the submodule was added: an older
			// release, say.
			gitOut(t, clone, "-c", "submodule.recurse=true", "switch", "-q", "--detach", "HEAD~2")
		},
		"deinitialised": func(t *testing.T, clone string) {
			gitOut(t, clone, "submodule", "deinit", "-q", "-f", "sub")
		},
	}
	for name, goAway := range away {
		for _, worktree := range []bool{false, true} {
			t.Run(name+map[bool]string{false: ", own clone", true: ", worktree clone"}[worktree], func(t *testing.T) {
				ws, clone := superWithSubmodule(t, worktree)
				gitOut(t, clone, "-c", "protocol.file.allow=always", "submodule", "update", "-q", "--init")
				commitFile(t, filepath.Join(clone, "sub"), "work.txt", "precious\n", "submodule work")
				gitOut(t, clone, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-qam", "bump sub")
				gitOut(t, clone, "push", "-q", "origin", "HEAD:main")
				goAway(t, clone)
				if _, err := os.Stat(filepath.Join(clone, "sub", ".git")); err == nil {
					t.Fatal("the submodule should not be checked out")
				}
				repo := gitOut(t, clone, "rev-parse", "--path-format=absolute", "--git-path", "modules/sub")
				if _, err := os.Stat(repo); err != nil {
					t.Fatalf("the submodule's repository should still be in the clone: %v", err)
				}

				st, err := Inspect(ws, gitx.Git{}, nil, nil)
				if err != nil {
					t.Fatal(err)
				}
				want := filepath.Join("super", "sub") + " (not checked out; its repository is "
				if !hasBlocker(st, want) || !hasBlocker(st, "has 1 unpushed commit") {
					t.Fatalf("reap would delete the only copy of the submodule commit: %v", st.Blockers())
				}

				gitOut(t, repo, "push", "-q", "origin", "HEAD:main")
				st, err = Inspect(ws, gitx.Git{}, nil, nil)
				if err != nil {
					t.Fatal(err)
				}
				if b := st.Blockers(); len(b) != 0 {
					t.Fatalf("a pushed submodule that is not checked out still holds: %v", b)
				}

				// A stash entry goes with it too; git stash list cannot count it
				// without a work tree.
				gitOut(t, repo, "update-ref", "--create-reflog", "-m", "WIP", "refs/stash", "HEAD")
				st, err = Inspect(ws, gitx.Git{}, nil, nil)
				if err != nil {
					t.Fatal(err)
				}
				if !hasBlocker(st, "has 1 stash entry") {
					t.Fatalf("reap would delete the submodule's stash: %v", st.Blockers())
				}
			})
		}
	}
}

// A repository added with `git add` and no .gitmodules is a gitlink, git's
// "embedded repository": not ignored, not a submodule, and once the gitlink is
// pushed the clone reads clean. Its commits are judged as a clone's
// (facet#311).
func TestReapHoldsAnEmbeddedRepositoryWithoutGitmodules(t *testing.T) {
	ws, clone := issueWorkspace(t)
	sub := bareRemote(t, filepath.Join(t.TempDir(), "sub.git"))
	inner := filepath.Join(clone, "inner")
	gitOut(t, "", "clone", "-q", sub, inner)
	commitFile(t, inner, "work.txt", "precious\n", "inner work")
	gitOut(t, clone, "switch", "-q", "-c", "feature")
	gitOut(t, clone, "add", "inner")
	gitOut(t, clone, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-qm", "embed inner")
	gitOut(t, clone, "push", "-q", "origin", "feature")
	if _, err := os.Stat(filepath.Join(clone, ".gitmodules")); err == nil {
		t.Fatal("the fixture should have no .gitmodules")
	}

	st, err := InspectIssue(ws, gitx.Git{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !hasBlocker(st, filepath.Join("repo", "inner")+" has 1 unpushed commit") {
		t.Fatalf("reap would delete the only copy of inner's commit: %v", st.Blockers())
	}

	gitOut(t, inner, "push", "-q", "origin", "HEAD:main")
	st, err = InspectIssue(ws, gitx.Git{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if b := st.Blockers(); len(b) != 0 {
		t.Fatalf("a pushed embedded repository still holds: %v", b)
	}
}

// A repository made inside a directory the clone tracks files in (`git init`
// in docs/) is neither untracked nor ignored, so git status says nothing of
// its commits. Reap judges it as a clone (facet#311).
func TestReapHoldsARepositoryInATrackedDirectory(t *testing.T) {
	ws, clone := issueWorkspace(t)
	if err := os.Mkdir(filepath.Join(clone, "docs"), 0o777); err != nil {
		t.Fatal(err)
	}
	gitOut(t, clone, "switch", "-q", "-c", "feature")
	commitFile(t, clone, "docs/guide.txt", "the guide\n", "add docs")
	gitOut(t, clone, "push", "-q", "origin", "feature")
	docs := filepath.Join(clone, "docs")
	gitOut(t, docs, "init", "-q")
	commitFile(t, docs, "guide.txt", "the guide\n", "docs history")

	st, err := InspectIssue(ws, gitx.Git{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !hasBlocker(st, filepath.Join("repo", "docs")+" has 1 unpushed commit") {
		t.Fatalf("reap would delete the only copy of docs' history: %v", st.Blockers())
	}
}
