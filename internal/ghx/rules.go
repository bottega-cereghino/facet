package ghx

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// RequiredChecks lists the status-check contexts the base branch requires,
// from its rulesets and from classic branch protection. known is false when
// either source fails for a reason other than being absent, so the caller
// does not treat a partial answer as the whole list. An absent source (see
// absentSource) contributes nothing.
func (CLI) RequiredChecks(repo, branch string) (names []string, known bool, err error) {
	if _, _, err := splitRepo("RequiredChecks", repo); err != nil {
		return nil, false, err
	}
	if strings.TrimSpace(branch) == "" {
		return nil, false, fmt.Errorf("required checks for %s: the base branch is required", repo)
	}
	branch = url.PathEscape(branch)
	rulesBody, rulesAbsent, rulesErr := apiGet(fmt.Sprintf("repos/%s/rules/branches/%s", repo, branch))
	var rules []string
	if rulesErr == nil && !rulesAbsent {
		rules, rulesErr = parseRuleContexts(rulesBody)
		if rulesErr != nil {
			rulesErr = fmt.Errorf("parse rules for %s: %w", repo, rulesErr)
		}
	}
	protBody, protAbsent, protErr := apiGet(fmt.Sprintf("repos/%s/branches/%s/protection/required_status_checks", repo, branch))
	var prot []string
	if protErr == nil && !protAbsent {
		prot, protErr = parseProtectionContexts(protBody)
		if protErr != nil {
			protErr = fmt.Errorf("parse protection for %s: %w", repo, protErr)
		}
	}
	return combineRequired(rules, rulesAbsent, rulesErr, prot, protAbsent, protErr)
}

// planLimitAnswer is gh's whole answer when the repository's plan has no
// rulesets or branch protection (a private repository on a free plan).
const planLimitAnswer = "gh: Upgrade to GitHub Pro or make this repository public to enable this feature. (HTTP 403)"

// apiGet reads one REST source of branch rules. absent means the repository
// has no such source, which is an answer and not an error.
func apiGet(path string, flags ...string) (body []byte, absent bool, err error) {
	body, err = run(append(append([]string{"api"}, flags...), path)...)
	if err == nil {
		return body, false, nil
	}
	if absentSource(err.Error()) {
		return nil, true, nil
	}
	return nil, false, err
}

// absentSource reports whether a failed gh api call says the source is not
// there: HTTP 404, or exactly planLimitAnswer. Any other 403 (a missing
// permission, a rate limit) is not. msg is run's error text, the command line
// and then gh's stderr. Only gh's words at the end of a line count, because
// the command line holds repository and branch names, which may hold "404".
func absentSource(msg string) bool {
	if strings.HasSuffix(msg, ": "+planLimitAnswer) {
		return true
	}
	// gh may add a line after the answer, such as a missing scope.
	for _, line := range strings.Split(msg, "\n") {
		// The second form is gh's when the response gives no message.
		if strings.HasSuffix(line, " (HTTP 404)") || strings.HasSuffix(line, ": gh: HTTP 404") {
			return true
		}
	}
	return false
}

func parseRuleContexts(body []byte) ([]string, error) {
	var rules []struct {
		Type       string `json:"type"`
		Parameters struct {
			Checks []struct {
				Context string `json:"context"`
			} `json:"required_status_checks"`
		} `json:"parameters"`
	}
	if err := json.Unmarshal(body, &rules); err != nil {
		return nil, err
	}
	var out []string
	for _, rule := range rules {
		if rule.Type != "required_status_checks" {
			continue
		}
		for _, c := range rule.Parameters.Checks {
			if c.Context != "" {
				out = append(out, c.Context)
			}
		}
	}
	return out, nil
}

func parseProtectionContexts(body []byte) ([]string, error) {
	var prot struct {
		Contexts []string `json:"contexts"`
		Checks   []struct {
			Context string `json:"context"`
		} `json:"checks"`
	}
	if err := json.Unmarshal(body, &prot); err != nil {
		return nil, err
	}
	out := append([]string{}, prot.Contexts...)
	for _, c := range prot.Checks {
		if c.Context != "" {
			out = append(out, c.Context)
		}
	}
	return out, nil
}

// combineRequired unions the two sources. A real error from either one makes
// the answer unknown. Both absent means nothing is required, and that is known.
func combineRequired(rules []string, rulesAbsent bool, rulesErr error, prot []string, protAbsent bool, protErr error) ([]string, bool, error) {
	if rulesErr != nil && !rulesAbsent {
		return nil, false, rulesErr
	}
	if protErr != nil && !protAbsent {
		return nil, false, protErr
	}
	return unionNames(rules, prot), true, nil
}

func unionNames(lists ...[]string) []string {
	seen := map[string]bool{}
	var out []string
	for _, list := range lists {
		for _, n := range list {
			if n == "" || seen[n] {
				continue
			}
			seen[n] = true
			out = append(out, n)
		}
	}
	return out
}
