package ghx

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeGhRoutes puts a gh on PATH that answers `gh api -i <path>` with the
// response filed under that path, and appends every call's arguments (one line
// per call) to a log. A path with no route answers 404. The returned func
// reads the log. gh's exit code is 0 on 2xx and 1 otherwise, as the real one.
func fakeGhRoutes(t *testing.T, routes map[string]string) func() []string {
	t.Helper()
	dir := t.TempDir()
	q := "'" + dir + "'"
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" >> " + q + "/calls\n" +
		"case \"$3\" in\n"
	i := 0
	for path, resp := range routes {
		i++
		file := filepath.Join(dir, "resp"+strconv.Itoa(i))
		if err := os.WriteFile(file, []byte(resp), 0o600); err != nil {
			t.Fatal(err)
		}
		script += "'" + path + "') /bin/cat '" + file + "'; exit 1 ;;\n"
	}
	script += "*) printf 'HTTP/2.0 404 Not Found\\r\\n\\r\\n{\"message\":\"Not Found\"}'; exit 1 ;;\nesac\n"
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":/bin:/usr/bin")
	return func() []string {
		raw, err := os.ReadFile(filepath.Join(dir, "calls"))
		if err != nil {
			return nil
		}
		return strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	}
}

const (
	page1Path = "repos/acme/widgets/issues?labels=next&state=open&per_page=100"
	page2URL  = "https://api.github.com/repositories/1/issues?labels=next&state=open&per_page=100&page=2"
)

func TestOpenIssuesAsksRESTForOpenIssuesWithTheLabel(t *testing.T) {
	withTempCache(t)
	calls := fakeGhRoutes(t, map[string]string{page1Path: "HTTP/2.0 200 OK\r\nEtag: W/\"a\"\r\n\r\n[]"})
	got, err := (CLI{}).OpenIssues("acme/widgets", "next")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("OpenIssues = %+v, want none", got)
	}
	if c := calls(); len(c) != 1 || c[0] != "api -i "+page1Path {
		t.Errorf("gh calls = %q, want one REST list of open issues with the label", c)
	}
}

// REST names the fields differently from gh's JSON: created_at, html_url and a
// lower-case state. They must land where the ordering and IsOpen read them. A
// pull request in the list is dropped.
func TestOpenIssuesMapsRESTFieldsAndDropsPullRequests(t *testing.T) {
	withTempCache(t)
	body := `[
	  {"number": 7, "title": "Fix the bell", "html_url": "https://github.com/acme/widgets/issues/7",
	   "state": "open", "labels": [{"id": 1, "name": "next"}, {"name": "p1"}], "created_at": "2026-01-02T03:04:05Z"},
	  {"number": 9, "title": "A PR", "html_url": "https://github.com/acme/widgets/pull/9",
	   "state": "open", "labels": [{"name": "next"}], "created_at": "2026-01-03T03:04:05Z",
	   "pull_request": {"url": "https://api.github.com/repos/acme/widgets/pulls/9"}},
	  {"number": 8, "title": "Later", "html_url": "https://github.com/acme/widgets/issues/8",
	   "state": "open", "labels": [{"name": "next"}], "created_at": "2026-02-02T03:04:05Z"}
	]`
	fakeGhRoutes(t, map[string]string{page1Path: "HTTP/2.0 200 OK\r\nEtag: W/\"a\"\r\n\r\n" + body})
	got, err := (CLI{}).OpenIssues("acme/widgets", "next")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Number != 7 || got[1].Number != 8 {
		t.Fatalf("OpenIssues = %+v, want issues 7 and 8 and no pull request", got)
	}
	if got[0].CreatedAt != "2026-01-02T03:04:05Z" || got[0].URL != "https://github.com/acme/widgets/issues/7" {
		t.Errorf("first = %+v", got[0])
	}
	if !got[0].IsOpen() || got[0].State != "OPEN" {
		t.Errorf("state %q is not read as open", got[0].State)
	}
	names := got[0].LabelNames()
	if len(names) != 2 || names[0] != "next" || names[1] != "p1" {
		t.Errorf("labels = %q", names)
	}
}

func TestOpenIssuesFollowsTheNextLink(t *testing.T) {
	withTempCache(t)
	calls := fakeGhRoutes(t, map[string]string{
		page1Path: "HTTP/2.0 200 OK\r\nEtag: W/\"a\"\r\nLink: <" + page2URL + ">; rel=\"next\", <x>; rel=\"last\"\r\n\r\n" +
			`[{"number":1,"state":"open","created_at":"2026-01-01T00:00:00Z"}]`,
		page2URL: "HTTP/2.0 200 OK\r\nEtag: W/\"b\"\r\n\r\n" +
			`[{"number":2,"state":"open","created_at":"2026-01-02T00:00:00Z"}]`,
	})
	got, err := (CLI{}).OpenIssues("acme/widgets", "next")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Number != 1 || got[1].Number != 2 {
		t.Fatalf("OpenIssues = %+v, want both pages", got)
	}
	if c := calls(); len(c) != 2 {
		t.Errorf("gh calls = %q, want two pages", c)
	}
}

// A 304 carries no Link and no body. Page 1 must then lead to page 2 from its
// cached next URL, and each page is sent with its own ETag. Control: the first
// round made no conditional request, so a 304 on the second is the cache's.
func TestOpenIssuesReusesTheCachedListOn304(t *testing.T) {
	withTempCache(t)
	writeCache(page1Path, cacheEntry{ETag: `W/"a"`, Next: page2URL,
		Body: []byte(`[{"number":1,"state":"open","created_at":"2026-01-01T00:00:00Z"}]`)})
	writeCache(page2URL, cacheEntry{ETag: `W/"b"`,
		Body: []byte(`[{"number":2,"state":"open","created_at":"2026-01-02T00:00:00Z"}]`)})
	calls := fakeGhRoutes(t, map[string]string{
		page1Path: "HTTP/2.0 304 Not Modified\r\nEtag: W/\"a\"\r\n\r\n",
		page2URL:  "HTTP/2.0 304 Not Modified\r\nEtag: W/\"b\"\r\n\r\n",
	})
	got, err := (CLI{}).OpenIssues("acme/widgets", "next")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Number != 1 || got[1].Number != 2 {
		t.Fatalf("OpenIssues = %+v, want the cached list", got)
	}
	c := calls()
	if len(c) != 2 || !strings.Contains(c[0], `If-None-Match: W/"a"`) || !strings.Contains(c[1], `If-None-Match: W/"b"`) {
		t.Errorf("gh calls = %q, want each page sent with its own ETag", c)
	}
}

// A 200 on page 1 replaces its cached next URL: a list that shrank to one page
// must not keep walking to a page that no longer exists.
func TestOpenIssuesA200ReplacesTheCachedNextURL(t *testing.T) {
	withTempCache(t)
	writeCache(page1Path, cacheEntry{ETag: `W/"old"`, Next: page2URL, Body: []byte(`[]`)})
	calls := fakeGhRoutes(t, map[string]string{
		page1Path: "HTTP/2.0 200 OK\r\nEtag: W/\"new\"\r\n\r\n" + `[{"number":1,"state":"open","created_at":"2026-01-01T00:00:00Z"}]`,
	})
	got, err := (CLI{}).OpenIssues("acme/widgets", "next")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || len(calls()) != 1 {
		t.Fatalf("got %+v after %q, want one issue from one call", got, calls())
	}
	if e, _ := readCache(page1Path); e.Next != "" || e.ETag != `W/"new"` {
		t.Errorf("cache = %+v, want the new ETag and no next URL", e)
	}
}

func TestOpenIssuesStopsAtTheCap(t *testing.T) {
	withTempCache(t)
	var items []string
	for i := 1; i <= OpenIssueLimit+5; i++ {
		items = append(items, `{"number":`+strconv.Itoa(i)+`,"state":"open","created_at":"2026-01-01T00:00:00Z"}`)
	}
	fakeGhRoutes(t, map[string]string{page1Path: "HTTP/2.0 200 OK\r\nEtag: W/\"a\"\r\n\r\n[" + strings.Join(items, ",") + "]"})
	got, err := (CLI{}).OpenIssues("acme/widgets", "next")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != OpenIssueLimit {
		t.Errorf("len = %d, want the cap %d", len(got), OpenIssueLimit)
	}
}

func TestOpenIssuesRefusesABadRepoBeforeCallingGh(t *testing.T) {
	// PATH has no gh. A refusal that actually called out would fail here
	// instead of reaching a real gh.
	t.Setenv("PATH", t.TempDir())
	_, err := (CLI{}).OpenIssues("not-a-repo", "next")
	if err == nil || !strings.Contains(err.Error(), "not-a-repo") {
		t.Fatalf("OpenIssues = %v, want an error naming the repo", err)
	}
}

func TestOpenIssuesRefusesAnEmptyLabel(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	_, err := (CLI{}).OpenIssues("acme/widgets", " ")
	if err == nil || !strings.Contains(err.Error(), "fix:") {
		t.Fatalf("OpenIssues = %v, want a refusal that says how to fix it", err)
	}
}

func TestOpenIssuesReportsAGhFailure(t *testing.T) {
	withTempCache(t)
	fakeGh(t, "", "no such repository\n", 1)
	_, err := (CLI{}).OpenIssues("acme/widgets", "next")
	if err == nil || !strings.Contains(err.Error(), "no such repository") {
		t.Fatalf("OpenIssues = %v, want gh's reason", err)
	}
}

func TestOpenIssuesRefusesJSONThatIsNotAList(t *testing.T) {
	withTempCache(t)
	fakeGhRoutes(t, map[string]string{page1Path: "HTTP/2.0 200 OK\r\n\r\n" + `{"message": "no"}`})
	_, err := (CLI{}).OpenIssues("acme/widgets", "next")
	if err == nil || !strings.Contains(err.Error(), "parse open issues") {
		t.Fatalf("OpenIssues = %v, want a parse error", err)
	}
}

func TestOpenIssuesRateLimitIsATypedErrorWithTheReset(t *testing.T) {
	for _, tc := range []struct {
		name, resp string
		want       time.Time
	}{
		{"429 with x-ratelimit-reset", "HTTP/2.0 429 Too Many Requests\r\nX-Ratelimit-Reset: 1900000000\r\n\r\n{\"message\":\"slow down\"}", time.Unix(1900000000, 0)},
		{"403 with remaining 0", "HTTP/2.0 403 Forbidden\r\nX-Ratelimit-Remaining: 0\r\nX-Ratelimit-Reset: 1900000001\r\n\r\n{\"message\":\"x\"}", time.Unix(1900000001, 0)},
		{"403 with a message", "HTTP/2.0 403 Forbidden\r\nX-Ratelimit-Reset: 1900000002\r\n\r\n{\"message\":\"API rate limit exceeded\"}", time.Unix(1900000002, 0)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withTempCache(t)
			fakeGhRoutes(t, map[string]string{page1Path: tc.resp})
			_, err := (CLI{}).OpenIssues("acme/widgets", "next")
			var rl *RateLimitError
			if !errors.As(err, &rl) {
				t.Fatalf("err = %v, want a *RateLimitError", err)
			}
			if !rl.Reset.Equal(tc.want) {
				t.Errorf("Reset = %v, want %v", rl.Reset, tc.want)
			}
		})
	}
}

// Control: a 403 that is not a rate limit belongs to that one repository and
// must not be read as a wait for the whole queue.
func TestOpenIssuesAnotherForbiddenIsNotARateLimit(t *testing.T) {
	withTempCache(t)
	fakeGhRoutes(t, map[string]string{page1Path: "HTTP/2.0 403 Forbidden\r\nX-Ratelimit-Remaining: 4000\r\n\r\n{\"message\":\"Resource not accessible\"}"})
	_, err := (CLI{}).OpenIssues("acme/widgets", "next")
	var rl *RateLimitError
	if err == nil || errors.As(err, &rl) {
		t.Fatalf("err = %v, want a plain failure", err)
	}
}

func TestRateLimitedRetryAfterWinsOverTheResetHeader(t *testing.T) {
	now := time.Unix(1000, 0)
	rl := rateLimited(403, map[string]string{"retry-after": "30", "x-ratelimit-reset": "9999"}, "m", now, func() (time.Time, bool) { return time.Time{}, false })
	if rl == nil || !rl.Reset.Equal(now.Add(30*time.Second)) {
		t.Fatalf("rateLimited = %+v, want a reset 30s after now", rl)
	}
}

func TestRateLimitedFallsBackToTheRateLimitEndpoint(t *testing.T) {
	want := time.Unix(5000, 0)
	rl := rateLimited(429, map[string]string{}, "m", time.Unix(1000, 0), func() (time.Time, bool) { return want, true })
	if rl == nil || !rl.Reset.Equal(want) {
		t.Fatalf("rateLimited = %+v, want the fallback's reset", rl)
	}
}
