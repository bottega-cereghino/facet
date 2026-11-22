package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bottega-cereghino/facet/internal/gitx"
)

// The top level of a workspace holds on anything reap cannot account for
// (#364): a loose file counts only when the enclosing repository has
// committed it at that exact path with the same bytes.

// An enclosing repository that ignores the workspace itself (issue
// workspaces are "ephemeral and gitignored", says Dirs) answers the status
// over "." with one record, "!! <workspace>/", which underName never matches
// to a name inside it. A note beside the clones then read as committed
// (#353).
func TestReapHoldsANoteInAWorkspaceTheEnclosingRepoIgnores(t *testing.T) {
	root := t.TempDir()
	ws, _ := longLivedWorkspace(t, root)
	enc := enclosingRepoIn(t, root)
	writeTestFile(t, filepath.Join(enc, ".gitignore"), filepath.Base(ws)+"/\n")
	mustGit(t, enc, "add", "--", ".gitignore")
	mustGit(t, enc, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-qm", "ignore the workspace")
	writeTestFile(t, filepath.Join(ws, "notes.md"), "the only copy of my notes\n")

	if b := blockersOf(t, ws); !blockersMention(b, "notes.md") {
		t.Fatalf("notes.md exists nowhere else, and reap would delete it: %v", b)
	}
}

// splitRepositories looks for a repository only at the top of each name. A
// hand clone one level down, which `git add -A` in ~/Workspaces records as a
// gitlink (its HEAD's sha, none of its commits), reads clean in the status,
// so its local commit was deleted with the workspace (#354).
func TestReapHoldsAGitlinkedHandCloneOneLevelDown(t *testing.T) {
	root := t.TempDir()
	ws, _ := longLivedWorkspace(t, root)
	enc := enclosingRepoIn(t, root)
	hand := filepath.Join(ws, "scratch", "hand")
	mustGit(t, "", "init", "-q", "-b", "main", hand)
	commitFile(t, hand, "fix.txt", "never pushed\n", "fix")
	mustGit(t, enc, "add", "-A")
	mustGit(t, enc, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-qm", "track")

	if b := blockersOf(t, ws); !blockersMention(b, "hand") && !blockersMention(b, "scratch") {
		t.Fatalf("scratch/hand holds a commit no other repository has, and reap would delete it: %v", b)
	}
}

// os.ReadDir gives a name as the disk keeps it; git on macOS
// (core.precomposeUnicode, set by git init there) prints it precomposed. A
// note whose name the disk keeps decomposed (copied from an HFS+ volume, an
// archive, many tools) was never matched to its status record (#355).
func TestReapHoldsANoteWhoseNameIsDecomposed(t *testing.T) {
	root := t.TempDir()
	ws, _ := longLivedWorkspace(t, root)
	enc := enclosingRepoIn(t, root)
	// Only git on macOS precomposes names; elsewhere git prints the name the
	// disk keeps, and the two never differ.
	if v, err := (gitx.Git{}).Run(enc, nil, "config", "--get", "core.precomposeunicode"); err != nil || strings.TrimSpace(v) != "true" {
		t.Skip("needs git's precomposeUnicode (macOS)")
	}
	nfd := "café.md" // e + combining acute accent
	writeTestFile(t, filepath.Join(ws, nfd), "the only copy of my notes\n")
	entries, err := os.ReadDir(ws)
	if err != nil {
		t.Fatal(err)
	}
	kept := false
	for _, e := range entries {
		kept = kept || e.Name() == nfd
	}
	if !kept {
		t.Skip("the disk does not keep a decomposed name")
	}

	// Control: the same note under its precomposed name holds.
	root2 := t.TempDir()
	ws2, _ := longLivedWorkspace(t, root2)
	enclosingRepoIn(t, root2)
	writeTestFile(t, filepath.Join(ws2, "café.md"), "the only copy of my notes\n")
	if b := blockersOf(t, ws2); len(b) == 0 {
		t.Fatalf("control: the precomposed name does not hold either")
	}

	if b := blockersOf(t, ws); len(b) == 0 {
		t.Fatalf("%q exists nowhere else, and reap would delete it: %v", nfd, b)
	}
}

// A file the enclosing repository has committed at that path with the same
// bytes is accounted for; staged and not committed, or committed with other
// bytes, it holds.
func TestReapAccountsOnlyForCommittedBytes(t *testing.T) {
	root := t.TempDir()
	ws, _ := longLivedWorkspace(t, root)
	enc := enclosingRepoIn(t, root)
	rel, err := filepath.Rel(enc, ws)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(ws, "docs"), 0o777); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(ws, "docs", "plan.md"), "the plan\n")
	mustGit(t, enc, "add", "--", filepath.Join(rel, "docs", "plan.md"))
	mustGit(t, enc, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-qm", "plan")
	if b := blockersOf(t, ws); blockersMention(b, "plan.md") {
		t.Fatalf("docs/plan.md is committed with these bytes, yet it holds: %v", b)
	}

	writeTestFile(t, filepath.Join(ws, "docs", "plan.md"), "the plan\nand a staged edit\n")
	mustGit(t, enc, "add", "--", filepath.Join(rel, "docs", "plan.md"))
	if b := blockersOf(t, ws); !blockersMention(b, "docs/plan.md differs from what is committed") {
		t.Fatalf("a staged edit is not committed, and reap would delete it: %v", b)
	}
}

// A symlink is skipped only as a link the manifest declares. An undeclared one
// is judged as any loose entry.
func TestReapHoldsAnUndeclaredSymlink(t *testing.T) {
	root := t.TempDir()
	ws, _ := longLivedWorkspace(t, root)
	enclosingRepoIn(t, root)
	if err := os.Symlink("/nowhere/in/particular", filepath.Join(ws, "shortcut")); err != nil {
		t.Fatal(err)
	}
	if b := blockersOf(t, ws); !blockersMention(b, "shortcut is not committed") {
		t.Fatalf("an undeclared symlink is not accounted for, yet reap would delete it: %v", b)
	}
}

// A repository marker of any kind, at any depth of a loose directory, holds,
// whatever the enclosing repository has recorded.
func TestReapHoldsARepositoryDeepInALooseDirectory(t *testing.T) {
	root := t.TempDir()
	ws, _ := longLivedWorkspace(t, root)
	enclosingRepoIn(t, root)
	if err := os.MkdirAll(filepath.Join(ws, "a", "b", "c", ".hg"), 0o777); err != nil {
		t.Fatal(err)
	}
	if b := blockersOf(t, ws); !blockersMention(b, "a/b/c is a repository of its own") {
		t.Fatalf("a Mercurial repository three levels down does not hold: %v", b)
	}
}
