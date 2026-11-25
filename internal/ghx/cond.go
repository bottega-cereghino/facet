// Conditional reads: asking GitHub for something we already have, and being
// charged nothing when it has not changed.
//
// WHY THIS EXISTS, AND WHY IT IS REST RATHER THAN GRAPHQL. The two APIs have
// SEPARATE budgets, and only one of them can be asked conditionally:
//
//	GraphQL  5000 POINTS/hour   billed on the nodes a query COULD return,
//	                            in full, every time, for ever. No ETags.
//	REST     5000 REQUESTS/hour a 304 Not Modified costs ZERO.
//
// Measured, on this account, minutes apart: `graphql 0/5000` beside
// `core 4928/5000` -- an exhausted GraphQL budget next to an untouched REST
// one. And a conditional repeat: remaining 4921 -> 4921, delta 0.
//
// That matters because an issue read is usually a REPEAT: spawn reads the
// issue, and reads it again before writing its body back, and a re-spawn reads
// it once more. GraphQL has no way to say "unchanged" and bills the full price
// each time; conditional REST bills it once. ViewIssue reads one resource
// through condGet; OpenIssues reads a list through condGetPage, page by page.
//
// AND A CACHE MUST NEVER SERVE STALE DATA. Cheap and wrong is worse than
// paying in full. Nothing here decides for itself that an entry is still
// good: GitHub decides, by answering 304, and a 200 replaces the entry
// outright. There is no expiry to tune and no staleness window to be wrong
// about.

package ghx

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// cacheEntry is what is kept on disk per request path.
type cacheEntry struct {
	ETag string `json:"etag"`
	Body []byte `json:"body"`
	// Next is the URL of the page after this one, from the Link header of the
	// 200 that stored the entry. A 304 carries no Link, so a cached page of a
	// list must remember its own next page. Empty for the last page and for a
	// single resource.
	Next string `json:"next,omitempty"`
}

// condPage is one answer of a conditional read.
type condPage struct {
	Body []byte
	// Next is the URL of the following page of a list, or empty.
	Next string
}

// cacheDir is where conditional entries live. A cache that cannot be created
// is not an error: every read still works, it just costs what it always did.
//
// FACET_CACHE names it explicitly. That exists because os.UserCacheDir is
// PLATFORM-SPECIFIC and does not honour XDG_CACHE_HOME on macOS -- so without
// it a test trying to isolate itself silently writes to the real cache, which
// is how a test suite comes to depend on a developer's machine state. It is
// also the switch for pointing a run at a scratch cache on purpose.
func cacheDir() (string, bool) {
	base := os.Getenv("FACET_CACHE")
	if base == "" {
		var err error
		base, err = os.UserCacheDir()
		if err != nil {
			return "", false
		}
	}
	dir := filepath.Join(base, "facet", "rest")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", false
	}
	return dir, true
}

func cachePath(requestPath string) (string, bool) {
	dir, ok := cacheDir()
	if !ok {
		return "", false
	}
	sum := sha256.Sum256([]byte(requestPath))
	return filepath.Join(dir, hex.EncodeToString(sum[:])+".json"), true
}

func readCache(requestPath string) (cacheEntry, bool) {
	p, ok := cachePath(requestPath)
	if !ok {
		return cacheEntry{}, false
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return cacheEntry{}, false
	}
	var e cacheEntry
	if err := json.Unmarshal(raw, &e); err != nil || e.ETag == "" {
		return cacheEntry{}, false
	}
	return e, true
}

func writeCache(requestPath string, e cacheEntry) {
	p, ok := cachePath(requestPath)
	if !ok {
		return
	}
	raw, err := json.Marshal(e)
	if err != nil {
		return
	}
	// Write beside and rename, so a run interrupted mid-write cannot leave a
	// half-written entry that later reads as a valid one.
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, p)
}

// condGet performs a conditional GET through gh and returns the body, from
// the cache when GitHub answers 304.
//
// `gh api -i` is what makes this expressible: WITHOUT it, gh exits 1 on a 304
// with only "gh: HTTP 304" on stderr, and the response is indistinguishable
// from a failure. With it, gh exits 0 and the status line and headers are
// readable. Measured both ways.
//
// condGet is for one resource. A list goes through condGetPage, which keeps
// the next-page URL beside each cached page, because a 304 carries no Link
// header and a cached first page would otherwise be served as the whole answer.
func condGet(requestPath string) ([]byte, error) {
	page, err := condGetPage(requestPath)
	return page.Body, err
}

// condGetPage is condGet for a page of a list. Each page is cached under its
// own request path with the URL of the next page. A 304 serves both from the
// cache; a 200 replaces both. A rate-limit answer is a *RateLimitError.
func condGetPage(requestPath string) (condPage, error) {
	cached, haveCache := readCache(requestPath)

	args := []string{"api", "-i", requestPath}
	if haveCache {
		args = append(args, "-H", "If-None-Match: "+cached.ETag)
	}
	cmd := exec.Command("gh", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()

	// THE EXIT CODE IS NOT THE ANSWER HERE. gh exits NON-ZERO on 304 -- the
	// very outcome this function exists to produce -- while still printing the
	// full status line and headers. So the response decides, and the exit code
	// is consulted only when the response says nothing.
	//
	// This was measured wrongly first, and the mistake is worth naming: the
	// probe that reported exit 0 was piped through `head`, so it read HEAD's
	// exit code rather than gh's. A verification behind a pipe reports on the
	// pipe.
	status, headers, body := splitResponse(stdout.Bytes())
	if status == 0 {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" && runErr != nil {
			msg = runErr.Error()
		}
		if msg == "" {
			msg = "no HTTP status in the response"
		}
		return condPage{}, fmt.Errorf("gh api -i %s: %s", requestPath, msg)
	}
	switch {
	case status == 304 && haveCache:
		return condPage{Body: cached.Body, Next: cached.Next}, nil
	case status == 304:
		// A 304 with nothing to serve it from. Never observed, and if it ever
		// happens the honest answer is that this read did not answer.
		return condPage{}, fmt.Errorf("%s: 304 Not Modified with no cached copy to serve", requestPath)
	case status < 200 || status > 299:
		msg := failureMessage(body, stderr.String())
		full := fmt.Sprintf("%s: HTTP %d: %s", requestPath, status, msg)
		if rl := rateLimited(status, headers, full, time.Now(), rateLimitResetFromGh); rl != nil {
			return condPage{}, rl
		}
		return condPage{}, errors.New(full)
	}

	next := nextLink(headers["link"])
	if etag := headers["etag"]; etag != "" {
		writeCache(requestPath, cacheEntry{ETag: etag, Body: body, Next: next})
	}
	return condPage{Body: body, Next: next}, nil
}

// RateLimitError says GitHub refused because the budget is spent. Reset is when
// it may be asked again.
type RateLimitError struct {
	Reset   time.Time
	Message string
}

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("%s (rate limit; resets %s)\nfix: wait until then; the queue lists no issue before it", e.Message, e.Reset.Format(time.RFC3339))
}

// rateLimited returns a *RateLimitError when the answer means the budget is
// spent: 429, or 403 with x-ratelimit-remaining 0, a retry-after header or a
// rate-limit message. Any other 403 is an ordinary failure and returns nil.
// The wait ends at retry-after, else at x-ratelimit-reset, else at what
// resetFallback says (`gh api rate_limit`).
func rateLimited(status int, headers map[string]string, msg string, now time.Time, resetFallback func() (time.Time, bool)) *RateLimitError {
	_, retry := headers["retry-after"]
	limited := status == 429 ||
		(status == 403 && (headers["x-ratelimit-remaining"] == "0" || retry ||
			strings.Contains(strings.ToLower(msg), "rate limit")))
	if !limited {
		return nil
	}
	e := &RateLimitError{Message: msg}
	if secs, err := strconv.Atoi(headers["retry-after"]); err == nil && secs >= 0 {
		e.Reset = now.Add(time.Duration(secs) * time.Second)
		return e
	}
	if unix, err := strconv.ParseInt(headers["x-ratelimit-reset"], 10, 64); err == nil {
		e.Reset = time.Unix(unix, 0)
		return e
	}
	if t, ok := resetFallback(); ok {
		e.Reset = t
		return e
	}
	e.Reset = now.Add(time.Minute)
	return e
}

// rateLimitResetFromGh reads when the REST budget resets. `gh api rate_limit`
// does not count against it.
func rateLimitResetFromGh() (time.Time, bool) {
	out, err := run("api", "rate_limit", "--jq", ".resources.core.reset")
	if err != nil {
		return time.Time{}, false
	}
	unix, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	return time.Unix(unix, 0), true
}

// nextLink returns the rel="next" URL of a Link header, or "".
func nextLink(link string) string {
	for _, part := range strings.Split(link, ",") {
		url, rel, ok := strings.Cut(part, ";")
		if !ok || !strings.Contains(rel, `rel="next"`) {
			continue
		}
		return strings.Trim(strings.TrimSpace(url), "<>")
	}
	return ""
}

// failureMessage says why GitHub refused, in GitHub's words: the `message` of
// its JSON error body ("API rate limit exceeded for …", "Bad credentials",
// "Not Found"), else what gh printed on stderr. The status alone reads the
// same for a rate limit and a missing permission, and those need different
// fixes.
func failureMessage(body []byte, stderr string) string {
	var e struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &e) == nil && strings.TrimSpace(e.Message) != "" {
		return strings.TrimSpace(e.Message)
	}
	if msg := strings.TrimSpace(stderr); msg != "" {
		return msg
	}
	return "GitHub gave no reason"
}

// splitResponse pulls the status code, the headers and the body out of what
// `gh api -i` prints. gh may print more than one header block when it follows
// a redirect, so the LAST status line is the one that answered.
func splitResponse(raw []byte) (status int, headers map[string]string, body []byte) {
	headers = map[string]string{}
	rest := raw
	for {
		sep := []byte("\r\n\r\n")
		i := bytes.Index(rest, sep)
		if i < 0 {
			sep = []byte("\n\n")
			i = bytes.Index(rest, sep)
		}
		if i < 0 {
			break
		}
		block := rest[:i]
		remainder := rest[i+len(sep):]
		if !bytes.HasPrefix(block, []byte("HTTP/")) {
			break
		}
		status, headers = parseHeaderBlock(block)
		rest = remainder
		if !bytes.HasPrefix(rest, []byte("HTTP/")) {
			break
		}
	}
	return status, headers, rest
}

func parseHeaderBlock(block []byte) (int, map[string]string) {
	headers := map[string]string{}
	status := 0
	for i, line := range strings.Split(strings.ReplaceAll(string(block), "\r\n", "\n"), "\n") {
		if i == 0 {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				status, _ = strconv.Atoi(fields[1])
			}
			continue
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		headers[strings.ToLower(strings.TrimSpace(name))] = strings.TrimSpace(value)
	}
	return status, headers
}
