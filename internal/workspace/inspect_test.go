package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bottega-cereghino/facet/internal/ghx"
	"github.com/bottega-cereghino/facet/internal/gitx"
	"github.com/bottega-cereghino/facet/internal/manifest"
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
	// A sync that crashed leaves its lock behind.
	writeTestFile(t, filepath.Join(ws, syncLockName), "")

	st, err := Inspect(ws, gitx.Git{}, nil, nil)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if b := st.Blockers(); len(b) != 0 {
		t.Errorf("facet's own files must not hold the workspace, got %v", b)
	}
}

// ~/Workspaces is itself a repository that tracks long-lived workspaces. A
// loose file committed and pushed there has another copy, so it does not hold
// the reap; one modified since, or never added, does.
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
	pushEnclosing(t, enclosing)

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

// A long-lived workspace someone ran `git init` in is its own repository, not
// one it sits inside: what is committed there lives in the .git that reap
// deletes, so it is no other copy. Here two branches no remote has would go
// with it (#312).
func TestReapHoldsAWorkspaceThatIsItsOwnRepository(t *testing.T) {
	ws, _ := longLivedWorkspace(t, t.TempDir())
	gitOut(t, ws, "init", "-q", "-b", "notes")
	commitFile(t, ws, ".gitignore", "repo/\n", "ignore the clone")
	commitFile(t, ws, "notes.md", "a week of notes\n", "notes, on no remote")
	gitOut(t, ws, "switch", "-q", "-c", "ideas")
	commitFile(t, ws, "ideas.md", "only here\n", "ideas, on no remote")
	gitOut(t, ws, "switch", "-q", "notes")

	st, err := Inspect(ws, gitx.Git{}, nil, nil)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if !blockersMention(st.Blockers(), "is itself a git repository") {
		t.Errorf("the workspace's own repository has branches no remote has, yet it does not hold the workspace: %v", st.Blockers())
	}
	if st.Enclosing != "" {
		t.Errorf("the workspace's own repository is not one it sits inside, and reap would tell the user to commit there: Enclosing = %q", st.Enclosing)
	}
}

// A repository at the workspace root holds the workspace whatever it holds:
// reap judges files against a repository that survives the delete, and this
// one does not. It holds when everything in it is pushed too, since its .git
// keeps what no remote has (stashes, hooks, config, the reflog), and when git
// cannot read it at all, where git would look past it to the repository
// above (#312).
func TestReapHoldsARepositoryAtTheWorkspaceRoot(t *testing.T) {
	cases := map[string]func(t *testing.T, root, ws string){
		"a note committed there": func(t *testing.T, _, ws string) {
			gitOut(t, ws, "init", "-q", "-b", "main")
			commitFile(t, ws, "notes.md", "findings, committed only here\n", "notes")
		},
		"everything pushed": func(t *testing.T, root, ws string) {
			remote := filepath.Join(root, "notes.git")
			gitOut(t, "", "init", "-q", "--bare", "-b", "main", remote)
			gitOut(t, ws, "init", "-q", "-b", "main")
			commitFile(t, ws, ".gitignore", "repo/\n", "ignore the clone")
			commitFile(t, ws, "notes.md", "findings\n", "notes")
			gitOut(t, ws, "remote", "add", "origin", remote)
			gitOut(t, ws, "push", "-q", "origin", "main")
		},
		"a .git git cannot read, inside an enclosing repository": func(t *testing.T, root, ws string) {
			enclosing := filepath.Join(root, "Workspaces")
			gitOut(t, enclosing, "init", "-q", "-b", "main")
			commitFile(t, enclosing, ".gitignore", "delivery/repo/\n", "track the workspaces")
			if err := os.MkdirAll(filepath.Join(ws, ".git", "objects"), 0o777); err != nil {
				t.Fatal(err)
			}
			writeTestFile(t, filepath.Join(ws, ".git", "objects", "pack-history"), "the only copy of a history whose HEAD was lost\n")
		},
	}
	for name, setUp := range cases {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			ws, _ := longLivedWorkspace(t, root)
			setUp(t, root, ws)

			st, err := Inspect(ws, gitx.Git{}, nil, nil)
			if err != nil {
				t.Fatalf("Inspect: %v", err)
			}
			if !blockersMention(st.Blockers(), "is itself a git repository") {
				t.Errorf("reap would delete the repository at the workspace root: Blockers() = %v", st.Blockers())
			}
		})
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
	if err := Reap(st, gitx.Git{}); err != nil {
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

// A repository inside a directory the clone ignores (a fork cloned into
// vendor/ to patch, a scratch clone in tmp/) has commits of its own, and is
// more than an ignored file: prune keeps such a directory, and reap holds on
// it. In a worktree clone too, since the ignored directory goes with it.
func TestReapHoldsARepositoryInAnIgnoredDirectory(t *testing.T) {
	for _, worktree := range []bool{false, true} {
		t.Run(map[bool]string{false: "own clone", true: "worktree clone"}[worktree], func(t *testing.T) {
			roots, ws, _, checkout := worktreeSetup(t)
			excludeIn(t, checkout, "vendor/")
			if err := Sync(roots, ws, gitx.Git{}, quiet(), SyncOptions{Worktrees: worktree}); err != nil {
				t.Fatal(err)
			}
			clone := filepath.Join(ws, "repo")
			if !worktree {
				excludeIn(t, clone, "vendor/")
			}
			lib := filepath.Join(clone, "vendor", "lib")
			gitOut(t, "", "init", "-q", lib)
			writeTestFile(t, filepath.Join(lib, "fix.txt"), "local fix, never pushed\n")
			gitOut(t, lib, "add", "fix.txt")
			gitOut(t, lib, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-qm", "fix")
			if got := gitOut(t, clone, "status", "--porcelain"); got != "" {
				t.Fatalf("the clone should read clean, vendor/ being ignored: %q", got)
			}
			st, err := Inspect(ws, gitx.Git{}, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			if !hasBlocker(st, filepath.Join("vendor", "lib")) {
				t.Fatalf("reap would delete vendor/lib and its commit: %v", st.Blockers())
			}

			// An ignored directory with no repository in it holds as no
			// repository; vendor/ is nothing a tool rebuilds, so its files
			// hold as ignored files (#364, part 7).
			if err := os.RemoveAll(lib); err != nil {
				t.Fatal(err)
			}
			writeTestFile(t, filepath.Join(clone, "vendor", "cache.bin"), "hand-made\n")
			st, err = Inspect(ws, gitx.Git{}, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			if hasBlocker(st, "repository") || !hasBlocker(st, "files git ignores that nothing rebuilds: vendor/") {
				t.Fatalf("an ignored directory with no repository: %v", st.Blockers())
			}
		})
	}
}

// A hand clone beside the clones, in a ~/Workspaces committed with
// `git add -A`, is recorded as a gitlink: the sha of its HEAD and none of its
// commits. Status then read clean, and reap deleted the only copy (#319).
func TestReapHoldsAHandCloneTheEnclosingRepoHasAsAGitlink(t *testing.T) {
	root := t.TempDir()
	ws, _ := longLivedWorkspace(t, root)
	enc := enclosingRepoIn(t, root)
	hand := filepath.Join(ws, "hand")
	mustGit(t, "", "init", "-q", "-b", "main", hand)
	commitFile(t, hand, "fix.txt", "never pushed\n", "fix")
	rel, err := filepath.Rel(enc, ws)
	if err != nil {
		t.Fatal(err)
	}
	mustGit(t, enc, "add", "--", filepath.Join(rel, "hand"), filepath.Join(rel, ".workspace.json"))
	mustGit(t, enc, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-qm", "track")
	if b := blockersOf(t, ws); !blockersMention(b, "hand is a repository of its own") {
		t.Fatalf("hand/, whose commit the enclosing repository records only by sha, does not hold: %v", b)
	}
}

// diff.ignoreSubmodules=all in the enclosing repository hid a gitlinked hand
// clone that moved on, with a new commit and an untracked file. Every
// repository beside the clones holds now; the status that judges the other
// names ignores no submodule either (#319).
func TestReapHoldsAGitlinkedHandCloneThatMovedOn(t *testing.T) {
	root := t.TempDir()
	ws, _ := longLivedWorkspace(t, root)
	enc := enclosingRepoIn(t, root)
	remote := filepath.Join(root, "hand.git")
	mustGit(t, "", "init", "-q", "--bare", remote)
	hand := filepath.Join(ws, "hand")
	mustGit(t, "", "clone", "-q", "--", remote, hand)
	commitFile(t, hand, "a.txt", "pushed\n", "a")
	mustGit(t, hand, "push", "-q", "origin", "HEAD:refs/heads/main")
	rel, err := filepath.Rel(enc, ws)
	if err != nil {
		t.Fatal(err)
	}
	mustGit(t, enc, "add", "--", filepath.Join(rel, "hand"), filepath.Join(rel, ".workspace.json"))
	mustGit(t, enc, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-qm", "track")
	mustGit(t, enc, "config", "diff.ignoreSubmodules", "all")
	commitFile(t, hand, "more.txt", "never pushed\n", "more")
	writeTestFile(t, filepath.Join(hand, "untracked.txt"), "only copy\n")
	if b := blockersOf(t, ws); !blockersMention(b, "hand is a repository of its own") {
		t.Fatalf("hand/ with a new commit and an untracked file, under diff.ignoreSubmodules=all, does not hold: %v", b)
	}
}

// A repository of another VCS, or a bare one, in a directory the clone
// ignores was walked past: only a .git counted. A bare backup the clone pushes
// to also made its commits read as pushed, so reap deleted both copies (#336).
func TestReapHoldsAnyRepositoryInAnIgnoredDirectory(t *testing.T) {
	for _, tc := range []struct {
		name  string
		build func(t *testing.T, clone, dir string)
	}{
		{"bare git backup", func(t *testing.T, clone, dir string) {
			mustGit(t, "", "init", "-q", "--bare", dir)
			mustGit(t, clone, "remote", "add", "backup", dir)
			commitFile(t, clone, "work.txt", "pushed only to the backup\n", "work")
			mustGit(t, clone, "push", "-q", "backup", "HEAD:refs/heads/wip")
		}},
		{"mercurial checkout", func(t *testing.T, _, dir string) {
			mkdirs(t, filepath.Join(dir, ".hg", "store"))
			writeTestFile(t, filepath.Join(dir, ".hg", "store", "00changelog.i"), "history\n")
			writeTestFile(t, filepath.Join(dir, "fix.txt"), "a local fix\n")
		}},
		{"fossil repository file", func(t *testing.T, _, dir string) {
			mkdirs(t, dir)
			writeTestFile(t, filepath.Join(dir, "notes.fossil"), "SQLite format 3\n")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws, clone := longLivedWorkspace(t, t.TempDir())
			excludeIn(t, clone, "vendor/")
			dir := filepath.Join(clone, "vendor", "lib")
			tc.build(t, clone, dir)
			if got := gitOut(t, clone, "status", "--porcelain"); got != "" {
				t.Fatalf("the clone should read clean, vendor/ being ignored: %q", got)
			}
			st, err := Inspect(ws, gitx.Git{}, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			if !hasBlocker(st, filepath.Join("vendor", "lib")) {
				t.Fatalf("reap would delete vendor/lib: %v", st.Blockers())
			}
		})
	}
}

func mkdirs(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

// A note committed as Notes.md and renamed to notes.md on a case-insensitive
// disk (macOS's default) matched neither the index entry nor an untracked
// file as the pathspec notes.md, so its later edits were deleted unseen
// (#320). TestUnderNameFoldsCase holds the matching on every disk.
func TestReapHoldsAnEditedNoteRenamedInCaseOnly(t *testing.T) {
	root := t.TempDir()
	ws, _ := longLivedWorkspace(t, root)
	writeTestFile(t, filepath.Join(root, "Probe"), "")
	if _, err := os.Stat(filepath.Join(root, "probe")); err != nil {
		t.Skip("needs a case-insensitive disk (the macOS job runs it)")
	}
	enc := enclosingRepoIn(t, root)
	rel, err := filepath.Rel(enc, ws)
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(ws, "Notes.md"), "committed\n")
	mustGit(t, enc, "add", "--", filepath.Join(rel, "Notes.md"))
	mustGit(t, enc, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-qm", "notes")
	if err := os.Rename(filepath.Join(ws, "Notes.md"), filepath.Join(ws, "notes.md")); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(ws, "notes.md"), "committed\nedited since, only copy\n")
	if b := blockersOf(t, ws); !blockersMention(b, "otes.md is not committed") {
		t.Fatalf("notes.md, edited after a rename in case only, does not hold: %v", b)
	}
}
