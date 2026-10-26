package ghx

import "testing"

// The REST API's `url` is the API address. Issue.URL is the page: the spawn
// plan prints it, the manifest records it, and the board move hands it to
// `gh project item-add`, which wants the page.
func TestParseRESTIssueTakesThePageURL(t *testing.T) {
	body := []byte(`{
		"number": 67,
		"title": "Rehearse a restore",
		"state": "open",
		"url": "https://api.github.com/repos/acme/platform/issues/67",
		"html_url": "https://github.com/acme/platform/issues/67",
		"labels": [{"name": "area/backups"}]
	}`)
	iss, err := parseRESTIssue(body)
	if err != nil {
		t.Fatal(err)
	}
	if want := "https://github.com/acme/platform/issues/67"; iss.URL != want {
		t.Errorf("URL = %q, want %q", iss.URL, want)
	}
	if iss.Number != 67 || iss.State != "OPEN" || len(iss.Labels) != 1 {
		t.Errorf("parsed %+v", iss)
	}
}
