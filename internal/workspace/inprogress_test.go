package workspace

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/bottega-cereghino/facet/internal/gitx"
)

// A rebase stopped at a break leaves a clean tree, an empty stash list and HEAD
// on pushed commits; the edits it autostashed are named only by a file in
// rebase-merge (#379).
func TestReapHoldsARebaseInProgress(t *testing.T) {
	ws, clone := issueWorkspace(t)
	commitFile(t, clone, "second.txt", "second\n", "second")
	gitOut(t, clone, "push", "-q", "origin", "HEAD:refs/heads/second")
	writeTestFile(t, filepath.Join(clone, "README"), "an edit nobody committed\n")
	// Stop the rebase before its first pick, as `break` or `edit` would.
	editor := filepath.Join(t.TempDir(), "seq-editor")
	writeTestFile(t, editor, "#!/bin/sh\nprintf 'break\\n' | cat - \"$1\" > \"$1.new\" && mv \"$1.new\" \"$1\"\n")
	if err := os.Chmod(editor, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := (gitx.Git{}).Run(clone, []string{"GIT_SEQUENCE_EDITOR=" + editor},
		"-c", "user.name=t", "-c", "user.email=t@example.com", "rebase", "-q", "-i", "--autostash", "HEAD~1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(clone, ".git", "rebase-merge", "autostash")); err != nil {
		t.Fatalf("setup: the rebase should have stopped holding an autostash: %v", err)
	}
	if out := gitOut(t, clone, "status", "--porcelain"); out != "" {
		t.Fatalf("setup: the work tree should read clean, got %q", out)
	}
	st, err := InspectIssue(ws, gitx.Git{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !hasBlocker(st, "a rebase in progress") {
		t.Fatalf("the README edit is only in the rebase's autostash; blockers = %v", st.Blockers())
	}
}

// A clean clone has nothing in progress.
func TestOpsInProgressNoneInACleanClone(t *testing.T) {
	_, clone := issueWorkspace(t)
	ops, err := opsInProgress(gitx.Git{}, clone)
	if err != nil || len(ops) != 0 {
		t.Fatalf("opsInProgress = %v, %v; want none", ops, err)
	}
}
