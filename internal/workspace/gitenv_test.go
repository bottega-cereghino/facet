package workspace

import (
	"path/filepath"
	"testing"

	"github.com/bottega-cereghino/facet/internal/gitx"
	"github.com/bottega-cereghino/facet/internal/manifest"
)

// The tests here hold the guarantee that reap reads the repository on disk,
// not what the environment or a local-only ref tells git to show. Each was
// red before gitx cleaned git's environment (#310, #314).

// commitFile writes file in dir and commits it.
func commitFile(t *testing.T, dir, file, contents, msg string) {
	t.Helper()
	writeTestFile(t, filepath.Join(dir, file), contents)
	gitOut(t, dir, "add", "--", file)
	gitOut(t, dir, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-qm", msg)
}

// requireHeld fails unless reap would refuse the workspace, and also unless
// --reapable would leave it alone.
func requireHeld(t *testing.T, st *IssueState, why string) {
	t.Helper()
	if len(st.Blockers()) == 0 && !st.StaleFetch() {
		t.Fatalf("%s: reap would delete the only copy; Blockers() = %v, Notes() = %v", why, st.Blockers(), st.Notes())
	}
}

// git sets GIT_DIR for a `!` alias (`alias.reap = !facet reap` run inside a
// checkout), and a probe that inherits it reads the checkout's refs and index:
// the clone's own unpushed branch is invisible.
func TestReapIgnoresAnInheritedGitDir(t *testing.T) {
	// Serial, not t.Parallel: t.Setenv changes the environment of the whole process.
	roots, ws, _, checkout := worktreeSetup(t)
	if err := Sync(roots, ws, gitx.Git{}, quiet(), SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	clone := filepath.Join(ws, "repo")
	gitOut(t, clone, "switch", "-q", "-c", "side")
	commitFile(t, clone, "work.txt", "a day of work\n", "only in the clone")
	gitOut(t, clone, "switch", "-q", "main")

	t.Setenv("GIT_DIR", filepath.Join(checkout, ".git"))
	st, err := Inspect(ws, gitx.Git{}, nil, nil)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if !blockersMention(st.Blockers(), "unpushed") {
		t.Errorf("the clone's branch side has a commit no remote has, but with GIT_DIR set reap sees %v", st.Blockers())
	}
}

// GIT_DIR and GIT_WORK_TREE together (a hook, a dotfiles shell) make every
// clone read as that other repository, clean.
func TestReapIgnoresAnInheritedGitDirAndWorkTree(t *testing.T) {
	// Serial, not t.Parallel: t.Setenv changes the environment of the whole process.
	root := t.TempDir()
	ws, clone := longLivedWorkspace(t, root)
	commitFile(t, clone, "new.txt", "only copy\n", "local only")
	writeTestFile(t, filepath.Join(clone, "untracked.txt"), "only copy\n")
	other := filepath.Join(root, "other")
	mustGit(t, "", "clone", "-q", "--", filepath.Join(root, "origin"), other)

	t.Setenv("GIT_DIR", filepath.Join(other, ".git"))
	t.Setenv("GIT_WORK_TREE", other)
	st, err := Inspect(ws, gitx.Git{}, nil, nil)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	for _, want := range []string{"unpushed", "uncommitted"} {
		if !blockersMention(st.Blockers(), want) {
			t.Errorf("under an inherited GIT_DIR, reap misses %q: %v", want, st.Blockers())
		}
	}
}

// Replace refs are local and never fetched or pushed. `git replace --graft
// origin/main <local commit>` makes that commit reachable from --remotes for
// any git that honours them, though no remote has it.
func TestReapIgnoresAReplaceRefThatGraftsALocalCommitOntoTheRemote(t *testing.T) {
	t.Parallel()
	ws, clone := issueWorkspace(t)
	m, err := manifest.Read(ws)
	if err != nil {
		t.Fatal(err)
	}
	gitOut(t, clone, "switch", "-q", "-c", "feature")
	commitFile(t, clone, "idea.txt", "the idea\n", "the idea")
	local := gitOut(t, clone, "rev-parse", "HEAD")
	landOnDefault(t, gitx.Git{}, m.Clones["repo"], "theirs.txt", "their work\n")
	gitOut(t, clone, "fetch", "-q", "origin")
	gitOut(t, clone, "replace", "--graft", "refs/remotes/origin/main", local)

	st, err := InspectIssue(ws, gitx.Git{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	requireHeld(t, st, "the commit is on no remote; only a local replace ref says origin/main reaches it")
}

// Without refs/remotes/origin/HEAD the default branch was the short name
// "origin/main", which git resolves to refs/heads/origin/main before
// refs/remotes/origin/main: a local branch of that name passed the landing
// proof with commits no remote has.
func TestReapHoldsALocalBranchNamedOriginMain(t *testing.T) {
	t.Parallel()
	ws, clone := issueWorkspace(t)
	gitOut(t, clone, "config", "remote.origin.followRemoteHEAD", "never")
	gitOut(t, clone, "remote", "set-head", "origin", "--delete")
	gitOut(t, clone, "switch", "-q", "-c", "origin/main")
	commitFile(t, clone, "idea.txt", "the idea\n", "the idea")

	st, err := InspectIssue(ws, gitx.Git{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	requireHeld(t, st, "the commit is on a local branch only")
}

// The landing proof's content check passes the file names a ref changed to
// `git diff` as pathspecs. ":mine.txt" was pathspec magic for "mine.txt", so
// the file itself was never compared, and one rewritten on the default branch
// since the squash merge read as landed. The same fixture with "mine.txt" is
// TestSquashLandedWorkspaceStillRefusesWhenItsOwnFileDivergedAfterwards.
func TestReapComparesAFileNamedLikePathspecMagic(t *testing.T) {
	t.Parallel()
	ws, clone := issueWorkspace(t)
	m, err := manifest.Read(ws)
	if err != nil {
		t.Fatal(err)
	}
	origin := m.Clones["repo"]
	g := gitx.Git{}
	gitOut(t, clone, "checkout", "-qb", "1-x")
	commitFile(t, clone, ":mine.txt", "my work\n", "feature work")
	sha := squashLand(t, g, clone, origin, "1-x")
	pr := fakeCommitPR{merged: map[string]bool{sha: true}}
	landOnDefault(t, g, origin, ":mine.txt", "my work, rewritten by someone else\n")

	st, err := InspectIssue(ws, g, pr, nil)
	if err != nil {
		t.Fatal(err)
	}
	requireHeld(t, st, "a file this branch touched diverged on the default branch afterwards")
}

// The names beside a long-lived workspace's clones go to `git status` as
// pathspecs too: ":todo.md" was looked up as "todo.md", and a file named
// ":(exclude)*" hid itself and every other loose name.
func TestReapHoldsAFileWhoseNameIsPathspecMagic(t *testing.T) {
	t.Parallel()
	enclose := func(t *testing.T, root string) {
		t.Helper()
		enc := filepath.Join(root, "Workspaces")
		mustGit(t, enc, "init", "-q", "-b", "main")
	}
	for _, name := range []string{":todo.md", ":(exclude)*"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			ws, _ := longLivedWorkspace(t, root)
			enclose(t, root)
			writeTestFile(t, filepath.Join(ws, name), "only copy\n")
			st, err := Inspect(ws, gitx.Git{}, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			if !blockersMention(st.Blockers(), name) {
				t.Fatalf("reap would delete %s, never added anywhere: %v", name, st.Blockers())
			}
		})
	}
	t.Run("a magic name hides no neighbour", func(t *testing.T) {
		root := t.TempDir()
		ws, _ := longLivedWorkspace(t, root)
		enclose(t, root)
		writeTestFile(t, filepath.Join(ws, ":(exclude)*"), "x\n")
		writeTestFile(t, filepath.Join(ws, "NOTES.md"), "only copy\n")
		st, err := Inspect(ws, gitx.Git{}, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if !blockersMention(st.Blockers(), "NOTES.md") {
			t.Fatalf("reap would delete NOTES.md: %v", st.Blockers())
		}
	})
}
