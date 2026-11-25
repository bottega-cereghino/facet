package ghx

import (
	"strings"
	"testing"
)

// A failed GraphQL call names its request without printing the whole query
// ahead of the reason (facet#143); every other argument is kept as given.
func TestCommandLineCutsAMultiLineQueryToItsFirstLine(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{
			"graphql",
			[]string{"api", "graphql", "-f", "query=query($owner: String!) {\n  repository(owner: $owner) { id }\n}", "-f", "owner=o"},
			"api graphql -f query=query($owner: String!) { … -f owner=o",
		},
		{
			"one-line query",
			[]string{"api", "graphql", "-f", "query={ viewer { login } }"},
			"api graphql -f query={ viewer { login } }",
		},
		{
			"rest",
			[]string{"api", "repos/o/r/issues/1"},
			"api repos/o/r/issues/1",
		},
	} {
		if got := commandLine(tc.args); got != tc.want {
			t.Errorf("%s: commandLine = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// gh's own "no pull requests found" is the one failure that means there is no
// PR for the branch.
func TestViewPRReadsNoPullRequestsFoundAsNone(t *testing.T) {
	fakeGh(t, "", `no pull requests found for branch "feature/5-fix"`+"\n", 1)
	pr, err := CLI{}.ViewPR("acme/lab", "feature/5-fix")
	if err != nil || pr != nil {
		t.Fatalf("ViewPR = %+v, %v; want nil, nil", pr, err)
	}
}

// A repository that was renamed, transferred or cannot be read answers
// "Could not resolve to a Repository". That is not "no PR": reap's open-PR
// guard would pass on a lookup that could not tell.
func TestViewPRRefusesWhenTheRepositoryCannotBeResolved(t *testing.T) {
	fakeGh(t, "", "GraphQL: Could not resolve to a Repository with the name 'acme/gone'. (repository)\n", 1)
	pr, err := CLI{}.ViewPR("acme/gone", "feature/5-fix")
	if err == nil {
		t.Fatalf("ViewPR = %+v, nil; an unresolvable repository was read as no PR", pr)
	}
	if !strings.Contains(err.Error(), "Could not resolve to a Repository") {
		t.Errorf("the error lost gh's reason: %v", err)
	}
}

// GitHub answers a commit it does not have with HTTP 422 "No commit found for
// SHA" (seen with gh api against a public repository). A commit the forge does
// not know is in no pull request: nil, nil.
func TestMergedPRForSHAReadsAnUnknownCommitAsNoPullRequest(t *testing.T) {
	fakeGh(t, `{"message":"No commit found for SHA: deadbeef","status":"422"}`, "gh: No commit found for SHA: deadbeef (HTTP 422)\n", 1)
	pr, err := CLI{}.MergedPRForSHA("acme/lab", "deadbeef")
	if err != nil || pr != nil {
		t.Fatalf("MergedPRForSHA = %+v, %v; want nil, nil", pr, err)
	}
}

// Every other failure is still an error, so the workspace stays held.
func TestMergedPRForSHAKeepsOtherFailuresAsErrors(t *testing.T) {
	for _, stderr := range []string{
		"gh: API rate limit exceeded (HTTP 403)\n",
		"gh: Not Found (HTTP 404)\n",
		"gh: Validation Failed (HTTP 422)\n",
		"gh: No commit found for SHA: deadbeef (HTTP 500)\n",
	} {
		fakeGh(t, "", stderr, 1)
		pr, err := CLI{}.MergedPRForSHA("acme/lab", "deadbeef")
		if err == nil {
			t.Errorf("%q: MergedPRForSHA = %+v, nil; a failure was read as no pull request", stderr, pr)
		}
	}
}

func TestViewPRParsesAFoundPR(t *testing.T) {
	fakeGh(t, `{"number":7,"state":"OPEN","mergedAt":"","url":"https://github.com/acme/lab/pull/7"}`, "", 0)
	pr, err := CLI{}.ViewPR("acme/lab", "feature/5-fix")
	if err != nil || pr == nil || pr.Number != 7 || pr.State != "OPEN" {
		t.Fatalf("ViewPR = %+v, %v", pr, err)
	}
}

// With no --base, gh branches from the repository's default branch, whatever
// it is called. facet passes --base only when one was asked for.
func TestDevelopBranchPassesBaseOnlyWhenGiven(t *testing.T) {
	for _, tc := range []struct {
		base string
		want string // "" means no --base at all
	}{
		{"", ""},
		{"develop", "develop"},
	} {
		args := fakeGh(t, "", "", 0)
		if _, err := (CLI{}).DevelopBranch("acme/lab", 5, tc.base, "feature/5-fix"); err != nil {
			t.Fatalf("base %q: DevelopBranch: %v", tc.base, err)
		}
		got := ""
		a := args()
		for i, v := range a {
			if v == "--base" && i+1 < len(a) {
				got = a[i+1]
			}
		}
		if got != tc.want {
			t.Errorf("base %q: gh got --base %q (args %q), want %q", tc.base, got, a, tc.want)
		}
	}
}
