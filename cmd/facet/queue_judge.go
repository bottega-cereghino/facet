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

// judgePR reads the latest run of each check, by workflow and name. Pending wins over red, so a check that is running
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

// checkKey is what tells two checks apart: the workflow and the name, so a
// check called "test" in one workflow does not hide one of the same name in
// another. A commit status has no workflow.
func checkKey(c ghx.CheckRun) string {
	if isStatus(c) {
		return "\n" + c.Context
	}
	return c.WorkflowName + "\n" + c.Name
}

// newerCheck reports whether c is a later run than prev of the same check.
// A run that is queued or in progress is newer than a finished one whatever
// its start time says: a queued run has not started, and its time is empty
// or zero. Otherwise the most recently started wins, and of two that
// started together, the later one listed.
func newerCheck(c, prev ghx.CheckRun) bool {
	if cp, pp := checkPending(c), checkPending(prev); cp != pp {
		return cp
	}
	return compareCreated(c.StartedAt, prev.StartedAt) >= 0
}

// latestChecks keeps the latest run of each check, by workflow and name, in
// that order.
func latestChecks(all []ghx.CheckRun) []ghx.CheckRun {
	byKey := map[string]ghx.CheckRun{}
	for _, c := range all {
		key := checkKey(c)
		if prev, ok := byKey[key]; ok && !newerCheck(c, prev) {
			continue
		}
		byKey[key] = c
	}
	keys := make([]string, 0, len(byKey))
	for k := range byKey {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]ghx.CheckRun, len(keys))
	for i, k := range keys {
		out[i] = byKey[k]
	}
	return out
}
