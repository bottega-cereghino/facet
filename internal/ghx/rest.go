package ghx

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ViewIssue fetches one issue, conditionally.
//
// It moved off `gh issue view` -- which is GraphQL, and so is billed in full
// every time -- onto the REST endpoint carrying the same fields, which can be
// asked conditionally and costs nothing when the issue has not changed.
func (CLI) ViewIssue(repo string, number int) (*Issue, error) {
	res, err := condGet(fmt.Sprintf("repos/%s/issues/%d", repo, number))
	if err != nil {
		return nil, err
	}
	iss, err := parseRESTIssue(res.Body)
	if err != nil {
		return nil, fmt.Errorf("parse issue %s#%d: %w", repo, number, err)
	}
	return iss, nil
}

// parseRESTIssue decodes an issue from the REST API. There, `url` is the API
// address (https://api.github.com/repos/...) and the page a person opens is
// `html_url`. Issue.URL means the page everywhere else in facet (gh's own
// `--json url` is the page, and `gh project item-add --url` wants the page),
// so the REST field is mapped onto it here.
func parseRESTIssue(body []byte) (*Issue, error) {
	var raw struct {
		Issue
		HTMLURL string `json:"html_url"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	iss := raw.Issue
	iss.URL = raw.HTMLURL
	iss.State = strings.ToUpper(iss.State)
	return &iss, nil
}
