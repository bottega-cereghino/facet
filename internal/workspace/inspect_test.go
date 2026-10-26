package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RiccardoCereghino/facet/internal/ghx"
	"github.com/RiccardoCereghino/facet/internal/gitx"
	"github.com/RiccardoCereghino/facet/internal/manifest"
)

// longLivedWorkspace builds a workspace the way `facet new` does: an origin
// repo, a clone of it, and a manifest with no issue block. It sits under root,
// which the caller may turn into an enclosing repository first.
func longLivedWorkspace(t *testing.T, root string) (ws string, clone string) {
	t.Helper()
	origin := originRepo(t, filepath.Join(root, "origin"))
	ws = filepath.Join(root, "Workspaces", "delivery")
	if err := os.MkdirAll(ws, 0o777); err != nil {
		t.Fatal(err)
	}
	m := &manifest.Manifest{Name: "delivery", Clones: map[string]string{"repo": origin}}
	if err := m.Write(ws); err != nil {
		t.Fatal(err)
	}
	if err := Sync(testRoots(root), ws, gitx.Git{}, quiet(), SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	return ws, filepath.Join(ws, "repo")
}

func mustGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	if out, err := (gitx.Git{}).Run(dir, nil, args...); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func writeTestFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o666); err != nil {
		t.Fatal(err)
	}
}

// A workspace `facet new` made is reapable once its clones hold nothing to
// lose -- the same rule as an issue workspace (facet#135).
func TestInspectLongLivedCleanWorkspaceIsReapable(t *testing.T) {
	ws, _ := longLivedWorkspace(t, t.TempDir())
	st, err := Inspect(ws, gitx.Git{}, nil, nil)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if st.Issue != nil {
		t.Errorf("a long-lived workspace has no issue, got %+v", st.Issue)
	}
	if b := st.Blockers(); len(b) != 0 {
		t.Errorf("a clean long-lived workspace should be reapable, got %v", b)
	}
}

// Every clone check still applies: here, a commit that was never pushed.
func TestInspectLongLivedWorkspaceBlocksOnUnpushedWork(t *testing.T) {
	ws, clone := longLivedWorkspace(t, t.TempDir())
	mustGit(t, clone, "config", "user.email", "t@example.com")
	mustGit(t, clone, "config", "user.name", "t")
	writeTestFile(t, filepath.Join(clone, "new.txt"), "x\n")
	mustGit(t, clone, "add", "new.txt")
	mustGit(t, clone, "commit", "-q", "-m", "local only")

	st, err := Inspect(ws, gitx.Git{}, nil, nil)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if !blockersMention(st.Blockers(), "unpushed") {
		t.Errorf("an unpushed commit must hold the workspace, got %v", st.Blockers())
	}
}

// A long-lived workspace can hold files of its own beside its clones -- notes,
// a CLAUDE.md. Outside any repository, the workspace is their only copy.
func TestLooseFileOutsideARepositoryBlocksReap(t *testing.T) {
	ws, _ := longLivedWorkspace(t, t.TempDir())
	writeTestFile(t, filepath.Join(ws, "notes.md"), "the only copy\n")

	st, err := Inspect(ws, gitx.Git{}, nil, nil)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if !blockersMention(st.Blockers(), "notes.md") {
		t.Errorf("a loose file with no other copy must hold the workspace, got %v", st.Blockers())
	}
}

// facet's own files are not loose: the manifest and .scope describe the
// workspace, and deleting the workspace is deleting them.
func TestFacetsOwnFilesAreNotLoose(t *testing.T) {
	ws, _ := longLivedWorkspace(t, t.TempDir())
	writeTestFile(t, filepath.Join(ws, ".scope"), "o/repo#1\n")

	st, err := Inspect(ws, gitx.Git{}, nil, nil)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if b := st.Blockers(); len(b) != 0 {
		t.Errorf("facet's own files must not hold the workspace, got %v", b)
	}
}

// ~/Workspaces is itself a repository that tracks long-lived workspaces. A
// loose file committed there has another copy, so it does not hold the reap;
// one modified since, or never added, does.
func TestLooseFilesInAnEnclosingRepository(t *testing.T) {
	root := t.TempDir()
	ws, _ := longLivedWorkspace(t, root)
	enclosing := filepath.Join(root, "Workspaces")
	mustGit(t, enclosing, "init", "-q", "-b", "main")
	mustGit(t, enclosing, "config", "user.email", "t@example.com")
	mustGit(t, enclosing, "config", "user.name", "t")
	writeTestFile(t, filepath.Join(enclosing, ".gitignore"), "delivery/repo/\n")
	writeTestFile(t, filepath.Join(ws, "CLAUDE.md"), "committed\n")
	mustGit(t, enclosing, "add", ".")
	mustGit(t, enclosing, "commit", "-q", "-m", "track the workspace")

	st, err := Inspect(ws, gitx.Git{}, nil, nil)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if b := st.Blockers(); len(b) != 0 {
		t.Fatalf("committed loose files must not hold the workspace, got %v", b)
	}
	// git reports the resolved path: on macOS the temp dir is under a symlink.
	if want, _ := filepath.EvalSymlinks(enclosing); st.Enclosing != want {
		t.Errorf("Enclosing = %q, want %q", st.Enclosing, want)
	}

	writeTestFile(t, filepath.Join(ws, "CLAUDE.md"), "edited since\n")
	writeTestFile(t, filepath.Join(ws, "todo.md"), "never added\n")
	st, err = Inspect(ws, gitx.Git{}, nil, nil)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	for _, want := range []string{"CLAUDE.md", "todo.md"} {
		if !blockersMention(st.Blockers(), want) {
			t.Errorf("%s must hold the workspace, got %v", want, st.Blockers())
		}
	}
}

// A link is a window into a shared checkout, not something the workspace owns:
// it is neither loose nor inspected, and reaping removes the link and leaves
// its target intact.
func TestReapRemovesALinkAndLeavesItsTarget(t *testing.T) {
	root := t.TempDir()
	ws, _ := longLivedWorkspace(t, root)
	target := filepath.Join(root, "Projects", "shared")
	if err := os.MkdirAll(target, 0o777); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(target, "keep.txt"), "not ours\n")
	if err := os.Symlink(target, filepath.Join(ws, "shared")); err != nil {
		t.Fatal(err)
	}
	m, err := manifest.Read(ws)
	if err != nil {
		t.Fatal(err)
	}
	m.Links = map[string]string{"shared": "shared"}
	if err := m.Write(ws); err != nil {
		t.Fatal(err)
	}

	st, err := Inspect(ws, gitx.Git{}, nil, nil)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if b := st.Blockers(); len(b) != 0 {
		t.Fatalf("a link must not hold the workspace, got %v", b)
	}
	if err := Reap(st); err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if _, err := os.Stat(ws); !os.IsNotExist(err) {
		t.Errorf("workspace still present after reap (err = %v)", err)
	}
	if _, err := os.Stat(filepath.Join(target, "keep.txt")); err != nil {
		t.Errorf("reap reached through the link into its target: %v", err)
	}
}

// InspectIssue keeps its meaning for the commands that only make sense for an
// issue workspace (`facet issues`, `facet attach`).
func TestInspectIssueStillRefusesALongLivedWorkspace(t *testing.T) {
	ws, _ := longLivedWorkspace(t, t.TempDir())
	if _, err := InspectIssue(ws, gitx.Git{}, nil, nil); err == nil {
		t.Fatal("InspectIssue accepted a workspace with no issue")
	}
}

func blockersMention(blockers []string, want string) bool {
	for _, b := range blockers {
		if strings.Contains(b, want) {
			return true
		}
	}
	return false
}

// recordingCommitPR is fakeCommitPR that also records which repo it was asked
// about, so a test can assert where the lookup was pointed.
type recordingCommitPR struct {
	fakeCommitPR
	repos *[]string
}

func (f recordingCommitPR) MergedPRForSHA(repo, sha string) (*ghx.PRForCommit, error) {
	*f.repos = append(*f.repos, repo)
	return f.fakeCommitPR.MergedPRForSHA(repo, sha)
}

// A long-lived workspace names no issue repo, but each clone's GitHub URL names
// its own, so the squash-merge landing proof still runs: a branch squash-merged
// and deleted upstream does not hold the workspace as "unpushed".
func TestLongLivedSquashLandedCloneReapsClean(t *testing.T) {
	ws, _, _, pr := squashLandedWorkspace(t)
	m, err := manifest.Read(ws)
	if err != nil {
		t.Fatal(err)
	}
	m.Issue = nil
	m.Clones["repo"] = "git@github.com:o/repo.git"
	if err := m.Write(ws); err != nil {
		t.Fatal(err)
	}
	var repos []string
	st, err := Inspect(ws, gitx.Git{}, recordingCommitPR{pr, &repos}, nil)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if b := st.Blockers(); len(b) != 0 {
		t.Errorf("a squash-landed clone must not hold the workspace, got %v", b)
	}
	if len(repos) == 0 || repos[0] != "o/repo" {
		t.Errorf("the landing proof asked about %v, want o/repo", repos)
	}
}

func TestGithubRepo(t *testing.T) {
	for in, want := range map[string]string{
		"git@github.com:o/repo.git":       "o/repo",
		"git@github.com:o/repo":           "o/repo",
		"https://github.com/o/repo.git":   "o/repo",
		"https://github.com/o/repo":       "o/repo",
		"ssh://git@github.com/o/repo.git": "o/repo",
		"git@gitlab.com:o/repo.git":       "",
		"/local/path/repo.git":            "",
		"https://github.com/o":            "",
	} {
		if got := githubRepo(in); got != want {
			t.Errorf("githubRepo(%q) = %q, want %q", in, got, want)
		}
	}
}
