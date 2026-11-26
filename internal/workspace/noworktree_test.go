package workspace

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/bottega-cereghino/facet/internal/gitx"
)

// The queue syncs with Worktrees off and NoHardlinks on: the clone has a .git
// directory of its own and origin is the repository's remote, though a checkout
// of that repository sits under the projects root.
func TestSyncWithoutWorktreesClonesEvenWhereACheckoutExists(t *testing.T) {
	t.Parallel()
	roots, ws, origin, _ := worktreeSetup(t)
	if err := Sync(roots, ws, gitx.Git{}, quiet(), SyncOptions{NoHardlinks: true}); err != nil {
		t.Fatal(err)
	}
	clone := filepath.Join(ws, "repo")
	if isGitFile(t, clone) {
		t.Fatal("the clone has a .git file; want a .git directory")
	}
	if got := strings.TrimSpace(gitOut(t, clone, "remote", "get-url", "origin")); got != origin {
		t.Errorf("origin = %q, want %q", got, origin)
	}
}
