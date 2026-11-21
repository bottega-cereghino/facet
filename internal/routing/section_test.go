package routing

import (
	"reflect"
	"testing"

	"github.com/bottega-cereghino/facet/internal/ghx"
)

// UpsertScope records the repo set a human confirmed at spawn time, so the next
// spawn does not have to guess again. It rewrites one section of someone's issue
// body, which is unforgiving: the neighbouring sections must come back byte for
// byte, and running it twice must change nothing the second time.
func TestUpsertScope(t *testing.T) {
	tests := []struct {
		name  string
		body  string
		repos []string
		want  string
		// changed reports whether the body should differ from the input.
		changed bool
	}{
		{
			name:    "replaces an existing section, leaves its neighbours alone",
			body:    "### Summary\n\nx\n\n### Repos in scope\n\nplatform\n\n### Acceptance\n\ny\n",
			repos:   []string{"platform", "gateway"},
			want:    "### Summary\n\nx\n\n### Repos in scope\n\nplatform, gateway\n\n### Acceptance\n\ny\n",
			changed: true,
		},
		{
			name:    "idempotent: the same set twice is a no-op",
			body:    "### Repos in scope\n\nplatform, gateway\n",
			repos:   []string{"platform", "gateway"},
			want:    "### Repos in scope\n\nplatform, gateway\n",
			changed: false,
		},
		{
			name:    "a heading quoted in a code block is not the section",
			body:    "```\n### Repos in scope\n\nlegacy\n```\n",
			repos:   []string{"platform"},
			want:    "```\n### Repos in scope\n\nlegacy\n```\n\n## Repos in scope\n\nplatform\n",
			changed: true,
		},
		{
			name:    "a bare ## line is not a heading, and does not swallow the next one",
			body:    "x\n##\n\n## Repos in scope\n\nplatform\n",
			repos:   []string{"platform"},
			want:    "x\n##\n\n## Repos in scope\n\nplatform\n",
			changed: false,
		},
		{
			name:    "appends when the section is absent",
			body:    "## Problem\n\nThe backup has never been restored.\n",
			repos:   []string{"platform", "infra"},
			want:    "## Problem\n\nThe backup has never been restored.\n\n## Repos in scope\n\nplatform, infra\n",
			changed: true,
		},
		{
			name:    "a form's checkbox rendering is replaced by the confirmed set",
			body:    "### Repos in scope\n\n- [x] platform\n- [ ] infra\n\n### Blocked by\n\n_No response_\n",
			repos:   []string{"platform"},
			want:    "### Repos in scope\n\nplatform\n\n### Blocked by\n\n_No response_\n",
			changed: true,
		},
		{
			name:    "an empty repo set never rewrites the body",
			body:    "### Repos in scope\n\nplatform\n",
			repos:   nil,
			want:    "### Repos in scope\n\nplatform\n",
			changed: false,
		},
		{
			// The heading level is whatever the body already uses: forms emit ###,
			// hand-written issues use ##. Do not "normalise" someone's markdown.
			name:    "an existing heading keeps its level",
			body:    "## Repos in scope\n\nplatform\n",
			repos:   []string{"platform", "infra"},
			want:    "## Repos in scope\n\nplatform, infra\n",
			changed: true,
		},
		{
			name:    "an empty body gets a section",
			body:    "",
			repos:   []string{"platform"},
			want:    "## Repos in scope\n\nplatform\n",
			changed: true,
		},
		{
			// facet#69's audit finding (round 1, L1): a hand-written "Repos
			// involved" section is just as authoritative to READ as "Repos in
			// scope" -- but rewriting its CONTENT must not silently rename the
			// author's own heading text along with it. Only a brand new
			// section (the "appends when absent" case above) gets the
			// canonical name.
			name:    "an existing Repos involved heading keeps its own name on a content rewrite",
			body:    "### Repos involved\n\nbilling\n",
			repos:   []string{"billing", "facet"},
			want:    "### Repos involved\n\nbilling, facet\n",
			changed: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, changed := UpsertScope(tt.body, tt.repos)
			if got != tt.want {
				t.Errorf("body:\n%q\nwant:\n%q", got, tt.want)
			}
			if changed != tt.changed {
				t.Errorf("changed = %v, want %v", changed, tt.changed)
			}
			// Running it again must be a no-op, whatever the first pass did.
			again, changedAgain := UpsertScope(got, tt.repos)
			if changedAgain {
				t.Errorf("second pass reported a change")
			}
			if again != got {
				t.Errorf("second pass rewrote the body:\n%q", again)
			}
		})
	}
}

// The whole point is that a rewritten body still routes to the same repos.
func TestUpsertScopeRoundTrips(t *testing.T) {
	r := load(t)
	body := "### Summary\n\nx\n\n### Repos in scope\n\n- [x] gateway\n- [ ] infra-core\n\n### Acceptance\n\ny\n"
	out, changed := UpsertScope(body, []string{"platform", "gateway"})
	if !changed {
		t.Fatal("expected a rewrite")
	}
	got := r.scopeField(out)
	if len(got) != 2 || got[0] != "platform" || got[1] != "gateway" {
		t.Fatalf("rewritten body parses to %v", got)
	}
}

// A hand-written issue often names its repos as owner/repo, or by URL. Those
// resolve through ownerRepoToKey like any other spelling, and win over labels.
func TestScopeFieldResolvesOwnerRepo(t *testing.T) {
	r := load(t)
	iss := &ghx.Issue{
		Body:   "### Repos involved\n\nacme/Gateway, https://github.com/acme/infra-core.git\n",
		Labels: []ghx.Label{{Name: "area/monitoring"}},
	}
	sel, _ := r.Infer("acme/platform", iss)
	if got := Keys(sel); !reflect.DeepEqual(got, []string{"platform", "gateway", "infra"}) {
		t.Errorf("Infer = %v, want platform (home), gateway and infra from the field", got)
	}
	if u := r.UnknownScope(iss); len(u) != 0 {
		t.Errorf("UnknownScope = %v, want none", u)
	}
}

// An owner/repo routing has no key for is dropped from the scope; when nothing
// in the field resolves, Infer falls back to labels, and UnknownScope names
// what was dropped so the fallback is not mistaken for the author's answer.
// Bare prose is never reported.
func TestUnknownScopeNamesWhatTheFieldCouldNotResolve(t *testing.T) {
	r := load(t)
	iss := &ghx.Issue{
		Body:   "### Repos in scope\n\nOldOwner/platform-ish, probably\n\n_No response_\n",
		Labels: []ghx.Label{{Name: "area/terraform"}},
	}
	sel, _ := r.Infer("acme/platform", iss)
	if got := Keys(sel); !reflect.DeepEqual(got, []string{"platform", "infra"}) {
		t.Errorf("Infer = %v, want the label fallback: platform, infra", got)
	}
	if got := r.UnknownScope(iss); !reflect.DeepEqual(got, []string{"oldowner/platform-ish"}) {
		t.Errorf("UnknownScope = %v, want [oldowner/platform-ish]", got)
	}
}

// The doc comment's promise, held for every heading that can follow the
// section: what comes before it and what comes after it are returned byte for
// byte. The section used to end only at a `##`-`######` heading, so an H1
// after it was taken for scope content and deleted with it.
func TestUpsertScopeKeepsNeighboursByteForByte(t *testing.T) {
	before := "intro\n\n### Summary\n\nx\n\n"
	for _, after := range []string{
		"# Notes\n\nimportant prose",
		"# Notes\n\nimportant prose\n",
		"#\n\nan empty H1 is a heading too\n",
		"## Acceptance\n\ny\n",
		"###### Deep\n\nz",
		"# Notes\r\n\r\nprose from the web editor\r\n",
	} {
		body := before + "## Repos in scope\n\nlegacy\n\n" + after
		want := before + "## Repos in scope\n\nplatform\n\n" + after
		got, changed := UpsertScope(body, []string{"platform"})
		if got != want || !changed {
			t.Errorf("UpsertScope(%q)\n= %q, %v\nwant %q, true", body, got, changed, want)
		}
	}
}

// What the next spawn reads is what this one wrote: prose under the H1 that
// follows is not part of the scope field.
func TestScopeFieldEndsAtAnH1(t *testing.T) {
	r := load(t)
	body := "## Repos in scope\n\nplatform\n\n# Notes\n\ngateway is mentioned here only in passing\n"
	if got := r.scopeField(body); !reflect.DeepEqual(got, []string{"platform"}) {
		t.Errorf("scopeField = %v, want [platform]", got)
	}
}
