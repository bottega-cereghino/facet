package ghx

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
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
	// HeadRefOid is the head commit, so a merge takes exactly the commit
	// that was judged green.
	HeadRefOid  string `json:"headRefOid"`
	BaseRefName string `json:"baseRefName"`
	// IsCrossRepository is set for a pull request from a fork.
	IsCrossRepository bool   `json:"isCrossRepository"`
	IsDraft           bool   `json:"isDraft"`
	CreatedAt         string `json:"createdAt"`
	// Mergeable is MERGEABLE, CONFLICTING or UNKNOWN.
	Mergeable string `json:"mergeable"`
	// MergeStateStatus is GitHub's view of the merge against the base:
	// BEHIND, BLOCKED, CLEAN, DIRTY, DRAFT, HAS_HOOKS, UNKNOWN or UNSTABLE.
	MergeStateStatus string `json:"mergeStateStatus"`
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

// openPRFields is what OpenPRs asks gh for.
const openPRFields = "number,headRefName,headRefOid,baseRefName,isCrossRepository,isDraft,createdAt," +
	"mergeable,mergeStateStatus,statusCheckRollup"

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
		"--json", openPRFields,
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

// ErrUpdateConflict is UpdatePRBranch's answer when the base cannot be
// merged into the pull request without a conflict.
var ErrUpdateConflict = errors.New("the base does not merge cleanly into the pull request")

// UpdatePRBranch merges the base into the pull request's branch on GitHub.
// It never rebases: `gh pr update-branch` merges unless it is told
// --rebase, and a rebase would rewrite a branch someone may have pulled. A
// conflict is reported as ErrUpdateConflict, wrapped with gh's words.
func (CLI) UpdatePRBranch(repo string, number int) error {
	if _, _, err := splitRepo("UpdatePRBranch", repo); err != nil {
		return err
	}
	_, err := run("pr", "update-branch", strconv.Itoa(number), "--repo", repo)
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "conflict") {
		return fmt.Errorf("%w: %w", ErrUpdateConflict, err)
	}
	return err
}

// MergeMethod reads which merge method the repository allows: squash,
// merge or rebase, in that order of preference when more than one is.
func (CLI) MergeMethod(repo string) (string, error) {
	if _, _, err := splitRepo("MergeMethod", repo); err != nil {
		return "", err
	}
	out, err := run("repo", "view", repo, "--json", "squashMergeAllowed,mergeCommitAllowed,rebaseMergeAllowed")
	if err != nil {
		return "", err
	}
	var allowed struct {
		Squash bool `json:"squashMergeAllowed"`
		Merge  bool `json:"mergeCommitAllowed"`
		Rebase bool `json:"rebaseMergeAllowed"`
	}
	if err := json.Unmarshal(out, &allowed); err != nil {
		return "", fmt.Errorf("parse the merge settings of %s: %w", repo, err)
	}
	switch {
	case allowed.Squash:
		return "squash", nil
	case allowed.Merge:
		return "merge", nil
	case allowed.Rebase:
		return "rebase", nil
	}
	return "", fmt.Errorf("%s allows no merge method\nfix: allow squash, merge or rebase in the repository's settings", repo)
}

// MergePR merges the pull request with method (squash, merge or rebase),
// only while its head is still head. It never passes --admin: required
// checks and branch rules are GitHub's to enforce, and a refusal is an
// error for the caller to log.
func (CLI) MergePR(repo string, number int, method, head string) error {
	if _, _, err := splitRepo("MergePR", repo); err != nil {
		return err
	}
	switch method {
	case "squash", "merge", "rebase":
	default:
		return fmt.Errorf("merge %s#%d: unknown merge method %q\nfix: use squash, merge or rebase", repo, number, method)
	}
	args := []string{"pr", "merge", strconv.Itoa(number), "--repo", repo, "--" + method}
	if head != "" {
		args = append(args, "--match-head-commit", head)
	}
	_, err := run(args...)
	return err
}
