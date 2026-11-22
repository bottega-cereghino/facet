package ghx

import (
	"strings"
	"testing"
)

func TestOpenPRBranchesListsTheHeadOfEachOpenPullRequest(t *testing.T) {
	args := fakeGh(t, `[{"headRefName": "feature/7-bell"}, {"headRefName": "renovate/go"}]`, "", 0)
	got, err := (CLI{}).OpenPRBranches("acme/widgets")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "feature/7-bell,renovate/go" {
		t.Fatalf("OpenPRBranches = %q", got)
	}
	want := []string{"pr", "list", "--repo", "acme/widgets", "--state", "open", "--limit", "1000", "--json", "headRefName"}
	if strings.Join(args(), "\x00") != strings.Join(want, "\x00") {
		t.Errorf("gh args = %q\nwant %q", args(), want)
	}
}

func TestOpenPRBranchesFailsWhenGhFails(t *testing.T) {
	fakeGh(t, "", "HTTP 502", 1)
	if _, err := (CLI{}).OpenPRBranches("acme/widgets"); err == nil {
		t.Fatal("a failed listing returned no error")
	}
}
