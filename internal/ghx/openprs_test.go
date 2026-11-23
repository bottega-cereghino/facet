package ghx

import (
	"strings"
	"testing"
)

func TestOpenPRsListsEachOpenPullRequestWithItsChecks(t *testing.T) {
	args := fakeGh(t, `[{"number": 4, "headRefName": "feature/7-bell", "baseRefName": "main", "mergeable": "CONFLICTING",
		"statusCheckRollup": [{"__typename": "CheckRun", "name": "ci", "status": "COMPLETED", "conclusion": "FAILURE", "startedAt": "2026-01-01T00:00:00Z"},
		{"__typename": "StatusContext", "context": "deploy", "state": "PENDING"}]},
		{"number": 5, "headRefName": "renovate/go", "mergeable": "MERGEABLE", "statusCheckRollup": []}]`, "", 0)
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
	want := []string{"pr", "list", "--repo", "acme/widgets", "--state", "open", "--limit", "1000",
		"--json", "number,headRefName,baseRefName,mergeable,statusCheckRollup"}
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
