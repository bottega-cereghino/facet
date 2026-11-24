package ghx

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeDevelopGh puts a gh on PATH whose `issue develop` fails with developErr
// and whose `api` call (the ref read) exits refExit. It records every call.
func fakeDevelopGh(t *testing.T, developErr string, refExit string) func() string {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	script := "#!/bin/sh\n" +
		"echo \"$*\" >> '" + log + "'\n" +
		"case \"$1\" in\n" +
		"issue) echo '" + developErr + "' >&2; exit 1;;\n" +
		"api) echo 'Not Found (HTTP 404)' >&2; exit " + refExit + ";;\n" +
		"esac\nexit 2\n"
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	return func() string {
		raw, _ := os.ReadFile(log)
		return string(raw)
	}
}

// An unlinked branch of that name makes gh answer "empty branch name". With the
// ref on the remote the spawn continues on that branch, after one ref read.
func TestDevelopBranchReusesAnUnlinkedBranchThatIsOnTheRemote(t *testing.T) {
	calls := fakeDevelopGh(t, "API returned empty branch name", "0")
	got, err := (CLI{}).DevelopBranch("acme/lab", 5, "", "feature/5-fix")
	if err != nil || got != "feature/5-fix" {
		t.Fatalf("DevelopBranch = %q, %v; want the branch and no error", got, err)
	}
	if want := "api repos/acme/lab/git/ref/heads/feature/5-fix"; !strings.Contains(calls(), want) {
		t.Errorf("no ref read %q in the calls:\n%s", want, calls())
	}
}

// Without the ref the answer stays a failure, and it names the fix.
func TestDevelopBranchFailsOnEmptyBranchNameWithNoRef(t *testing.T) {
	fakeDevelopGh(t, "API returned empty branch name", "1")
	got, err := (CLI{}).DevelopBranch("acme/lab", 5, "", "feature/5-fix")
	if err == nil {
		t.Fatalf("DevelopBranch = %q, nil; a missing ref was read as success", got)
	}
	for _, want := range []string{"empty branch name", "feature/5-fix", "push it or pick another name"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error lacks %q: %v", want, err)
		}
	}
}

// Any other failure is returned as it was, with no ref read.
func TestDevelopBranchOtherFailuresStayFailures(t *testing.T) {
	calls := fakeDevelopGh(t, "HTTP 500", "0")
	if _, err := (CLI{}).DevelopBranch("acme/lab", 5, "", "feature/5-fix"); err == nil {
		t.Fatal("an unrelated gh failure was read as success")
	}
	if strings.Contains(calls(), "api ") {
		t.Errorf("a ref was read for an unrelated failure:\n%s", calls())
	}
}

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
