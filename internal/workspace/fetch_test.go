package workspace

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/bottega-cereghino/facet/internal/gitx"
	"github.com/bottega-cereghino/facet/internal/gitx/gitxtest"
	"github.com/bottega-cereghino/facet/internal/manifest"
)

// Work pushed to a second remote, a fork, whose branch was deleted there
// since: a stale fork/* ref must not read as pushed. Every remote the clone
// has is fetched with --prune, not only origin (facet#268).
func TestReapFetchesEveryRemoteBeforeCountingWorkAsPushed(t *testing.T) {
	ws, clone := issueWorkspace(t)
	fork := filepath.Join(t.TempDir(), "fork.git")
	gitOut(t, "", "init", "-q", "--bare", fork)
	gitOut(t, clone, "remote", "add", "fork", fork)
	gitOut(t, clone, "switch", "-q", "-c", "feature")
	writeTestFile(t, filepath.Join(clone, "idea.txt"), "the idea\n")
	gitOut(t, clone, "add", "idea.txt")
	gitOut(t, clone, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-qm", "the idea")
	gitOut(t, clone, "push", "-q", "fork", "feature")

	st, err := InspectIssue(ws, gitx.Git{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if b := st.Blockers(); len(b) != 0 {
		t.Fatalf("work pushed to the fork holds: %v", b)
	}

	gitOut(t, fork, "branch", "-q", "-D", "feature")
	st, err = InspectIssue(ws, gitx.Git{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !hasBlocker(st, "1 unpushed commit") {
		t.Fatalf("the branch is gone from the fork, but its stale ref still reads as pushed: %v", st.Blockers())
	}
}

// A remote that cannot be fetched is named in the blocker, so the operator
// knows which one failed.
func TestReapNamesTheRemoteItCouldNotFetch(t *testing.T) {
	ws, clone := issueWorkspace(t)
	gitOut(t, clone, "remote", "add", "fork", filepath.Join(t.TempDir(), "gone.git"))
	git := &gitxtest.Runner{
		Real: gitx.Git{},
		Fail: func(args []string) bool { return len(args) > 0 && args[0] == "fetch" && args[len(args)-1] == "fork" },
		Err:  errors.New("could not read from remote repository"),
	}
	st, err := InspectIssue(ws, git, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if b := st.Blockers(); len(b) != 1 || !strings.Contains(b[0], "could not fetch fork") || strings.Contains(b[0], "origin") {
		t.Errorf("Blockers() = %v; want one naming fork alone", b)
	}
}

// countingGit is the real git, counting fetches; safe for concurrent use.
type countingGit struct {
	mu      sync.Mutex
	fetches int
}

func (c *countingGit) Run(dir string, env []string, args ...string) (string, error) {
	if len(args) > 0 && args[0] == "fetch" {
		c.mu.Lock()
		c.fetches++
		c.mu.Unlock()
	}
	return gitx.Git{}.Run(dir, env, args...)
}

// Worktree clones of one checkout share its refs, so their fetches, run at
// once by the parallel inspection, collide ("reference already exists") and
// leave the count judged stale. Through one SharedFetch each repository is
// fetched once, whichever workspace asks first (facet#267).
func TestSharedFetchFetchesEachRepositoryOnce(t *testing.T) {
	roots, _, origin, checkout := worktreeSetup(t)
	var dirs []string
	for _, name := range []string{"a", "b", "c", "d"} {
		ws := filepath.Join(roots.Workspaces, name)
		m := &manifest.Manifest{Name: name, Clones: map[string]string{"repo": origin}}
		if err := os.MkdirAll(ws, 0o777); err != nil {
			t.Fatal(err)
		}
		if err := m.Write(ws); err != nil {
			t.Fatal(err)
		}
		if err := Sync(roots, ws, gitx.Git{}, quiet(), SyncOptions{Worktrees: true}); err != nil {
			t.Fatal(err)
		}
		if !isGitFile(t, filepath.Join(ws, "repo")) {
			t.Fatalf("%s/repo is not a worktree of %s", name, checkout)
		}
		dirs = append(dirs, ws)
	}
	counting := &countingGit{}
	shared := NewSharedFetch(counting)
	states := make([]*IssueState, len(dirs))
	var wg sync.WaitGroup
	for i, ws := range dirs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			st, err := Inspect(ws, shared, nil, nil)
			if err != nil {
				t.Error(err)
				return
			}
			states[i] = st
		}()
	}
	wg.Wait()
	if counting.fetches != 1 {
		t.Errorf("%d fetches for one repository with one remote; want 1", counting.fetches)
	}
	for i, st := range states {
		if st != nil && len(st.Clone) == 1 && len(st.Clone[0].StaleRemotes) > 0 {
			t.Errorf("%s judged stale: %v", dirs[i], st.Clone[0].StaleRemotes)
		}
	}
}

// A remote whose configured refspec writes into refs/heads (`git remote add
// --mirror=fetch`) deletes, under --prune, every local branch it lacks: the
// inspection itself, a dry run included, would delete the only copy of a
// commit and then count nothing unpushed. Reap fetches into refs/remotes only,
// whatever the remote's refspec says (facet#321).
func TestReapsFetchNeverWritesOutsideRefsRemotes(t *testing.T) {
	ws, clone := issueWorkspace(t)
	mirror := filepath.Join(t.TempDir(), "mirror.git")
	gitOut(t, "", "init", "-q", "--bare", mirror)
	gitOut(t, clone, "remote", "add", "--mirror=fetch", "mirror", mirror)
	gitOut(t, clone, "switch", "-q", "-c", "wip")
	writeTestFile(t, filepath.Join(clone, "wip.txt"), "only copy\n")
	gitOut(t, clone, "add", "wip.txt")
	gitOut(t, clone, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-qm", "wip")
	gitOut(t, clone, "switch", "-q", "--detach", "HEAD~1")

	st, err := InspectIssue(ws, gitx.Git{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (gitx.Git{}).Run(clone, nil, "rev-parse", "--verify", "refs/heads/wip"); err != nil {
		t.Fatalf("inspecting the workspace deleted branch wip, the only copy of a commit: %v", err)
	}
	if !hasBlocker(st, "1 unpushed commit") {
		t.Errorf("branch wip's commit is not held: %v", st.Blockers())
	}
}

// A branch pushed to origin, then deleted or force-pushed away there since the
// last good fetch, while the next fetch fails (network down, unreachable remote,
// or missing path): the commit exists only in this clone and its stale
// refs/remotes/origin/<branch> ref. Reap must hold the workspace rather than
// counting the stale remote ref as pushed (#491).
func TestReapHoldsWorkWhenTheFetchFailsAndOnlyAStaleRemoteRefHasIt(t *testing.T) {
	setup := func(t *testing.T) (ws, clone, origin string) {
		t.Helper()
		root := t.TempDir()
		origin = filepath.Join(root, "origin.git")
		gitOut(t, "", "init", "-q", "--bare", "-b", "main", origin)
		seed := filepath.Join(root, "seed")
		gitOut(t, "", "clone", "-q", origin, seed)
		commitFile(t, seed, "README", "hello\n", "init")
		gitOut(t, seed, "push", "-q", "origin", "main")

		ws = filepath.Join(root, "iss-repo-491-x")
		if err := os.MkdirAll(ws, 0o777); err != nil {
			t.Fatal(err)
		}
		m := &manifest.Manifest{
			Name:   "iss-repo-491-x",
			Clones: map[string]string{"repo": origin},
			Issue:  &manifest.Issue{Repo: "o/repo", Number: 491, Branch: "feature", Home: "repo"},
		}
		if err := m.Write(ws); err != nil {
			t.Fatal(err)
		}
		if err := Sync(testRoots(root), ws, gitx.Git{}, quiet(), SyncOptions{}); err != nil {
			t.Fatal(err)
		}
		clone = filepath.Join(ws, "repo")

		// Push a branch so refs/remotes/origin/feature records the commit.
		gitOut(t, clone, "switch", "-q", "-c", "feature")
		commitFile(t, clone, "idea.txt", "only copy\n", "only copy")
		gitOut(t, clone, "push", "-q", "origin", "feature")

		// Rewrite origin so the commit exists nowhere on origin anymore, leaving
		// the clone's refs/remotes/origin/feature stale.
		gitOut(t, seed, "switch", "-q", "-c", "feature")
		commitFile(t, seed, "other.txt", "rewritten\n", "rewritten")
		gitOut(t, seed, "push", "-q", "--force", "origin", "feature")
		return ws, clone, origin
	}

	t.Run("fetch fails while remote is unreachable", func(t *testing.T) {
		ws, _, _ := setup(t)
		st, err := InspectIssue(ws, fetchFailGit(), nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if !hasBlocker(st, "fetch failed, so the remote's state is unknown") {
			t.Fatalf("reap would delete the only copy when fetch fails and only a stale remote ref has it; Blockers() = %v, Notes() = %v", st.Blockers(), st.Notes())
		}
	})

	t.Run("remote points at a missing path", func(t *testing.T) {
		ws, clone, _ := setup(t)
		gitOut(t, clone, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "missing.git"))
		st, err := InspectIssue(ws, gitx.Git{}, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if !hasBlocker(st, "fetch failed, so the remote's state is unknown") {
			t.Fatalf("reap would delete the only copy when the remote path is missing and only a stale remote ref has it; Blockers() = %v, Notes() = %v", st.Blockers(), st.Notes())
		}
	})
}
