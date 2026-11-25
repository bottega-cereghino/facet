package main

import "testing"

// The queue's workspaces are clones that share no file with their source; a
// hand spawn keeps its worktrees (both flags off).
func TestQueueSpawnOptsCloneWithoutSharingFiles(t *testing.T) {
	o := queueSpawnOpts("acme/widgets", 7)
	if !o.NoWorktree || !o.NoHardlinks {
		t.Errorf("queue spawn opts = %+v; want NoWorktree and NoHardlinks", o)
	}
	if !o.Yes || o.Number != 7 || o.Repo != "acme/widgets" {
		t.Errorf("queue spawn opts = %+v; want the issue and Yes", o)
	}
	if hand := (spawnOpts{}); hand.NoWorktree || hand.NoHardlinks {
		t.Errorf("a hand spawn defaults to %+v; want worktrees", hand)
	}
}
