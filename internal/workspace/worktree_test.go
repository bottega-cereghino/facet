package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bottega-cereghino/facet/internal/config"
	"github.com/bottega-cereghino/facet/internal/ghx"
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
// also refuse one holding a submodule) is deleted by hand, and the checkout's
// entry for it dropped.
func TestRemoveWorktreeFallsBackToDeletingByHand(t *testing.T) {
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
		t.Errorf("checkout still lists the worktree %s:\n%s", clone, got)
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

// plainWorktree adds a linked worktree of checkout outside any workspace, on a
// new branch, as `git worktree add` or an agent makes one.
func plainWorktree(t *testing.T, checkout, branch string) string {
	t.Helper()
	wt := filepath.Join(t.TempDir(), "wt")
	gitOut(t, checkout, "worktree", "add", "-q", "-b", branch, wt, "origin/main")
	return wt
}

// githubOrigin makes checkout's origin a GitHub URL, so a pull request is
// looked up for its worktrees, with a proxy on a closed local port so the
// fetch reap runs fails at once instead of leaving the machine.
func githubOrigin(t *testing.T, checkout string) {
	t.Helper()
	gitOut(t, checkout, "remote", "set-url", "origin", "https://github.com/o/r.git")
	gitOut(t, checkout, "config", "http.proxy", "http://127.0.0.1:9")
}

func TestInspectWorktreeRefusesACheckout(t *testing.T) {
	_, _, _, checkout := worktreeSetup(t)
	_, err := InspectWorktree(checkout, gitx.Git{}, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "fix:") {
		t.Fatalf("InspectWorktree(checkout) = %v; want a refusal that prints the fix", err)
	}
}

func TestPlainWorktreeIsJudgedByItsOwnWork(t *testing.T) {
	_, _, _, checkout := worktreeSetup(t)
	wt := plainWorktree(t, checkout, "topic")
	st, err := InspectWorktree(wt, gitx.Git{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if b := st.Blockers(); len(b) > 0 {
		t.Fatalf("a clean worktree is held: %v", b)
	}
	if err := os.WriteFile(filepath.Join(wt, "README"), []byte("changed\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	gitOut(t, wt, "commit", "-qam", "unpushed on topic")
	st, err = InspectWorktree(wt, gitx.Git{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if st.Clone[0].Unpushed != 1 {
		t.Fatalf("Unpushed = %d; want 1 (blockers %v)", st.Clone[0].Unpushed, st.Blockers())
	}
}

func TestPlainWorktreeIsHeldByItsBranchsPullRequest(t *testing.T) {
	_, _, _, checkout := worktreeSetup(t)
	wt := plainWorktree(t, checkout, "topic")
	githubOrigin(t, checkout)

	st, err := InspectWorktree(wt, gitx.Git{}, fakePR{pr: &ghx.PR{Number: 7, State: "OPEN"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !hasBlocker(st, "pull request #7 is still open") {
		t.Errorf("an open pull request does not hold: %v", st.Blockers())
	}
	st, err = InspectWorktree(wt, gitx.Git{}, fakePRErr{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !st.PRUnknown {
		t.Errorf("a failed lookup is read as no pull request: %v", st.Blockers())
	}
	st, err = InspectWorktree(wt, gitx.Git{}, fakePR{pr: &ghx.PR{Number: 7, State: "MERGED"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if b := st.Blockers(); len(b) > 0 {
		t.Errorf("a merged pull request holds: %v", b)
	}
}

func TestReapRemovesAPlainWorktreeAndKeepsItsBranch(t *testing.T) {
	_, _, _, checkout := worktreeSetup(t)
	wt := plainWorktree(t, checkout, "topic")
	st, err := InspectWorktree(wt, gitx.Git{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := Reap(st, gitx.Git{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(wt); !os.IsNotExist(err) {
		t.Fatalf("worktree still there: %v", err)
	}
	if got := gitOut(t, checkout, "worktree", "list", "--porcelain"); strings.Count(got, "worktree ") != 1 {
		t.Errorf("checkout still lists the worktree:\n%s", got)
	}
	gitOut(t, checkout, "rev-parse", "--verify", "refs/heads/topic")
}

// --reapable sweeps only worktrees inside their checkout's directory, Claude
// Code's .claude/worktrees among them. One checked out elsewhere may be a
// long-lived working copy: beside the checkout, or a service run from a tag
// with its ignored data beside the code (facet#275).
func TestProjectWorktreesOffersOnlyWorktreesInsideTheirCheckout(t *testing.T) {
	roots, ws, _, checkout := worktreeSetup(t)
	excludeIn(t, checkout, ".claude/")
	if err := Sync(roots, ws, gitx.Git{}, quiet(), SyncOptions{Worktrees: true}); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(checkout, ".claude", "worktrees", "topic")
	gitOut(t, checkout, "worktree", "add", "-q", "-b", "topic", inside, "origin/main")
	// A worktree beside the checkout is a repository directly under projects
	// itself, and lists the same worktrees again.
	beside := filepath.Join(roots.Projects, "repo-wt")
	gitOut(t, checkout, "worktree", "add", "-q", "-b", "beside", beside, "origin/main")
	service := filepath.Join(t.TempDir(), "services", "svc")
	gitOut(t, checkout, "worktree", "add", "-q", "--detach", service, "origin/main")
	if err := os.WriteFile(filepath.Join(service, "prod.db"), []byte("rows\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	gone := filepath.Join(checkout, ".claude", "worktrees", "gone")
	gitOut(t, checkout, "worktree", "add", "-q", "-b", "gone", gone, "origin/main")
	if err := os.RemoveAll(gone); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(roots.Projects, ".mirrors"), 0o777); err != nil {
		t.Fatal(err)
	}

	got, unchecked, err := ProjectWorktrees(gitx.Git{}, roots.Projects, roots.Workspaces)
	if err != nil || len(unchecked) > 0 {
		t.Fatalf("ProjectWorktrees: %v %v", err, unchecked)
	}
	if len(got) != 1 || realPath(got[0]) != realPath(inside) {
		t.Fatalf("ProjectWorktrees = %v; want only %s", got, inside)
	}
}

// bareLayout lays out the "bare repository plus worktrees" layout at
// projects/app: the bare repository at app/<gitDir>, and the everyday checkout
// at app/main, a linked worktree on main.
func bareLayout(t *testing.T, projects, origin, gitDir string) (app, everyday string) {
	t.Helper()
	app = filepath.Join(projects, "app")
	gitOut(t, "", "clone", "-q", "--bare", origin, filepath.Join(app, gitDir))
	if gitDir != ".git" {
		if err := os.WriteFile(filepath.Join(app, ".git"), []byte("gitdir: ./"+gitDir+"\n"), 0o666); err != nil {
			t.Fatal(err)
		}
	}
	everyday = filepath.Join(app, "main")
	gitOut(t, app, "worktree", "add", "-q", everyday, "main")
	return app, everyday
}

// With a bare repository, every working copy is a linked worktree, the
// everyday one included, so the sweep offers none of them: whether the bare
// repository sits in app/.bare, named by a .git file, or in app/.git itself
// (facet#275). `facet reap <path>` still takes one by name.
func TestProjectWorktreesSkipsABareLayout(t *testing.T) {
	for _, gitDir := range []string{".bare", ".git"} {
		t.Run(gitDir, func(t *testing.T) {
			roots, _, origin := setup(t)
			_, everyday := bareLayout(t, roots.Projects, origin, gitDir)
			got, unchecked, err := ProjectWorktrees(gitx.Git{}, roots.Projects, roots.Workspaces)
			if err != nil || len(unchecked) > 0 {
				t.Fatalf("ProjectWorktrees: %v %v", err, unchecked)
			}
			if len(got) != 0 {
				t.Fatalf("ProjectWorktrees = %v; want none: %s is the everyday checkout", got, everyday)
			}
			if _, err := InspectWorktree(everyday, gitx.Git{}, nil, nil); err != nil {
				t.Errorf("reap by path no longer takes the worktree: %v", err)
			}
		})
	}
}

// A worktree made from inside a worktree clone (Claude Code's
// .claude/worktrees, often ignored, so the clone's status is clean) is deleted
// with the clone, so it holds the workspace; a sibling elsewhere does not
// (TestWorktreeCloneIsNotHeldByWhatTheCheckoutKeeps). facet#265.
func TestWorktreeCloneIsHeldByAWorktreeNestedInIt(t *testing.T) {
	roots, ws, _, checkout := worktreeSetup(t)
	excludeIn(t, checkout, ".claude/")
	if err := Sync(roots, ws, gitx.Git{}, quiet(), SyncOptions{Worktrees: true}); err != nil {
		t.Fatal(err)
	}
	clone := filepath.Join(ws, "repo")
	nested := filepath.Join(clone, ".claude", "worktrees", "side")
	gitOut(t, clone, "worktree", "add", "-q", "--detach", nested)
	if err := os.WriteFile(filepath.Join(nested, "draft.txt"), []byte("only copy\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	if got := gitOut(t, clone, "status", "--porcelain"); got != "" {
		t.Fatalf("the clone should read clean, the nested worktree being ignored: %q", got)
	}
	st, err := Inspect(ws, gitx.Git{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	// The census holds the nested worktree's files as well.
	if b := st.Blockers(); len(b) == 0 || !strings.Contains(b[0], "linked worktree at ") || !strings.Contains(b[0], filepath.Join("repo", ".claude", "worktrees", "side")+" -- ") {
		t.Fatalf("not held by the nested worktree: %v", st.Blockers())
	}

	// Once the nested worktree's directory is gone, deleting the clone loses
	// nothing more: its entry lives in the checkout, which stays.
	if err := os.RemoveAll(nested); err != nil {
		t.Fatal(err)
	}
	st, err = Inspect(ws, gitx.Git{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if b := st.Blockers(); len(b) > 0 {
		t.Fatalf("held by a nested worktree whose directory is gone: %v", b)
	}
}

// The same for a plain worktree reaped by path. facet#265.
func TestPlainWorktreeIsHeldByAWorktreeNestedInIt(t *testing.T) {
	_, _, _, checkout := worktreeSetup(t)
	excludeIn(t, checkout, "inner/")
	wt := plainWorktree(t, checkout, "topic")
	gitOut(t, wt, "worktree", "add", "-q", "--detach", filepath.Join(wt, "inner"))
	st, err := InspectWorktree(wt, gitx.Git{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if b := st.Blockers(); len(b) == 0 || !strings.Contains(b[0], "linked worktree at ") || !strings.Contains(b[0], filepath.Join("wt", "inner")+" -- ") {
		t.Fatalf("not held by the nested worktree: %v", st.Blockers())
	}
}

// excludeIn makes checkout's repository ignore pattern, for the checkout and
// every worktree of it.
func excludeIn(t *testing.T, checkout, pattern string) {
	t.Helper()
	path := filepath.Join(checkout, ".git", "info", "exclude")
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o666)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(pattern + "\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// A folder inside a plain worktree is not a worktree: reaping it would delete
// that folder alone and leave the worktree (facet#274).
func TestInspectWorktreeRefusesAFolderInsideOne(t *testing.T) {
	_, _, _, checkout := worktreeSetup(t)
	wt := plainWorktree(t, checkout, "topic")
	src := filepath.Join(wt, "src")
	if err := os.MkdirAll(src, 0o777); err != nil {
		t.Fatal(err)
	}
	_, err := InspectWorktree(src, gitx.Git{}, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "folder inside the worktree") || !strings.Contains(err.Error(), "fix: facet reap ") {
		t.Fatalf("InspectWorktree(%s) = %v; want a refusal naming the worktree", src, err)
	}
}

// removeWorktree deletes by hand only a directory its checkout lists as a
// worktree, so a folder inside one is never deleted on its own (facet#274).
func TestRemoveWorktreeLeavesAFolderItDoesNotList(t *testing.T) {
	_, _, _, checkout := worktreeSetup(t)
	wt := plainWorktree(t, checkout, "topic")
	src := filepath.Join(wt, "src")
	if err := os.MkdirAll(src, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "settings.local"), []byte("ignored, only copy\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	if err := removeWorktree(gitx.Git{}, checkout, src); err == nil {
		t.Error("removeWorktree returned no error for a folder that is not a worktree")
	}
	if _, err := os.Stat(filepath.Join(src, "settings.local")); err != nil {
		t.Fatalf("removeWorktree deleted a folder inside a worktree: %v", err)
	}
}

// Per-worktree refs (refs/worktree/*, refs/bisect/*) live in the worktree's
// admin directory, which `git worktree remove` deletes. A commit only such a
// ref reaches goes with a worktree clone, so it holds like one HEAD reaches
// (#315).
func TestReapHoldsACommitOnlyAPerWorktreeRefReaches(t *testing.T) {
	for _, ref := range []string{"refs/worktree/keep", "refs/bisect/bad", "refs/rewritten/side"} {
		t.Run(ref, func(t *testing.T) {
			roots, ws, _, _ := worktreeSetup(t)
			if err := Sync(roots, ws, gitx.Git{}, quiet(), SyncOptions{Worktrees: true}); err != nil {
				t.Fatal(err)
			}
			clone := filepath.Join(ws, "repo")
			commitFile(t, clone, "work.txt", "a day of work\n", "kept under a per-worktree ref")
			gitOut(t, clone, "update-ref", ref, gitOut(t, clone, "rev-parse", "HEAD"))
			gitOut(t, clone, "checkout", "-q", "--detach", "refs/remotes/origin/main")

			st, err := Inspect(ws, gitx.Git{}, nil, nil)
			if err != nil {
				t.Fatalf("Inspect: %v", err)
			}
			if !blockersMention(st.Blockers(), "unpushed") {
				t.Errorf("a commit only %s reaches goes with the worktree, yet reap sees %v (notes %v)", ref, st.Blockers(), st.Notes())
			}
		})
	}
}

// refs/rewritten/* is the third per-worktree namespace, where an interrupted
// `rebase --rebase-merges` keeps its labels. A commit rewritten during that
// rebase (a conflict resolved, an amend) and labelled, then left behind by a
// `reset`, has no other ref until the rebase reaches its `merge`; stopped
// there, the clone reads clean. Reap counts it (facet#361).
func TestReapHoldsACommitOnlyARebaseLabelReaches(t *testing.T) {
	ws, clone := issueWorkspace(t)
	gitOut(t, clone, "switch", "-q", "-c", "feature")
	commitFile(t, clone, "a.txt", "a\n", "a")
	gitOut(t, clone, "push", "-q", "origin", "feature") // feature itself is pushed

	todo := filepath.Join(t.TempDir(), "todo")
	writeTestFile(t, todo, "label onto\nreset onto\npick feature\n"+
		"exec echo 'resolved by hand' > resolved.txt && git add resolved.txt && git commit -q --amend --no-edit\n"+
		"label side\nreset onto\nbreak\n")
	t.Setenv("GIT_SEQUENCE_EDITOR", "cp "+todo)
	for _, kv := range [][2]string{{"GIT_AUTHOR_NAME", "t"}, {"GIT_AUTHOR_EMAIL", "t@example.com"}, {"GIT_COMMITTER_NAME", "t"}, {"GIT_COMMITTER_EMAIL", "t@example.com"}} {
		t.Setenv(kv[0], kv[1])
	}
	gitOut(t, clone, "rebase", "-q", "-i", "-r", "refs/remotes/origin/main")
	side := gitOut(t, clone, "rev-parse", "refs/rewritten/side")
	if got := gitOut(t, clone, "branch", "-a", "--contains", side); got != "" {
		t.Fatalf("the rewritten commit should be on no branch: %q", got)
	}

	st, err := InspectIssue(ws, gitx.Git{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	requireHeld(t, st, "resolved.txt is committed only on refs/rewritten/side")
}

// git cannot empty a directory without write permission (a Go module cache),
// and goes on after the failed delete: it drops the worktree's admin
// directory, so the checkout forgets a worktree whose HEAD and index are
// gone. removeWorktree makes directories writable first and finishes the
// delete either way (#316).
func TestRemoveWorktreeEmptiesAReadOnlyDirectory(t *testing.T) {
	_, _, _, checkout := worktreeSetup(t)
	wt := plainWorktree(t, checkout, "topic")
	ro := filepath.Join(wt, "cache", "mod")
	if err := os.MkdirAll(ro, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ro, "go.mod"), []byte("module x\n"), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(ro, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(ro, 0o755) })
	assertWorktreeGone(t, checkout, wt)
	gitOut(t, checkout, "rev-parse", "--verify", "refs/heads/topic")
}

// The fallback drops the checkout's entry for this worktree alone: `git
// worktree prune` would drop every prunable one, a worktree on a disk that is
// not mounted included (#316).
func TestRemoveWorktreeFallbackKeepsOtherWorktreeEntries(t *testing.T) {
	_, _, _, checkout := worktreeSetup(t)
	away := plainWorktree(t, checkout, "away")
	if err := os.RemoveAll(away); err != nil { // its disk, unmounted
		t.Fatal(err)
	}
	wt := plainWorktree(t, checkout, "topic")
	if err := os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: /nowhere\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	if err := removeWorktree(gitx.Git{}, checkout, wt); err != nil {
		t.Fatal(err)
	}
	list := gitOut(t, checkout, "worktree", "list", "--porcelain")
	if n := len(linkedWorktrees(list)); n != 1 {
		t.Errorf("the checkout lists %d worktrees, want 1 (%s):\n%s", n, away, list)
	}
	if !strings.Contains(list, filepath.Base(filepath.Dir(away))) {
		t.Errorf("the fallback pruned %s, another worktree whose directory is away:\n%s", away, list)
	}
}

// A plain worktree named through a symlink is inspected at its real path: a
// walk does not descend into a symlink at its root, so a fork with an
// unpushed commit in an ignored node_modules went unseen through the link
// (#381).
func TestReapHoldsARepositoryInAWorktreeNamedThroughASymlink(t *testing.T) {
	root := t.TempDir()
	origin := originRepo(t, filepath.Join(root, "origin"))
	checkout := filepath.Join(root, "checkout")
	gitOut(t, "", "clone", "-q", origin, checkout)
	wt := filepath.Join(root, "wt")
	gitOut(t, checkout, "worktree", "add", "-q", "--detach", wt, "origin/main")
	writeTestFile(t, filepath.Join(wt, ".gitignore"), "node_modules/\n")
	gitOut(t, wt, "add", ".gitignore")
	gitOut(t, wt, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-qm", "ignore")
	gitOut(t, wt, "push", "-q", "origin", "HEAD:refs/heads/ignore")
	fork := filepath.Join(wt, "node_modules", "lib")
	gitOut(t, "", "clone", "-q", origin, fork)
	commitFile(t, fork, "patch.txt", "a patch that exists only here\n", "patch")

	alias := filepath.Join(t.TempDir(), "wt-link")
	if err := os.Symlink(wt, alias); err != nil {
		t.Fatal(err)
	}
	st, err := InspectWorktree(alias, gitx.Git{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	requireHeld(t, st, "the worktree named through a symlink holds the fork's only copy")
	if want := realPath(wt); st.Dir != want {
		t.Errorf("Dir = %q, want the real path %q", st.Dir, want)
	}
}
