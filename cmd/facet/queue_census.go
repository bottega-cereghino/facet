package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// The reasons the census gives an open, labelled issue. Every issue gets
// exactly one. The census reads only what the tick already read, so a
// reason that needs a call the tick did not make is not here: a pull
// request is judged only for a repository whose listing is already in hand.
const (
	censusRunning       = "running"
	censusStartable     = "startable"
	censusPRPending     = "pr_pending"
	censusPRRed         = "pr_red"
	censusPRConflict    = "pr_conflict"
	censusPROrphan      = "pr_orphan"
	censusBlockedFix    = "blocked_after_fix"
	censusBlockedLabel  = "blocked_label"
	censusRanNoPR       = "ran_no_pr"
	censusPerRepoCap    = "per_repo_cap"
	censusGateWait      = "gate_wait"
	censusRetryWait     = "retry_wait"
	censusCapped        = "capped"
	censusLogEvery      = time.Hour
	censusDefaultBlock  = "blocked"
	censusFileLogSuffix = "l"
)

// The kinds of configuration drift the census names.
const (
	driftPriorityNoRepo = "priority_not_in_repos"
	driftNoRoutingKey   = "repo_no_routing_key"
	driftNotInPriority  = "repo_not_in_priorities"
)

// censusDrift is one finding where the config, the priorities file and the
// routing file disagree. Subject is a project name or a repository.
type censusDrift struct {
	Kind    string `json:"kind"`
	Subject string `json:"subject"`
	Detail  string `json:"detail"`
}

// censusIssue is one issue and the one reason it is not, or is, startable.
type censusIssue struct {
	Repo   string `json:"repo"`
	Number int    `json:"number"`
	Reason string `json:"reason"`
}

// queueCensus is census.json: every open issue the tick saw with the queue
// label, each with one reason. The counts always sum to Total.
type queueCensus struct {
	At        time.Time      `json:"at"`
	Total     int            `json:"total"`
	Startable int            `json:"startable"`
	Running   int            `json:"running"`
	FreeSlots int            `json:"free_slots"`
	ByReason  map[string]int `json:"by_reason"`
	Issues    []censusIssue  `json:"issues"`
	Drift     []censusDrift  `json:"drift"`
}

// censusRow is one line of census.jsonl: the counts without the issues.
type censusRow struct {
	At        time.Time      `json:"at"`
	Total     int            `json:"total"`
	Startable int            `json:"startable"`
	Running   int            `json:"running"`
	FreeSlots int            `json:"free_slots"`
	ByReason  map[string]int `json:"by_reason"`
}

// censusLogPath is the hourly log beside the census file: census.json
// gives census.jsonl.
func censusLogPath(census string) string {
	return census + censusFileLogSuffix
}

// censusReason is the one reason for iss. The caller holds r.mu. skipped is
// the issues the pick passed over as capped.
func (r *queueRunner) censusReason(iss queuedIssue, now time.Time, skipped map[string]bool) string {
	k := iss.key()
	block := censusDefaultBlock
	if r.cfg.Fix != nil && r.cfg.Fix.BlockLabel != "" {
		block = r.cfg.Fix.BlockLabel
	}
	for _, l := range iss.Issue.LabelNames() {
		if strings.EqualFold(l, block) {
			if rec := r.fixRecordLocked(k); rec != nil && rec.Blocked {
				return censusBlockedFix
			}
			return censusBlockedLabel
		}
	}
	if r.inflight[k] {
		return censusRunning
	}
	if _, alive := r.activeAgentAliveLocked(k); alive {
		return censusRunning
	}
	if why := r.censusPR(iss); why != "" {
		return why
	}
	if r.done[k] {
		return censusRanNoPR
	}
	if r.repoRunning[iss.Repo] >= r.cfg.PerRepo {
		return censusPerRepoCap
	}
	if until, ok := r.repoWait[iss.Repo]; ok && now.Before(until) {
		return censusGateWait
	}
	if until, ok := r.retryAt[k]; ok && now.Before(until) {
		return censusRetryWait
	}
	if skipped[k] {
		return censusCapped
	}
	return censusStartable
}

func (r *queueRunner) fixRecordLocked(k string) *fixRecord {
	if r.state == nil {
		return nil
	}
	return r.state.Fixes[k]
}

// censusPR is the reason an open pull request from iss's branch gives, or
// "" when there is none or the repository's listing is not in hand. It never
// lists: a listing the tick did not make is not made for the census. The
// caller holds r.mu.
func (r *queueRunner) censusPR(iss queuedIssue) string {
	got, ok := r.round[iss.Repo]
	if !ok || got.err != nil {
		return ""
	}
	pr, ok := branchPR(iss, got.prs)
	if !ok {
		return ""
	}
	if r.state != nil {
		if _, tracked := r.state.Started[iss.key()]; !tracked {
			return censusPROrphan
		}
	}
	verdict, why := judgePR(pr)
	switch {
	case verdict != prNeedsFix:
		return censusPRPending
	case strings.HasPrefix(why, "conflicts"):
		return censusPRConflict
	default:
		return censusPRRed
	}
}

// buildCensus gives each issue its reason. The counts sum to the number of
// issues, because every issue lands in exactly one count.
func (r *queueRunner) buildCensus(ranked []queuedIssue, skipped map[string]bool, projects []string) queueCensus {
	r.mu.Lock()
	now := r.env.now()
	c := queueCensus{
		At:        now.UTC(),
		Total:     len(ranked),
		FreeSlots: max(r.cfg.Max-r.running, 0),
		ByReason:  map[string]int{},
		Issues:    make([]censusIssue, 0, len(ranked)),
	}
	for _, iss := range ranked {
		why := r.censusReason(iss, now, skipped)
		c.ByReason[why]++
		c.Issues = append(c.Issues, censusIssue{Repo: iss.Repo, Number: iss.Issue.Number, Reason: why})
	}
	r.mu.Unlock()
	c.Drift = r.censusDrift(projects)
	c.Startable = c.ByReason[censusStartable]
	c.Running = c.ByReason[censusRunning]
	sort.SliceStable(c.Issues, func(i, j int) bool {
		a, b := c.Issues[i], c.Issues[j]
		if a.Repo != b.Repo {
			return a.Repo < b.Repo
		}
		return a.Number < b.Number
	})
	return c
}

// repoName is the part of owner/name after the slash.
func repoName(repo string) string {
	if _, n, ok := strings.Cut(repo, "/"); ok && n != "" {
		return n
	}
	return repo
}

// censusDrift lists where the priorities file, the config's repos and the
// routing file disagree. It makes no GitHub call: the routing check is the
// local seam spawn uses, and a nil seam skips that finding. A name in the
// priorities file that no repository carries never starts; a repository the
// file does not name ranks last; a repository without a routing key fails
// every spawn. Without a priorities file only the routing finding applies.
func (r *queueRunner) censusDrift(projects []string) []censusDrift {
	drift := []censusDrift{}
	for _, p := range projects {
		found := false
		for _, repo := range r.cfg.Repos {
			if strings.EqualFold(repoName(repo), p) {
				found = true
				break
			}
		}
		if !found {
			drift = append(drift, censusDrift{Kind: driftPriorityNoRepo, Subject: p,
				Detail: "named in priorities but in no repos entry, so it never starts\nfix: add its owner/name to repos, or remove the line"})
		}
	}
	for _, repo := range r.cfg.Repos {
		if len(projects) > 0 && projectRank(repo, projects) == len(projects) {
			drift = append(drift, censusDrift{Kind: driftNotInPriority, Subject: repo,
				Detail: "in repos but not in priorities, so it ranks after every named project\nfix: add " + repoName(repo) + " to the priorities file"})
		}
		if r.env.route == nil {
			continue
		}
		if err := r.env.route(repo); err != nil {
			drift = append(drift, censusDrift{Kind: driftNoRoutingKey, Subject: repo, Detail: err.Error()})
		}
	}
	return drift
}

// writeCensus writes census.json atomically on every pick, and appends one
// row to census.jsonl when an hour has passed since the last row. A dry run
// or a config without census does nothing. A failed write is logged and
// never stops the queue.
func (r *queueRunner) writeCensus(ranked []queuedIssue, skipped map[string]bool, projects []string) {
	if r.dry || r.cfg.Census == "" {
		return
	}
	c := r.buildCensus(ranked, skipped, projects)
	if err := writeFileAtomic(r.cfg.Census, c); err != nil {
		r.logf("queue: census: %v", err)
	}
	r.mu.Lock()
	due := r.censusLogged.IsZero() || !c.At.Before(r.censusLogged.Add(censusLogEvery))
	if due {
		r.censusLogged = c.At
	}
	r.mu.Unlock()
	if !due {
		return
	}
	row := censusRow{At: c.At, Total: c.Total, Startable: c.Startable, Running: c.Running, FreeSlots: c.FreeSlots, ByReason: c.ByReason}
	if err := appendJSONLine(censusLogPath(r.cfg.Census), row); err != nil {
		r.logf("queue: census: %v", err)
	}
}

// writeFileAtomic writes v as indented JSON: a temp file in the same
// directory, flushed, renamed into place, the directory flushed.
func writeFileAtomic(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("write %s: %w\nfix: the directory of census must be writable", path, err)
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp*")
	if err != nil {
		return fmt.Errorf("write %s: %w\nfix: the directory of census must exist and be writable", path, err)
	}
	name := tmp.Name()
	_, werr := tmp.Write(append(b, '\n'))
	serr := syncFile(tmp)
	cerr := tmp.Close()
	if err := firstErr(werr, serr, cerr); err != nil {
		_ = os.Remove(name)
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return fmt.Errorf("write %s: %w", path, err)
	}
	return syncDir(dir)
}

func appendJSONLine(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("append %s: %w", path, err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("append %s: %w", path, err)
	}
	_, werr := f.Write(append(b, '\n'))
	if err := firstErr(werr, f.Close()); err != nil {
		return fmt.Errorf("append %s: %w", path, err)
	}
	return nil
}
