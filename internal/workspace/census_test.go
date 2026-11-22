package workspace

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/bottega-cereghino/facet/internal/gitx"
	"github.com/bottega-cereghino/facet/internal/manifest"
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

// What something rebuilds is named relative to the work tree of the
// repository holding it, never across a repository's boundary (#400). A
// clone named dist, target or node_modules is a repository, not a build
// output, so the census looks inside it; so is a repository at a rebuilt name
// inside a clone. A file only the census sees (one in a .Git of another case)
// holds in each.
func TestCensusLooksInsideRepositoriesAtRebuiltNames(t *testing.T) {
	for _, name := range []string{"repo", "dist", "target", "node_modules"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			origin := originRepo(t, filepath.Join(root, "origin"))
			ws := filepath.Join(root, "iss-repo-1-x")
			mkdirs(t, ws)
			m := &manifest.Manifest{
				Name:   "iss-repo-1-x",
				Clones: map[string]string{name: origin},
				Issue:  &manifest.Issue{Repo: "o/repo", Number: 1, Branch: "1-x", Home: name},
			}
			if err := m.Write(ws); err != nil {
				t.Fatal(err)
			}
			if err := Sync(testRoots(root), ws, gitx.Git{}, quiet(), SyncOptions{}); err != nil {
				t.Fatal(err)
			}
			mkdirs(t, filepath.Join(ws, name, "notes", ".Git"))
			writeTestFile(t, filepath.Join(ws, name, "notes", ".Git", "todo.txt"), "only copy\n")
			if held := censusOf(t, ws); !censusNames(held, name+"/notes/.Git/todo.txt") {
				t.Fatalf("the census skips the clone %s: %v", name, held)
			}
		})
	}

	t.Run("a repository at a rebuilt name in a clone", func(t *testing.T) {
		ws, clone := issueWorkspace(t)
		excludeIn(t, clone, "node_modules/")
		inner := filepath.Join(clone, "node_modules", "fork")
		mkdirs(t, filepath.Join(clone, "node_modules"))
		gitOut(t, "", "init", "-q", filepath.Join(clone, "node_modules"))
		mkdirs(t, inner)
		writeTestFile(t, filepath.Join(inner, "patch.js"), "a local fix, committed nowhere\n")
		// Nothing judges the inner repository, so its git directory holds
		// as well, and the capped list may not reach patch.js.
		held := censusOf(t, ws)
		if len(held) == 0 || !strings.HasPrefix(held[0], "repo/node_modules/") {
			t.Fatalf("the census skips a repository named node_modules: %v", held)
		}
	})
}

// A pushed Git LFS pointer names content; it does not show that any LFS store
// has it. A push with --no-verify, or from a clone with no pre-push hook,
// sends the pointer and never the object, and facet reads no store, so a file
// only its pointer accounts for holds, saying how to clear it (#397). The
// same file left as its pointer (a checkout that skipped the smudge) is the
// pushed blob itself and goes.
func TestCensusHoldsLFSContentOnlyItsPointerAccountsFor(t *testing.T) {
	ws, clone := issueWorkspace(t)
	content := "a render, made today, uploaded nowhere\n"
	pointer := fmt.Sprintf("version https://git-lfs.github.com/spec/v1\noid sha256:%x\nsize %d\n",
		sha256.Sum256([]byte(content)), len(content))
	pointerFile := filepath.Join(t.TempDir(), "pointer")
	writeTestFile(t, pointerFile, pointer)
	writeTestFile(t, filepath.Join(clone, ".gitattributes"), "*.bin filter=media\n")
	mustGit(t, clone, "config", "filter.media.clean", "cat '"+pointerFile+"'")
	mustGit(t, clone, "config", "filter.media.smudge", "cat")
	gitOut(t, clone, "add", ".gitattributes")
	commitFile(t, clone, "render.bin", content, "render")
	mustGit(t, clone, "push", "-q", "--no-verify", "origin", "HEAD:refs/heads/render")
	held := censusOf(t, ws)
	if len(held) != 1 || !censusNames(held, "repo/render.bin") || !strings.Contains(held[0], "git lfs push") {
		t.Fatalf("the census should hold render.bin alone, saying how to push its object: %v", held)
	}

	writeTestFile(t, filepath.Join(clone, "render.bin"), pointer)
	if held := censusOf(t, ws); len(held) != 0 {
		t.Fatalf("a file that is its pushed pointer holds: %v", held)
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
