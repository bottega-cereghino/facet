package workspace

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bottega-cereghino/facet/internal/gitx"
)

// The tests here hold the guarantee that reap sees every modified file
// whatever git's config says: git status trusts the index's marks, the clean
// filters, the fsmonitor hook and core.worktree, and each of them can hide an
// edit that exists nowhere else (#318).

// blockersOf inspects ws and returns what holds it.
func blockersOf(t *testing.T, ws string) []string {
	t.Helper()
	st, err := Inspect(ws, gitx.Git{}, nil, nil)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	return st.Blockers()
}

// enclosingRepoIn makes root/Workspaces a repository, as ~/Workspaces usually
// is, and returns it.
func enclosingRepoIn(t *testing.T, root string) string {
	t.Helper()
	enc := filepath.Join(root, "Workspaces")
	mustGit(t, enc, "init", "-q", "-b", "main")
	return enc
}

// An edit to a tracked file marked assume-unchanged or skip-worktree, the
// usual way to keep a local config change out of git status, is not shown by
// git status, so the clone read clean.
func TestReapSeesAnEditBehindAnIndexBit(t *testing.T) {
	t.Parallel()
	for _, bit := range []string{"--assume-unchanged", "--skip-worktree"} {
		t.Run(bit, func(t *testing.T) {
			ws, clone := longLivedWorkspace(t, t.TempDir())
			writeTestFile(t, filepath.Join(clone, "README"), "hello\nlocal edit, only copy\n")
			mustGit(t, clone, "update-index", bit, "README")
			if b := blockersOf(t, ws); !blockersMention(b, "README") {
				t.Fatalf("reap would delete README's edit behind %s: %v", bit, b)
			}
		})
	}
}

// A mark on a file that matches what is committed hides nothing, so it does
// not hold the workspace; a sparse checkout's skip-worktree files, absent from
// the disk, do not either.
func TestReapDoesNotHoldAnUneditedFileBehindAnIndexBit(t *testing.T) {
	t.Parallel()
	ws, clone := longLivedWorkspace(t, t.TempDir())
	mustGit(t, clone, "update-index", "--assume-unchanged", "README")
	commitFile(t, clone, "gone.txt", "committed\n", "gone")
	mustGit(t, clone, "push", "-q", "origin", "HEAD:refs/heads/gone")
	mustGit(t, clone, "update-index", "--skip-worktree", "gone.txt")
	if err := os.Remove(filepath.Join(clone, "gone.txt")); err != nil {
		t.Fatal(err)
	}
	if b := blockersOf(t, ws); len(b) != 0 {
		t.Fatalf("marked files that hold nothing of their own must not hold the workspace: %v", b)
	}
}

// A clean filter set in the clone's config (nbstripout's shape) maps the
// working file to what is committed, so content only the working copy holds
// read as unmodified.
func TestReapSeesContentACleanFilterHides(t *testing.T) {
	t.Parallel()
	ws, clone := longLivedWorkspace(t, t.TempDir())
	writeTestFile(t, filepath.Join(clone, ".git", "info", "attributes"), "README filter=strip\n")
	mustGit(t, clone, "config", "filter.strip.clean", "grep -v '^output:'")
	mustGit(t, clone, "config", "filter.strip.smudge", "cat")
	commitFile(t, clone, "README", "hello\nmore code\noutput: three hours of results, only copy\n", "code")
	mustGit(t, clone, "push", "-q", "origin", "HEAD:refs/heads/code")
	if b := blockersOf(t, ws); !blockersMention(b, "README") || !blockersMention(b, `"strip"`) {
		t.Fatalf("reap would delete the output lines the clean filter strips: %v", b)
	}
}

// Git LFS is a clean filter too: the blob committed is a pointer, never the
// file. A file whose content is the object its pointer names holds nothing
// the pointer does not name, so it is no hidden edit; it still holds the
// workspace, as LFS content no store is seen to have (#397). One edited since
// is a hidden edit, even when the filter (a stand-in that always prints the
// same pointer here) maps both to the same blob.
func TestReapJudgesAnLFSFileByThePointerItsBlobHolds(t *testing.T) {
	t.Parallel()
	ws, clone := longLivedWorkspace(t, t.TempDir())
	content := "a large binary, stored on the LFS server\n"
	pointer := fmt.Sprintf("version https://git-lfs.github.com/spec/v1\noid sha256:%x\nsize %d\n",
		sha256.Sum256([]byte(content)), len(content))
	pointerFile := filepath.Join(t.TempDir(), "pointer")
	writeTestFile(t, pointerFile, pointer)
	writeTestFile(t, filepath.Join(clone, ".git", "info", "attributes"), "*.bin filter=media\n")
	mustGit(t, clone, "config", "filter.media.clean", "cat '"+pointerFile+"'")
	mustGit(t, clone, "config", "filter.media.smudge", "cat")
	commitFile(t, clone, "asset.bin", content, "asset")
	mustGit(t, clone, "push", "-q", "origin", "HEAD:refs/heads/asset")
	if blob := gitOut(t, clone, "cat-file", "blob", ":asset.bin"); blob+"\n" != pointer {
		t.Fatalf("the committed blob should be the pointer, got %q", blob)
	}
	if b := blockersOf(t, ws); len(b) != 1 || !blockersMention(b, "Git LFS content") {
		t.Fatalf("a file that is the object its pointer names should hold only as LFS content no store is seen to have: %v", b)
	}

	// The same size: git status takes a change of size as a change without
	// asking the filter, and only an edit that keeps it reaches the filter.
	writeTestFile(t, filepath.Join(clone, "asset.bin"), strings.ToUpper(content))
	if got := gitOut(t, clone, "status", "--porcelain"); got != "" {
		t.Fatalf("the edit should read clean to git status, got %q", got)
	}
	if b := blockersOf(t, ws); !blockersMention(b, "asset.bin differs from what is committed") {
		t.Fatalf("reap would delete an edit to asset.bin its pointer does not name: %v", b)
	}
}

// core.fsmonitor, a config setting, lets a hook answer for which files
// changed; one that misses an event hid the edit from status.
func TestReapSeesAnEditTheFSMonitorMisses(t *testing.T) {
	t.Parallel()
	ws, clone := longLivedWorkspace(t, t.TempDir())
	hook := filepath.Join(t.TempDir(), "fsmonitor.sh")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nprintf 'token\\0'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustGit(t, clone, "config", "core.fsmonitor", hook)
	mustGit(t, clone, "status", "--porcelain")
	mustGit(t, clone, "status", "--porcelain")
	writeTestFile(t, filepath.Join(clone, "README"), "hello\nedit the monitor missed\n")
	if got := gitOut(t, clone, "status", "--porcelain"); got != "" {
		t.Fatalf("the hook should hide the edit from git status, got %q", got)
	}
	if b := blockersOf(t, ws); !blockersMention(b, "uncommitted") {
		t.Fatalf("reap would delete README's edit, hidden by core.fsmonitor: %v", b)
	}
}

// core.worktree in the clone's config points git at another directory, so the
// files in the clone's own directory were never looked at.
func TestReapSeesFilesWhenCoreWorktreePointsElsewhere(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	ws, clone := longLivedWorkspace(t, root)
	other := filepath.Join(root, "other")
	mustGit(t, "", "clone", "-q", "--", filepath.Join(root, "origin"), other)
	mustGit(t, clone, "config", "core.worktree", other)
	writeTestFile(t, filepath.Join(clone, "new.txt"), "only copy\n")
	if b := blockersOf(t, ws); !blockersMention(b, "core.worktree") {
		t.Fatalf("reap would delete new.txt in a clone whose core.worktree is elsewhere: %v", b)
	}
}

// An edit to a note committed in the enclosing repository, hidden there by an
// index bit, read as committed.
func TestReapHoldsAnEnclosingEditBehindAnIndexBit(t *testing.T) {
	t.Parallel()
	for _, bit := range []string{"--assume-unchanged", "--skip-worktree"} {
		t.Run(bit, func(t *testing.T) {
			root := t.TempDir()
			ws, _ := longLivedWorkspace(t, root)
			enc := enclosingRepoIn(t, root)
			commitFile(t, enc, "delivery/notes.md", "committed\n", "notes")
			writeTestFile(t, filepath.Join(ws, "notes.md"), "committed\nedited since, only copy\n")
			mustGit(t, enc, "update-index", bit, "delivery/notes.md")
			if b := blockersOf(t, ws); !blockersMention(b, "notes.md") {
				t.Fatalf("reap would delete notes.md's edit behind %s in the enclosing repository: %v", bit, b)
			}
		})
	}
}
