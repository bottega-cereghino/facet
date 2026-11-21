package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bottega-cereghino/facet/internal/config"
	"github.com/bottega-cereghino/facet/internal/gitx"
	"github.com/bottega-cereghino/facet/internal/manifest"
)

// worktreeSetup builds a workspace declaring one clone of origin, and a
// checkout of origin at <projects>/repo for it to be a worktree of.
func worktreeSetup(t *testing.T) (roots config.Roots, ws, origin, checkout string) {
	t.Helper()
	roots, ws, origin = setup(t)
	checkout = filepath.Join(roots.Projects, "repo")
	g := gitx.Git{}
	if _, err := g.Run("", nil, "clone", "-q", origin, checkout); err != nil {
		t.Fatal(err)
	}
	gitOut(t, checkout, "config", "user.email", "t@example.com")
	gitOut(t, checkout, "config", "user.name", "t")
	m := &manifest.Manifest{Name: "w", Clones: map[string]string{"repo": origin}}
	if err := m.Write(ws); err != nil {
		t.Fatal(err)
	}
	return roots, ws, origin, checkout
}

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := gitx.Git{}.Run(dir, nil, args...)
	if err != nil {
		t.Fatalf("git %v in %s: %v", args, dir, err)
	}
	return out
}

func isGitFile(t *testing.T, dir string) bool {
	t.Helper()
	fi, err := os.Lstat(filepath.Join(dir, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().IsRegular()
}

func TestSyncMakesAWorktreeOfTheProjectCheckout(t *testing.T) {
	roots, ws, _, checkout := worktreeSetup(t)
	if err := Sync(roots, ws, gitx.Git{}, quiet(), SyncOptions{Worktrees: true}); err != nil {
		t.Fatal(err)
	}
	clone := filepath.Join(ws, "repo")
	if !isGitFile(t, clone) {
		t.Fatal("the clone has a .git directory; want a worktree of the checkout")
	}
	if _, err := os.Stat(filepath.Join(clone, "README")); err != nil {
		t.Fatalf("worktree not checked out: %v", err)
	}
	if got := gitOut(t, clone, "rev-parse", "--abbrev-ref", "HEAD"); got != "HEAD" {
		t.Errorf("worktree is on %q; want a detached HEAD", got)
	}
	// The checkout itself is left as it was.
	if got := gitOut(t, checkout, "rev-parse", "--abbrev-ref", "HEAD"); got != "main" {
		t.Errorf("checkout moved to %q; want it still on main", got)
	}
	if got := gitOut(t, checkout, "status", "--porcelain"); got != "" {
		t.Errorf("checkout's tree changed: %q", got)
	}
	if got := gitOut(t, checkout, "worktree", "list", "--porcelain"); !strings.Contains(got, clone) {
		t.Errorf("checkout does not list the worktree:\n%s", got)
	}
}

func TestSyncClonesWhenTheCheckoutTracksAnotherRepository(t *testing.T) {
	roots, ws, _, checkout := worktreeSetup(t)
	other := originRepo(t, filepath.Join(t.TempDir(), "other"))
	gitOut(t, checkout, "remote", "set-url", "origin", other)
	if err := Sync(roots, ws, gitx.Git{}, quiet(), SyncOptions{Worktrees: true}); err != nil {
		t.Fatal(err)
	}
	if isGitFile(t, filepath.Join(ws, "repo")) {
		t.Fatal("made a worktree of a checkout whose origin is another repository")
	}
}

func TestSyncWithoutWorktreesClones(t *testing.T) {
	roots, ws, _, _ := worktreeSetup(t)
	if err := Sync(roots, ws, gitx.Git{}, quiet(), SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	if isGitFile(t, filepath.Join(ws, "repo")) {
		t.Fatal("made a worktree although Worktrees was off")
	}
}

// What removing a worktree leaves in place does not hold it: the checkout's
// own unpushed branch, its stash, and its other worktrees.
func TestWorktreeCloneIsNotHeldByWhatTheCheckoutKeeps(t *testing.T) {
	roots, ws, _, checkout := worktreeSetup(t)
	if err := Sync(roots, ws, gitx.Git{}, quiet(), SyncOptions{Worktrees: true}); err != nil {
		t.Fatal(err)
	}
	gitOut(t, checkout, "switch", "-q", "-c", "mine")
	if err := os.WriteFile(filepath.Join(checkout, "mine.txt"), []byte("only here\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	gitOut(t, checkout, "add", "mine.txt")
	gitOut(t, checkout, "commit", "-qm", "unpushed in the checkout")
	if err := os.WriteFile(filepath.Join(checkout, "README"), []byte("stashed\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	gitOut(t, checkout, "stash", "-q")
	gitOut(t, checkout, "worktree", "add", "-q", "--detach", filepath.Join(t.TempDir(), "sibling"))

	st, err := Inspect(ws, gitx.Git{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if b := st.Blockers(); len(b) > 0 {
		t.Fatalf("held by the checkout's own state: %v", b)
	}
	if got := st.Clone[0].WorktreeOf; got != gitOut(t, checkout, "rev-parse", "--show-toplevel") {
		t.Errorf("WorktreeOf = %q; want the checkout", got)
	}
}

func TestWorktreeCloneIsHeldByItsOwnWork(t *testing.T) {
	t.Run("uncommitted", func(t *testing.T) {
		roots, ws, _, _ := worktreeSetup(t)
		if err := Sync(roots, ws, gitx.Git{}, quiet(), SyncOptions{Worktrees: true}); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(ws, "repo", "new.txt"), []byte("x\n"), 0o666); err != nil {
			t.Fatal(err)
		}
		st, err := Inspect(ws, gitx.Git{}, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if !st.Clone[0].Dirty {
			t.Fatalf("an untracked file in the worktree does not hold it: %v", st.Blockers())
		}
	})
	t.Run("commit on its detached HEAD", func(t *testing.T) {
		roots, ws, _, _ := worktreeSetup(t)
		if err := Sync(roots, ws, gitx.Git{}, quiet(), SyncOptions{Worktrees: true}); err != nil {
			t.Fatal(err)
		}
		clone := filepath.Join(ws, "repo")
		if err := os.WriteFile(filepath.Join(clone, "README"), []byte("changed\n"), 0o666); err != nil {
			t.Fatal(err)
		}
		gitOut(t, clone, "commit", "-qam", "only on this HEAD")
		st, err := Inspect(ws, gitx.Git{}, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if st.Clone[0].Unpushed != 1 {
			t.Fatalf("Unpushed = %d; want 1 (blockers %v)", st.Clone[0].Unpushed, st.Blockers())
		}
	})
}

func TestReapRemovesTheWorktreeAndLeavesTheCheckout(t *testing.T) {
	roots, ws, _, checkout := worktreeSetup(t)
	if err := Sync(roots, ws, gitx.Git{}, quiet(), SyncOptions{Worktrees: true}); err != nil {
		t.Fatal(err)
	}
	gitOut(t, checkout, "branch", "kept")
	st, err := Inspect(ws, gitx.Git{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := Reap(st, gitx.Git{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(ws); !os.IsNotExist(err) {
		t.Fatalf("workspace still there: %v", err)
	}
	if got := gitOut(t, checkout, "worktree", "list", "--porcelain"); strings.Count(got, "worktree ") != 1 {
		t.Errorf("checkout still lists a worktree:\n%s", got)
	}
	gitOut(t, checkout, "rev-parse", "--verify", "refs/heads/kept")
	if _, err := os.Stat(filepath.Join(checkout, "README")); err != nil {
		t.Errorf("checkout's files went: %v", err)
	}
}

// A locked worktree is removed too: reap has decided by then.
func TestRemoveWorktreeRemovesALockedOne(t *testing.T) {
	roots, ws, _, checkout := worktreeSetup(t)
	if err := Sync(roots, ws, gitx.Git{}, quiet(), SyncOptions{Worktrees: true}); err != nil {
		t.Fatal(err)
	}
	clone := filepath.Join(ws, "repo")
	gitOut(t, checkout, "worktree", "lock", clone)
	assertWorktreeGone(t, checkout, clone)
}

// A worktree git will not remove (here its .git file was damaged; older gits
// also refuse one holding a submodule) is deleted by hand, and the checkout
// pruned.
func TestRemoveWorktreeFallsBackToPrune(t *testing.T) {
	roots, ws, _, checkout := worktreeSetup(t)
	if err := Sync(roots, ws, gitx.Git{}, quiet(), SyncOptions{Worktrees: true}); err != nil {
		t.Fatal(err)
	}
	clone := filepath.Join(ws, "repo")
	if err := os.WriteFile(filepath.Join(clone, ".git"), []byte("gitdir: /nowhere\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	if _, err := (gitx.Git{}).Run(checkout, nil, "worktree", "remove", "--force", "--force", clone); err == nil {
		t.Fatal("git removed the damaged worktree; the fallback below is no longer exercised")
	}
	assertWorktreeGone(t, checkout, clone)
}

func assertWorktreeGone(t *testing.T, checkout, clone string) {
	t.Helper()
	if err := removeWorktree(gitx.Git{}, checkout, clone); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(clone); !os.IsNotExist(err) {
		t.Fatalf("worktree still there: %v", err)
	}
	if got := gitOut(t, checkout, "worktree", "list", "--porcelain"); strings.Count(got, "worktree ") != 1 {
		t.Errorf("checkout still lists the worktree:\n%s", got)
	}
}

func TestSameRepoURL(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"git@github.com:acme/platform.git", "https://github.com/acme/platform", true},
		{"git@github.com:acme/platform.git", "ssh://git@github.com/acme/platform.git", true},
		{"git@github.com:acme/platform.git", "git@github.com:acme/infra.git", false},
		{"git@github.com:acme/platform.git", "git@gitlab.com:acme/platform.git", false},
		{"/src/repo", "/src/repo/", true},
		{"/src/repo", "git@github.com:acme/repo.git", false},
		{"", "", false},
	} {
		if got := sameRepoURL(c.a, c.b); got != c.want {
			t.Errorf("sameRepoURL(%q, %q) = %v; want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestRepoName(t *testing.T) {
	for in, want := range map[string]string{
		"git@github.com:acme/platform.git": "platform",
		"https://github.com/acme/infra":    "infra",
		"/src/origin/repo/":                "repo",
		"git@host:solo.git":                "solo",
	} {
		if got := repoName(in); got != want {
			t.Errorf("repoName(%q) = %q; want %q", in, got, want)
		}
	}
}
