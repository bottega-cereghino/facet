package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/bottega-cereghino/facet/internal/gitx"
)

// pushEnclosing gives the enclosing repository enc a bare remote and pushes
// its HEAD there, as ~/Workspaces is pushed.
func pushEnclosing(t *testing.T, enc string) {
	t.Helper()
	bare := filepath.Join(t.TempDir(), "enclosing.git")
	gitOut(t, "", "init", "-q", "--bare", bare)
	gitOut(t, enc, "remote", "add", "origin", bare)
	gitOut(t, enc, "push", "-q", "origin", "HEAD:refs/heads/main")
	gitOut(t, enc, "fetch", "-q", "origin")
}

// censusOf inspects ws and returns what the census holds.
func censusOf(t *testing.T, ws string) []string {
	t.Helper()
	st, err := Inspect(ws, gitx.Git{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if (st.UnaccountedCount > 0) != (len(st.Unaccounted) > 0) {
		t.Fatalf("count %d disagrees with the list %v", st.UnaccountedCount, st.Unaccounted)
	}
	return st.Unaccounted
}

func censusNames(held []string, path string) bool {
	for _, h := range held {
		if strings.HasPrefix(h, path+" ") {
			return true
		}
	}
	return false
}

// A clean clone of pushed work holds nothing in the census, and neither do
// facet's own files or what something rebuilds.
func TestCensusAccountsForPushedWork(t *testing.T) {
	ws, clone := issueWorkspace(t)
	excludeIn(t, clone, "node_modules/")
	mkdirs(t, filepath.Join(clone, "node_modules", "dep"))
	writeTestFile(t, filepath.Join(clone, "node_modules", "dep", "index.js"), "rebuilt\n")
	writeTestFile(t, filepath.Join(ws, ".DS_Store"), "finder\n")
	if held := censusOf(t, ws); len(held) != 0 {
		t.Fatalf("the census holds pushed work: %v", held)
	}
}

// Every way a file can be the only copy holds, whichever check also sees it:
// an untracked file, a commit no remote has, a file in a .git of another
// case, a fifo.
func TestCensusHoldsWhatNoRemoteHas(t *testing.T) {
	ws, clone := issueWorkspace(t)
	writeTestFile(t, filepath.Join(clone, "draft.txt"), "untracked\n")
	commitFile(t, clone, "local.txt", "committed, never pushed\n", "local")
	mkdirs(t, filepath.Join(clone, "notes", ".Git"))
	writeTestFile(t, filepath.Join(clone, "notes", ".Git", "todo.txt"), "a .git of another case\n")
	if err := syscall.Mkfifo(filepath.Join(clone, "pipe"), 0o600); err != nil {
		t.Fatal(err)
	}
	held := censusOf(t, ws)
	for _, want := range []string{"repo/draft.txt", "repo/local.txt", "repo/notes/.Git/todo.txt", "repo/pipe"} {
		if !censusNames(held, want) {
			t.Errorf("the census lets %s go: %v", want, held)
		}
	}
}

// A file checked out with eol=crlf (gradle writes `*.bat text eol=crlf` into
// every project) is on disk with CRLF while its pushed blob has LF; git status
// calls it clean. It is exactly what checkout writes from that blob, so it
// goes (#402). A copy that differs from what checkout would write, here one
// bare LF that clean conversion folds into the same blob, holds.
func TestCensusAccountsForWhatCheckoutWritesFromAPushedBlob(t *testing.T) {
	ws, clone := issueWorkspace(t)
	writeTestFile(t, filepath.Join(clone, ".gitattributes"), "*.bat text eol=crlf\n")
	writeTestFile(t, filepath.Join(clone, "gradlew.bat"), "@echo off\r\nrem start\r\n")
	gitOut(t, clone, "add", ".gitattributes", "gradlew.bat")
	gitOut(t, clone, "commit", "-qm", "wrapper")
	gitOut(t, clone, "push", "-q", "origin", "HEAD:refs/heads/wrapper")
	if out := gitOut(t, clone, "status", "--porcelain"); out != "" {
		t.Fatalf("setup: the clone should be clean, got %q", out)
	}
	if held := censusOf(t, ws); len(held) != 0 {
		t.Fatalf("the census holds a pushed file as checkout wrote it: %v", held)
	}

	writeTestFile(t, filepath.Join(clone, "gradlew.bat"), "@echo off\nrem start\r\n")
	if held := censusOf(t, ws); !censusNames(held, "repo/gradlew.bat") {
		t.Fatalf("the census lets go a file checkout would not write: %v", held)
	}
}

// A loose file the enclosing repository committed but never pushed has no
// other copy off this disk, so it holds; pushed, it does not.
func TestCensusHoldsALooseFileCommittedButNotPushed(t *testing.T) {
	root := t.TempDir()
	ws, _ := longLivedWorkspace(t, root)
	enc := enclosingRepoIn(t, root)
	writeTestFile(t, filepath.Join(enc, ".gitignore"), "delivery/repo/\n")
	writeTestFile(t, filepath.Join(ws, "notes.md"), "notes\n")
	mustGit(t, enc, "add", ".")
	mustGit(t, enc, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-qm", "track")
	if held := censusOf(t, ws); !censusNames(held, "notes.md") {
		t.Fatalf("committed and never pushed, notes.md is let go: %v", held)
	}
	pushEnclosing(t, enc)
	if held := censusOf(t, ws); len(held) != 0 {
		t.Fatalf("pushed, the workspace still holds: %v", held)
	}
}

// A file of a repository no check judges holds, and so does its git
// directory: the census skips only a judged repository's.
func TestCensusHoldsAnUnjudgedRepository(t *testing.T) {
	ws, clone := issueWorkspace(t)
	inner := filepath.Join(clone, "vendor-tool")
	gitOut(t, "", "init", "-q", "--template=", inner)
	writeTestFile(t, filepath.Join(inner, "main.go"), "package main\n")
	held := censusOf(t, ws)
	if !censusNames(held, "repo/vendor-tool/main.go") {
		t.Errorf("a file of an inner repository is let go: %v", held)
	}
	if !censusNames(held, "repo/vendor-tool/.git/HEAD") {
		t.Errorf("an inner repository's git directory is let go: %v", held)
	}
}

// A file checked out with the ident attribute is the pushed blob with its
// keyword expanded, so it does not hold; a note written into the keyword,
// even one that keeps the size, does (#385).
func TestCensusReadsTheIdentKeywordExactly(t *testing.T) {
	ws, clone := issueWorkspace(t)
	writeTestFile(t, filepath.Join(clone, ".gitattributes"), "*.txt ident\n")
	gitOut(t, clone, "add", ".gitattributes")
	commitFile(t, clone, "v.txt", "version: $Id$\n", "ident")
	gitOut(t, clone, "push", "-q", "origin", "HEAD:refs/heads/ident")
	gitOut(t, clone, "fetch", "-q", "origin")
	gitOut(t, clone, "rm", "-q", "--cached", "v.txt")
	gitOut(t, clone, "reset", "-q", "--hard")
	b, err := os.ReadFile(filepath.Join(clone, "v.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "$Id: ") {
		t.Fatalf("setup: checkout did not expand the keyword: %q", b)
	}
	if held := censusOf(t, ws); len(held) != 0 {
		t.Fatalf("an expanded keyword holds: %v", held)
	}
	writeTestFile(t, filepath.Join(clone, "v.txt"), "version: $Id: call alice re key 0123456789abcdefghijkl $\n")
	if out := gitOut(t, clone, "status", "--porcelain"); out != "" {
		t.Fatalf("setup: git status should call the edit unchanged, got %q", out)
	}
	if held := censusOf(t, ws); !censusNames(held, "repo/v.txt") {
		t.Fatalf("a note in the keyword is let go: %v", held)
	}
}

// A plain worktree reaped by path is censused too, against its checkout's
// remotes.
func TestCensusOfAPlainWorktree(t *testing.T) {
	_, _, _, checkout := worktreeSetup(t)
	wt := plainWorktree(t, checkout, "topic")
	st, err := InspectWorktree(wt, gitx.Git{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if st.UnaccountedCount != 0 {
		t.Fatalf("a fresh worktree holds: %v", st.Unaccounted)
	}
	writeTestFile(t, filepath.Join(wt, "draft.txt"), "only copy\n")
	if st, err = InspectWorktree(wt, gitx.Git{}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if !censusNames(st.Unaccounted, "draft.txt") {
		t.Fatalf("the worktree's draft is let go: %v", st.Unaccounted)
	}
}

// The list is capped, the count is not, and the blocker says both.
func TestCensusCapsItsList(t *testing.T) {
	ws, clone := issueWorkspace(t)
	for i := range maxUnaccounted + 3 {
		writeTestFile(t, filepath.Join(clone, "n"+string(rune('a'+i))), "x\n")
	}
	st, err := Inspect(ws, gitx.Git{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Unaccounted) != maxUnaccounted || st.UnaccountedCount != maxUnaccounted+3 {
		t.Fatalf("listed %d of %d", len(st.Unaccounted), st.UnaccountedCount)
	}
	if b := st.unaccountedBlocker(); !strings.Contains(b, "and 3 more") {
		t.Fatalf("the blocker does not count the rest: %s", b)
	}
}

// facet's own names are let go only as regular files: a directory at one,
// even a repository with commits, is read like any loose entry (#401).
func TestReapReadsWhatIsAtAnOwnFilesName(t *testing.T) {
	for _, name := range []string{".seat", ".scope", ".DS_Store", syncLockName} {
		t.Run(name, func(t *testing.T) {
			ws, _ := issueWorkspace(t)
			dir := filepath.Join(ws, name)
			mustGit(t, "", "init", "-q", dir)
			commitFile(t, dir, "notes.md", "a week of notes\n", "notes")
			st, err := Inspect(ws, gitx.Git{}, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			if !blockersMention(st.Blockers(), name) {
				t.Fatalf("a repository at %s is let go: %v", name, st.Blockers())
			}
			if st.UnaccountedCount == 0 {
				t.Errorf("the census lets %s/ go unread", name)
			}
		})
	}
}
