package main

import (
	"time"

	"github.com/bottega-cereghino/facet/internal/ghx"
)

// prListing is one repository's open pull requests, and when they were
// listed. files caches each pull request's changed-file list for the life
// of the listing.
type prListing struct {
	prs   []ghx.OpenPR
	files map[int][]string
	err   error
	at    time.Time
}

// prListTTL is how long the fix stage reuses a repository's listing. Every
// session on the machine shares one GitHub API limit, so the fix stage does
// not list each repository every tick: a pull request is judged at most
// this late. A listing older than a fix run's settling time is never used
// to judge that run's pull request.
const prListTTL = 5 * time.Minute

// listPRs is repo's open pull requests for new work's open pull request
// check: the round's listing, else a cached one younger than prListTTL,
// else a new one shared with the fix stage. An issue waiting for its pull
// request to land is seen in every round's pick, so a fresh listing each
// round would spend a call per tick on a pull request nothing acts on. An
// issue this process started is not started again whatever the listing
// says, and a restarted process lists afresh.
func (r *queueRunner) listPRs(repo string) ([]ghx.OpenPR, error) {
	got := r.fixListing(repo, time.Time{})
	return got.prs, got.err
}

func (r *queueRunner) roundListing(repo string) prListing {
	r.mu.Lock()
	got, ok := r.round[repo]
	r.mu.Unlock()
	if ok {
		return got
	}
	prs, err := r.env.prs(repo)
	var files map[int][]string
	if err == nil {
		files = map[int][]string{}
	}
	got = prListing{prs: prs, files: files, err: err, at: r.env.now()}
	r.mu.Lock()
	r.round[repo] = got
	if err == nil {
		r.prCache[repo] = got
	}
	r.mu.Unlock()
	if err == nil {
		r.pruneIgnored(repo, prs)
	}
	return got
}

// fixListing is repo's listing for the fix stage: the round's, else a cached
// one younger than prListTTL and taken no earlier than after, else a new one.
func (r *queueRunner) fixListing(repo string, after time.Time) prListing {
	r.mu.Lock()
	got, inRound := r.round[repo]
	cached, ok := r.prCache[repo]
	now := r.env.now()
	r.mu.Unlock()
	if inRound {
		return got
	}
	if ok && now.Sub(cached.at) < prListTTL && !cached.at.Before(after) {
		return cached
	}
	return r.roundListing(repo)
}

// heldListing is a listing already in hand for repo, without a call.
func (r *queueRunner) heldListing(repo string) (prListing, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if got, ok := r.round[repo]; ok && got.err == nil {
		return got, true
	}
	got, ok := r.prCache[repo]
	return got, ok && r.env.now().Sub(got.at) < prListTTL
}

// forgetListing drops repo's cached listing after the queue changed one of
// its pull requests, so the next round sees the change.
func (r *queueRunner) forgetListing(repo string) {
	r.mu.Lock()
	delete(r.prCache, repo)
	r.mu.Unlock()
}

// prFiles returns pull request number's changed files, cached with got so
// each pull request is read at most once per listing.
func (r *queueRunner) prFiles(repo string, number int, got prListing) ([]string, error) {
	if got.files != nil {
		r.mu.Lock()
		cached, ok := got.files[number]
		r.mu.Unlock()
		if ok {
			return cached, nil
		}
	}
	files, err := r.env.prFiles(repo, number)
	if err != nil {
		return nil, err
	}
	if got.files != nil {
		r.mu.Lock()
		got.files[number] = files
		r.mu.Unlock()
	}
	return files, nil
}
