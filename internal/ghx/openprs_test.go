package ghx

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenPRsListsEachOpenPullRequestWithItsChecks(t *testing.T) {
	args := fakeGh(t, `[{"number": 4, "headRefName": "feature/7-bell", "baseRefName": "main", "mergeable": "CONFLICTING",
		"author": {"login": "octo"}, "body": "text\nFacet-Queue-Run: abc",
		"statusCheckRollup": [{"__typename": "CheckRun", "workflowName": "build", "name": "ci", "status": "COMPLETED", "conclusion": "FAILURE", "startedAt": "2026-01-01T00:00:00Z"},
		{"__typename": "StatusContext", "context": "deploy", "state": "PENDING"}]},
		{"number": 5, "headRefName": "renovate/go", "headRefOid": "abc", "isCrossRepository": true, "isDraft": true,
		"createdAt": "2026-01-02T00:00:00Z", "mergeable": "MERGEABLE", "mergeStateStatus": "BEHIND", "statusCheckRollup": []}]`, "", 0)
	got, err := (CLI{}).OpenPRs("acme/widgets")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Number != 4 || got[0].HeadRefName != "feature/7-bell" || got[0].BaseRefName != "main" ||
		got[0].Mergeable != "CONFLICTING" || got[1].HeadRefName != "renovate/go" ||
		got[0].Author.Login != "octo" || got[0].Body != "text\nFacet-Queue-Run: abc" {
		t.Fatalf("OpenPRs = %+v", got)
	}
	if c := got[0].Checks; len(c) != 2 || c[0].Name != "ci" || c[0].WorkflowName != "build" || c[0].Conclusion != "FAILURE" || c[1].Context != "deploy" || c[1].State != "PENDING" {
		t.Fatalf("checks = %+v", got[0].Checks)
	}
	if p := got[1]; p.HeadRefOid != "abc" || !p.IsCrossRepository || !p.IsDraft || p.CreatedAt != "2026-01-02T00:00:00Z" || p.MergeStateStatus != "BEHIND" {
		t.Fatalf("second = %+v", p)
	}
	want := []string{"pr", "list", "--repo", "acme/widgets", "--state", "open", "--limit", "1000",
		"--json", "number,headRefName,headRefOid,baseRefName,isCrossRepository,isDraft,createdAt,author,body,mergeable,mergeStateStatus,statusCheckRollup"}
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

func TestOpenPRsFailsWhenResultMayBeTruncated(t *testing.T) {
	stdout := "[" + strings.TrimSuffix(strings.Repeat(`{"headRefName":"feature/7-bell"},`, OpenPRLimit), ",") + "]"
	fakeGh(t, stdout, "", 0)
	if _, err := (CLI{}).OpenPRs("acme/widgets"); err == nil ||
		!strings.Contains(err.Error(), "reached limit 1000") ||
		!strings.Contains(err.Error(), "results may be incomplete") {
		t.Fatalf("OpenPRs error = %v; want an incomplete-list error", err)
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
		got, err := (CLI{}).MergeMethod("acme/widgets", "")
		if err != nil || got != tc.want {
			t.Errorf("%s: MergeMethod = %q, %v, want %q", tc.out, got, err, tc.want)
		}
		want := []string{"repo", "view", "acme/widgets", "--json", "squashMergeAllowed,mergeCommitAllowed,rebaseMergeAllowed"}
		if strings.Join(args(), "\x00") != strings.Join(want, "\x00") {
			t.Errorf("gh args = %q\nwant %q", args(), want)
		}
	}
	fakeGh(t, `{}`, "", 0)
	if _, err := (CLI{}).MergeMethod("acme/widgets", ""); err == nil || !strings.Contains(err.Error(), "fix:") {
		t.Fatalf("no method allowed: err = %v", err)
	}
}

func TestMergeMethodRespectsBranchRuleset(t *testing.T) {
	install := func(t *testing.T, repoJSON, rulesJSON string, rulesExit int) {
		t.Helper()
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "repo"), []byte(repoJSON), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "rules"), []byte(rulesJSON), 0o600); err != nil {
			t.Fatal(err)
		}
		q := "'" + dir + "'"
		script := "#!/bin/sh\n" +
			"case \"$1\" in\n" +
			"repo) /bin/cat " + q + "/repo; exit 0 ;;\n" +
			"api)\n" +
			"  if [ " + strings.TrimSpace(string(rune('0'+rulesExit))) + " -ne 0 ]; then\n" +
			"    /bin/cat " + q + "/rules >&2; exit 1\n" +
			"  fi\n" +
			"  /bin/cat " + q + "/rules; exit 0 ;;\n" +
			"esac\n"
		if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", dir)
	}

	allRepo := `{"squashMergeAllowed": true, "mergeCommitAllowed": true, "rebaseMergeAllowed": true}`
	t.Run("ruleset restricts to merge and rebase", func(t *testing.T) {
		install(t, allRepo, `[{"type":"pull_request","parameters":{"allowed_merge_methods":["merge","rebase"]}}]`, 0)
		got, err := (CLI{}).MergeMethod("acme/widgets", "main")
		if err != nil || got != "merge" {
			t.Fatalf("MergeMethod = %q, %v; want merge", got, err)
		}
	})
	t.Run("ruleset linear history excludes merge", func(t *testing.T) {
		repo := `{"squashMergeAllowed": false, "mergeCommitAllowed": true, "rebaseMergeAllowed": true}`
		install(t, repo, `[{"type":"required_linear_history"}]`, 0)
		got, err := (CLI{}).MergeMethod("acme/widgets", "main")
		if err != nil || got != "rebase" {
			t.Fatalf("MergeMethod = %q, %v; want rebase", got, err)
		}
	})
	t.Run("disjoint repo and ruleset fails with fix", func(t *testing.T) {
		repo := `{"squashMergeAllowed": true, "mergeCommitAllowed": false, "rebaseMergeAllowed": false}`
		install(t, repo, `[{"type":"pull_request","parameters":{"allowed_merge_methods":["rebase"]}}]`, 0)
		if _, err := (CLI{}).MergeMethod("acme/widgets", "main"); err == nil || !strings.Contains(err.Error(), "fix:") {
			t.Fatalf("err = %v, want error with fix:", err)
		}
	})
	t.Run("absent ruleset (404) uses repo settings", func(t *testing.T) {
		install(t, allRepo, "gh: Not Found (HTTP 404)", 1)
		got, err := (CLI{}).MergeMethod("acme/widgets", "main")
		if err != nil || got != "squash" {
			t.Fatalf("MergeMethod = %q, %v; want squash", got, err)
		}
	})
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

func TestPRFilesListsChangedFilesAcrossPages(t *testing.T) {
	args := fakeGh(t,
		`[{"filename":"a.go"},{"filename":"new.json","previous_filename":"old.json"}][{"filename":"go.sum"}]`,
		"", 0)
	got, err := (CLI{}).PRFiles("acme/widgets", 4)
	if err != nil {
		t.Fatal(err)
	}
	wantFiles := "a.go|new.json|old.json|go.sum"
	if strings.Join(got, "|") != wantFiles {
		t.Fatalf("PRFiles = %q, want %q", got, wantFiles)
	}
	wantArgs := []string{"api", "--paginate", "repos/acme/widgets/pulls/4/files?per_page=100"}
	if strings.Join(args(), "\x00") != strings.Join(wantArgs, "\x00") {
		t.Errorf("gh args = %q\nwant %q", args(), wantArgs)
	}
	fakeGh(t, "", "HTTP 502", 1)
	if _, err := (CLI{}).PRFiles("acme/widgets", 4); err == nil {
		t.Fatal("a failed PRFiles returned no error")
	}
}

func TestHeadBehindBaseReadsCompareStatus(t *testing.T) {
	for _, tc := range []struct {
		out  string
		want bool
	}{
		{`{"status":"ahead","behind_by":0}`, false},
		{`{"status":"identical","behind_by":0}`, false},
		{`{"status":"behind","behind_by":2}`, true},
		{`{"status":"diverged","behind_by":1}`, true},
		{`{"status":"ahead","behind_by":1}`, true},
	} {
		args := fakeGh(t, tc.out, "", 0)
		got, err := (CLI{}).HeadBehindBase("acme/widgets", "release/1", "abc123")
		if err != nil || got != tc.want {
			t.Errorf("%s: HeadBehindBase = %v, %v; want %v", tc.out, got, err, tc.want)
		}
		wantArgs := []string{"api", "repos/acme/widgets/compare/release%2F1...abc123"}
		if strings.Join(args(), "\x00") != strings.Join(wantArgs, "\x00") {
			t.Errorf("gh args = %q\nwant %q", args(), wantArgs)
		}
	}
	fakeGh(t, `{}`, "", 0)
	if _, err := (CLI{}).HeadBehindBase("acme/widgets", "main", "abc123"); err == nil {
		t.Fatal("empty status returned no error")
	}
	fakeGh(t, "", "HTTP 502", 1)
	if _, err := (CLI{}).HeadBehindBase("acme/widgets", "main", "abc123"); err == nil {
		t.Fatal("failed compare returned no error")
	}
}

func TestViewerReadsTheLogin(t *testing.T) {
	args := fakeGh(t, `{"login": "octo", "id": 1}`, "", 0)
	got, err := (CLI{}).Viewer()
	if err != nil || got != "octo" {
		t.Fatalf("Viewer = %q, %v", got, err)
	}
	if strings.Join(args(), " ") != "api user" {
		t.Errorf("gh args = %q", args())
	}
}

func TestViewerFailsWithoutALogin(t *testing.T) {
	fakeGh(t, `{}`, "", 0)
	if _, err := (CLI{}).Viewer(); err == nil {
		t.Fatal("an empty login returned no error")
	}
}

// gh --paginate prints one array per page; a restriction on a later page
// still counts, and the request asks for every page.
func TestMergeMethodReadsEveryRulesPage(t *testing.T) {
	dir := t.TempDir()
	q := "'" + dir + "'"
	repo := `{"squashMergeAllowed": true, "mergeCommitAllowed": true, "rebaseMergeAllowed": true}`
	rules := `[{"type":"creation"}][{"type":"pull_request","parameters":{"allowed_merge_methods":["rebase"]}}]`
	for name, body := range map[string]string{"repo": repo, "rules": rules} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	script := "#!/bin/sh\n" +
		"case \"$1\" in\n" +
		"repo) /bin/cat " + q + "/repo ;;\n" +
		"api) for a in \"$@\"; do printf '%s\\n' \"$a\"; done > " + q + "/args; /bin/cat " + q + "/rules ;;\n" +
		"esac\n"
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	got, err := (CLI{}).MergeMethod("acme/widgets", "main")
	if err != nil || got != "rebase" {
		t.Fatalf("MergeMethod = %q, %v; want rebase from the second page", got, err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "args"))
	if err != nil || !strings.Contains(string(raw), "--paginate") {
		t.Fatalf("gh api args = %q, %v; want --paginate", raw, err)
	}
}
