package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/bottega-cereghino/facet/internal/ghx"
)

// prVerdict is what the queue makes of one pull request this round.
type prVerdict int

const (
	prGreen prVerdict = iota
	// prPending is not judged: a check is still running, or GitHub has not
	// worked out whether it merges.
	prPending
	prNeedsFix
)

// redConclusions are the check results that make a pull request red.
var redConclusions = []string{"FAILURE", "TIMED_OUT", "ACTION_REQUIRED", "STARTUP_FAILURE", "ERROR"}

// judgePR reads a pull request as `gh pr checks` would: the latest run of
// each check by name. Pending wins over red, so a check that is running
// again is not counted as a failure. A conflict is reported before a red
// check, since a fix has to merge the base first.
func judgePR(pr ghx.OpenPR) (prVerdict, string) {
	if pr.Mergeable == "" || strings.EqualFold(pr.Mergeable, "UNKNOWN") {
		return prPending, ""
	}
	latest := latestChecks(pr.Checks)
	for _, c := range latest {
		if checkPending(c) {
			return prPending, ""
		}
	}
	if strings.EqualFold(pr.Mergeable, "CONFLICTING") {
		base := pr.BaseRefName
		if base == "" {
			base = "its base"
		}
		return prNeedsFix, "conflicts with " + base
	}
	for _, c := range latest {
		if res := checkResult(c); isRed(res) {
			return prNeedsFix, fmt.Sprintf("red: %s is %s", checkName(c), strings.ToUpper(res))
		}
	}
	return prGreen, ""
}

func isStatus(c ghx.CheckRun) bool {
	return c.Typename == "StatusContext" || (c.Typename == "" && c.Context != "")
}

func checkName(c ghx.CheckRun) string {
	if isStatus(c) {
		return c.Context
	}
	return c.Name
}

func checkPending(c ghx.CheckRun) bool {
	if isStatus(c) {
		return strings.EqualFold(c.State, "PENDING") || strings.EqualFold(c.State, "EXPECTED")
	}
	return !strings.EqualFold(c.Status, "COMPLETED")
}

func checkResult(c ghx.CheckRun) string {
	if isStatus(c) {
		return c.State
	}
	return c.Conclusion
}

func isRed(result string) bool {
	for _, r := range redConclusions {
		if strings.EqualFold(result, r) {
			return true
		}
	}
	return false
}

// latestChecks keeps the most recently started run of each check name, in
// name order. Of two that started together, the later one listed wins.
func latestChecks(all []ghx.CheckRun) []ghx.CheckRun {
	byName := map[string]ghx.CheckRun{}
	for _, c := range all {
		name := checkName(c)
		if prev, ok := byName[name]; ok && compareCreated(c.StartedAt, prev.StartedAt) < 0 {
			continue
		}
		byName[name] = c
	}
	names := make([]string, 0, len(byName))
	for n := range byName {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]ghx.CheckRun, len(names))
	for i, n := range names {
		out[i] = byName[n]
	}
	return out
}
