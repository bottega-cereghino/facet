package routing

// Fuzz tests for the parsers that read issue bodies and titles: text anyone who
// can open an issue writes. `make test` runs only the seeds below and the
// inputs saved under testdata/fuzz, so it stays fast; run one for longer by
// hand, for example
//
//	go test ./internal/routing -run '^$' -fuzz '^FuzzUpsertScope$' -fuzztime 60s
//
// A failure the fuzzer finds is written to testdata/fuzz/<FuzzName>/. Commit it
// with the fix: it then runs on every `make test`.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/bottega-cereghino/facet/internal/ghx"
)

// seedBodies are the issue bodies every body fuzzer starts from: the fixture
// issues, and a few shapes the parsers have special cases for.
func seedBodies(f *testing.F) []string {
	f.Helper()
	bodies := []string{
		"",
		"### Repos in scope\n\n- [x] gateway\n- [ ] infra-core\n",
		"## Repos involved\n\nacme/Gateway, https://github.com/acme/infra-core.git\n",
		"### Repos in scope\n\n_No response_\n\n### Complexity\n\nc2 roughly\n",
		"### Blocked by / waiting on\n\n- #12\n- acme/gateway#5\n- gateway#7 and widgetapi #9\n",
		"```\n## Repos in scope\n\nlegacy\n```\n\n## Repos in scope\n\nplatform\n",
		"### Complexity\r\n\r\ncomplexity/3\r\n",
		"x\n##\n",
		"## Repos involved\n\ngateway\n\n## Repos in scope\n\ninfra\n",
		"é\n```\nü\n```\n## Repos in scope\n\ninfra\n",
	}
	paths, err := filepath.Glob(filepath.Join("..", "..", "testdata", "issues", "*.json"))
	if err != nil {
		f.Fatal(err)
	}
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			f.Fatal(err)
		}
		var iss ghx.Issue
		if err := json.Unmarshal(b, &iss); err != nil {
			f.Fatal(err)
		}
		bodies = append(bodies, iss.Body)
	}
	return bodies
}

func loadForFuzz(f *testing.F) *Routing {
	f.Helper()
	r, err := Load(filepath.Join("..", "..", "testdata", "routing.json"))
	if err != nil {
		f.Fatal(err)
	}
	return r
}

// UpsertScope writes spawn's decision back into the issue. What it writes must
// read back as exactly that decision, and writing it again must change nothing,
// or every spawn churns the issue's edit history.
func FuzzUpsertScope(f *testing.F) {
	r := loadForFuzz(f)
	keys := sortedKeys(r.Repos)
	for i, b := range seedBodies(f) {
		f.Add(b, uint8(i), uint8(i*3+1))
	}
	f.Fuzz(func(t *testing.T, body string, a, b uint8) {
		repos := []string{keys[int(a)%len(keys)]}
		if other := keys[int(b)%len(keys)]; other != repos[0] {
			repos = append(repos, other)
		}

		out, _ := UpsertScope(body, repos)
		again, changed := UpsertScope(out, repos)
		if changed || again != out {
			t.Fatalf("a second UpsertScope changed the body\nbody: %q\nonce: %q\ntwice: %q", body, out, again)
		}
		if got := r.scopeField(stripFences(out)); !reflect.DeepEqual(got, repos) {
			t.Fatalf("the written scope reads back as %v, want %v\nbody: %q\nout: %q", got, repos, body, out)
		}
	})
}

// Infer reads the whole body. Whatever the body says, the home repo comes first.
func FuzzInfer(f *testing.F) {
	r := loadForFuzz(f)
	for _, b := range seedBodies(f) {
		f.Add(b, "area/monitoring")
	}
	f.Fuzz(func(t *testing.T, body, label string) {
		iss := &ghx.Issue{Body: body, Labels: []ghx.Label{{Name: label}}}
		sel, _ := r.Infer("acme/platform", iss)
		if keys := Keys(sel); len(keys) == 0 || keys[0] != "platform" {
			t.Fatalf("Infer = %v, want the home repo platform first\nbody: %q", keys, body)
		}
		_ = r.UnknownScope(iss)
	})
}

// stripFences blanks code blocks so the other scans keep their offsets: the
// result must be exactly as long as the input, line for line.
func FuzzStripFences(f *testing.F) {
	for _, b := range seedBodies(f) {
		f.Add(b)
	}
	f.Fuzz(func(t *testing.T, body string) {
		out := stripFences(body)
		if len(out) != len(body) {
			t.Fatalf("stripFences changed the length from %d to %d: %q", len(body), len(out), body)
		}
		if strings.Count(out, "\n") != strings.Count(body, "\n") {
			t.Fatalf("stripFences changed the line count: %q", body)
		}
	})
}

var digits = regexp.MustCompile(`^\d+$`)

// ParseComplexity becomes a label: a number, or an error that says why not.
func FuzzParseComplexity(f *testing.F) {
	for _, b := range seedBodies(f) {
		f.Add(b)
	}
	f.Fuzz(func(t *testing.T, body string) {
		n, found, err := ParseComplexity(body)
		switch {
		case !found && (n != "" || err != nil):
			t.Fatalf("no section, yet n=%q err=%v", n, err)
		case found && err == nil && !digits.MatchString(n):
			t.Fatalf("n = %q, want digits\nbody: %q", n, body)
		}
	})
}

// Each blocked-by reference becomes a GitHub API call: it must name a real
// issue number, once.
func FuzzParseBlockedBy(f *testing.F) {
	r := loadForFuzz(f)
	for _, b := range seedBodies(f) {
		f.Add(b)
	}
	f.Fuzz(func(t *testing.T, body string) {
		for _, rt := range []*Routing{r, nil} {
			seen := map[BlockedByRef]bool{}
			for _, ref := range rt.ParseBlockedBy(body) {
				if ref.Number < 1 {
					t.Fatalf("ref %+v has no issue number\nbody: %q", ref, body)
				}
				if ref.OwnerRepo != "" && strings.Count(ref.OwnerRepo, "/") != 1 {
					t.Fatalf("ref %+v is not owner/repo\nbody: %q", ref, body)
				}
				if seen[ref] {
					t.Fatalf("ref %+v twice\nbody: %q", ref, body)
				}
				seen[ref] = true
			}
		}
	})
}

// SearchTerms goes into a GitHub search, where a colon starts a qualifier.
func FuzzSearchTerms(f *testing.F) {
	for _, s := range []string{
		"gateway-admin: the login page loops",
		"fix: a b c",
		"",
		"retry: backoff:exponential for the uploader",
		"`facet ls` prints (nothing) when the root is empty",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, title string) {
		got := SearchTerms(title)
		if strings.Contains(got, ":") {
			t.Fatalf("SearchTerms(%q) = %q, which GitHub reads as a qualifier", title, got)
		}
		if strings.IndexFunc(got, func(r rune) bool { return r == '\n' || r == '\r' }) >= 0 {
			t.Fatalf("SearchTerms(%q) = %q, more than one line", title, got)
		}
	})
}
