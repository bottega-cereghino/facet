package main

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

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

// stallAfter is how long a queue pull request may keep the same head and the
// same verdict before the queue reports it as unlanded.
const stallAfter = 2 * time.Hour

// decisionStalled is the decision log row for a queue pull request that
// stayed unlanded for stallAfter.
const decisionStalled = "stalled"

// stallWatch is the clock of one queue pull request. A new head or a new
// reason starts it again. It is kept in memory: a restart of the queue
// starts every clock again.
type stallWatch struct {
	issue    string
	repo     string
	number   int
	sig      string
	reason   string
	since    time.Time
	reported bool
}

// stalledPR is a reported stall, as the heartbeat and `facet queue status`
// show it.
type stalledPR struct {
	Repo   string    `json:"repo"`
	Number int       `json:"number"`
	Reason string    `json:"reason"`
	Since  time.Time `json:"since"`
}

// warning is the line `facet queue status` prints for s.
func (s stalledPR) warning() string {
	return fmt.Sprintf("warning: %s#%d has not landed since %s: %s; fix: a person reads the pull request", s.Repo, s.Number, s.Since.Format(time.RFC3339), s.Reason)
}

// alreadyReported is whether the queue labelled and notified for pr already:
// block after the last fix try, or stopMerging after five refused merges.
func (r *queueRunner) alreadyReported(iss queuedIssue, pr ghx.OpenPR) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec := r.state.Fixes[iss.key()]
	hold := r.state.Merges[landKey(iss.Repo, pr.Number)]
	return (rec != nil && rec.Blocked) || (hold != nil && hold.Stopped)
}

// stallReason is why pr is not landing now, or "" when nothing is wrong:
// it is green and landable, or it is already reported (see alreadyReported).
// The reason names what holds it, so a new reason starts the clock again.
func (r *queueRunner) stallReason(repo string, pr ghx.OpenPR, reported bool) string {
	if reported {
		return ""
	}
	if pr.IsDraft {
		return "the pull request is a draft"
	}
	if len(pr.Checks) == 0 {
		return "the pull request has no checks"
	}
	switch verdict, why := judgePR(pr); verdict {
	case prPending:
		return "a check is pending, or GitHub has not computed whether it merges"
	case prNeedsFix:
		return why
	}
	if why := r.landBlockReason(repo, pr); why != "" {
		return "not landable: " + why
	}
	if r.cfg.Fix != nil && !r.cfg.Fix.Land {
		return "fix.land is off, so the queue does not merge it"
	}
	return "green, but not merged (merge state " + pr.MergeStateStatus + ")"
}

// watchStall keeps the clock of the queue pull request pr of iss and reports
// it once when its head and reason have not changed for stallAfter. reason
// "" clears the clock. When it reports, an issue that lost the queue label,
// is closed or is blocked names that instead, because it is the cause.
func (r *queueRunner) watchStall(ctx context.Context, iss queuedIssue, pr ghx.OpenPR, reason string) {
	key := landKey(iss.Repo, pr.Number)
	if reason == "" {
		r.mu.Lock()
		delete(r.stalls, key)
		r.mu.Unlock()
		return
	}
	sig := pr.HeadRefOid + "\n" + reason
	now := r.env.now()
	r.mu.Lock()
	w := r.stalls[key]
	if w == nil || w.sig != sig {
		w = &stallWatch{issue: iss.key(), repo: iss.Repo, number: pr.Number, sig: sig, reason: reason, since: now}
		r.stalls[key] = w
	}
	if w.reported || now.Sub(w.since) < stallAfter {
		r.mu.Unlock()
		return
	}
	since := w.since
	r.mu.Unlock()
	if why := r.touchStop(iss); why != "" {
		reason = why
	}
	age := now.Sub(since).Round(time.Minute)
	if r.dry {
		r.logf("dry-run: would report %s PR #%d unlanded for %s: %s", iss.key(), pr.Number, age, reason)
		return
	}
	r.mu.Lock()
	if w := r.stalls[key]; w != nil && w.sig == sig {
		w.reported = true
		w.reason = reason
	}
	r.mu.Unlock()
	r.recordDecision(iss.Repo, strconv.Itoa(iss.Issue.Number), "land", decisionStalled, fmt.Sprintf("PR #%d unlanded for %s: %s", pr.Number, age, reason))
	r.logf("queue: %s PR #%d has not landed for %s: %s", iss.key(), pr.Number, age, reason)
	r.writeHeartbeat()
	if r.cfg.Fix == nil || len(r.cfg.Fix.Notify) == 0 {
		return
	}
	vars := map[string]string{"repo": iss.Repo, "number": strconv.Itoa(iss.Issue.Number), "pr": strconv.Itoa(pr.Number), "reason": reason}
	if err := r.runCmd(ctx, "", expandArgv(r.cfg.Fix.Notify, vars), nil); err != nil {
		r.logf("queue: %s#%d notify: %v", iss.Repo, iss.Issue.Number, err)
	}
}

// clearStalls drops the clocks of issue key k: its pull request is merged,
// closed or no longer the queue's.
func (r *queueRunner) clearStalls(k string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for key, w := range r.stalls {
		if w.issue == k {
			delete(r.stalls, key)
		}
	}
}

// stalledListLocked is the reported stalls, in a stable order. The caller
// holds r.mu.
func (r *queueRunner) stalledListLocked() []stalledPR {
	out := make([]stalledPR, 0, len(r.stalls))
	for _, w := range r.stalls {
		if w.reported {
			out = append(out, stalledPR{Repo: w.repo, Number: w.number, Reason: w.reason, Since: w.since.UTC()})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Repo != out[j].Repo {
			return out[i].Repo < out[j].Repo
		}
		return out[i].Number < out[j].Number
	})
	return out
}

// watchUnowned keeps the clock of a pull request on iss's spawn branch that
// the queue does not own: no run marker, or opened by someone else. The
// queue leaves it alone, so only the report tells a person.
func (r *queueRunner) watchUnowned(ctx context.Context, iss queuedIssue, prs []ghx.OpenPR) {
	pr, ok := branchPR(iss, prs)
	if !ok {
		r.clearStalls(iss.key())
		return
	}
	r.watchStall(ctx, iss, pr, "the pull request is not the queue's (no run marker, or another author), so the queue leaves it alone")
}
