package ghx

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withTempCache points the conditional cache somewhere disposable, so a test
// never reads or writes the real one.
func withTempCache(t *testing.T) {
	t.Helper()
	t.Setenv("FACET_CACHE", t.TempDir())
	if _, ok := cacheDir(); !ok {
		t.Skip("no usable cache directory on this platform")
	}
}

func TestSplitResponseReadsTheStatusHeadersAndBody(t *testing.T) {
	raw := "HTTP/2.0 200 OK\r\n" +
		"Etag: W/\"abc123\"\r\n" +
		"Link: <https://api.github.com/x?page=2>; rel=\"next\"\r\n" +
		"\r\n" +
		`[{"number":1}]`

	status, headers, body := splitResponse([]byte(raw))
	if status != 200 {
		t.Errorf("status = %d, want 200", status)
	}
	if headers["etag"] != `W/"abc123"` {
		t.Errorf("etag = %q", headers["etag"])
	}
	if string(body) != `[{"number":1}]` {
		t.Errorf("body = %q", body)
	}
	if !strings.Contains(headers["link"], `rel="next"`) {
		t.Errorf("link = %q", headers["link"])
	}
}

// A 304 has NO body. Reading it as an empty answer -- rather than as "the
// cached copy still stands" -- would turn every unchanged issue into an empty
// one, which is the worst possible shape for this cache to fail in.
func TestSplitResponseHandlesA304WithNoBody(t *testing.T) {
	status, headers, body := splitResponse([]byte("HTTP/2.0 304 Not Modified\r\nEtag: W/\"abc\"\r\n\r\n"))
	if status != 304 {
		t.Fatalf("status = %d, want 304", status)
	}
	if headers["etag"] != `W/"abc"` {
		t.Errorf("etag = %q", headers["etag"])
	}
	if len(body) != 0 {
		t.Errorf("body = %q, want empty", body)
	}
}

// gh prints one header block per hop when it follows a redirect. The LAST one
// answered; reading the first would take a 301's headers as the response.
func TestSplitResponseTakesTheLastHopNotTheFirst(t *testing.T) {
	raw := "HTTP/2.0 301 Moved Permanently\r\nLocation: /elsewhere\r\n\r\n" +
		"HTTP/2.0 200 OK\r\nEtag: W/\"final\"\r\n\r\n" +
		`{"ok":true}`
	status, headers, body := splitResponse([]byte(raw))
	if status != 200 || headers["etag"] != `W/"final"` {
		t.Fatalf("status=%d etag=%q, want the hop that answered", status, headers["etag"])
	}
	if string(body) != `{"ok":true}` {
		t.Errorf("body = %q", body)
	}
}

func TestTheCacheRoundTripsAnEntry(t *testing.T) {
	withTempCache(t)
	const path = "repos/acme/lab/issues/1"

	if _, ok := readCache(path); ok {
		t.Fatal("a fresh cache already had an entry")
	}
	writeCache(path, cacheEntry{ETag: `W/"one"`, Body: []byte(`[{"number":2}]`)})

	got, ok := readCache(path)
	if !ok || got.ETag != `W/"one"` || string(got.Body) != `[{"number":2}]` {
		t.Fatalf("round trip = %+v ok=%v", got, ok)
	}
}

// Two different requests must never share an entry. Keying on anything that
// collides would serve one issue as another.
func TestDifferentRequestsGetDifferentEntries(t *testing.T) {
	withTempCache(t)
	writeCache("repos/acme/lab/issues/1", cacheEntry{ETag: `W/"a"`, Body: []byte("1")})
	writeCache("repos/acme/lab/issues/2", cacheEntry{ETag: `W/"b"`, Body: []byte("2")})

	one, _ := readCache("repos/acme/lab/issues/1")
	two, _ := readCache("repos/acme/lab/issues/2")
	if one.ETag == two.ETag || string(one.Body) == string(two.Body) {
		t.Fatal("two requests collided onto one cache entry")
	}
}

// An entry with no ETag is unusable: without one there is nothing to send as
// If-None-Match, so serving its body would be serving data nothing revalidated.
// THAT is the stale read this cache must never perform.
func TestAnEntryWithNoETagIsNotUsed(t *testing.T) {
	withTempCache(t)
	const path = "repos/acme/lab/issues/9"
	p, ok := cachePath(path)
	if !ok {
		t.Skip("no cache path")
	}
	if err := os.WriteFile(p, []byte(`{"body":"WQ=="}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := readCache(path); ok {
		t.Fatal("an entry with no ETag was offered for use -- nothing could revalidate it")
	}
}

func TestACorruptEntryIsIgnoredRatherThanFatal(t *testing.T) {
	withTempCache(t)
	const path = "repos/acme/lab/issues/8"
	p, _ := cachePath(path)
	if err := os.WriteFile(p, []byte("not json at all"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := readCache(path); ok {
		t.Fatal("a corrupt entry was treated as usable")
	}
}

// A write must not be able to leave a half-written entry that later reads as a
// valid one -- so it lands by rename, and nothing is left behind.
func TestAWriteLeavesNoTemporaryFileBehind(t *testing.T) {
	withTempCache(t)
	writeCache("repos/acme/lab/issues/7", cacheEntry{ETag: `W/"x"`, Body: []byte("[]")})
	dir, _ := cacheDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("a temporary file survived the write: %s", filepath.Join(dir, e.Name()))
		}
	}
}

// !! gh EXITS NON-ZERO ON 304 -- the very outcome this cache exists to
// produce. So the RESPONSE decides, and the exit code is consulted only when
// there is no response at all.
//
// The first probe of this reported exit 0 and was wrong, because it was piped
// through `head` and read head's exit code rather than gh's. This is that
// probe done properly, kept so the next reader need not repeat it.
func TestA304IsAnAnswerEvenThoughGhCallsItAFailure(t *testing.T) {
	status, headers, body := splitResponse([]byte(
		"HTTP/2.0 304 Not Modified\r\nEtag: W/\"unchanged\"\r\nX-Ratelimit-Remaining: 4921\r\n\r\n"))
	if status != 304 {
		t.Fatalf("status = %d -- a 304 must be readable from the response, never inferred from the exit code", status)
	}
	if len(body) != 0 {
		t.Errorf("body = %q, want empty", body)
	}
	if headers["etag"] == "" {
		t.Error("the ETag was lost, so the entry could never be refreshed")
	}
}

// A 200 is cached under its ETag, or nothing is ever cheap.
func TestCondGetCachesA200(t *testing.T) {
	withTempCache(t)
	const path = "repos/acme/lab/issues/9"
	fakeGh(t, "HTTP/2.0 200 OK\r\nEtag: W/\"whole\"\r\n\r\n{\"number\":9}", "", 0)
	body, err := condGet(path)
	if err != nil || string(body) != `{"number":9}` {
		t.Fatalf("condGet = %q, %v", body, err)
	}
	got, ok := readCache(path)
	if !ok || got.ETag != `W/"whole"` {
		t.Fatalf("a 200 was not cached: %+v ok=%v", got, ok)
	}
}

// A 304 serves the cached body, and the request carried its ETag. gh exits
// non-zero on it, which must not matter.
func TestCondGetServesTheCacheOnA304(t *testing.T) {
	withTempCache(t)
	const path = "repos/acme/lab/issues/9"
	writeCache(path, cacheEntry{ETag: `W/"held"`, Body: []byte(`{"number":9}`)})
	args := fakeGh(t, "HTTP/2.0 304 Not Modified\r\nEtag: W/\"held\"\r\n\r\n", "gh: HTTP 304\n", 1)
	body, err := condGet(path)
	if err != nil || string(body) != `{"number":9}` {
		t.Fatalf("condGet = %q, %v", body, err)
	}
	if a := strings.Join(args(), " "); !strings.Contains(a, `If-None-Match: W/"held"`) {
		t.Errorf("the request did not carry the ETag: %s", a)
	}
}

// A refusal keeps its status AND says why, in GitHub's words. "HTTP 403" alone
// reads the same for a rate limit and a missing permission, and the fixes
// differ.
func TestCondGetSaysWhyGitHubRefused(t *testing.T) {
	for _, tc := range []struct {
		name, stdout, stderr, want string
	}{
		{
			"rate limit",
			"HTTP/2.0 403 Forbidden\r\nX-Ratelimit-Remaining: 0\r\n\r\n" +
				`{"message":"API rate limit exceeded for user ID 1.","documentation_url":"https://docs.github.com/rest"}`,
			"gh: API rate limit exceeded for user ID 1. (HTTP 403)\n",
			"HTTP 403: API rate limit exceeded for user ID 1.",
		},
		{
			"bad credentials",
			"HTTP/2.0 401 Unauthorized\r\n\r\n" + `{"message":"Bad credentials"}`,
			"gh: Bad credentials (HTTP 401)\n",
			"HTTP 401: Bad credentials",
		},
		{
			"not found",
			"HTTP/2.0 404 Not Found\r\n\r\n" + `{"message":"Not Found"}`,
			"gh: Not Found (HTTP 404)\n",
			"HTTP 404: Not Found",
		},
		{
			"no JSON body: gh's stderr instead",
			"HTTP/2.0 502 Bad Gateway\r\n\r\n<html>bad gateway</html>",
			"gh: HTTP 502\n",
			"HTTP 502: gh: HTTP 502",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withTempCache(t)
			fakeGh(t, tc.stdout, tc.stderr, 1)
			_, err := condGet("repos/acme/lab/issues/9")
			if err == nil {
				t.Fatal("a refusal read as an answer")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to contain %q", err, tc.want)
			}
		})
	}
}
