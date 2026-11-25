package main

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/bottega-cereghino/facet/internal/ghx"
)

// The conditions of the ownership check that an ignored pull request fails.
const (
	ignoredFork   = "from a fork"
	ignoredAuthor = "author is neither the queue's account nor an App in fix.apps"
)

// ignoredPR is an open pull request that carries a started issue's marker
// line and fails the ownership check. The queue neither lands nor fixes it;
// the heartbeat and `facet queue status` say so.
type ignoredPR struct {
	Repo   string `json:"repo"`
	Number int    `json:"number"`
	Author string `json:"author"`
	// Reason is the condition that failed.
	Reason string `json:"reason"`
	// Issue is the started issue's key; the entry is kept under it.
	Issue string `json:"issue"`
}

// warning is the line `facet queue status` prints for p, with the fix.
func (p ignoredPR) warning() string {
	fix := "the queue takes as its own only a pull request its account or an App in fix.apps opened"
	switch {
	case p.Reason == ignoredFork:
		fix = "a pull request from a fork is never the queue's"
	case strings.HasPrefix(strings.ToLower(p.Author), "app/") || strings.HasSuffix(strings.ToLower(p.Author), "[bot]"):
		slug := strings.TrimSuffix(strings.TrimPrefix(strings.ToLower(p.Author), "app/"), "[bot]")
		fix = fmt.Sprintf("name the App in fix.apps: add %q", slug)
	}
	return fmt.Sprintf("warning: %s#%d is ignored by the queue (author %s: %s); fix: %s", p.Repo, p.Number, p.Author, p.Reason, fix)
}

func ignoredKey(repo string, number int) string {
	return fmt.Sprintf("%s#%d", repo, number)
}

// noteIgnoredPRs replaces the entries of iss with those of prs, a listing of
// its repository: an open pull request on the issue's branch that carries
// the start's marker line and fails the ownership check. A pull request that
// is closed, or passes the check, is no longer in the list. It changes
// nothing the queue does.
func (r *queueRunner) noteIgnoredPRs(iss queuedIssue, prs []ghx.OpenPR) {
	k := iss.key()
	r.mu.Lock()
	s, ok := r.state.Started[k]
	r.mu.Unlock()
	if !ok {
		return
	}
	prefix := issueBranchName(iss.Issue.Number, "")
	found := map[string]ignoredPR{}
	for _, pr := range prs {
		if !strings.HasPrefix(pr.HeadRefName, prefix) || !hasMarker(pr.Body, s.Marker) {
			continue
		}
		why, err := r.ownershipFailure(pr)
		if err != nil {
			// The account cannot be read: keep what is known.
			return
		}
		if why != "" {
			found[ignoredKey(iss.Repo, pr.Number)] = ignoredPR{Repo: iss.Repo, Number: pr.Number, Author: pr.Author.Login, Reason: why, Issue: k}
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.dropIgnoredLocked(k)
	for key, e := range found {
		r.ignored[key] = e
	}
}

// relistIgnored lists each repository that has an entry, through the cached
// listing, so a pull request that was closed leaves the heartbeat even when
// the queue forgot its start and has no other reason to list there. The
// listing is shared with the fix stage: at most one call per prListTTL.
func (r *queueRunner) relistIgnored() {
	r.mu.Lock()
	repos := map[string]bool{}
	for _, e := range r.ignored {
		repos[e.Repo] = true
	}
	r.mu.Unlock()
	for repo := range repos {
		r.fixListing(repo, time.Time{})
	}
}

// pruneIgnored drops the entries of repo whose pull request is not in prs,
// a listing of its open pull requests: a closed one. The entry of a start
// the queue forgot stays until then, because the queue never looks at that
// pull request again and nothing else would clear it.
func (r *queueRunner) pruneIgnored(repo string, prs []ghx.OpenPR) {
	open := map[int]bool{}
	for _, pr := range prs {
		open[pr.Number] = true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for key, e := range r.ignored {
		if e.Repo == repo && !open[e.Number] {
			delete(r.ignored, key)
		}
	}
}

// dropIgnoredLocked removes the entries of issue key k. The caller holds r.mu.
func (r *queueRunner) dropIgnoredLocked(k string) {
	for key, e := range r.ignored {
		if e.Issue == k {
			delete(r.ignored, key)
		}
	}
}

// ignoredListLocked is the entries, in a stable order. The caller holds r.mu.
func (r *queueRunner) ignoredListLocked() []ignoredPR {
	out := make([]ignoredPR, 0, len(r.ignored))
	for _, e := range r.ignored {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Repo != out[j].Repo {
			return out[i].Repo < out[j].Repo
		}
		return out[i].Number < out[j].Number
	})
	if len(out) == 0 {
		return nil
	}
	return out
}
