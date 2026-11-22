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

func TestOpenIssuesParsesCreatedAtAndLabels(t *testing.T) {
	stdout := `[
	  {
	    "number": 7,
	    "title": "Fix the bell",
	    "url": "https://github.com/acme/widgets/issues/7",
	    "state": "OPEN",
	    "labels": [{"id": "LA_x", "name": "next", "color": "ededed"}, {"name": "p1"}],
	    "createdAt": "2026-01-02T03:04:05Z"
	  },
	  {
	    "number": 8,
	    "title": "Later",
	    "url": "https://github.com/acme/widgets/issues/8",
	    "state": "OPEN",
	    "labels": [{"name": "next"}],
	    "createdAt": "2026-02-02T03:04:05Z"
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
	names := got[0].LabelNames()
	if len(names) != 2 || names[0] != "next" || names[1] != "p1" {
		t.Errorf("labels = %q", names)
	}
	if got[1].CreatedAt != "2026-02-02T03:04:05Z" || len(got[1].LabelNames()) != 1 {
		t.Errorf("second = %+v", got[1])
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
