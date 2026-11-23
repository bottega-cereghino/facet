package workspace

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bottega-cereghino/facet/internal/gitx"
	"github.com/bottega-cereghino/facet/internal/manifest"
)

// Guarantee: "sync writes only inside the workspace", the one exception being
// a fetch of origin and the worktree's entry in the checkout's .git. A remote
// the manifest declares for a worktree clone is added to the repository's
// shared config, which is the checkout's own .git/config.
func TestGrillSyncRemoteOnAWorktreeCloneWritesTheCheckoutConfig(t *testing.T) {
	roots, ws, origin, checkout := worktreeSetup(t)
	m := &manifest.Manifest{
		Name:    "w",
		Clones:  map[string]string{"repo": origin},
		Remotes: map[string]map[string]string{"repo": {"upstream": "https://example.invalid/fork.git"}},
	}
	if err := m.Write(ws); err != nil {
		t.Fatal(err)
	}
	if err := Sync(roots, ws, gitx.Git{}, quiet(), SyncOptions{Worktrees: true}); err != nil {
		t.Fatal(err)
	}
	if !isGitFile(t, filepath.Join(ws, "repo")) {
		t.Fatal("setup: the clone is not a worktree")
	}
	if url, err := (gitx.Git{}).Run(checkout, nil, "config", "--get", "remote.upstream.url"); err == nil {
		t.Errorf("sync added remote upstream=%s to the checkout %s, outside the workspace", url, checkout)
	}
}

// Guarantee: "sync writes only inside the workspace". A clone directory that
// is not a repository of its own (made by hand, or its .git removed) is still
// "present", and the remotes step runs git in it: git walks up to the
// enclosing repository (the README says ~/Workspaces usually is one) and adds
// the remote there.
func TestGrillSyncRemoteOnAPlainDirWritesTheEnclosingRepository(t *testing.T) {
	roots, ws, origin := setup(t)
	gitOut(t, roots.Workspaces, "init", "-q", "-b", "main")
	if err := os.MkdirAll(filepath.Join(ws, "notes"), 0o777); err != nil {
		t.Fatal(err)
	}
	m := &manifest.Manifest{
		Name:    "w",
		Clones:  map[string]string{"notes": origin},
		Remotes: map[string]map[string]string{"notes": {"upstream": "https://example.invalid/fork.git"}},
	}
	if err := m.Write(ws); err != nil {
		t.Fatal(err)
	}
	_ = Sync(roots, ws, gitx.Git{}, quiet(), SyncOptions{})
	if url, err := (gitx.Git{}).Run(roots.Workspaces, nil, "config", "--get", "remote.upstream.url"); err == nil {
		t.Errorf("sync added remote upstream=%s to the enclosing repository %s", url, roots.Workspaces)
	}
}

func TestSyncSkipsAnExtraRemoteOnAWorktreeClone(t *testing.T) {
	roots, ws, origin, checkout := worktreeSetup(t)
	if err := Sync(roots, ws, gitx.Git{}, quiet(), SyncOptions{Worktrees: true}); err != nil {
		t.Fatal(err)
	}
	m := &manifest.Manifest{
		Name:    "w",
		Clones:  map[string]string{"repo": origin},
		Remotes: map[string]map[string]string{"repo": {"upstream": "https://example.invalid/fork.git"}},
	}
	if err := m.Write(ws); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := Sync(roots, ws, gitx.Git{}, Reporter{W: &buf}, SyncOptions{Worktrees: true}); err != nil {
		t.Fatal(err)
	}
	want := "remote upstream on repo skipped: a worktree shares its checkout's git config, so the remote would be added there"
	if !strings.Contains(buf.String(), want) {
		t.Fatalf("warning %q missing from:\n%s", want, buf.String())
	}
	if url, err := (gitx.Git{}).Run(checkout, nil, "config", "--get", "remote.upstream.url"); err == nil {
		t.Errorf("sync added remote upstream=%s to the checkout %s", url, checkout)
	}
}

func TestSyncSkipsACloneFolderThatIsNotItsOwnRepository(t *testing.T) {
	roots, ws, origin := setup(t)
	gitOut(t, roots.Workspaces, "init", "-q", "-b", "main")
	notes := filepath.Join(ws, "notes")
	if err := os.MkdirAll(notes, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(notes, "keep"), []byte("mine\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	m := &manifest.Manifest{
		Name:    "w",
		Clones:  map[string]string{"notes": origin},
		Remotes: map[string]map[string]string{"notes": {"upstream": "https://example.invalid/fork.git"}},
	}
	if err := m.Write(ws); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := Sync(roots, ws, gitx.Git{}, Reporter{W: &buf}, SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "notes exists but is not the top level of its own repository") {
		t.Fatalf("skip warning missing from:\n%s", buf.String())
	}
	if _, err := os.Stat(filepath.Join(notes, ".git")); err == nil {
		t.Error("sync turned the folder into a repository")
	}
	got, err := os.ReadFile(filepath.Join(notes, "keep"))
	if err != nil || string(got) != "mine\n" {
		t.Errorf("sync wrote over the folder: %q, %v", got, err)
	}
	if url, err := (gitx.Git{}).Run(roots.Workspaces, nil, "config", "--get", "remote.upstream.url"); err == nil {
		t.Errorf("sync added remote upstream=%s to the enclosing repository %s", url, roots.Workspaces)
	}
}
