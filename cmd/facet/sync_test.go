package main

import (
	"strings"
	"testing"

	"github.com/RiccardoCereghino/facet/internal/config"
	"github.com/RiccardoCereghino/facet/internal/ghx"
)

// withUnsoundCredential points gh at a logged-out status and stubs the
// SSH-key half of preflight, so a refusal is attributable to the credential
// check alone and not to the CI runner's absent SSH key.
func withUnsoundCredential(t *testing.T) {
	t.Helper()
	prevGH := gh
	gh = &fakeGH{auth: &ghx.AuthStatus{State: ghx.StateAbsent}}
	t.Cleanup(func() { gh = prevGH })

	prevCheck := checkSSHKey
	checkSSHKey = func(string) ([]ghx.Problem, string) { return nil, "" }
	t.Cleanup(func() { checkSSHKey = prevCheck })
}

// The workspace core works on plain git repositories and needs no forge, so
// sync and restore must not consult the GitHub credential at all. Until this
// was reversed they refused outright on any credential `facet preflight`
// disliked -- including a default `gh auth login` token -- even for a manifest
// naming only local repositories. A bad credential still surfaces, from git,
// at the clone that needs it.
func TestSyncIgnoresTheCredential(t *testing.T) {
	withUnsoundCredential(t)

	err := runSync(t.TempDir()+"/does-not-exist", false, false, false)
	if err == nil {
		t.Fatal("sync succeeded against a path that does not exist")
	}
	if !strings.Contains(err.Error(), "workspace path not found") {
		t.Errorf("sync must fail on the missing path, not on the credential: %v", err)
	}
}

func TestRestoreIgnoresTheCredential(t *testing.T) {
	withUnsoundCredential(t)
	prev := roots
	roots = config.Roots{Workspaces: t.TempDir()}
	t.Cleanup(func() { roots = prev })

	if err := runRestore(); err != nil {
		t.Fatalf("restore over an empty root must succeed whatever the credential: %v", err)
	}
}
