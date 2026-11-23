package main

import (
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/bottega-cereghino/facet/internal/ghx"
)

// baselineFile matches a path whose base name is a baseline, a ratchet or a
// limit: size-limit.json matches, unlimited.go does not.
var baselineFile = regexp.MustCompile(`(?i)(?:^|[^a-z0-9])(?:baselines?|ratchets?|limits?)(?:[^a-z0-9]|$)`)

// suppression reports the first forbidden pattern a fix run added, comparing
// the pull request's head with the head recorded when the fix started. A
// clean diff, an empty recorded head, or a fix block that turns the check
// off is not a suppression. An error means the diff could not be read.
func (r *queueRunner) suppression(iss queuedIssue, pr ghx.OpenPR) (pattern, file string, bad bool, err error) {
	if r.cfg.Fix == nil || !r.cfg.Fix.checksForbid() {
		return "", "", false, nil
	}
	base := r.fixBase(iss.key())
	if base == "" || pr.HeadRefOid == "" || base == pr.HeadRefOid {
		return "", "", false, nil
	}
	if r.env.compareFiles == nil {
		return "", "", false, fmt.Errorf("cannot read the diff %s pushed from %s to %s", iss.key(), base, pr.HeadRefOid)
	}
	files, err := r.env.compareFiles(iss.Repo, base, pr.HeadRefOid)
	if err != nil {
		return "", "", false, fmt.Errorf("read the diff %s pushed from %s to %s: %w", iss.key(), base, pr.HeadRefOid, err)
	}
	pattern, file, bad = forbidHit(files, r.cfg.Fix.forbidPatterns())
	return pattern, file, bad, nil
}

func (r *queueRunner) fixBase(key string) string {
	if r.state == nil {
		return ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	rec := r.state.Fixes[key]
	if rec == nil {
		return ""
	}
	return rec.Head
}

// noteOnce runs f the first time key is seen with val. A later call with
// the same val does not run f, so a refusal is not logged on every tick.
func (r *queueRunner) noteOnce(key, val string, f func()) {
	r.mu.Lock()
	if r.noted[key] == val {
		r.mu.Unlock()
		return
	}
	r.noted[key] = val
	r.mu.Unlock()
	f()
}

// forbidHit is the first added line that matches a pattern, or a higher
// number in a file named like a baseline, ratchet or limit.
func forbidHit(files []ghx.DiffFile, patterns []*regexp.Regexp) (pattern, file string, bad bool) {
	for _, f := range files {
		for _, line := range addedLines(f.Patch) {
			if p := lineHit(line, patterns); p != "" {
				return p, f.Name, true
			}
		}
		if baselineFile.MatchString(path.Base(f.Name)) && higherNumber(f.Patch) {
			return "higher number", f.Name, true
		}
	}
	return "", "", false
}

func lineHit(line string, patterns []*regexp.Regexp) string {
	for _, p := range patterns {
		if p.MatchString(line) {
			return p.String()
		}
	}
	return ""
}

// addedLines is the text of lines the patch adds. A diff header (+++ ) is
// not one of them, and the leading + is not part of the text.
func addedLines(patch string) []string {
	var out []string
	for _, line := range strings.Split(patch, "\n") {
		if !strings.HasPrefix(line, "+") || strings.HasPrefix(line, "+++") {
			continue
		}
		out = append(out, line[1:])
	}
	return out
}

// higherNumber reports whether an added line carries a higher integer than
// the removed line with the same text around the numbers. A new number with
// no removed partner is not a raise.
func higherNumber(patch string) bool {
	removed := map[string][][]string{}
	type added struct {
		sk   string
		nums []string
	}
	var adds []added
	for _, line := range strings.Split(patch, "\n") {
		var body string
		switch {
		case strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++"):
			body = line[1:]
		case strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "---"):
			body = line[1:]
		default:
			continue
		}
		sk, nums := numberSkeleton(body)
		if len(nums) == 0 {
			continue
		}
		if strings.HasPrefix(line, "-") {
			removed[sk] = append(removed[sk], nums)
			continue
		}
		adds = append(adds, added{sk: sk, nums: nums})
	}
	for _, a := range adds {
		bucket := removed[a.sk]
		if len(bucket) == 0 {
			continue
		}
		prev := bucket[0]
		removed[a.sk] = bucket[1:]
		n := len(a.nums)
		if len(prev) < n {
			n = len(prev)
		}
		for i := 0; i < n; i++ {
			if digitLess(prev[i], a.nums[i]) {
				return true
			}
		}
	}
	return false
}

// numberSkeleton replaces each integer with # and returns those integers
// as decimal strings, so "max: 10" and "max: 11" share a skeleton.
func numberSkeleton(line string) (string, []string) {
	var nums []string
	var b strings.Builder
	i := 0
	for i < len(line) {
		if line[i] < '0' || line[i] > '9' {
			b.WriteByte(line[i])
			i++
			continue
		}
		j := i
		for j < len(line) && line[j] >= '0' && line[j] <= '9' {
			j++
		}
		nums = append(nums, line[i:j])
		b.WriteByte('#')
		i = j
	}
	return b.String(), nums
}

// digitLess reports whether a is numerically less than b. The strings are
// decimal integers without a sign. A value that does not fit in an int64
// is compared by length and digits, so a raised limit is still seen.
func digitLess(a, b string) bool {
	ai, aerr := strconv.ParseInt(a, 10, 64)
	bi, berr := strconv.ParseInt(b, 10, 64)
	if aerr == nil && berr == nil {
		return ai < bi
	}
	a = strings.TrimLeft(a, "0")
	b = strings.TrimLeft(b, "0")
	if a == "" {
		a = "0"
	}
	if b == "" {
		b = "0"
	}
	if len(a) != len(b) {
		return len(a) < len(b)
	}
	return a < b
}
