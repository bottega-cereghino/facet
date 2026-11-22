package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bottega-cereghino/facet/internal/config"
	"github.com/bottega-cereghino/facet/internal/launchd"
	"github.com/bottega-cereghino/facet/internal/manifest"
	"github.com/bottega-cereghino/facet/internal/workspace"
)

// reap deletes a whole workspace or worktree, never a folder inside one and
// never a directory its path did not lead to when it asked (#364, part 6).

// A repository that tracks a .workspace.json at its root (a workspace
// definition kept in git) passes for a workspace wherever it is checked out.
// Cloned into a workspace, `facet reap <ws>/<clone>` takes the clone for a
// workspace of its own instead of refusing a folder inside one; its files are
// "committed in the enclosing repository", which is the clone's own .git, so
// nothing holds it, and reap deletes the clone with its unpushed branch.
func TestReapRefusesACloneWhoseRepositoryCommitsAManifest(t *testing.T) {
	reapableRoot(t)
	root := t.TempDir()
	origin := filepath.Join(root, "origin")
	if err := os.MkdirAll(origin, 0o777); err != nil {
		t.Fatal(err)
	}
	gitIn(t, origin, "init", "-q", "-b", "main")
	if err := (&manifest.Manifest{Name: "tools"}).Write(origin); err != nil {
		t.Fatal(err)
	}
	gitIn(t, origin, "add", manifest.FileName)
	gitIn(t, origin, "-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-q", "-m", "the team's workspace")

	ws := filepath.Join(root, "Workspaces", "w")
	if err := os.MkdirAll(ws, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := (&manifest.Manifest{Name: "w", Clones: map[string]string{"tools": origin}}).Write(ws); err != nil {
		t.Fatal(err)
	}
	clone := filepath.Join(ws, "tools")
	gitIn(t, "", "clone", "-q", origin, clone)
	gitIn(t, clone, "switch", "-q", "-c", "side")
	if err := os.WriteFile(filepath.Join(clone, "work.txt"), []byte("a day of work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, clone, "add", "work.txt")
	gitIn(t, clone, "-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-q", "-m", "only in the clone")
	gitIn(t, clone, "switch", "-q", "main")

	st, err := inspectTarget(clone, git, nil)
	if err == nil {
		t.Errorf("inspectTarget(%s) took a clone inside the workspace %s for a workspace; want a refusal naming %s (blockers: %v)",
			clone, ws, ws, st.Blockers())
	}
	if st != nil && len(st.Blockers()) == 0 {
		if err := workspace.Reap(st, git); err != nil {
			t.Fatal(err)
		}
		if !exists(t, filepath.Join(clone, ".git")) {
			t.Errorf("reap deleted the clone %s and its unpushed branch side", clone)
		}
	}
}

// The same shape on a plain worktree --reapable offers: a worktree of such a
// repository has a manifest, so it is inspected and deleted as a workspace,
// with removeAll instead of `git worktree remove`. The commits only its
// detached HEAD reaches are never counted.
func TestReapInspectsAWorktreeWithACommittedManifestAsAWorktree(t *testing.T) {
	reapableRoot(t)
	checkout, _ := branchSetup(t)
	if err := (&manifest.Manifest{Name: "tools"}).Write(checkout); err != nil {
		t.Fatal(err)
	}
	gitIn(t, checkout, "add", manifest.FileName)
	gitIn(t, checkout, "-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-q", "-m", "the team's workspace")
	gitIn(t, checkout, "push", "-q", "origin", "HEAD:refs/heads/ws")
	roots.Projects = filepath.Dir(checkout)

	wt := filepath.Join(checkout, ".claude", "worktrees", "agent")
	gitIn(t, checkout, "worktree", "add", "-q", "--detach", wt, "origin/ws")
	gitIn(t, wt, "-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "only on this detached HEAD")
	sha := gitIn(t, wt, "rev-parse", "HEAD")
	// .claude is the checkout's own business: ignore it, as a repo using
	// Claude Code does.
	if err := os.WriteFile(filepath.Join(checkout, ".git", "info", "exclude"), []byte(".claude/\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	st, err := inspectTarget(wt, git, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Worktree {
		t.Errorf("the worktree %s was inspected as a workspace (blockers: %v); its HEAD %s is on no branch",
			wt, st.Blockers(), sha[:8])
	}
	if len(st.Blockers()) == 0 {
		if err := workspace.Reap(st, git); err != nil {
			t.Fatal(err)
		}
		if list := gitIn(t, checkout, "worktree", "list", "--porcelain"); strings.Contains(list, "prunable") {
			t.Errorf("reap deleted the worktree by hand; the checkout still lists it, prunable, and the next prune drops %s:\n%s", sha[:8], list)
		}
	}
}

// inspectTarget asked whether the parent is inside a workspace only when the
// target holds no manifest of its own; a folder of a clone that tracks a
// .workspace.json (an example, a fixture, a dotfiles copy) is taken for a
// workspace, every file in it reads as committed in the "enclosing
// repository" -- the clone -- and the folder was deleted out of the clone
// (#313).
func TestReapRefusesAFolderOfACloneThatTracksAManifest(t *testing.T) {
	root, _, _ := reapableRoot(t)
	ws, app := forceWorkspace(t, root, "facet-test-grill-target")
	demo := filepath.Join(app, "examples", "demo")
	if err := os.MkdirAll(demo, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := (&manifest.Manifest{Name: "demo"}).Write(demo); err != nil {
		t.Fatal(err)
	}
	putFile(t, filepath.Join(demo, "README.md"), "how to use the example\n")
	gitIn(t, app, "add", "--", "examples")
	gitIn(t, app, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-qm", "example")
	gitIn(t, app, "push", "-q", "origin", "HEAD:main")

	if _, err := inspectTarget(demo, git, nil); err != nil {
		return // refused, as the guarantee says
	}
	err := runReap(t, func(string) bool { return true }, "--yes", "--path", demo)
	if !exists(t, demo) {
		t.Fatalf("reap deleted %s, a folder inside the clone app of the workspace %s (err=%v)", demo, ws, err)
	}
	t.Fatalf("inspectTarget accepted %s, a folder inside the workspace %s (reap err=%v)", demo, ws, err)
}

// A path's parent can be swapped while the prompt waits. The second
// inspection followed the replacement symlink, and with --force and the same
// holders Reap removed that other directory through the original path. The
// check after the prompt refuses when the path leads elsewhere (#363).
func TestReapDoesNotFollowASwappedTargetParent(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "workspaces")
	selected := filepath.Join(root, "task")
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(selected, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(outside, "task"), 0o755); err != nil {
		t.Fatal(err)
	}
	origin := forceOrigin(t)
	for _, dir := range []string{selected, filepath.Join(outside, "task")} {
		if err := (&manifest.Manifest{Name: "task", Clones: map[string]string{"app": origin}}).Write(dir); err != nil {
			t.Fatal(err)
		}
		gitIn(t, "", "clone", "-q", origin, filepath.Join(dir, "app"))
		if err := os.WriteFile(filepath.Join(dir, "app", ".git", "info", "exclude"), []byte("marker.txt\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "app", "marker.txt"), []byte("ignored data\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	savedRoots, savedJobs := roots, jobs
	t.Cleanup(func() { roots, jobs = savedRoots, savedJobs })
	roots = config.Roots{Workspaces: root}
	jobs = &launchd.Jobs{Dirs: []string{t.TempDir()}, Run: noTools}

	backup := filepath.Join(base, "original-workspaces")
	err := runReap(t, func(string) bool {
		if err := os.Rename(root, backup); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, root); err != nil {
			t.Fatal(err)
		}
		return true
	}, selected, "--force")
	if err == nil || !strings.Contains(err.Error(), "leads to") {
		t.Fatalf("reap went on after its target's parent was swapped for a symlink: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "task")); os.IsNotExist(err) {
		t.Fatalf("reap deleted outside its selected target via the swapped parent")
	}
	if _, err := os.Stat(filepath.Join(backup, "task", "app", "marker.txt")); err != nil {
		t.Fatalf("the selected workspace should remain at its original location: %v", err)
	}
}
