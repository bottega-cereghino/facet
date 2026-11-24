package main

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// queueCap limits how often the queue starts standards or CI work in a
// repository whose own check already passes. Nil means there is no cap.
type queueCap struct {
	// Labels are whole labels, stored lowercase. A label matches without
	// regard to case. standards-extra is not standards.
	Labels []string
	// Num and Den are the share: at most Num of every Den starts.
	Num    int
	Den    int
	Passes []string
}

// queueCapFile is the JSON shape of cap, before defaults.
type queueCapFile struct {
	Labels []string `json:"labels"`
	Share  []int    `json:"share"`
	Passes []string `json:"passes"`
}

// normalizeCap resolves the cap block. Nil means the queue starts every
// issue, as before.
func normalizeCap(raw *queueCapFile) (*queueCap, error) {
	if raw == nil {
		return nil, nil
	}
	labels, err := normalizeCapLabels(raw.Labels)
	if err != nil {
		return nil, err
	}
	num, den, err := normalizeCapShare(raw.Share)
	if err != nil {
		return nil, err
	}
	passes, err := normalizeCapPasses(raw.Passes)
	if err != nil {
		return nil, err
	}
	return &queueCap{Labels: labels, Num: num, Den: den, Passes: passes}, nil
}

func normalizeCapLabels(raw []string) ([]string, error) {
	if raw == nil {
		return []string{"standards", "ci"}, nil
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("queue config: cap.labels is empty\nfix: list the labels that are standards or CI work, or omit labels for standards and ci")
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(raw))
	for _, label := range raw {
		n := strings.ToLower(strings.TrimSpace(label))
		if n == "" {
			return nil, fmt.Errorf("queue config: cap.labels has an empty label\nfix: name whole labels, such as standards and ci")
		}
		if seen[n] {
			return nil, fmt.Errorf("queue config: cap.labels lists %q twice\nfix: keep one entry per label", strings.TrimSpace(label))
		}
		seen[n] = true
		out = append(out, n)
	}
	return out, nil
}

// normalizeCapShare reads [at most, of every]. Omitted means [1, 3].
func normalizeCapShare(raw []int) (int, int, error) {
	if raw == nil {
		return 1, 3, nil
	}
	if len(raw) != 2 || raw[0] < 1 || raw[1] < raw[0] {
		return 0, 0, fmt.Errorf("queue config: cap.share must be two numbers [M, N] with 1 <= M <= N\nfix: set share to [1, 3], or omit it for that")
	}
	return raw[0], raw[1], nil
}

// normalizeCapPasses keeps the argument list. Empty means the cap does not
// apply: the repository is still catching up until a check is named.
func normalizeCapPasses(raw []string) ([]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	if strings.TrimSpace(raw[0]) == "" {
		return nil, fmt.Errorf("queue config: cap.passes has an empty command\nfix: set passes to an argument list, with {repo} where the repository goes, or omit passes")
	}
	return raw, nil
}

// matches reports that one of labels is a cap label. The match is the whole
// label, without regard to case.
func (c *queueCap) matches(labels []string) bool {
	if c == nil {
		return false
	}
	for _, label := range labels {
		n := strings.ToLower(strings.TrimSpace(label))
		if n == "" {
			continue
		}
		for _, want := range c.Labels {
			if n == want {
				return true
			}
		}
	}
	return false
}

// noteStart records a start for the cap window. The fix stage drops a
// merged issue from Started; the window still has to count that run.
func (st *queueState) noteStart(rec startedIssue) {
	for i, s := range st.Starts {
		if s.Repo == rec.Repo && s.Number == rec.Number {
			st.Starts[i] = rec
			return
		}
	}
	st.Starts = append(st.Starts, rec)
}

// window is the repository's last n starts, newest first. A start kept only
// in Started still counts, so a state file written before the cap does too.
func (st *queueState) window(repo string, n int) []startedIssue {
	if st == nil || n < 1 {
		return nil
	}
	byKey := map[string]startedIssue{}
	consider := func(s startedIssue) {
		if s.Repo != repo || s.Number == 0 {
			return
		}
		k := s.Repo + "#" + strconv.Itoa(s.Number)
		prev, ok := byKey[k]
		if !ok || s.At.After(prev.At) {
			byKey[k] = s
		}
	}
	for _, s := range st.Starts {
		consider(s)
	}
	for _, s := range st.Started {
		consider(s)
	}
	all := make([]startedIssue, 0, len(byKey))
	for _, s := range byKey {
		all = append(all, s)
	}
	sort.Slice(all, func(i, j int) bool {
		if !all[i].At.Equal(all[j].At) {
			return all[i].At.After(all[j].At)
		}
		return all[i].Number > all[j].Number
	})
	if len(all) > n {
		all = all[:n]
	}
	return all
}

// standardsInWindow is how many of the repository's last Den starts carry a
// cap label, other than except. A candidate's own earlier start does not
// fill the window against itself, so a restarted queue can still run it.
// The caller does not hold r.mu.
func (r *queueRunner) standardsInWindow(repo, except string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state == nil || r.cfg.Cap == nil {
		return 0
	}
	n := 0
	for _, s := range r.state.window(repo, r.cfg.Cap.Den) {
		if s.Repo+"#"+strconv.Itoa(s.Number) == except {
			continue
		}
		if r.cfg.Cap.matches(s.Labels) {
			n++
		}
	}
	return n
}

// capApplies reports that this repository's standards check passed, so the
// cap is on. passes runs once per round per repository. An omitted passes,
// or any result other than exit 0, leaves the cap off. A cancelled queue
// is an error, not a failed check.
func (r *queueRunner) capApplies(ctx context.Context, repo string) (bool, error) {
	c := r.cfg.Cap
	if c == nil || len(c.Passes) == 0 {
		return false, nil
	}
	r.mu.Lock()
	if r.capSeen == nil {
		r.capSeen = map[string]bool{}
		r.capPass = map[string]bool{}
	}
	if r.capSeen[repo] {
		applies := r.capPass[repo]
		r.mu.Unlock()
		return applies, nil
	}
	r.mu.Unlock()

	err := r.env.run(ctx, "", expandArgv(c.Passes, map[string]string{"repo": repo}))
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	applies := err == nil
	r.mu.Lock()
	if r.capSeen == nil {
		r.capSeen = map[string]bool{}
		r.capPass = map[string]bool{}
	}
	r.capSeen[repo] = true
	r.capPass[repo] = applies
	r.mu.Unlock()
	if !applies {
		r.logPassOnce(repo, err)
	}
	return applies, nil
}

func (r *queueRunner) logPassOnce(repo string, err error) {
	msg := fmt.Sprintf("cap: %s standards check did not pass (%v); cap does not apply", repo, err)
	r.mu.Lock()
	if r.cappedLog == nil {
		r.cappedLog = map[string]string{}
	}
	if r.cappedLog["pass:"+repo] == msg {
		r.mu.Unlock()
		return
	}
	r.cappedLog["pass:"+repo] = msg
	r.mu.Unlock()
	if r.dry {
		r.logf("dry-run: %s", msg)
		return
	}
	r.logf("queue: %s", msg)
}

// staysAlone reports that iss is a run of one. A feature or a research
// issue always is. While cap is set, a standards or CI issue is too, so one
// run cannot carry two of them past the share.
func (c queueConfig) staysAlone(labels []string) bool {
	if issueStaysAlone(labels) {
		return true
	}
	return c.Cap != nil && c.Cap.matches(labels)
}

// skipCapped marks issues the cap will not start this round, and logs each
// one once. A later candidate is then taken. Fix runs and landing never
// come through here.
func (r *queueRunner) skipCapped(ctx context.Context, ranked []queuedIssue, skip map[string]bool) error {
	if r.cfg.Cap == nil {
		return nil
	}
	for _, iss := range ranked {
		if skip[iss.key()] || !r.cfg.Cap.matches(iss.Issue.LabelNames()) {
			continue
		}
		r.mu.Lock()
		going := r.inflight[iss.key()] || r.done[iss.key()]
		r.mu.Unlock()
		if going {
			continue
		}
		applies, err := r.capApplies(ctx, iss.Repo)
		if err != nil {
			return err
		}
		if !applies {
			continue
		}
		count := r.standardsInWindow(iss.Repo, iss.key())
		if count < r.cfg.Cap.Num {
			continue
		}
		skip[iss.key()] = true
		r.logCapOnce(iss, count)
	}
	return nil
}

func (r *queueRunner) logCapOnce(iss queuedIssue, count int) {
	msg := fmt.Sprintf("capped: %s, %d of the last %d runs in %s was standards work", iss.key(), count, r.cfg.Cap.Den, iss.Repo)
	r.mu.Lock()
	if r.cappedLog == nil {
		r.cappedLog = map[string]string{}
	}
	if r.cappedLog[iss.key()] == msg {
		r.mu.Unlock()
		return
	}
	r.cappedLog[iss.key()] = msg
	r.mu.Unlock()
	if r.dry {
		r.logf("dry-run: %s", msg)
		return
	}
	r.logf("queue: %s", msg)
}

// resetCapRound forgets this round's passes results, so the next round runs
// the check once per repository again.
func (r *queueRunner) resetCapRound() {
	r.mu.Lock()
	r.capSeen = nil
	r.capPass = nil
	r.mu.Unlock()
}
