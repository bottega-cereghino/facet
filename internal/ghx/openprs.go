package ghx

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
)

// OpenPRLimit is how many open pull requests OpenPRs reads from one
// repository. A result at this limit may be truncated, so OpenPRs returns an
// error instead of treating it as complete.
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
	// Author opened the pull request. The queue takes as its own only a
	// pull request opened by the account it runs as or by an App it is
	// told to trust; gh prints an App's login as app/<slug>.
	Author User `json:"author"`
	// Body is the pull request's description. The queue looks in it for
	// the line its own agent was told to end it with.
	Body string `json:"body"`
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
	// WorkflowName is the workflow a check run belongs to, so two checks of
	// the same name in different workflows are told apart. Empty for a
	// commit status.
	WorkflowName string `json:"workflowName"`
}

// openPRFields is what OpenPRs asks gh for.
const openPRFields = "number,headRefName,headRefOid,baseRefName,isCrossRepository,isDraft,createdAt," +
	"author,body,mergeable,mergeStateStatus,statusCheckRollup"

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
	if len(prs) == OpenPRLimit {
		return nil, fmt.Errorf("list open pull requests in %s: reached limit %d; results may be incomplete", repo, OpenPRLimit)
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

// MergeMethod reads which merge method repo allows on branch: squash,
// merge or rebase, in that order of preference when more than one is. It
// intersects the repository settings with any branch ruleset restrictions
// (allowed_merge_methods and required_linear_history). When branch is empty,
// or the rules are absent (see absentSource), only the repository settings
// are read.
func (CLI) MergeMethod(repo, branch string) (string, error) {
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
	var (
		ruleAllowed   map[string]bool
		restricted    bool
		linearHistory bool
	)
	if branch = strings.TrimSpace(branch); branch != "" {
		rulesBody, absent, err := apiGet(fmt.Sprintf("repos/%s/rules/branches/%s?per_page=%d", repo, url.PathEscape(branch), restPerPage), "--paginate")
		if err != nil {
			return "", err
		}
		if !absent {
			ruleAllowed, restricted, linearHistory, err = parseRuleMergeMethods(rulesBody)
			if err != nil {
				return "", fmt.Errorf("parse merge rules for %s %s: %w", repo, branch, err)
			}
		}
	}
	repoAllowed := map[string]bool{
		"squash": allowed.Squash,
		"merge":  allowed.Merge,
		"rebase": allowed.Rebase,
	}
	for _, m := range []string{"squash", "merge", "rebase"} {
		if !repoAllowed[m] {
			continue
		}
		if linearHistory && m == "merge" {
			continue
		}
		if restricted && !ruleAllowed[m] {
			continue
		}
		return m, nil
	}
	return "", fmt.Errorf("%s allows no merge method\nfix: allow squash, merge or rebase in the repository's settings and branch ruleset", repo)
}

func parseRuleMergeMethods(body []byte) (allowed map[string]bool, restricted, linearHistory bool, err error) {
	var rules []struct {
		Type       string `json:"type"`
		Parameters struct {
			AllowedMergeMethods []string `json:"allowed_merge_methods"`
		} `json:"parameters"`
	}
	// --paginate prints one JSON array per page, back to back.
	dec := json.NewDecoder(bytes.NewReader(body))
	for {
		var page []struct {
			Type       string `json:"type"`
			Parameters struct {
				AllowedMergeMethods []string `json:"allowed_merge_methods"`
			} `json:"parameters"`
		}
		if err := dec.Decode(&page); err == io.EOF {
			break
		} else if err != nil {
			return nil, false, false, err
		}
		rules = append(rules, page...)
	}
	for _, rule := range rules {
		switch rule.Type {
		case "required_linear_history":
			linearHistory = true
		case "pull_request":
			if len(rule.Parameters.AllowedMergeMethods) == 0 {
				continue
			}
			set := map[string]bool{}
			for _, m := range rule.Parameters.AllowedMergeMethods {
				set[strings.ToLower(strings.TrimSpace(m))] = true
			}
			if !restricted {
				allowed = set
				restricted = true
				continue
			}
			for m := range allowed {
				if !set[m] {
					delete(allowed, m)
				}
			}
		}
	}
	return allowed, restricted, linearHistory, nil
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

// PRFiles lists the file paths changed by pull request number in repo.
// It uses the paginated REST endpoint so a pull request with more than one
// page of files is not silently truncated.
func (CLI) PRFiles(repo string, number int) ([]string, error) {
	if _, _, err := splitRepo("PRFiles", repo); err != nil {
		return nil, err
	}
	out, err := run("api", "--paginate",
		fmt.Sprintf("repos/%s/pulls/%d/files?per_page=%d", repo, number, restPerPage))
	if err != nil {
		return nil, err
	}
	files, err := parsePRFiles(out)
	if err != nil {
		return nil, fmt.Errorf("parse files of %s#%d: %w", repo, number, err)
	}
	return files, nil
}

func parsePRFiles(out []byte) ([]string, error) {
	dec := json.NewDecoder(strings.NewReader(string(out)))
	var all []string
	for dec.More() {
		var page []struct {
			Filename         string `json:"filename"`
			PreviousFilename string `json:"previous_filename"`
		}
		if err := dec.Decode(&page); err != nil {
			return nil, err
		}
		for _, f := range page {
			if f.Filename != "" {
				all = append(all, f.Filename)
			}
			if f.PreviousFilename != "" && f.PreviousFilename != f.Filename {
				all = append(all, f.PreviousFilename)
			}
		}
	}
	return all, nil
}

// HeadBehindBase reports whether base has commits that head does not
// contain, via GitHub's compare endpoint. A pull request whose head is
// behind its base must merge the base in and pass checks again before
// landing, whatever the repository's branch protection requires.
func (CLI) HeadBehindBase(repo, base, head string) (bool, error) {
	if _, _, err := splitRepo("HeadBehindBase", repo); err != nil {
		return false, err
	}
	if base == "" || head == "" {
		return false, fmt.Errorf("compare %s: base and head are required", repo)
	}
	out, err := run("api",
		fmt.Sprintf("repos/%s/compare/%s...%s", repo, url.PathEscape(base), url.PathEscape(head)))
	if err != nil {
		return false, err
	}
	var cmp struct {
		Status   string `json:"status"`
		BehindBy int    `json:"behind_by"`
	}
	if err := json.Unmarshal(out, &cmp); err != nil {
		return false, fmt.Errorf("parse compare %s %s...%s: %w", repo, base, head, err)
	}
	if cmp.BehindBy > 0 {
		return true, nil
	}
	switch strings.ToLower(cmp.Status) {
	case "ahead", "identical":
		return false, nil
	case "behind", "diverged":
		return true, nil
	default:
		return false, fmt.Errorf("compare %s %s...%s: unexpected status %q", repo, base, head, cmp.Status)
	}
}

// Viewer is the login of the account gh runs as.
func (CLI) Viewer() (string, error) {
	out, err := run("api", "user")
	if err != nil {
		return "", err
	}
	var u User
	if err := json.Unmarshal(out, &u); err != nil {
		return "", fmt.Errorf("parse the signed-in user: %w", err)
	}
	if u.Login == "" {
		return "", errors.New("the signed-in user has no login\nfix: run gh auth status, and gh auth login if it shows no account")
	}
	return u.Login, nil
}
