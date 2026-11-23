package ghx

import (
	"encoding/json"
	"fmt"
	"strconv"
)

// OpenPRLimit is how many open pull requests OpenPRs reads from one
// repository. A repository with more than this is truncated, and the caller
// can see that it got exactly this many.
const OpenPRLimit = 1000

// OpenPR is one open pull request, with what the queue needs to tell
// whether it is red or conflicting.
type OpenPR struct {
	Number      int    `json:"number"`
	HeadRefName string `json:"headRefName"`
	BaseRefName string `json:"baseRefName"`
	// Mergeable is MERGEABLE, CONFLICTING or UNKNOWN.
	Mergeable string `json:"mergeable"`
	// Checks is every check run and commit status on the head commit, as
	// GitHub reports them, older runs of the same check included.
	Checks []CheckRun `json:"statusCheckRollup"`
}

// CheckRun is one entry of a pull request's statusCheckRollup. A check run
// fills Name, Status and Conclusion; a commit status fills Context and
// State.
type CheckRun struct {
	Typename   string `json:"__typename"`
	Name       string `json:"name"`
	Context    string `json:"context"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	State      string `json:"state"`
	StartedAt  string `json:"startedAt"`
}

// OpenPRs lists every open pull request in repo with its head branch, its
// mergeable state and its checks, in one call.
func (CLI) OpenPRs(repo string) ([]OpenPR, error) {
	if _, _, err := splitRepo("OpenPRs", repo); err != nil {
		return nil, err
	}
	out, err := run("pr", "list",
		"--repo", repo,
		"--state", "open",
		"--limit", strconv.Itoa(OpenPRLimit),
		"--json", "number,headRefName,baseRefName,mergeable,statusCheckRollup",
	)
	if err != nil {
		return nil, err
	}
	var prs []OpenPR
	if err := json.Unmarshal(out, &prs); err != nil {
		return nil, fmt.Errorf("parse open pull requests in %s: %w", repo, err)
	}
	return prs, nil
}

// AddIssueLabel adds label to the issue. The label must already exist in
// the repository.
func (CLI) AddIssueLabel(repo string, number int, label string) error {
	if _, _, err := splitRepo("AddIssueLabel", repo); err != nil {
		return err
	}
	_, err := run("issue", "edit", strconv.Itoa(number), "--repo", repo, "--add-label", label)
	return err
}
