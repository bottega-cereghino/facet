package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"

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
	if pr.IsCrossRepository || !hasMarker(pr.Body, s.Marker) {
		return false, nil
	}
	if r.cfg.Fix != nil && authorIsApp(pr.Author.Login, r.cfg.Fix.Apps) {
		return true, nil
	}
	login, err := r.viewerLogin()
	if err != nil {
		return false, err
	}
	return strings.EqualFold(pr.Author.Login, login), nil
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
