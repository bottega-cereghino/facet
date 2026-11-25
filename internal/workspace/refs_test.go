package workspace

import (
	"strings"
	"testing"

	"github.com/bottega-cereghino/facet/internal/gitx"
)

// Every ref outside refs/remotes, refs/tags and refs/notes is local work, in
// any namespace, so a commit only it reaches holds. refs/replace and a
// namespace no git command writes are the shapes a list of what to count
// misses (#364).
func TestReapHoldsACommitOnlyALocalRefReaches(t *testing.T) {
	t.Parallel()
	for _, ref := range []string{"refs/keep/deep/work", "refs/replace/"} {
		t.Run(ref, func(t *testing.T) {
			ws, clone := issueWorkspace(t)
			commitFile(t, clone, "only.txt", "only here\n", "only")
			work := gitOut(t, clone, "rev-parse", "HEAD")
			if ref == "refs/replace/" {
				// A replace ref is named after the object it stands in for.
				ref += gitOut(t, clone, "rev-parse", "HEAD~1")
			}
			gitOut(t, clone, "update-ref", ref, work)
			gitOut(t, clone, "reset", "-q", "--hard", "HEAD~1")
			if got := gitOut(t, clone, "branch", "-a", "--contains", work); got != "" {
				t.Fatalf("the commit should be on no branch: %q", got)
			}

			st, err := InspectIssue(ws, gitx.Git{}, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			if !hasBlocker(st, "has 1 unpushed commit") {
				t.Fatalf("only.txt is committed only on %s: %v", ref, st.Blockers())
			}
		})
	}
}

// The namespaces that are not local work do not hold: a counted remote's
// refs, tags and notes. The stash holds as a stash, not as unpushed commits.
func TestReapDoesNotCountRefsOutsideLocalWork(t *testing.T) {
	t.Parallel()
	ws, clone := issueWorkspace(t)
	commitFile(t, clone, "tagged.txt", "tagged\n", "tagged")
	gitOut(t, clone, "tag", "v1")
	gitOut(t, clone, "-c", "user.name=t", "-c", "user.email=t@example.com", "notes", "add", "-m", "a note")
	gitOut(t, clone, "reset", "-q", "--hard", "HEAD~1")

	st, err := InspectIssue(ws, gitx.Git{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if b := st.Blockers(); len(b) != 0 {
		t.Fatalf("a tag and a note are not protected work (README), yet reap holds: %v", b)
	}

	writeTestFile(t, clone+"/README", "stash me\n")
	gitOut(t, clone, "stash", "-q")
	st, err = InspectIssue(ws, gitx.Git{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !hasBlocker(st, "stash") || hasBlocker(st, "unpushed") {
		t.Fatalf("a stash holds as a stash entry, not as unpushed commits: %v", st.Blockers())
	}
}

// A local ref may point at a tree or a blob. It holds no commit to count, and
// nothing tells whether a remote has what it keeps, so it holds, with the fix.
func TestReapHoldsALocalRefThatIsNotACommit(t *testing.T) {
	t.Parallel()
	ws, clone := issueWorkspace(t)
	blob := gitOut(t, clone, "hash-object", "-w", "README")
	gitOut(t, clone, "update-ref", "refs/keep/blob", blob)

	st, err := InspectIssue(ws, gitx.Git{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !hasBlocker(st, "refs/keep/blob points at a blob") || !hasBlocker(st, "git update-ref -d refs/keep/blob") {
		t.Fatalf("reap would delete a ref it cannot judge: %v", st.Blockers())
	}
	gitOut(t, clone, "update-ref", "-d", "refs/keep/blob")
	st, err = InspectIssue(ws, gitx.Git{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if b := st.Blockers(); len(b) != 0 {
		t.Fatalf("with the ref deleted, reap still holds: %v", b)
	}
}

// The count and the walk name the same refs: notLocal is spelled once and
// read by both.
func TestWalkedRefsCountAndNamesAgree(t *testing.T) {
	t.Parallel()
	_, clone := issueWorkspace(t)
	head := gitOut(t, clone, "rev-parse", "HEAD")
	for _, ref := range []string{"refs/keep/a", "refs/tags/t", "refs/notes/n", "refs/remotes/x/y", "refs/stash", "refs/stashed/z"} {
		gitOut(t, clone, "update-ref", ref, head)
	}
	r, err := walkedRefs(gitx.Git{}, clone, true)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(r.names, " ")
	want := "refs/heads/main refs/keep/a refs/stashed/z"
	if got != want {
		t.Fatalf("names = %q, want %q", got, want)
	}
	for _, ref := range []string{"refs/keep/a", "refs/stashed/z"} {
		gitOut(t, clone, "update-ref", "-d", ref)
	}
	// With every local ref at HEAD, the count's own --exclude list must leave
	// out the same refs: a commit only a tag reaches is not counted.
	commitFile(t, clone, "t.txt", "t\n", "t")
	gitOut(t, clone, "update-ref", "refs/tags/t", "HEAD")
	gitOut(t, clone, "update-ref", "refs/stash", "HEAD")
	gitOut(t, clone, "reset", "-q", "--hard", "HEAD~1")
	count := gitOut(t, clone, append([]string{"rev-list", "--count"}, append(r.countArgs, "--not", "--remotes")...)...)
	if count != "0" {
		t.Fatalf("the count saw %s commits only a tag and the stash reach", count)
	}
}
