package workspace

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bottega-cereghino/facet/internal/gitx"
	"github.com/bottega-cereghino/facet/internal/gitx/gitxtest"
)

// Work pushed to a second remote, a fork, whose branch was deleted there
// since: a stale fork/* ref must not read as pushed. Every remote the clone
// has is fetched with --prune, not only origin (facet#268).
func TestReapFetchesEveryRemoteBeforeCountingWorkAsPushed(t *testing.T) {
	ws, clone := issueWorkspace(t)
	fork := filepath.Join(t.TempDir(), "fork.git")
	gitOut(t, "", "init", "-q", "--bare", fork)
	gitOut(t, clone, "remote", "add", "fork", fork)
	gitOut(t, clone, "switch", "-q", "-c", "feature")
	writeTestFile(t, filepath.Join(clone, "idea.txt"), "the idea\n")
	gitOut(t, clone, "add", "idea.txt")
	gitOut(t, clone, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-qm", "the idea")
	gitOut(t, clone, "push", "-q", "fork", "feature")

	st, err := InspectIssue(ws, gitx.Git{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if b := st.Blockers(); len(b) != 0 {
		t.Fatalf("work pushed to the fork holds: %v", b)
	}

	gitOut(t, fork, "branch", "-q", "-D", "feature")
	st, err = InspectIssue(ws, gitx.Git{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !hasBlocker(st, "1 unpushed commit") {
		t.Fatalf("the branch is gone from the fork, but its stale ref still reads as pushed: %v", st.Blockers())
	}
}

// A remote that cannot be fetched is named in the note, so the operator
// knows which one the count was judged against stale.
func TestReapNamesTheRemoteItCouldNotFetch(t *testing.T) {
	ws, clone := issueWorkspace(t)
	gitOut(t, clone, "remote", "add", "fork", filepath.Join(t.TempDir(), "gone.git"))
	git := &gitxtest.Runner{
		Real: gitx.Git{},
		Fail: func(args []string) bool { return len(args) > 0 && args[0] == "fetch" && args[len(args)-1] == "fork" },
		Err:  errors.New("could not read from remote repository"),
	}
	st, err := InspectIssue(ws, git, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if n := st.Notes(); len(n) != 1 || !strings.Contains(n[0], "could not fetch fork") || strings.Contains(n[0], "origin") {
		t.Errorf("Notes() = %v; want one naming fork alone", n)
	}
}
