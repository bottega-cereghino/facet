package workspace

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bottega-cereghino/facet/internal/gitx"
	"github.com/bottega-cereghino/facet/internal/gitx/gitxtest"
)

// prunableWorkspace builds a long-lived workspace whose clone holds, beside its
// tracked files, every kind of directory prune must tell apart.
func prunableWorkspace(t *testing.T) (ws, clone string) {
	t.Helper()
	ws, clone = longLivedWorkspace(t, t.TempDir())
	// Only the root node_modules is ignored: sub/node_modules is untracked
	// work as far as git knows.
	writeTestFile(t, filepath.Join(clone, ".gitignore"), "/node_modules/\n.venv/\n.secrets/\n")
	mustGit(t, clone, "-c", "user.email=t@example.com", "-c", "user.name=t", "add", ".gitignore")
	// A directory named like a cache but committed: it is source here.
	writeMiB(t, filepath.Join(clone, ".terraform", "committed"))
	mustGit(t, clone, "add", ".terraform")
	mustGit(t, clone, "-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-qm", "tracked")

	writeMiB(t, filepath.Join(clone, "node_modules", "pkg", "index.js"))
	writeMiB(t, filepath.Join(clone, "api", ".venv", "lib", "site.py"))
	writeMiB(t, filepath.Join(clone, "sub", "node_modules", "mine.js"))
	writeMiB(t, filepath.Join(clone, ".secrets", "key"))
	// A worktree or nested repository has its own checkout to answer for.
	writeMiB(t, filepath.Join(clone, "nested", "node_modules", "x.js"))
	writeTestFile(t, filepath.Join(clone, "nested", ".git"), "gitdir: elsewhere\n")
	return ws, clone
}

func foundRels(s *PruneState) []string {
	var out []string
	for _, r := range s.Found {
		out = append(out, r.Clone+"/"+r.Rel)
	}
	return out
}

// Prune names only directories that are regenerable by name, ignored by git,
// free of tracked files, and outside nested checkouts. Anything else in the
// tree might be the only copy of something.
func TestInspectPruneFindsOnlyIgnoredRegenerableDirectories(t *testing.T) {
	ws, _ := prunableWorkspace(t)
	st, err := InspectPrune(ws, gitx.Git{}, nil, time.Now(), 0)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(foundRels(st), ",")
	if got != "repo/api/.venv,repo/node_modules" {
		t.Fatalf("found %q; want repo/api/.venv and repo/node_modules only", got)
	}
	if len(st.Held) != 0 {
		t.Errorf("held %v; want nothing holding a workspace with no idle window", st.Held)
	}
	if st.Frees() < 2*mib {
		t.Errorf("Frees() = %d; want at least the two MiB found", st.Frees())
	}
}

func TestPruneDeletesWhatWasFoundAndNothingElse(t *testing.T) {
	ws, clone := prunableWorkspace(t)
	st, err := InspectPrune(ws, gitx.Git{}, nil, time.Now(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := Prune(st); err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{"node_modules", "api/.venv"} {
		if _, err := os.Stat(filepath.Join(clone, gone)); !os.IsNotExist(err) {
			t.Errorf("%s survived prune (stat err %v)", gone, err)
		}
	}
	for _, kept := range []string{".terraform/committed", "sub/node_modules/mine.js", ".secrets/key", "nested/node_modules/x.js", "README"} {
		if _, err := os.Stat(filepath.Join(clone, kept)); err != nil {
			t.Errorf("%s is gone; prune must leave it: %v", kept, err)
		}
	}
	// What git tracks is untouched, so the clone reads exactly as before.
	if out, err := (gitx.Git{}).Run(clone, nil, "status", "--porcelain", "--untracked-files=no"); err != nil || out != "" {
		t.Errorf("git status after prune = %q, %v; want a clean tracked tree", out, err)
	}
}

// A workspace used recently is somebody's: its dependencies stay installed.
func TestInspectPruneHoldsAWorkspaceUsedWithinTheIdleWindow(t *testing.T) {
	ws, _ := prunableWorkspace(t)
	now := time.Now()
	st, err := InspectPrune(ws, gitx.Git{}, nil, now, 14*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Held) != 1 || !strings.Contains(st.Held[0], "idle window of 14 days") {
		t.Fatalf("held %v; want the idle window to hold a workspace used just now", st.Held)
	}
	if err := Prune(st); err == nil {
		t.Fatal("Prune deleted from a held workspace")
	}

	st, err = InspectPrune(ws, gitx.Git{}, nil, now.Add(30*24*time.Hour), 14*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Held) != 0 {
		t.Errorf("held %v thirty days on; want it prunable", st.Held)
	}
}

func TestInspectPruneHoldsAWorkspaceSomethingRunsFrom(t *testing.T) {
	ws, _ := prunableWorkspace(t)
	st, err := InspectPrune(ws, gitx.Git{}, fakeLiveRoots{roots: []string{"pid 42 (node)"}}, time.Now(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Held) != 1 || !strings.Contains(st.Held[0], "pid 42") {
		t.Fatalf("held %v; want the running process named", st.Held)
	}
}

// "Could not list what git ignores" must never read as "nothing is tracked".
func TestInspectPruneHoldsWhenIgnoredFilesCannotBeListed(t *testing.T) {
	ws, _ := prunableWorkspace(t)
	g := &gitxtest.Runner{
		Real: gitx.Git{},
		Fail: func(args []string) bool { return len(args) > 0 && args[0] == "ls-files" },
		Err:  errors.New("index file corrupt"),
	}
	st, err := InspectPrune(ws, g, nil, time.Now(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Found) != 0 || len(st.Held) != 1 || !strings.Contains(st.Held[0], "index file corrupt") {
		t.Fatalf("found %v, held %v; want nothing found and the failure holding it", foundRels(st), st.Held)
	}
}
