package ghx

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// OpenIssueLimit is how many open issues one repository contributes to a
// listing. gh's own default is 30 and it does not say when it stopped, so the
// cap is named here: a repository with more open issues carrying the label
// than this is truncated, and the caller can see that it got exactly this many.
const OpenIssueLimit = 1000

// OpenIssues lists open issues in repo that carry label.
//
// The JSON is gh's `--json` shape, including createdAt and milestone, so a
// caller can order the list itself. gh's own order is not part of the result.
func (CLI) OpenIssues(repo, label string) ([]Issue, error) {
	if _, _, err := splitRepo("OpenIssues", repo); err != nil {
		return nil, err
	}
	if strings.TrimSpace(label) == "" {
		return nil, fmt.Errorf("OpenIssues: a label is required\nfix: name the label whose open issues you want, for example next")
	}
	out, err := run("issue", "list",
		"--repo", repo,
		"--state", "open",
		"--label", label,
		"--limit", strconv.Itoa(OpenIssueLimit),
		"--json", openIssueFields,
	)
	if err != nil {
		return nil, err
	}
	var found []Issue
	if err := json.Unmarshal(out, &found); err != nil {
		return nil, fmt.Errorf("parse open issues in %s: %w", repo, err)
	}
	return found, nil
}

// openIssueFields is the --json list OpenIssues asks for. createdAt and
// milestone are what an ordering needs; the body is left out on purpose,
// because a queue does not put someone else's issue text on a command line.
const openIssueFields = "number,title,url,state,labels,createdAt,milestone"
