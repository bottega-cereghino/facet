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
func (r *queueRunner) land(iss queuedIssue, pr ghx.OpenPR, got prListing, mergedRepo map[string]bool) bool {
	if pr.IsDraft || len(pr.Checks) == 0 {
		return false
	}
	status := strings.ToUpper(pr.MergeStateStatus)
	switch status {
	case "", "UNKNOWN", "DRAFT", "DIRTY":
		return false
	}
	key := iss.Repo + "#" + strconv.Itoa(pr.Number)
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
	behind, err := r.isBehindBase(iss.Repo, pr, status)
	if err != nil {
		r.markLanded(key, got.at)
		r.logf("queue: check base of %s PR #%d: %v; trying again on the next listing", iss.key(), pr.Number, err)
		return false
	}
	if behind {
		r.markLanded(key, got.at)
		return r.updatePR(iss, pr)
	}
	if mergedRepo[iss.Repo] {
		return false
	}
	r.markLanded(key, got.at)
	if r.mergePR(iss, pr) {
		mergedRepo[iss.Repo] = true
		r.clearWaited(key)
	}
	return false
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
// own method. A merge GitHub refuses is logged, never forced. It reports
// whether the pull request was merged (or would be merged in dry-run).
func (r *queueRunner) mergePR(iss queuedIssue, pr ghx.OpenPR) bool {
	k := iss.key()
	if r.dry {
		r.logf("dry-run: would merge %s PR #%d", k, pr.Number)
		return true
	}
	if r.env.mergePR == nil || r.env.mergeMethod == nil {
		return false
	}
	method, err := r.mergeMethodFor(iss.Repo)
	if err != nil {
		r.logf("queue: merge %s PR #%d: %v; trying again on the next listing", k, pr.Number, err)
		return false
	}
	if err := r.env.mergePR(iss.Repo, pr.Number, method, pr.HeadRefOid); err != nil {
		r.logf("queue: merge %s PR #%d: %v; trying again on the next listing", k, pr.Number, err)
		return false
	}
	r.forgetListing(iss.Repo)
	r.mu.Lock()
	delete(r.state.Fixes, k)
	r.mu.Unlock()
	r.saveState()
	r.logf("queue: merged %s PR #%d (%s)", k, pr.Number, method)
	return true
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
