package ghx

import (
	"encoding/json"
	"fmt"
	"strconv"
)

// OpenPRLimit is how many open pull requests OpenPRBranches reads from one
// repository. A repository with more than this is truncated, and the caller
// can see that it got exactly this many.
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
	branches := make([]string, len(prs))
	for i, pr := range prs {
		branches[i] = pr.HeadRefName
	}
	return branches, nil
}
