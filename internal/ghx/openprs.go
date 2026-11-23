package ghx

import (
	"encoding/json"
	"fmt"
	"strconv"
)

// OpenPRLimit is how many open pull requests OpenPRBranches reads from one
// repository. A result at this limit may be truncated, so OpenPRBranches
// returns an error instead of treating it as complete.
const OpenPRLimit = 1000

// OpenPRBranches lists the head branch of every open pull request in repo.
func (CLI) OpenPRBranches(repo string) ([]string, error) {
	if _, _, err := splitRepo("OpenPRBranches", repo); err != nil {
		return nil, err
	}
	out, err := run("pr", "list",
		"--repo", repo,
		"--state", "open",
		"--limit", strconv.Itoa(OpenPRLimit),
		"--json", "headRefName",
	)
	if err != nil {
		return nil, err
	}
	var prs []struct {
		HeadRefName string `json:"headRefName"`
	}
	if err := json.Unmarshal(out, &prs); err != nil {
		return nil, fmt.Errorf("parse open pull requests in %s: %w", repo, err)
	}
	if len(prs) == OpenPRLimit {
		return nil, fmt.Errorf("list open pull requests in %s: reached limit %d; results may be incomplete", repo, OpenPRLimit)
	}
	branches := make([]string, len(prs))
	for i, pr := range prs {
		branches[i] = pr.HeadRefName
	}
	return branches, nil
}
