package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/bottega-cereghino/facet/internal/ghx"
)

// maxMergeRefusals is how many times GitHub may refuse the same head before
// the queue stops trying that pull request.
const maxMergeRefusals = 5

// maxMergeBackoff is the longest wait between refused merges. The wait
// starts at the queue's wait and doubles each refusal, and it does not
// grow past this.
const maxMergeBackoff = time.Hour

// land brings a green queue pull request in without a model run: one
// behind its base is updated by merging the base into it, and one up to
// date is merged with the repository's own method. Neither takes a slot or
// runs the gate. Each pull request gets at most one of them per listing, so
// a refusal is tried again on the next listing, not every tick. It reports
// a conflict on the update, which the caller sends to the fix path.
//
// Every queue merge goes through the update-and-wait path whatever the
// repository's branch protection requires: if the base moved past the head
// commit that checks ran on, the base is merged in and checks run again.
// At most one pull request per repository is merged per round. When
// serialize is configured, at most one open queue pull request touching a
// shared file lands at a time per repository, oldest first.
//
// A draft is not landed, and neither is a pull request with no checks: no
// checks cannot tell a pull request without CI from one whose checks have
// not been listed yet.
func (r *queueRunner) land(ctx context.Context, iss queuedIssue, pr ghx.OpenPR, got prListing, mergedRepo map[string]bool) bool {
	if pr.IsDraft || len(pr.Checks) == 0 {
		return false
	}
	if why := r.landBlockReason(iss.Repo, pr); why != "" {
		return false
	}
	status := strings.ToUpper(pr.MergeStateStatus)
	switch status {
	case "", "UNKNOWN", "DRAFT", "DIRTY":
		return false
	}
	key := landKey(iss.Repo, pr.Number)
	// A refused merge waits on its own clock. Skip the base check until
	// that wait is over: the pull request was up to date when the merge
	// was refused. A head GitHub now calls BEHIND is updated anyway.
	if status != "BEHIND" && !r.mayMerge(pr, key) {
		r.stopMerging(ctx, iss, pr)
		return false
	}
	if r.actedOnListing(key, got.at) {
		return false
	}
	if r.cfg.Serialize != nil {
		waiting, err := r.waitSerialized(iss, pr, got)
		if err != nil {
			r.markLanded(key, got.at)
			r.logf("queue: %s PR #%d: %v; trying again on the next listing", iss.key(), pr.Number, err)
			return false
		}
		if waiting {
			return false
		}
	}
	// An update or a merge acts on the pull request: read the issue again
	// first.
	if r.touchStop(iss) != "" {
		return false
	}
	behind, err := r.isBehindBase(iss.Repo, pr, status)
	if err != nil {
		r.markLanded(key, got.at)
		r.logf("queue: check base of %s PR #%d: %v; trying again on the next listing", iss.key(), pr.Number, err)
		return false
	}
	if behind {
		r.markLanded(key, got.at)
		if !r.mayRetry(r.updateHolds, key, pr.HeadRefOid) {
			return false
		}
		return r.updatePR(iss, pr)
	}
	if mergedRepo[iss.Repo] {
		return false
	}
	if r.mergePR(ctx, iss, pr, got.at) {
		mergedRepo[iss.Repo] = true
		r.clearWaited(key)
	}
	return false
}

func landKey(repo string, pr int) string {
	return repo + "#" + strconv.Itoa(pr)
}

func (r *queueRunner) actedOnListing(key string, listed time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	last, acted := r.landed[key]
	return acted && !listed.After(last)
}

func (r *queueRunner) markLanded(key string, listed time.Time) {
	r.mu.Lock()
	r.landed[key] = listed
	r.mu.Unlock()
}

func (r *queueRunner) isBehindBase(repo string, pr ghx.OpenPR, status string) (bool, error) {
	if status == "BEHIND" {
		return true, nil
	}
	if r.env.behindBase == nil || pr.HeadRefOid == "" {
		return false, nil
	}
	return r.env.behindBase(repo, baseName(pr), pr.HeadRefOid)
}

// updatePR merges the base into a green pull request that is behind it.
// A failure other than a conflict keeps the listing and backs the pull
// request off with the merge backoff, logged once per step, so a failing
// update is not tried on every tick.
func (r *queueRunner) updatePR(iss queuedIssue, pr ghx.OpenPR) bool {
	k := iss.key()
	if r.dry {
		r.logf("dry-run: would update %s PR #%d: merge %s into it", k, pr.Number, baseName(pr))
		return false
	}
	if r.env.updateBranch == nil {
		return false
	}
	key := landKey(iss.Repo, pr.Number)
	err := r.env.updateBranch(iss.Repo, pr.Number)
	if err != nil && !errors.Is(err, ghx.ErrUpdateConflict) {
		wait := r.noteFailure(r.updateHolds, key, pr.HeadRefOid)
		r.logf("queue: update %s PR #%d: %v; trying again after %s", k, pr.Number, err, wait)
		return false
	}
	r.clearFailure(r.updateHolds, key)
	r.forgetListing(iss.Repo)
	if err != nil {
		r.logf("queue: update %s PR #%d: %s does not merge cleanly; fixing it as a conflict", k, pr.Number, baseName(pr))
		return true
	}
	r.logf("queue: updated %s PR #%d: merged %s into it", k, pr.Number, baseName(pr))
	return false
}

// retryHold is one pull request's failed reads or updates of one kind, for
// this process. A new head starts the count again.
type retryHold struct {
	head  string
	count int
	next  time.Time
}

// mayRetry reports whether the step that failed for key at head may be
// tried again now.
func (r *queueRunner) mayRetry(holds map[string]*retryHold, key, head string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	h := holds[key]
	if h == nil || h.head != head {
		return true
	}
	return !r.env.now().Before(h.next)
}

// noteFailure counts one failure of the step for key at head and returns
// how long it now waits: the queue's wait, doubling up to an hour, as a
// refused merge does.
func (r *queueRunner) noteFailure(holds map[string]*retryHold, key, head string) time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	h := holds[key]
	if h == nil || h.head != head {
		h = &retryHold{head: head}
		holds[key] = h
	}
	h.count++
	wait := mergeBackoff(r.cfg.Wait, h.count)
	h.next = r.env.now().Add(wait)
	return wait
}

func (r *queueRunner) clearFailure(holds map[string]*retryHold, key string) {
	r.mu.Lock()
	delete(holds, key)
	r.mu.Unlock()
}

// mergePR merges a green, up to date pull request with the repository's
// own method. A merge GitHub refuses is logged, never forced, and retried
// with a growing wait. It reports whether the pull request was merged (or
// would be merged in dry-run).
func (r *queueRunner) mergePR(ctx context.Context, iss queuedIssue, pr ghx.OpenPR, listed time.Time) bool {
	k := iss.key()
	if r.dry {
		r.logf("dry-run: would merge %s PR #%d", k, pr.Number)
		return true
	}
	if r.env.mergePR == nil || r.env.mergeMethod == nil {
		return false
	}
	key := landKey(iss.Repo, pr.Number)
	method, err := r.mergeMethodFor(iss.Repo)
	if err != nil {
		if !r.actedOnListing(key, listed) {
			r.markLanded(key, listed)
			r.logf("queue: merge %s PR #%d: %v; trying again on the next listing", k, pr.Number, err)
		}
		return false
	}
	if err := r.env.mergePR(iss.Repo, pr.Number, method, pr.HeadRefOid); err != nil {
		r.noteMergeRefusal(iss, pr, err)
		r.stopMerging(ctx, iss, pr)
		return false
	}
	r.forgetListing(iss.Repo)
	r.mu.Lock()
	delete(r.state.Fixes, k)
	delete(r.state.Merges, key)
	r.mu.Unlock()
	r.saveState()
	r.logf("queue: merged %s PR #%d (%s)", k, pr.Number, method)
	return true
}

// mergeBackoff is the wait after refusal n (1 is the queue's wait, 2 is
// twice that) capped at maxMergeBackoff.
func mergeBackoff(base time.Duration, n int) time.Duration {
	if n < 1 || base <= 0 {
		return 0
	}
	d := base
	for i := 1; i < n; i++ {
		if d > maxMergeBackoff/2 {
			return maxMergeBackoff
		}
		d *= 2
	}
	if d > maxMergeBackoff {
		return maxMergeBackoff
	}
	return d
}

// mayMerge reports whether a refused merge may be tried now. A different
// head or a different check result clears the refusal. The state is saved
// when it is cleared, so a restart does not keep a count for a head that
// has already changed.
func (r *queueRunner) mayMerge(pr ghx.OpenPR, key string) bool {
	if r.state == nil {
		return true
	}
	fp := checksFingerprint(pr.Checks)
	r.mu.Lock()
	h := r.state.Merges[key]
	if h == nil {
		r.mu.Unlock()
		return true
	}
	if h.Head != pr.HeadRefOid || h.Checks != fp {
		delete(r.state.Merges, key)
		r.mu.Unlock()
		r.saveState()
		return true
	}
	if h.Stopped || r.env.now().Before(h.Next) {
		r.mu.Unlock()
		return false
	}
	r.mu.Unlock()
	return true
}

// noteMergeRefusal records one refusal of this head and logs it once for
// this step. The wait doubles each time, up to an hour.
func (r *queueRunner) noteMergeRefusal(iss queuedIssue, pr ghx.OpenPR, cause error) {
	if r.state == nil {
		r.logf("queue: merge %s PR #%d: %v", iss.key(), pr.Number, cause)
		return
	}
	key := landKey(iss.Repo, pr.Number)
	fp := checksFingerprint(pr.Checks)
	now := r.env.now()
	r.mu.Lock()
	if r.state.Merges == nil {
		r.state.Merges = map[string]*mergeHold{}
	}
	h := r.state.Merges[key]
	if h == nil || h.Head != pr.HeadRefOid || h.Checks != fp {
		h = &mergeHold{Head: pr.HeadRefOid, Checks: fp}
		r.state.Merges[key] = h
	}
	if h.Stopped {
		r.mu.Unlock()
		return
	}
	h.Count++
	h.Reason = cause.Error()
	count := h.Count
	var wait time.Duration
	if count >= maxMergeRefusals {
		h.Stopped = true
		h.Next = now
	} else {
		wait = mergeBackoff(r.cfg.Wait, count)
		h.Next = now.Add(wait)
	}
	r.mu.Unlock()
	r.saveState()
	if count >= maxMergeRefusals {
		r.logf("queue: merge %s PR #%d: %s; refusal %d of %d, not trying again", iss.key(), pr.Number, cause, count, maxMergeRefusals)
		return
	}
	r.logf("queue: merge %s PR #%d: %s; refusal %d of %d, trying again after %s", iss.key(), pr.Number, cause, count, maxMergeRefusals, wait)
}

// stopMerging adds the block label and notifies once, after five refusals
// of the same head. It does not mark the fix record blocked: a new head
// clears the refusal and the merge is tried again. The label stays.
func (r *queueRunner) stopMerging(ctx context.Context, iss queuedIssue, pr ghx.OpenPR) {
	if r.state == nil || r.cfg.Fix == nil {
		return
	}
	key := landKey(iss.Repo, pr.Number)
	r.mu.Lock()
	h := r.state.Merges[key]
	if h == nil || !h.Stopped || h.Labeled {
		r.mu.Unlock()
		return
	}
	reason := h.Reason
	if now := r.env.now(); now.Before(r.labelRetry[iss.key()]) {
		r.mu.Unlock()
		return
	}
	r.mu.Unlock()
	fix := r.cfg.Fix
	if r.dry {
		r.logf("dry-run: would label %s#%d %s: merge refused %d times (%s)", iss.Repo, iss.Issue.Number, fix.BlockLabel, maxMergeRefusals, reason)
		return
	}
	if r.env.addLabel == nil {
		r.noteOnce(key+" label", "missing", func() {
			r.logf("queue: %s#%d: cannot add the %s label: no label seam", iss.Repo, iss.Issue.Number, fix.BlockLabel)
		})
		return
	}
	if err := r.env.addLabel(iss.Repo, iss.Issue.Number, fix.BlockLabel); err != nil {
		r.mu.Lock()
		r.labelRetry[iss.key()] = r.env.now().Add(r.cfg.Wait)
		r.mu.Unlock()
		r.logf("queue: %s#%d: add label %s: %v\nfix: create the label %s in %s, or set fix.blockLabel to a label that exists; trying again after %s",
			iss.Repo, iss.Issue.Number, fix.BlockLabel, err, fix.BlockLabel, iss.Repo, r.cfg.Wait)
		return
	}
	r.mu.Lock()
	if h := r.state.Merges[key]; h != nil {
		h.Labeled = true
	}
	r.mu.Unlock()
	r.saveState()
	r.logf("queue: %s#%d PR #%d: merge refused %d times (%s); labelled %s", iss.Repo, iss.Issue.Number, pr.Number, maxMergeRefusals, reason, fix.BlockLabel)
	if len(fix.Notify) == 0 {
		return
	}
	vars := map[string]string{"repo": iss.Repo, "number": strconv.Itoa(iss.Issue.Number), "pr": strconv.Itoa(pr.Number), "reason": reason}
	if err := r.env.run(ctx, "", expandArgv(fix.Notify, vars)); err != nil {
		r.logf("queue: %s#%d notify: %v", iss.Repo, iss.Issue.Number, err)
	}
}

// checksFingerprint is the latest check results, so a new run or a new
// conclusion resets a merge refusal. The names are already sorted.
func checksFingerprint(checks []ghx.CheckRun) string {
	latest := latestChecks(checks)
	var b strings.Builder
	for _, c := range latest {
		fmt.Fprintf(&b, "%s %s %s %s %s\n", checkName(c), c.Status, c.Conclusion, c.State, c.StartedAt)
	}
	return b.String()
}

// checkRules is one base branch's required checks, remembered for the process.
type checkRules struct {
	names []string
	known bool
}

// landBlockReason is why pr is not green enough to land. Empty means it is.
// Every latest check must have passed: success, or neutral or skipped for a
// check the base branch does not require. Required checks come from the
// base branch; when they cannot be read, every check is held to that rule
// and none is known to be required.
func (r *queueRunner) landBlockReason(repo string, pr ghx.OpenPR) string {
	names, known := r.requiredNames(repo, pr.BaseRefName)
	if ok, why := landableChecks(pr, names, known); !ok {
		return why
	}
	return ""
}

func (r *queueRunner) requiredNames(repo, branch string) ([]string, bool) {
	branch = strings.TrimSpace(branch)
	if branch == "" || r.env.requiredChecks == nil {
		return nil, false
	}
	key := repo + "\n" + branch
	r.mu.Lock()
	if got, ok := r.rules[key]; ok {
		r.mu.Unlock()
		return got.names, got.known
	}
	r.mu.Unlock()
	names, known, err := r.env.requiredChecks(repo, branch)
	if err != nil {
		r.logf("queue: required checks on %s %s: %v; a non-success check is not green", repo, branch, err)
		names, known = nil, false
	}
	r.mu.Lock()
	r.rules[key] = checkRules{names: names, known: known}
	r.mu.Unlock()
	return names, known
}

// landableChecks reports whether the checks allow a merge. Every latest
// check must be complete and a success, or neutral or skipped when it is not
// required: a cancelled, stale, failed or unset check never passes, whether
// or not the base branch requires it. When known is true, every required
// name must also be present and a success: a skipped or missing required
// check does not pass. An empty required list that is known checks every
// check the same way.
func landableChecks(pr ghx.OpenPR, required []string, known bool) (bool, string) {
	latest := latestChecks(pr.Checks)
	isRequired := map[string]bool{}
	if known {
		for _, name := range required {
			isRequired[name] = true
		}
	}
	byName := map[string]ghx.CheckRun{}
	for _, c := range latest {
		name := checkName(c)
		byName[name] = c
		if checkPending(c) {
			return false, name + " is pending"
		}
		res := strings.ToUpper(checkResult(c))
		switch {
		case res == "SUCCESS":
		case (res == "NEUTRAL" || res == "SKIPPED") && !isRequired[name]:
		default:
			if res == "" {
				res = "UNSET"
			}
			return false, fmt.Sprintf("%s is %s", name, res)
		}
	}
	for _, name := range required {
		if !known {
			break
		}
		if _, ok := byName[name]; !ok {
			return false, name + " is missing"
		}
	}
	return true, ""
}

// mergeMethodFor reads the repository's merge method once per process.
func (r *queueRunner) mergeMethodFor(repo string) (string, error) {
	r.mu.Lock()
	m, ok := r.mergeMethods[repo]
	r.mu.Unlock()
	if ok {
		return m, nil
	}
	m, err := r.env.mergeMethod(repo)
	if err != nil {
		return "", err
	}
	r.mu.Lock()
	r.mergeMethods[repo] = m
	r.mu.Unlock()
	return m, nil
}
