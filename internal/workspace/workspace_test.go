package workspace

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bottega-cereghino/facet/internal/config"
	"github.com/bottega-cereghino/facet/internal/fslink"
	"github.com/bottega-cereghino/facet/internal/gitx"
	"github.com/bottega-cereghino/facet/internal/manifest"
)

// These exercise the real git binary against temp repos. They are the tests that
// protect the two properties whose failure destroys work.

func quiet() Reporter { return Reporter{W: io.Discard} }

// originRepo builds a throwaway repo with one commit and returns its path.
func originRepo(t *testing.T, dir string) string {
	t.Helper()
	g := gitx.Git{}
	if err := os.MkdirAll(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	mustRun := func(args ...string) {
		t.Helper()
		if _, err := g.Run(dir, nil, args...); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}
	mustRun("init", "-q", "-b", "main")
	mustRun("config", "user.email", "t@example.com")
	mustRun("config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte("hello\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	mustRun("add", "-A")
	mustRun("commit", "-qm", "init")
	return dir
}

func setup(t *testing.T) (config.Roots, string, string) {
	t.Helper()
	root := t.TempDir()
	roots := config.Roots{
		Workspaces: filepath.Join(root, "Workspaces"),
		Projects:   filepath.Join(root, "Projects"),
		Mirrors:    filepath.Join(root, "Projects", ".mirrors"),
	}
	ws := filepath.Join(roots.Workspaces, "w")
	if err := os.MkdirAll(ws, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(roots.Projects, 0o777); err != nil {
		t.Fatal(err)
	}
	origin := originRepo(t, filepath.Join(root, "origin", "repo"))
	return roots, ws, origin
}

func TestSyncCreatesMissingClone(t *testing.T) {
	t.Parallel()
	roots, ws, origin := setup(t)
	m := &manifest.Manifest{Name: "w", Clones: map[string]string{"repo": origin}}
	if err := m.Write(ws); err != nil {
		t.Fatal(err)
	}
	if err := Sync(roots, ws, gitx.Git{}, quiet(), SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(ws, "repo", "README")); err != nil {
		t.Fatalf("clone not checked out: %v", err)
	}
}

// The central guarantee: a clone that already exists is never pulled, reset or
// cleaned, because it may hold the only copy of unpushed work.
func TestSyncNeverTouchesExistingClone(t *testing.T) {
	t.Parallel()
	roots, ws, origin := setup(t)
	m := &manifest.Manifest{Name: "w", Clones: map[string]string{"repo": origin}}
	if err := m.Write(ws); err != nil {
		t.Fatal(err)
	}
	if err := Sync(roots, ws, gitx.Git{}, quiet(), SyncOptions{}); err != nil {
		t.Fatal(err)
	}

	clone := filepath.Join(ws, "repo")
	g := gitx.Git{}
	// Dirty the tree, add an untracked file, and move off the default branch.
	if err := os.WriteFile(filepath.Join(clone, "README"), []byte("LOCAL EDIT\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(clone, "untracked"), []byte("x\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Run(clone, nil, "checkout", "-qb", "wip"); err != nil {
		t.Fatal(err)
	}

	// A second sync must be a total no-op on the clone.
	if err := Sync(roots, ws, gitx.Git{}, quiet(), SyncOptions{}); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(filepath.Join(clone, "README"))
	if err != nil || string(got) != "LOCAL EDIT\n" {
		t.Errorf("sync reverted a dirty file: %q, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(clone, "untracked")); err != nil {
		t.Error("sync cleaned an untracked file")
	}
	branch, err := g.Run(clone, nil, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil || branch != "wip" {
		t.Errorf("sync moved the branch: %q, %v", branch, err)
	}
}

func TestSyncAddsExtraRemoteButNeverRewritesIt(t *testing.T) {
	t.Parallel()
	roots, ws, origin := setup(t)
	upstream := originRepo(t, filepath.Join(t.TempDir(), "upstream"))
	m := &manifest.Manifest{
		Name:    "w",
		Clones:  map[string]string{"repo": origin},
		Remotes: map[string]map[string]string{"repo": {"upstream": upstream}},
	}
	if err := m.Write(ws); err != nil {
		t.Fatal(err)
	}
	if err := Sync(roots, ws, gitx.Git{}, quiet(), SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	clone := filepath.Join(ws, "repo")
	g := gitx.Git{}
	got, ok := gitx.RemoteURL(g, clone, "upstream")
	if !ok || got != upstream {
		t.Fatalf("upstream = %q, %v; want %q", got, ok, upstream)
	}

	// A remote whose URL disagrees with the manifest is reported, never rewritten.
	if _, err := g.Run(clone, nil, "remote", "set-url", "upstream", "https://elsewhere.invalid/x.git"); err != nil {
		t.Fatal(err)
	}
	if err := Sync(roots, ws, g, quiet(), SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	got, _ = gitx.RemoteURL(g, clone, "upstream")
	if got != "https://elsewhere.invalid/x.git" {
		t.Errorf("sync silently rewrote a disagreeing remote to %q", got)
	}
}

func TestSyncLinkAndPrune(t *testing.T) {
	t.Parallel()
	roots, ws, origin := setup(t)
	// A link points at a project under ProjectsRoot.
	project := filepath.Join(roots.Projects, "proj")
	originRepo(t, project)
	const projOrigin = "https://github.com/o/proj.git"
	if _, err := (gitx.Git{}).Run(project, nil, "remote", "add", "origin", projOrigin); err != nil {
		t.Fatal(err)
	}

	m := &manifest.Manifest{Name: "w", Links: map[string]string{"proj": "proj"}}
	if err := m.Write(ws); err != nil {
		t.Fatal(err)
	}
	if err := Sync(roots, ws, gitx.Git{}, quiet(), SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	ok, err := fslink.IsLink(filepath.Join(ws, "proj"))
	if err != nil || !ok {
		t.Fatalf("link not created: %v", err)
	}
	// Sync captured the project's origin into the manifest.
	back, err := manifest.Read(ws)
	if err != nil {
		t.Fatal(err)
	}
	if got := back.Origins["proj"]; got != projOrigin {
		t.Errorf("sync captured origin %q for the link, want %q", got, projOrigin)
	}

	// Drop the link from the manifest, add a clone, then prune. The link goes;
	// the clone stays.
	stray := filepath.Join(ws, "proj")
	m2 := &manifest.Manifest{Name: "w", Clones: map[string]string{"repo": origin}}
	if err := m2.Write(ws); err != nil {
		t.Fatal(err)
	}
	if err := Sync(roots, ws, gitx.Git{}, quiet(), SyncOptions{Prune: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(stray); !os.IsNotExist(err) {
		t.Error("prune left an undeclared link behind")
	}
	if _, err := os.Stat(filepath.Join(ws, "repo", "README")); err != nil {
		t.Error("prune deleted a clone")
	}
	// The link target survives: only the reparse point/symlink was removed.
	if _, err := os.Stat(filepath.Join(project, "README")); err != nil {
		t.Fatal("!!! prune deleted the link's target contents")
	}
}

func TestDirsSkipsIssueWorkspaces(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	for _, name := range []string{"topical", "iss-repo-1-x", "no-manifest"} {
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(dir, 0o777); err != nil {
			t.Fatal(err)
		}
		if name != "no-manifest" {
			m := &manifest.Manifest{Name: name}
			if err := m.Write(dir); err != nil {
				t.Fatal(err)
			}
		}
	}
	got, err := Dirs(root, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || filepath.Base(got[0]) != "topical" {
		t.Errorf("Dirs(includeIssue=false) = %v; want just [topical]", got)
	}
	got, err = Dirs(root, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("Dirs(includeIssue=true) = %v; want 2", got)
	}
}

// testRoots builds Roots under a temp directory.
func testRoots(root string) config.Roots {
	return config.Roots{
		Workspaces: filepath.Join(root, "Workspaces"),
		Projects:   filepath.Join(root, "Projects"),
		Mirrors:    filepath.Join(root, "Projects", ".mirrors"),
	}
}

type hookSource struct {
	before func()
}

func (h hookSource) Resolve(url string) (string, string, error) {
	if h.before != nil {
		h.before()
	}
	return url, "", nil
}

// A symlink swapped into the clone's path between syncClone's Lstat and git
// clone must not be followed: git must not clone into the symlink's target,
// and the temporary staging directory must be cleaned up.
func TestSyncCloneDoesNotFollowASymlinkSwappedInBeforeTheClone(t *testing.T) {
	t.Parallel()
	roots, ws, origin := setup(t)
	m := &manifest.Manifest{Name: "w", Clones: map[string]string{"repo": origin}}
	if err := m.Write(ws); err != nil {
		t.Fatal(err)
	}

	outside := t.TempDir()
	clonePath := filepath.Join(ws, "repo")
	swapped := false
	src := hookSource{before: func() {
		if !swapped {
			swapped = true
			if err := os.Symlink(outside, clonePath); err != nil {
				t.Errorf("symlink swap failed: %v", err)
			}
		}
	}}

	if err := Sync(roots, ws, gitx.Git{}, quiet(), SyncOptions{Source: src}); err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("git cloned into the symlink target outside the workspace: %v", entries)
	}
	if ok, err := fslink.IsLink(clonePath); err != nil || !ok {
		t.Fatalf("symlink at %s was replaced (ok=%v, err=%v)", clonePath, ok, err)
	}
	assertNoStagingDirs(t, ws)
}

type hookGit struct {
	base   gitx.Runner
	onCall func(args []string)
}

func (h hookGit) Run(dir string, env []string, args ...string) (string, error) {
	if h.onCall != nil {
		h.onCall(args)
	}
	return h.base.Run(dir, env, args...)
}

// A symlink swapped into the clone's path between syncClone's Lstat and git
// worktree add must not be followed, and the checkout must not keep a dangling
// worktree entry for the discarded staging directory.
func TestSyncWorktreeDoesNotFollowASymlinkSwappedInBeforeAdd(t *testing.T) {
	t.Parallel()
	roots, ws, _, checkout := worktreeSetup(t)
	outside := t.TempDir()
	clonePath := filepath.Join(ws, "repo")
	swapped := false
	g := hookGit{
		base: gitx.Git{},
		onCall: func(args []string) {
			if !swapped && len(args) > 0 && args[0] == "fetch" {
				swapped = true
				if err := os.Symlink(outside, clonePath); err != nil {
					t.Errorf("symlink swap failed: %v", err)
				}
			}
		},
	}

	if err := Sync(roots, ws, g, quiet(), SyncOptions{Worktrees: true}); err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("git worktree add wrote into the symlink target outside the workspace: %v", entries)
	}
	if ok, err := fslink.IsLink(clonePath); err != nil || !ok {
		t.Fatalf("symlink at %s was replaced (ok=%v, err=%v)", clonePath, ok, err)
	}
	if list := gitOut(t, checkout, "worktree", "list", "--porcelain"); strings.Count(list, "worktree ") != 1 {
		t.Errorf("checkout kept a dangling worktree entry:\n%s", list)
	}
	assertNoStagingDirs(t, ws)
}

// Even if a symlink appears right before os.Rename, renaming a directory onto a
// symlink fails without following or replacing it, and placeStaged refuses it.
func TestPlaceStagedRefusesSymlinkDestination(t *testing.T) {
	t.Parallel()
	ws := t.TempDir()
	outside := t.TempDir()
	stage := filepath.Join(ws, "stage")
	if err := os.Mkdir(stage, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage, "marker"), []byte("x\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(ws, "repo")
	if err := os.Symlink(outside, dst); err != nil {
		t.Fatal(err)
	}

	if err := placeStaged(stage, dst); err == nil {
		t.Fatal("placeStaged succeeded over a symlink")
	}
	// Verify os.Rename itself refuses the symlink too (for the window between
	// Lstat and Rename).
	if err := os.Rename(stage, dst); err == nil {
		t.Fatal("os.Rename succeeded over a symlink")
	}
	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("symlink target was modified: %v", entries)
	}
	if ok, err := fslink.IsLink(dst); err != nil || !ok {
		t.Fatalf("symlink at %s was replaced (ok=%v, err=%v)", dst, ok, err)
	}
}

func assertNoStagingDirs(t *testing.T, ws string) {
	t.Helper()
	entries, err := os.ReadDir(ws)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".facet-clone-") {
			t.Errorf("leftover staging directory in workspace: %s", e.Name())
		}
	}
}
