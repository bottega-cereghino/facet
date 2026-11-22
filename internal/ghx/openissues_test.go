package ghx

import (
	"strings"
	"testing"
)

func TestOpenIssuesAsksOnlyForOpenIssuesWithTheLabel(t *testing.T) {
	args := fakeGh(t, "[]", "", 0)
	got, err := (CLI{}).OpenIssues("acme/widgets", "next")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("OpenIssues = %+v, want none", got)
	}
	want := []string{
		"issue", "list",
		"--repo", "acme/widgets",
		"--state", "open",
		"--label", "next",
		"--limit", "1000",
		"--json", openIssueFields,
	}
	if strings.Join(args(), "\x00") != strings.Join(want, "\x00") {
		t.Errorf("gh args = %q\nwant %q", args(), want)
	}
}

func TestOpenIssuesParsesCreatedAtAndMilestone(t *testing.T) {
	due := "2026-04-01T00:00:00Z"
	stdout := `[
	  {
	    "number": 7,
	    "title": "Fix the bell",
	    "url": "https://github.com/acme/widgets/issues/7",
	    "state": "OPEN",
	    "labels": [{"id": "LA_x", "name": "next", "color": "ededed"}, {"name": "codex"}],
	    "createdAt": "2026-01-02T03:04:05Z",
	    "milestone": {"number": 3, "title": "One", "description": "", "dueOn": null}
	  },
	  {
	    "number": 8,
	    "title": "Later",
	    "url": "https://github.com/acme/widgets/issues/8",
	    "state": "OPEN",
	    "labels": [{"name": "next"}],
	    "createdAt": "2026-02-02T03:04:05Z",
	    "milestone": {"number": 4, "title": "Two", "description": "d", "dueOn": "` + due + `"}
	  }
	]`
	fakeGh(t, stdout, "", 0)
	got, err := (CLI{}).OpenIssues("acme/widgets", "next")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].Number != 7 || got[0].CreatedAt != "2026-01-02T03:04:05Z" || got[0].State != "OPEN" {
		t.Errorf("first = %+v", got[0])
	}
	if got[0].Milestone == nil || got[0].Milestone.DueOn != nil || got[0].Milestone.Title != "One" || got[0].Milestone.Number != 3 {
		t.Errorf("first milestone = %+v, want One with a null due date", got[0].Milestone)
	}
	names := got[0].LabelNames()
	if len(names) != 2 || names[0] != "next" || names[1] != "codex" {
		t.Errorf("labels = %q", names)
	}
	if got[1].Milestone == nil || got[1].Milestone.DueOn == nil || *got[1].Milestone.DueOn != due {
		t.Errorf("second milestone = %+v, want due %s", got[1].Milestone, due)
	}
}

func TestOpenIssuesRefusesABadRepoBeforeCallingGh(t *testing.T) {
	// PATH has no gh. A refusal that actually called out would fail here
	// instead of reaching a real gh.
	t.Setenv("PATH", t.TempDir())
	_, err := (CLI{}).OpenIssues("not-a-repo", "next")
	if err == nil || !strings.Contains(err.Error(), "not-a-repo") {
		t.Fatalf("OpenIssues = %v, want an error naming the repo", err)
	}
}

func TestOpenIssuesRefusesAnEmptyLabel(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	_, err := (CLI{}).OpenIssues("acme/widgets", " ")
	if err == nil || !strings.Contains(err.Error(), "fix:") {
		t.Fatalf("OpenIssues = %v, want a refusal that says how to fix it", err)
	}
}

func TestOpenIssuesReportsAGhFailure(t *testing.T) {
	fakeGh(t, "", "no such repository\n", 1)
	_, err := (CLI{}).OpenIssues("acme/widgets", "next")
	if err == nil || !strings.Contains(err.Error(), "no such repository") {
		t.Fatalf("OpenIssues = %v, want gh's reason", err)
	}
}

func TestOpenIssuesRefusesJSONThatIsNotAList(t *testing.T) {
	fakeGh(t, `{"message": "no"}`, "", 0)
	_, err := (CLI{}).OpenIssues("acme/widgets", "next")
	if err == nil || !strings.Contains(err.Error(), "parse open issues") {
		t.Fatalf("OpenIssues = %v, want a parse error", err)
	}
}
