package ghx

import (
	"errors"
	"strings"
	"testing"
)

func TestOpenPRsListsEachOpenPullRequestWithItsChecks(t *testing.T) {
	args := fakeGh(t, `[{"number": 4, "headRefName": "feature/7-bell", "baseRefName": "main", "mergeable": "CONFLICTING",
		"statusCheckRollup": [{"__typename": "CheckRun", "name": "ci", "status": "COMPLETED", "conclusion": "FAILURE", "startedAt": "2026-01-01T00:00:00Z"},
		{"__typename": "StatusContext", "context": "deploy", "state": "PENDING"}]},
		{"number": 5, "headRefName": "renovate/go", "headRefOid": "abc", "isCrossRepository": true, "isDraft": true,
		"createdAt": "2026-01-02T00:00:00Z", "mergeable": "MERGEABLE", "mergeStateStatus": "BEHIND", "statusCheckRollup": []}]`, "", 0)
	got, err := (CLI{}).OpenPRs("acme/widgets")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Number != 4 || got[0].HeadRefName != "feature/7-bell" || got[0].BaseRefName != "main" ||
		got[0].Mergeable != "CONFLICTING" || got[1].HeadRefName != "renovate/go" {
		t.Fatalf("OpenPRs = %+v", got)
	}
	if c := got[0].Checks; len(c) != 2 || c[0].Name != "ci" || c[0].Conclusion != "FAILURE" || c[1].Context != "deploy" || c[1].State != "PENDING" {
		t.Fatalf("checks = %+v", got[0].Checks)
	}
	if p := got[1]; p.HeadRefOid != "abc" || !p.IsCrossRepository || !p.IsDraft || p.CreatedAt != "2026-01-02T00:00:00Z" || p.MergeStateStatus != "BEHIND" {
		t.Fatalf("second = %+v", p)
	}
	want := []string{"pr", "list", "--repo", "acme/widgets", "--state", "open", "--limit", "1000",
		"--json", "number,headRefName,headRefOid,baseRefName,isCrossRepository,isDraft,createdAt,mergeable,mergeStateStatus,statusCheckRollup"}
	if strings.Join(args(), "\x00") != strings.Join(want, "\x00") {
		t.Errorf("gh args = %q\nwant %q", args(), want)
	}
}

func TestOpenPRsFailsWhenGhFails(t *testing.T) {
	fakeGh(t, "", "HTTP 502", 1)
	if _, err := (CLI{}).OpenPRs("acme/widgets"); err == nil {
		t.Fatal("a failed listing returned no error")
	}
}

func TestAddIssueLabelAsksGhToAddIt(t *testing.T) {
	args := fakeGh(t, "", "", 0)
	if err := (CLI{}).AddIssueLabel("acme/widgets", 7, "blocked"); err != nil {
		t.Fatal(err)
	}
	want := []string{"issue", "edit", "7", "--repo", "acme/widgets", "--add-label", "blocked"}
	if strings.Join(args(), "\x00") != strings.Join(want, "\x00") {
		t.Errorf("gh args = %q\nwant %q", args(), want)
	}
	fakeGh(t, "", "label not found", 1)
	if err := (CLI{}).AddIssueLabel("acme/widgets", 7, "blocked"); err == nil {
		t.Fatal("a failed edit returned no error")
	}
}

func TestUpdatePRBranchMergesTheBaseAndNamesAConflict(t *testing.T) {
	args := fakeGh(t, "", "", 0)
	if err := (CLI{}).UpdatePRBranch("acme/widgets", 4); err != nil {
		t.Fatal(err)
	}
	want := []string{"pr", "update-branch", "4", "--repo", "acme/widgets"}
	if strings.Join(args(), "\x00") != strings.Join(want, "\x00") {
		t.Errorf("gh args = %q\nwant %q: a merge, never --rebase", args(), want)
	}
	fakeGh(t, "", "GraphQL: merge conflict between base and head (updatePullRequestBranch)", 1)
	if err := (CLI{}).UpdatePRBranch("acme/widgets", 4); !errors.Is(err, ErrUpdateConflict) {
		t.Fatalf("a conflict = %v, want ErrUpdateConflict", err)
	}
	fakeGh(t, "", "HTTP 502", 1)
	if err := (CLI{}).UpdatePRBranch("acme/widgets", 4); err == nil || errors.Is(err, ErrUpdateConflict) {
		t.Fatalf("a failure = %v, want an error that is not a conflict", err)
	}
}

func TestMergeMethodReadsTheRepositorySettings(t *testing.T) {
	for _, tc := range []struct{ out, want string }{
		{`{"squashMergeAllowed": true, "mergeCommitAllowed": false, "rebaseMergeAllowed": false}`, "squash"},
		{`{"squashMergeAllowed": true, "mergeCommitAllowed": true, "rebaseMergeAllowed": true}`, "squash"},
		{`{"squashMergeAllowed": false, "mergeCommitAllowed": true, "rebaseMergeAllowed": true}`, "merge"},
		{`{"squashMergeAllowed": false, "mergeCommitAllowed": false, "rebaseMergeAllowed": true}`, "rebase"},
	} {
		args := fakeGh(t, tc.out, "", 0)
		got, err := (CLI{}).MergeMethod("acme/widgets")
		if err != nil || got != tc.want {
			t.Errorf("%s: MergeMethod = %q, %v, want %q", tc.out, got, err, tc.want)
		}
		want := []string{"repo", "view", "acme/widgets", "--json", "squashMergeAllowed,mergeCommitAllowed,rebaseMergeAllowed"}
		if strings.Join(args(), "\x00") != strings.Join(want, "\x00") {
			t.Errorf("gh args = %q\nwant %q", args(), want)
		}
	}
	fakeGh(t, `{}`, "", 0)
	if _, err := (CLI{}).MergeMethod("acme/widgets"); err == nil || !strings.Contains(err.Error(), "fix:") {
		t.Fatalf("no method allowed: err = %v", err)
	}
}

func TestMergePRNeverForces(t *testing.T) {
	args := fakeGh(t, "", "", 0)
	if err := (CLI{}).MergePR("acme/widgets", 4, "squash", "abc"); err != nil {
		t.Fatal(err)
	}
	want := []string{"pr", "merge", "4", "--repo", "acme/widgets", "--squash", "--match-head-commit", "abc"}
	if strings.Join(args(), "\x00") != strings.Join(want, "\x00") {
		t.Errorf("gh args = %q\nwant %q", args(), want)
	}
	fakeGh(t, "", "Pull request is not mergeable: the base branch policy prohibits the merge", 1)
	if err := (CLI{}).MergePR("acme/widgets", 4, "squash", "abc"); err == nil {
		t.Fatal("a refused merge returned no error")
	}
	if err := (CLI{}).MergePR("acme/widgets", 4, "--admin", "abc"); err == nil {
		t.Fatal("an unknown method was passed to gh")
	}
}
