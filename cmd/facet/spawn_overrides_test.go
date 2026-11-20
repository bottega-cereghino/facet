package main

import (
	"strings"
	"testing"

	"github.com/bottega-cereghino/facet/internal/routing"
)

// overrideRouting is the shape a calling script actually meets: keys that are the
// repositories' short names, and ssh URLs built from those same names -- so a
// bare key and `key=url` name the same repo, which is what makes accepting both
// grammars honest rather than lenient.
func overrideRouting() *routing.Routing {
	return &routing.Routing{Repos: map[string]routing.Repo{
		"facet": {Dir: "facet", URL: "git@github.com:RiccardoCereghino/facet.git"},
		"api":   {Dir: "api", URL: "git@github.com:acme/api.git"},
		"infra": {Dir: "infra", URL: "git@github.com:acme/infra.git"},
		"web":   {Dir: "web", URL: "git@github.com:acme/web.git"},
		"cli":   {Dir: "cli", URL: "git@github.com:acme/cli.git"},
	}, Aliases: map[string]string{
		"backend":  "api",
		"plumbing": "infra",
		// A dangling alias: routing files carry them, and the flag must say
		// which hop is missing rather than blaming the operator's spelling.
		"quarry": "cava",
	}}
}

func keysOf(sel []routing.Selection) []string {
	out := make([]string, 0, len(sel))
	for _, s := range sel {
		out = append(out, s.Key)
	}
	return out
}

// TestApplyOverridesAcceptsBothGrammars pins what a recognised value does.
//
// The `key=url` rows are the ones facet#160 was filed about: api-home's
// lib/identity.sh passes one `--clone <name>=git@github.com:owner/<name>.git`
// per repo the seat's scope names, and every one of them was discarded, so a
// two-repo spawn produced a workspace holding the home repo alone.
func TestApplyOverridesAcceptsBothGrammars(t *testing.T) {
	route := overrideRouting()
	inferred := []routing.Selection{{Key: "facet", Reasons: []string{"home"}, Home: true}}

	tests := []struct {
		name string
		o    spawnOpts
		want []string
	}{
		{
			name: "--clone with bare keys",
			o:    spawnOpts{Clones: []string{"api", "infra"}},
			want: []string{"facet", "api", "infra"},
		},
		{
			name: "--clone with key=url, the form a calling script passes",
			o: spawnOpts{Clones: []string{
				"api=git@github.com:acme/api.git",
				"infra=git@github.com:acme/infra.git",
			}},
			want: []string{"facet", "api", "infra"},
		},
		{
			name: "--clone naming the home repo too, as a calling script does",
			o: spawnOpts{Clones: []string{
				"facet=git@github.com:RiccardoCereghino/facet.git",
				"api=git@github.com:acme/api.git",
			}},
			want: []string{"facet", "api"},
		},
		{
			name: "--clone mixing the two spellings",
			o:    spawnOpts{Clones: []string{"api", "infra=git@github.com:acme/infra.git"}},
			want: []string{"facet", "api", "infra"},
		},
		{
			name: "--add takes key=url as well",
			o:    spawnOpts{Add: []string{"web=git@github.com:acme/web.git"}},
			want: []string{"facet", "web"},
		},
		{
			name: "--add does not duplicate what is already selected",
			o:    spawnOpts{Clones: []string{"api"}, Add: []string{"api"}},
			want: []string{"facet", "api"},
		},
		{
			name: "--rm drops by key=url as well as by key",
			o: spawnOpts{
				Clones: []string{"api", "infra"},
				Remove: []string{"infra=git@github.com:acme/infra.git"},
			},
			want: []string{"facet", "api"},
		},
		{
			name: "--rm cannot remove the home repo: it carries the branch",
			o:    spawnOpts{Clones: []string{"api"}, Remove: []string{"facet"}},
			want: []string{"facet", "api"},
		},
		{
			name: "--rm of a routed repo that is not in the set is a no-op, not a refusal",
			o:    spawnOpts{Remove: []string{"cli"}},
			want: []string{"facet"},
		},
		{
			// route.Infer resolves aliases, so a flag that did not would refuse
			// a spelling routing itself can resolve -- and today drops it.
			name: "--clone takes an alias, as the inference does",
			o:    spawnOpts{Clones: []string{"backend"}},
			want: []string{"facet", "api"},
		},
		{
			name: "--clone takes alias=url, checked against the aliased repo's url",
			o:    spawnOpts{Clones: []string{"backend=git@github.com:acme/api.git"}},
			want: []string{"facet", "api"},
		},
		{
			name: "--add and --rm take an alias too",
			o:    spawnOpts{Clones: []string{"api", "infra"}, Add: []string{"web"}, Remove: []string{"plumbing"}},
			want: []string{"facet", "api", "web"},
		},
		{
			name: "an alias does not duplicate the key it resolves to",
			o:    spawnOpts{Clones: []string{"api", "backend"}},
			want: []string{"facet", "api"},
		},
		{
			name: "no overrides leaves the inference alone",
			o:    spawnOpts{},
			want: []string{"facet"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := applyOverrides(inferred, route, "facet", tt.o)
			if err != nil {
				t.Fatalf("applyOverrides refused a recognised value: %v", err)
			}
			if strings.Join(keysOf(got), " ") != strings.Join(tt.want, " ") {
				t.Errorf("selection = %v, want %v", keysOf(got), tt.want)
			}
		})
	}
}

// TestApplyOverridesRefusesWhatItCannotResolve is the issue itself: a value
// that cannot name a repository must not be discarded in silence.
//
// "I applied your override" and "I threw your override away" produced identical
// output -- exit 0, a workspace, no warning -- so every multi-repo spawn a
// calling script ever made produced a single-repo workspace and nothing said so.
// The refusal is what makes those two outcomes distinguishable.
func TestApplyOverridesRefusesWhatItCannotResolve(t *testing.T) {
	route := overrideRouting()
	inferred := []routing.Selection{{Key: "facet", Reasons: []string{"home"}, Home: true}}

	tests := []struct {
		name  string
		o     spawnOpts
		wants []string // every substring the refusal must carry
	}{
		{
			name:  "--clone with a key routing has never heard of",
			o:     spawnOpts{Clones: []string{"nosuchrepo"}},
			wants: []string{"--clone", "nosuchrepo", "known keys", "infra"},
		},
		{
			name: "--clone key=url where the key is unknown",
			o:    spawnOpts{Clones: []string{"nosuchrepo=git@github.com:RiccardoCereghino/nosuchrepo.git"}},
			// The refusal must say WHICH half it objected to, or the reader
			// re-checks the url they got right.
			wants: []string{"--clone", "key", "nosuchrepo", "known keys"},
		},
		{
			name: "--clone key=url where the url disagrees with routing",
			o:    spawnOpts{Clones: []string{"api=git@github.com:SomeoneElse/api.git"}},
			wants: []string{
				"--clone", "api",
				"git@github.com:SomeoneElse/api.git",
				"git@github.com:acme/api.git",
			},
		},
		{
			name:  "--clone with an empty key",
			o:     spawnOpts{Clones: []string{"=git@github.com:acme/api.git"}},
			wants: []string{"--clone", "names no repository"},
		},
		{
			name: "--clone alias=url where the url disagrees with the ALIASED repo",
			o:    spawnOpts{Clones: []string{"backend=git@github.com:acme/infra.git"}},
			wants: []string{
				"--clone", "backend",
				"git@github.com:acme/infra.git",
				"git@github.com:acme/api.git",
			},
		},
		{
			name:  "--clone with an alias routing defines no repo for names the missing hop",
			o:     spawnOpts{Clones: []string{"quarry"}},
			wants: []string{"--clone", "quarry", "cava", "alias"},
		},
		{
			name:  "--add with an unknown key refuses too, and did not before",
			o:     spawnOpts{Add: []string{"nosuchrepo"}},
			wants: []string{"--add", "nosuchrepo", "known keys"},
		},
		{
			name:  "--rm with an unknown key refuses too, and did not before",
			o:     spawnOpts{Remove: []string{"nosuchrepo"}},
			wants: []string{"--rm", "nosuchrepo", "known keys"},
		},
		{
			name:  "a typo in one of several --clone values refuses the whole spawn",
			o:     spawnOpts{Clones: []string{"api", "inf", "cli"}},
			wants: []string{"--clone", "inf", "known keys"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := applyOverrides(inferred, route, "facet", tt.o)
			if err == nil {
				t.Fatalf("applyOverrides accepted %+v and returned %v -- a dropped override is invisible", tt.o, keysOf(got))
			}
			if got != nil {
				t.Errorf("a refusal returned a selection (%v); the caller must have nothing to proceed with", keysOf(got))
			}
			for _, want := range tt.wants {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal %q does not mention %q", err, want)
				}
			}
			if !strings.Contains(err.Error(), "fix:") {
				t.Errorf("refusal %q does not tell the reader how to fix it", err)
			}
		})
	}
}

// TestSpawnRefusesAnUnresolvableCloneThroughRunSpawn reaches the refusal the
// way production does, through runSpawn, rather than by calling applyOverrides
// directly.
//
// The tests above prove the mechanism works when called. They cannot prove
// anything CALLS it: applyOverrides returned no error at all before this
// change, so a version of this fix that resolved every value correctly and had
// its error dropped at the call site would pass all of them.
func TestSpawnRefusesAnUnresolvableCloneThroughRunSpawn(t *testing.T) {
	withSpawnableRouting(t)

	reached := false
	prevGH := gh
	gh = &recordingGH{reached: &reached}
	t.Cleanup(func() { gh = prevGH })

	// --dry-run: the refusal must come from the override, not from the
	// workspace creation that would follow it.
	err := runSpawn(spawnOpts{
		Repo: "acme/gateway", Number: 1, DryRun: true,
		Clones: []string{"gateway=https://example.invalid/gateway.git", "nosuchrepo"},
	})
	if err == nil {
		t.Fatal("spawn accepted an unresolvable --clone and carried on")
	}
	if !strings.Contains(err.Error(), "nosuchrepo") {
		t.Errorf("error %q does not name the value it could not resolve", err)
	}
}

// TestSpawnAcceptsAKeyURLCloneThroughRunSpawn is the negative half: the exact
// spelling a calling script passes must go through untouched. A refusal wired too
// broadly would pass the test above and break every multi-repo spawn.
func TestSpawnAcceptsAKeyURLCloneThroughRunSpawn(t *testing.T) {
	withSpawnableRouting(t)

	reached := false
	prevGH := gh
	gh = &recordingGH{reached: &reached}
	t.Cleanup(func() { gh = prevGH })

	err := runSpawn(spawnOpts{
		Repo: "acme/gateway", Number: 1, DryRun: true,
		Clones: []string{"gateway=https://example.invalid/gateway.git"},
	})
	if err != nil {
		t.Fatalf("spawn refused the form a calling script passes: %v", err)
	}
	if !reached {
		t.Error("spawn never reached ViewIssue")
	}
}

// TestOverrideKeyListsKeysInAStableOrder: a refusal read differently on every
// run is one nobody can diff, and Go randomises map iteration deliberately.
func TestOverrideKeyListsKeysInAStableOrder(t *testing.T) {
	route := overrideRouting()
	var first string
	for i := 0; i < 20; i++ {
		_, err := overrideKey("clone", "nosuchrepo", route)
		if err == nil {
			t.Fatal("an unknown key was accepted")
		}
		if i == 0 {
			first = err.Error()
			continue
		}
		if err.Error() != first {
			t.Fatalf("refusal text varies between runs:\n%s\n%s", first, err)
		}
	}
	if !strings.Contains(first, "api cli facet infra web") {
		t.Errorf("known keys are not listed sorted: %q", first)
	}
}
