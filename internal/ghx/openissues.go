package ghx

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// OpenIssueLimit is how many open issues one repository contributes to a
// listing. A repository with more open issues carrying the label than this is
// truncated, and the caller can see that it got exactly this many.
const OpenIssueLimit = 1000

// openIssuePerPage is the page size REST allows at most.
const openIssuePerPage = 100

// OpenIssues lists open issues in repo that carry label.
//
// It reads REST, not GraphQL: `gh issue list` spends the GraphQL budget on
// every call, and a spent budget stopped the queue. Each page is a conditional
// read (see condGetPage), so an unchanged list answers 304 and costs no rate
// limit. A rate-limit answer is a *RateLimitError. Pull requests, which the
// issues endpoint also returns, are dropped.
//
// The result carries createdAt and labels, so a caller can order the list
// itself. The order is not part of the result.
func (CLI) OpenIssues(repo, label string) ([]Issue, error) {
	if _, _, err := splitRepo("OpenIssues", repo); err != nil {
		return nil, err
	}
	if strings.TrimSpace(label) == "" {
		return nil, fmt.Errorf("OpenIssues: a label is required\nfix: name the label whose open issues you want, for example next")
	}
	var found []Issue
	next := fmt.Sprintf("repos/%s/issues?labels=%s&state=open&per_page=%d", repo, url.QueryEscape(label), openIssuePerPage)
	for next != "" && len(found) < OpenIssueLimit {
		page, err := condGetPage(next)
		if err != nil {
			return nil, err
		}
		items, err := parseOpenIssuePage(page.Body)
		if err != nil {
			return nil, fmt.Errorf("parse open issues in %s: %w", repo, err)
		}
		found = append(found, items...)
		next = page.Next
	}
	if len(found) > OpenIssueLimit {
		found = found[:OpenIssueLimit]
	}
	return found, nil
}

// parseOpenIssuePage maps one page of REST issues onto Issue: created_at to
// CreatedAt, html_url to URL (the page, as ViewIssue maps it) and the
// lower-case state to gh's upper-case one. A pull request has a pull_request
// field and is dropped. The body is left out on purpose, because a queue does
// not put someone else's issue text on a command line.
func parseOpenIssuePage(body []byte) ([]Issue, error) {
	var raw []struct {
		Number      int             `json:"number"`
		Title       string          `json:"title"`
		HTMLURL     string          `json:"html_url"`
		State       string          `json:"state"`
		Labels      []Label         `json:"labels"`
		CreatedAt   string          `json:"created_at"`
		PullRequest json.RawMessage `json:"pull_request"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	out := make([]Issue, 0, len(raw))
	for _, r := range raw {
		if len(r.PullRequest) > 0 && string(r.PullRequest) != "null" {
			continue
		}
		out = append(out, Issue{
			Number:    r.Number,
			Title:     r.Title,
			URL:       r.HTMLURL,
			State:     strings.ToUpper(r.State),
			Labels:    r.Labels,
			CreatedAt: r.CreatedAt,
		})
	}
	return out, nil
}
