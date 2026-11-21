package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bottega-cereghino/facet/internal/config"
	"github.com/bottega-cereghino/facet/internal/ghx"
)

// withRouting points roots at a routing file carrying extra top-level keys.
func withRouting(t *testing.T, extra string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "routing.json")
	contents := `{
		"version": 1,
		"repos": {
			"lab": {"dir": "lab", "url": "https://example.invalid/lab.git"},
			"docs": {"dir": "docs", "url": "https://example.invalid/docs.git"},
			"tools": {"dir": "tools", "url": "https://example.invalid/tools.git"}
		},
		"ownerRepoToKey": {
			"acme/lab": "lab", "acme/docs": "docs", "acme/tools": "tools"
		},
		"aliases": {}, "areaMap": {}, "knowledgeByArea": {}, "pathHints": {}` + extra + `
	}`
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
	prev := roots
	roots = config.Roots{Routing: path}
	t.Cleanup(func() { roots = prev })
}

func iref(owner, repo string, n int) ghx.IssueRef {
	return ghx.IssueRef{Owner: owner, Repo: repo, Number: n}
}

const commentKinds = `,
	"commentKinds": {
		"plan":    "(?m)^#+ .*[Pp]lan",
		"finding": "(?m)^\\*\\*FINDING"
	}`

func commentFake() *fakeGH {
	f := &fakeGH{}
	f.comments = map[string][]ghx.Comment{
		"acme/lab#75": {
			{ID: 1, Body: "## Plan\nthe first attempt", CreatedAt: "2026-08-03T10:00:00Z", UpdatedAt: "2026-08-03T10:00:00Z"},
			{ID: 2, Body: "some discussion, not a plan", CreatedAt: "2026-08-03T10:30:00Z", UpdatedAt: "2026-08-03T10:30:00Z"},
			{ID: 3, Body: "## Plan — revision 2\nTHE ONE THAT COUNTS", CreatedAt: "2026-08-03T11:00:00Z", UpdatedAt: "2026-08-03T11:00:00Z"},
			{ID: 4, Body: "**FINDING** something else", CreatedAt: "2026-08-03T12:00:00Z", UpdatedAt: "2026-08-03T12:00:00Z"},
		},
	}
	return f
}

// The whole reason this command exists: where a decision is revised by posting
// it again, the newest one is the one that counts, and finding it by eye is how
// the wrong revision gets acted on.
func TestCommentLastReturnsTheNewestMatch(t *testing.T) {
	withRouting(t, commentKinds)
	var out bytes.Buffer
	if err := runCommentList(&out, commentFake(), iref("acme", "lab", 75), "plan", "", true); err != nil {
		t.Fatalf("last: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "THE ONE THAT COUNTS") {
		t.Errorf("did not return the newest plan:\n%s", got)
	}
	if strings.Contains(got, "the first attempt") {
		t.Errorf("returned an older revision as well:\n%s", got)
	}
	// A thread carrying one plan and a thread carrying five must not look the
	// same, so the count is reported.
	if !strings.Contains(got, "2 of 2 matching") {
		t.Errorf("the number of matches was not reported:\n%s", got)
	}
}

func TestCommentListFiltersByKind(t *testing.T) {
	withRouting(t, commentKinds)
	var out bytes.Buffer
	if err := runCommentList(&out, commentFake(), iref("acme", "lab", 75), "finding", "", false); err != nil {
		t.Fatalf("list: %v", err)
	}
	if n := strings.Count(strings.TrimSpace(out.String()), "\n") + 1; n != 1 {
		t.Errorf("got %d lines, want 1:\n%s", n, out.String())
	}
	if !strings.Contains(out.String(), "FINDING") {
		t.Errorf("wrong comment matched:\n%s", out.String())
	}
}

// facet does not know what a plan is. With no kinds declared the flag refuses
// and names the fix -- rather than matching nothing, which is indistinguishable
// from an issue that genuinely has no plan on it.
func TestCommentKindRefusesWithoutTheRoutingBlock(t *testing.T) {
	withRouting(t, "")
	var out bytes.Buffer
	err := runCommentList(&out, commentFake(), iref("acme", "lab", 75), "plan", "", true)
	if err == nil {
		t.Fatal("--kind was accepted with no commentKinds block")
	}
	for _, want := range []string{"no comment kinds", "fix:", "commentKinds"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal is missing %q:\n%s", want, err)
		}
	}
}

func TestCommentUnknownKindNamesTheOnesThatExist(t *testing.T) {
	withRouting(t, commentKinds)
	var out bytes.Buffer
	err := runCommentList(&out, commentFake(), iref("acme", "lab", 75), "exit", "", true)
	if err == nil {
		t.Fatal("an undeclared kind was accepted")
	}
	for _, want := range []string{"unknown comment kind", "finding", "plan"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal is missing %q:\n%s", want, err)
		}
	}
}

// --grep works with no routing block at all, so an adopter who has declared
// nothing can still find things.
func TestCommentGrepNeedsNoConfiguration(t *testing.T) {
	withRouting(t, "")
	var out bytes.Buffer
	if err := runCommentList(&out, commentFake(), iref("acme", "lab", 75), "", "revision 2", true); err != nil {
		t.Fatalf("grep: %v", err)
	}
	if !strings.Contains(out.String(), "THE ONE THAT COUNTS") {
		t.Errorf("grep did not find it:\n%s", out.String())
	}
}

// "No plan on this issue" and "nothing was searched for" must not print the
// same thing.
func TestCommentEmptyResultSaysWhatWasSearched(t *testing.T) {
	withRouting(t, commentKinds)
	f := commentFake()
	f.comments = map[string][]ghx.Comment{"acme/lab#75": {{ID: 1, Body: "just chatter"}}}
	var out bytes.Buffer
	if err := runCommentList(&out, f, iref("acme", "lab", 75), "plan", "", true); err != nil {
		t.Fatalf("last: %v", err)
	}
	got := out.String()
	for _, want := range []string{"no matching comments", "1 comments searched", "kind plan"} {
		if !strings.Contains(got, want) {
			t.Errorf("output is missing %q:\n%s", want, got)
		}
	}
}

// An edited plan is not the plan that was agreed to, so the edit is surfaced
// wherever a comment is read as a decision.
func TestCommentLastFlagsAnEditedComment(t *testing.T) {
	withRouting(t, commentKinds)
	f := commentFake()
	f.comments["acme/lab#75"] = []ghx.Comment{{
		ID: 9, Body: "## Plan\nquietly changed",
		CreatedAt: "2026-08-03T10:00:00Z", UpdatedAt: "2026-08-03T18:00:00Z",
	}}
	var out bytes.Buffer
	if err := runCommentList(&out, f, iref("acme", "lab", 75), "plan", "", true); err != nil {
		t.Fatalf("last: %v", err)
	}
	if !strings.Contains(out.String(), "edited") {
		t.Errorf("an edited comment was not marked:\n%s", out.String())
	}
}

func TestCommentPostRefusesAnEmptyBody(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/empty.md"
	if err := os.WriteFile(path, []byte("   \n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readBodyFile(path, "comment"); err == nil {
		t.Error("an empty body was accepted: a comment with no body is a note to nobody")
	}
	if _, err := readBodyFile("", "comment"); err == nil {
		t.Error("a missing --body-file was accepted")
	}
}

// The routing file is optional: --grep needs no configuration, and --kind
// refuses only because there genuinely are no kinds declared.
func TestCommentWorksWithNoRoutingFileAtAll(t *testing.T) {
	prev := roots
	roots = config.Roots{Routing: filepath.Join(t.TempDir(), "absent.json")}
	t.Cleanup(func() { roots = prev })

	f := &fakeGH{comments: map[string][]ghx.Comment{"acme/lab#46": {{ID: 1, Body: "## Plan\nx"}}}}
	if err := runCommentList(&bytes.Buffer{}, f, iref("acme", "lab", 46), "", "Plan", true); err != nil {
		t.Errorf("comment --grep refused without a routing file: %v", err)
	}
	if err := runCommentList(&bytes.Buffer{}, f, iref("acme", "lab", 46), "plan", "", true); err == nil {
		t.Error("--kind was accepted with no kinds declared anywhere")
	}
}

// The refusal must not teach the pattern this branch measured as wrong: an
// unanchored form returns a heading that merely CONTAINS the word.
func TestCommentKindRefusalDoesNotRecommendAnUnanchoredPattern(t *testing.T) {
	withRouting(t, "")
	err := runCommentList(&bytes.Buffer{}, commentFake(), iref("acme", "lab", 75), "plan", "", true)
	if err == nil {
		t.Fatal("--kind was accepted with no commentKinds block")
	}
	msg := err.Error()
	// What is OFFERED after "e.g." must be the anchored form. The unanchored
	// one may still appear -- naming it as the thing to avoid is the useful
	// half -- so this asserts what is recommended, not what is mentioned.
	offered, _, _ := strings.Cut(strings.SplitN(msg, "e.g. ", 2)[1], "\n")
	if !strings.Contains(offered, "(?mi)") || !strings.Contains(offered, "#{1,6}") {
		t.Errorf("the recommended pattern is not anchored and case-insensitive: %q", offered)
	}
	if strings.Contains(offered, ".*") {
		t.Errorf("the recommended pattern is unanchored: %q", offered)
	}
	if !strings.Contains(msg, "anchor") {
		t.Errorf("the fix line does not warn about anchoring:\n%s", msg)
	}
}

// A comment id is unique in the repository, so the API would edit comment 4
// even when the command names another issue. The edit refuses, edits nothing,
// and names the issue the comment is really on.
func TestCommentEditRefusesACommentOnAnotherIssue(t *testing.T) {
	f := commentFake()
	f.comments["acme/lab#5"] = []ghx.Comment{{ID: 50, Body: "on five"}}
	err := runCommentEdit(&bytes.Buffer{}, f, iref("acme", "lab", 5), 4, "new body")
	if err == nil {
		t.Fatal("comment 4, which is on #75, was edited through #5")
	}
	if len(f.editedComments) != 0 {
		t.Errorf("a comment was edited despite the refusal: %v", f.editedComments)
	}
	for _, want := range []string{"acme/lab#75", "fix:", "facet comment edit acme/lab#75 --id 4"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal is missing %q:\n%s", want, err)
		}
	}
}

func TestCommentEditEditsACommentOnTheNamedIssue(t *testing.T) {
	f := commentFake()
	if err := runCommentEdit(&bytes.Buffer{}, f, iref("acme", "lab", 75), 4, "new body"); err != nil {
		t.Fatalf("edit: %v", err)
	}
	if want := []string{"4: new body"}; !equalSlices(f.editedComments, want) {
		t.Errorf("edited = %v, want %v", f.editedComments, want)
	}
}

// A lookup that fails is not a pass: the edit refuses rather than editing a
// comment it could not place.
func TestCommentEditRefusesWhenTheCommentCannotBeLookedUp(t *testing.T) {
	f := commentFake()
	err := runCommentEdit(&bytes.Buffer{}, f, iref("acme", "lab", 75), 999, "new body")
	if err == nil {
		t.Fatal("an unknown comment id was edited")
	}
	if len(f.editedComments) != 0 {
		t.Errorf("a comment was edited despite the refusal: %v", f.editedComments)
	}
	if !strings.Contains(err.Error(), "fix:") {
		t.Errorf("refusal does not print the fix:\n%s", err)
	}
}
