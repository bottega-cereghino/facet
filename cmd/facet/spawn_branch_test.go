package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bottega-cereghino/facet/internal/gitx"
)

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := gitx.Git{}.Run(dir, nil, args...)
	if err != nil {
		t.Fatalf("git %v in %s: %v", args, dir, err)
	}
	return out
}

// branchSetup makes an origin carrying the issue branch, and a checkout of it
// with a linked worktree, which shares the checkout's branches as a spawned
// worktree clone does.
func branchSetup(t *testing.T) (checkout, worktree string) {
	t.Helper()
	root := t.TempDir()
	origin := filepath.Join(root, "origin")
	if err := os.MkdirAll(origin, 0o777); err != nil {
		t.Fatal(err)
	}
	gitIn(t, origin, "init", "-q", "-b", "main")
	gitIn(t, origin, "-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "init")
	gitIn(t, origin, "branch", "1-x")
	checkout = filepath.Join(root, "checkout")
	gitIn(t, "", "clone", "-q", origin, checkout)
	worktree = filepath.Join(root, "wt")
	gitIn(t, checkout, "worktree", "add", "-q", "--detach", worktree, "origin/main")

	saved := git
	t.Cleanup(func() { git = saved })
	git = gitx.Git{}
	return checkout, worktree
}

func TestCheckoutIssueBranchKeepsALocalBranchAheadOfOrigin(t *testing.T) {
	checkout, wt := branchSetup(t)
	gitIn(t, checkout, "branch", "1-x", "origin/1-x")
	gitIn(t, checkout, "switch", "-q", "1-x")
	gitIn(t, checkout, "-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "only here")
	ahead := gitIn(t, checkout, "rev-parse", "1-x")
	gitIn(t, checkout, "switch", "-q", "main")

	err := checkoutIssueBranch(wt, "1-x")
	if err == nil || !strings.Contains(err.Error(), "fix:") {
		t.Fatalf("checkoutIssueBranch = %v; want a refusal that prints the fix", err)
	}
	if got := gitIn(t, checkout, "rev-parse", "1-x"); got != ahead {
		t.Errorf("the local branch moved from %s to %s", ahead, got)
	}
}

func TestCheckoutIssueBranchTakesALocalBranchOriginHas(t *testing.T) {
	checkout, wt := branchSetup(t)
	gitIn(t, checkout, "branch", "1-x", "origin/1-x")
	if err := checkoutIssueBranch(wt, "1-x"); err != nil {
		t.Fatal(err)
	}
	if got := gitIn(t, wt, "rev-parse", "--abbrev-ref", "HEAD"); got != "1-x" {
		t.Errorf("worktree is on %q; want 1-x", got)
	}
}
