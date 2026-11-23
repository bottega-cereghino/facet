package main

import (
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/bottega-cereghino/facet/internal/ghx"
)

// land brings a green queue pull request in without a model run: one
// behind its base is updated by merging the base into it, and one up to
// date is merged with the repository's own method. Neither takes a slot or
// runs the gate. Each pull request gets at most one of them per listing, so
// a refusal is tried again on the next listing, not every tick. It reports
// a conflict on the update, which the caller sends to the fix path.
//
// A draft is not landed, and neither is a pull request with no checks: no
// checks cannot tell a pull request without CI from one whose checks have
// not been listed yet.
func (r *queueRunner) land(iss queuedIssue, pr ghx.OpenPR, listed time.Time) bool {
	if pr.IsDraft || len(pr.Checks) == 0 {
		return false
	}
	status := strings.ToUpper(pr.MergeStateStatus)
	switch status {
	case "", "UNKNOWN", "DRAFT", "DIRTY":
		return false
	}
	key := iss.Repo + "#" + strconv.Itoa(pr.Number)
	r.mu.Lock()
	last, acted := r.landed[key]
	if acted && !listed.After(last) {
		r.mu.Unlock()
		return false
	}
	r.landed[key] = listed
	r.mu.Unlock()
	if status == "BEHIND" {
		return r.updatePR(iss, pr)
	}
	r.mergePR(iss, pr)
	return false
}

// updatePR merges the base into a green pull request that is behind it.
func (r *queueRunner) updatePR(iss queuedIssue, pr ghx.OpenPR) bool {
	k := iss.key()
	if r.dry {
		r.logf("dry-run: would update %s PR #%d: merge %s into it", k, pr.Number, baseName(pr))
		return false
	}
	if r.env.updateBranch == nil {
		return false
	}
	err := r.env.updateBranch(iss.Repo, pr.Number)
	r.forgetListing(iss.Repo)
	switch {
	case errors.Is(err, ghx.ErrUpdateConflict):
		r.logf("queue: update %s PR #%d: %s does not merge cleanly; fixing it as a conflict", k, pr.Number, baseName(pr))
		return true
	case err != nil:
		r.logf("queue: update %s PR #%d: %v; trying again on the next listing", k, pr.Number, err)
		return false
	}
	r.logf("queue: updated %s PR #%d: merged %s into it", k, pr.Number, baseName(pr))
	return false
}

// mergePR merges a green, up to date pull request with the repository's
// own method. A merge GitHub refuses is logged, never forced.
func (r *queueRunner) mergePR(iss queuedIssue, pr ghx.OpenPR) {
	k := iss.key()
	if r.dry {
		r.logf("dry-run: would merge %s PR #%d", k, pr.Number)
		return
	}
	if r.env.mergePR == nil || r.env.mergeMethod == nil {
		return
	}
	method, err := r.mergeMethodFor(iss.Repo)
	if err != nil {
		r.logf("queue: merge %s PR #%d: %v; trying again on the next listing", k, pr.Number, err)
		return
	}
	if err := r.env.mergePR(iss.Repo, pr.Number, method, pr.HeadRefOid); err != nil {
		r.logf("queue: merge %s PR #%d: %v; trying again on the next listing", k, pr.Number, err)
		return
	}
	r.forgetListing(iss.Repo)
	r.mu.Lock()
	delete(r.state.Fixes, k)
	r.mu.Unlock()
	r.saveState()
	r.logf("queue: merged %s PR #%d (%s)", k, pr.Number, method)
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
