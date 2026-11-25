package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/bottega-cereghino/facet/internal/ghx"
)

// queueRunTrailer is the key of the line the queue's agent is told to end
// its pull request's description with. The value is a token made for that
// start and kept in the state file. A pull request on the issue's branch
// that does not carry it was not opened by that run: a person's own pull
// request has the same branch shape, and the queue must never fix, update
// or merge it.
const queueRunTrailer = "Facet-Queue-Run"

// markerInstruction is added to a prompt that does not place {marker}
// itself, so the agent always learns the line.
const markerInstruction = " End the pull request's description with this line, exactly as written: {marker}"

// newRunMarker is a token for one start. It only has to tell one run's pull
// request from another's and from a person's, so 64 random bits are plenty.
func newRunMarker() string {
	b := make([]byte, 8)
	// crypto/rand.Read does not fail: since Go 1.24 it crashes the program
	// rather than return an error.
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// markerLine is the trailer line for token.
func markerLine(token string) string {
	return queueRunTrailer + ": " + token
}

// hasMarker reports whether body has the trailer line for token on a line
// of its own. An empty token, from a start recorded before markers, is
// never found.
func hasMarker(body, token string) bool {
	if token == "" {
		return false
	}
	want := markerLine(token)
	for _, line := range strings.Split(body, "\n") {
		if strings.TrimSpace(line) == want {
			return true
		}
	}
	return false
}

// promptWithMarker is the prompt template for a start that has a marker:
// the configured prompt, with the instruction added when it does not place
// {marker} itself.
func promptWithMarker(prompt, marker string) string {
	if marker == "" || strings.Contains(prompt, "{marker}") {
		return prompt
	}
	return prompt + markerInstruction
}

// ownsPR reports whether pr is the one the start s opened: from this
// repository, not a fork; carrying s's trailer line; and opened by the
// account the queue runs as or by an App named in fix.apps. The trailer
// alone is never enough. An error means the account cannot be read, and
// the caller must not treat that as "not the queue's" and forget the start.
func (r *queueRunner) ownsPR(s startedIssue, pr ghx.OpenPR) (bool, error) {
	if !hasMarker(pr.Body, s.Marker) {
		return false, nil
	}
	why, err := r.ownershipFailure(pr)
	return why == "" && err == nil, err
}

// ownershipFailure is which condition of ownsPR, other than the trailer
// line, pr fails: "" when it passes. An error means the account cannot be
// read.
func (r *queueRunner) ownershipFailure(pr ghx.OpenPR) (string, error) {
	if pr.IsCrossRepository {
		return ignoredFork, nil
	}
	if r.cfg.Fix != nil && authorIsApp(pr.Author.Login, r.cfg.Fix.Apps) {
		return "", nil
	}
	login, err := r.viewerLogin()
	if err != nil {
		return "", err
	}
	if strings.EqualFold(pr.Author.Login, login) {
		return "", nil
	}
	return ignoredAuthor, nil
}

// authorIsApp reports whether login is one of apps, given by slug. gh's
// --json author prints an App as app/<slug>, and the REST API as
// <slug>[bot]. Only those two forms match: a person's login holds neither a
// slash nor brackets, so no account can pass as the App, while the bare
// slug is a login anyone may register.
func authorIsApp(login string, apps []string) bool {
	login = strings.ToLower(login)
	slug, ok := strings.CutPrefix(login, "app/")
	if !ok {
		slug, ok = strings.CutSuffix(login, "[bot]")
	}
	if !ok || slug == "" {
		return false
	}
	for _, app := range apps {
		if slug == app {
			return true
		}
	}
	return false
}

// viewerLogin is the account the queue runs as, read once per process.
func (r *queueRunner) viewerLogin() (string, error) {
	r.mu.Lock()
	login := r.viewer
	r.mu.Unlock()
	if login != "" {
		return login, nil
	}
	if r.env.viewer == nil {
		return "", fmt.Errorf("cannot tell which account the queue runs as: no viewer seam")
	}
	login, err := r.env.viewer()
	if err != nil {
		return "", fmt.Errorf("read the account the queue runs as: %w\nfix: run gh auth status; the queue takes as its own only a pull request that account opened", err)
	}
	r.mu.Lock()
	r.viewer = login
	r.mu.Unlock()
	return login, nil
}

// touchStop is why the queue must leave iss's pull request alone now, or
// "" when it may fix, update or merge it. The issue is read again, once per
// round, right before such an act: one closed, without the queue label, or
// carrying the block label is a person's call now. An issue that cannot be
// read is not touched either.
func (r *queueRunner) touchStop(iss queuedIssue) string {
	k := iss.key()
	r.mu.Lock()
	why, ok := r.issueRound[k]
	r.mu.Unlock()
	if ok {
		return why
	}
	why = r.readTouchStop(iss)
	r.mu.Lock()
	r.issueRound[k] = why
	r.mu.Unlock()
	if why != "" {
		r.noteOnce(k+" touch", why, func() {
			r.logf("queue: %s: %s; leaving its pull request alone", k, why)
		})
	} else {
		r.mu.Lock()
		delete(r.noted, k+" touch")
		r.mu.Unlock()
	}
	return why
}

func (r *queueRunner) readTouchStop(iss queuedIssue) string {
	if r.env.issue == nil {
		return "the issue cannot be read: no issue seam"
	}
	got, err := r.env.issue(iss.Repo, iss.Issue.Number)
	if err != nil {
		return "the issue cannot be read: " + firstErrorLine(err.Error())
	}
	if got == nil {
		return "the issue cannot be read"
	}
	if !got.IsOpen() {
		return "the issue is closed"
	}
	labels := got.LabelNames()
	if !hasLabelFold(labels, r.cfg.Label) {
		return "the issue no longer carries the " + r.cfg.Label + " label"
	}
	if r.cfg.Fix != nil && hasLabelFold(labels, r.cfg.Fix.BlockLabel) {
		return "the issue is labelled " + r.cfg.Fix.BlockLabel
	}
	return ""
}

func hasLabelFold(labels []string, want string) bool {
	for _, l := range labels {
		if strings.EqualFold(l, want) {
			return true
		}
	}
	return false
}

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
