package main

import (
	"strings"
	"testing"
)

func commitEmpty(t *testing.T, dir, msg string) {
	t.Helper()
	gitIn(t, dir, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "--allow-empty", "-m", msg)
}

// A branch whose tip is the merged head counts as landed, though GitHub's
// lookup knows nothing; one commit beyond that head holds the workspace.
func TestReapMergedHeadClearsOnlyTheMergedHead(t *testing.T) {
	root, _, _ := reapableRoot(t)
	ws, app := homeWorkspace(t, root, "iss-7-merged")
	gh = newClosedGH()
	commitEmpty(t, app, "squashed upstream under another sha")
	head := headOf(t, app)

	st, err := inspectTargetWith(ws, git, prLookupFor(head), nil)
	if err != nil {
		t.Fatal(err)
	}
	if b := st.Blockers(); len(b) != 0 {
		t.Fatalf("tip equal to the merged head is held: %q", b)
	}

	// Control: without the head the same workspace is held.
	st, err = inspectTargetWith(ws, git, prLookupFor(""), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Blockers()) == 0 {
		t.Fatal("control: without --merged-head the unpushed commit was not held")
	}

	commitEmpty(t, app, "work after the merge")
	st, err = inspectTargetWith(ws, git, prLookupFor(head), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Blockers()) == 0 {
		t.Fatal("a commit beyond the merged head was not held")
	}
}

func TestReapReasonKeepsReapsOwnWords(t *testing.T) {
	got := reapReason("iss-1  (o/r#1, 3M)\n\nheld by 1 thing(s):\n  - app: 2 unpushed commit(s)\nError: refusing to reap\n")
	for _, want := range []string{"held by 1 thing(s):", "app: 2 unpushed commit(s)", "refusing to reap"} {
		if !strings.Contains(got, want) {
			t.Errorf("reason %q lacks %q", got, want)
		}
	}
	if strings.Contains(got, "\n") {
		t.Errorf("reason has a newline: %q", got)
	}
	if reapReason("") == "" {
		t.Error("an empty output gives an empty reason")
	}
}
