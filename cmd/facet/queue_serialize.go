package main

import (
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/bottega-cereghino/facet/internal/ghx"
)

// queueSerializeFile is the JSON shape of serialize, before checks.
type queueSerializeFile struct {
	Paths []string `json:"paths"`
}

// queueSerialize holds the globs of shared files that serialize landing
// per repository. Omitted means no serialization.
type queueSerialize struct {
	Paths []string
}

func normalizeSerialize(raw *queueSerializeFile) (*queueSerialize, error) {
	if raw == nil {
		return nil, nil
	}
	if len(raw.Paths) == 0 {
		return nil, fmt.Errorf("queue config: serialize.paths is empty\nfix: set serialize.paths to a list of globs, or omit serialize")
	}
	paths := make([]string, 0, len(raw.Paths))
	for _, p := range raw.Paths {
		p = strings.TrimSpace(p)
		if p == "" {
			return nil, fmt.Errorf("queue config: serialize.paths has an empty entry\nfix: remove blank entries from serialize.paths")
		}
		if err := validateGlobPattern(p); err != nil {
			return nil, fmt.Errorf("queue config: serialize.paths %q: %w\nfix: use a valid glob pattern such as **/config.json or go.sum", p, err)
		}
		paths = append(paths, p)
	}
	return &queueSerialize{Paths: paths}, nil
}

// validateGlobPattern checks that pattern can match a repository-relative
// file path and that every non-doublestar segment is valid syntax for
// path.Match.
func validateGlobPattern(pattern string) error {
	if strings.HasSuffix(pattern, "/") {
		return fmt.Errorf("trailing slash never matches a file path (use %s** to match files under a directory)", pattern)
	}
	segs := splitGlobPath(pattern)
	if len(segs) == 0 {
		return fmt.Errorf("empty pattern")
	}
	for _, seg := range segs {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("segment %q never matches a file path", seg)
		}
		if seg == "**" {
			continue
		}
		if _, err := path.Match(seg, ""); err != nil {
			return err
		}
	}
	return nil
}

func splitGlobPath(p string) []string {
	p = strings.TrimPrefix(p, "./")
	p = strings.TrimPrefix(p, "/")
	if p == "" {
		return nil
	}
	return strings.Split(p, "/")
}

// matchGlob reports whether filePath matches pattern. A pattern without /
// matches at any depth, as gitignore does; a pattern with / is anchored to
// the repository root. A ** segment matches zero or more path segments;
// every other segment uses path.Match.
func matchGlob(pattern, filePath string) bool {
	pat := splitGlobPath(pattern)
	segs := splitGlobPath(filePath)
	if len(pat) == 0 || len(segs) == 0 {
		return false
	}
	if !strings.Contains(pattern, "/") && pat[0] != "**" {
		pat = append([]string{"**"}, pat...)
	}
	dp := make([]bool, len(segs)+1)
	dp[0] = true
	for _, p := range pat {
		next := make([]bool, len(segs)+1)
		if p == "**" {
			matched := false
			for j := 0; j <= len(segs); j++ {
				if dp[j] {
					matched = true
				}
				next[j] = matched
			}
		} else {
			for j := 0; j < len(segs); j++ {
				if dp[j] {
					ok, _ := path.Match(p, segs[j])
					if ok {
						next[j+1] = true
					}
				}
			}
		}
		dp = next
	}
	return dp[len(segs)]
}

// matchingSharedFile returns the first path in files that matches any glob
// in patterns, or "".
func matchingSharedFile(files, patterns []string) string {
	for _, f := range files {
		for _, p := range patterns {
			if matchGlob(p, f) {
				return f
			}
		}
	}
	return ""
}

// sharedFileWith returns a file in files that matches patterns, preferring
// one also present in leaderFiles.
func sharedFileWith(files, leaderFiles, patterns []string) string {
	inLeader := make(map[string]bool, len(leaderFiles))
	for _, f := range leaderFiles {
		inLeader[f] = true
	}
	first := ""
	for _, f := range files {
		if matchingSharedFile([]string{f}, patterns) == "" {
			continue
		}
		if inLeader[f] {
			return f
		}
		if first == "" {
			first = f
		}
	}
	return first
}

// prBefore orders open pull requests oldest first: earliest CreatedAt,
// breaking ties by lower pull request number.
func prBefore(a, b ghx.OpenPR) bool {
	if c := compareCreated(a.CreatedAt, b.CreatedAt); c != 0 {
		return c < 0
	}
	return a.Number < b.Number
}

// waitSerialized reports whether pr must wait behind an older open queue
// pull request in the same repository that also touches a shared file.
// When it waits, the wait is logged once per (PR, leader, path).
func (r *queueRunner) waitSerialized(iss queuedIssue, pr ghx.OpenPR, got prListing) (bool, error) {
	if r.cfg.Serialize == nil || r.env.prFiles == nil {
		return false, nil
	}
	files, err := r.prFiles(iss.Repo, pr.Number, got)
	if err != nil {
		return false, err
	}
	if matchingSharedFile(files, r.cfg.Serialize.Paths) == "" {
		return false, nil
	}
	older := r.olderQueuePRs(iss.Repo, pr, got.prs)
	for _, other := range older {
		otherFiles, err := r.prFiles(iss.Repo, other.Number, got)
		if err != nil {
			return false, err
		}
		if matchingSharedFile(otherFiles, r.cfg.Serialize.Paths) != "" {
			shared := sharedFileWith(files, otherFiles, r.cfg.Serialize.Paths)
			r.logWaitOnce(iss, pr.Number, other.Number, shared)
			return true, nil
		}
	}
	r.clearWaited(iss.Repo + "#" + strconv.Itoa(pr.Number))
	return false, nil
}

// olderQueuePRs returns the open queue pull requests in repo older than pr
// that can still land, ordered oldest first. A pull request that is a draft,
// blocked, or stopped after refused merges is skipped so it does not hold
// younger pull requests forever.
func (r *queueRunner) olderQueuePRs(repo string, pr ghx.OpenPR, prs []ghx.OpenPR) []ghx.OpenPR {
	var older []ghx.OpenPR
	seen := map[int]bool{pr.Number: true}
	for _, iss := range r.startedQueued() {
		if iss.Repo != repo {
			continue
		}
		other, ok, _ := r.queuePR(iss, prs)
		if !ok || seen[other.Number] {
			continue
		}
		seen[other.Number] = true
		if !r.canLeadSerialize(iss, other) {
			continue
		}
		if prBefore(other, pr) {
			older = append(older, other)
		}
	}
	sort.Slice(older, func(i, j int) bool {
		return prBefore(older[i], older[j])
	})
	return older
}

// canLeadSerialize reports whether pr is still eligible to land and thus to
// hold younger pull requests that touch a shared file.
func (r *queueRunner) canLeadSerialize(iss queuedIssue, pr ghx.OpenPR) bool {
	if pr.IsDraft || strings.EqualFold(pr.MergeStateStatus, "DRAFT") {
		return false
	}
	if r.cfg.Fix != nil && r.cfg.Fix.BlockLabel != "" && hasLabelFold(iss.Issue.LabelNames(), r.cfg.Fix.BlockLabel) {
		return false
	}
	if r.state == nil {
		return true
	}
	k := iss.key()
	mkey := landKey(iss.Repo, pr.Number)
	fp := checksFingerprint(pr.Checks)
	r.mu.Lock()
	defer r.mu.Unlock()
	if rec := r.state.Fixes[k]; rec != nil && rec.Blocked {
		return false
	}
	if h := r.state.Merges[mkey]; h != nil && h.Stopped && h.Head == pr.HeadRefOid && h.Checks == fp {
		return false
	}
	return true
}

func (r *queueRunner) logWaitOnce(iss queuedIssue, prNum, leaderNum int, sharedPath string) {
	key := iss.Repo + "#" + strconv.Itoa(prNum)
	val := fmt.Sprintf("#%d:%s", leaderNum, sharedPath)
	r.mu.Lock()
	prev := r.waited[key]
	if prev == val {
		r.mu.Unlock()
		return
	}
	r.waited[key] = val
	r.mu.Unlock()
	if r.dry {
		r.logf("dry-run: %s PR #%d waits for #%d, shared file %s", iss.key(), prNum, leaderNum, sharedPath)
		return
	}
	r.logf("queue: %s PR #%d waits for #%d, shared file %s", iss.key(), prNum, leaderNum, sharedPath)
}

func (r *queueRunner) clearWaited(key string) {
	r.mu.Lock()
	delete(r.waited, key)
	r.mu.Unlock()
}
