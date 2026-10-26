package main

import (
	"strings"
	"testing"

	"github.com/RiccardoCereghino/facet/internal/ghx"
)

// offlineGH fails the test if anything reaches the forge. The seat checks are
// meant to happen before the credential gate and before the issue lookup, and
// "it refuses" and "it refuses without having done anything first" are different
// claims -- only the second one means a mistyped seat name cannot leave a branch
// behind on the forge.
type offlineGH struct {
	fakeGH
	t *testing.T
}

func (o *offlineGH) Auth() (*ghx.AuthStatus, error) {
	o.t.Error("spawn reached the credential check before validating the scope")
	return o.fakeGH.Auth()
}

func (o *offlineGH) ViewIssue(repo string, number int) (*ghx.Issue, error) {
	o.t.Errorf("spawn looked up %s#%d before validating the scope", repo, number)
	return o.fakeGH.ViewIssue(repo, number)
}

func TestSpawnRefusesABadScopeBeforeTouchingTheForge(t *testing.T) {
	tests := []struct {
		name    string
		opts    spawnOpts
		wantMsg string
	}{
		{
			name:    "unqualified scope entry",
			opts:    spawnOpts{Number: 12, Repo: "owner/repo", Scope: []string{"repo#7"}},
			wantMsg: "--scope",
		},
		{
			name:    "scope entry with no issue number",
			opts:    spawnOpts{Number: 12, Repo: "owner/repo", Scope: []string{"owner/repo"}},
			wantMsg: "--scope",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prev := gh
			gh = &offlineGH{t: t}
			t.Cleanup(func() { gh = prev })

			err := runSpawn(tt.opts)
			if err == nil {
				t.Fatal("spawn accepted it")
			}
			if !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("error %q does not mention %q", err, tt.wantMsg)
			}
			if !strings.Contains(err.Error(), "fix:") {
				t.Errorf("error %q does not tell the reader how to fix it", err)
			}
		})
	}
}

func TestIssueBranchNameCarriesFeaturePrefix(t *testing.T) {
	got := issueBranchName(42, "some-slug")
	want := "feature/42-some-slug"
	if got != want {
		t.Errorf("issueBranchName(42, %q) = %q, want %q", "some-slug", got, want)
	}
}
