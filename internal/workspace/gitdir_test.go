package workspace

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/bottega-cereghino/facet/internal/gitx"
	"github.com/bottega-cereghino/facet/internal/manifest"
)

// A repository is judged by its refs only when nothing in its git directory
// rewrites history or borrows objects from the workspace, and a remote counts
// only when the repository git reads there lies outside it (#364).

// A backup made with `git clone --shared` (or --reference) borrows the
// clone's objects through objects/info/alternates. A push to it sends no
// object (receive-pack advertises the alternate's tips), so its ref names
// commits whose only copy is in the workspace. reap counted the branch as
// pushed and deleted the objects, leaving the backup's ref dangling (#358).
func TestReapHoldsWorkPushedToABackupThatBorrowsTheClonesObjects(t *testing.T) {
	ws, clone := issueWorkspace(t)
	backup := filepath.Join(t.TempDir(), "backup.git")
	gitOut(t, "", "clone", "-q", "--bare", "--shared", "--", clone, backup)
	gitOut(t, clone, "remote", "add", "backup", backup)
	gitOut(t, clone, "switch", "-q", "-c", "feature")
	commitFile(t, clone, "idea.txt", "the idea\n", "the idea")
	gitOut(t, clone, "push", "-q", "backup", "feature")

	st, err := InspectIssue(ws, gitx.Git{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Blockers()) == 0 {
		if err := Reap(st, gitx.Git{}); err != nil {
			t.Fatal(err)
		}
		_, err := gitx.Git{}.Run(backup, nil, "cat-file", "-e", "refs/heads/feature^{tree}")
		t.Fatalf("reap deleted the workspace; the backup can read feature afterwards: %v (nil means it can)", err)
	}
}

// A directory outside the workspace whose .git is a gitfile (or a symlink)
// into the clone is the clone itself, read under another name: every local
// branch fetched from it read as pushed (#359).
func TestReapHoldsWorkWhoseRemoteIsAGitfileIntoTheClone(t *testing.T) {
	for name, link := range map[string]func(t *testing.T, alias, clone string){
		"gitfile": func(t *testing.T, alias, clone string) {
			writeTestFile(t, filepath.Join(alias, ".git"), "gitdir: "+filepath.Join(clone, ".git")+"\n")
		},
		"symlinked .git": func(t *testing.T, alias, clone string) {
			if err := os.Symlink(filepath.Join(clone, ".git"), filepath.Join(alias, ".git")); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			ws, clone := issueWorkspace(t)
			alias := filepath.Join(t.TempDir(), "alias")
			mkdirs(t, alias)
			link(t, alias, clone)
			gitOut(t, clone, "remote", "add", "alias", alias)
			gitOut(t, clone, "switch", "-q", "-c", "feature")
			commitFile(t, clone, "idea.txt", "the idea\n", "the idea")

			st, err := InspectIssue(ws, gitx.Git{}, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			requireHeld(t, st, "the commit's only repository is the clone, fetched through "+alias)
		})
	}
}

// $GIT_DIR/info/grafts is the older form of replace refs' local-only
// rewrite, and GIT_NO_REPLACE_OBJECTS does not turn it off (#360). It holds,
// with the command that converts it to replace refs, which reap ignores.
func TestReapHoldsAGraftFile(t *testing.T) {
	ws, clone := issueWorkspace(t)
	gitOut(t, clone, "switch", "-q", "-c", "feature")
	commitFile(t, clone, "idea.txt", "the idea\n", "the idea")
	local := gitOut(t, clone, "rev-parse", "HEAD")
	m, err := manifest.Read(ws)
	if err != nil {
		t.Fatal(err)
	}
	landOnDefault(t, gitx.Git{}, m.Clones["repo"], "theirs.txt", "their work\n")
	gitOut(t, clone, "fetch", "-q", "origin")
	tip := gitOut(t, clone, "rev-parse", "refs/remotes/origin/main")
	parent := gitOut(t, clone, "rev-parse", "refs/remotes/origin/main^")
	// origin/main's tip, with its real parent and the local commit as parents.
	writeTestFile(t, filepath.Join(clone, ".git", "info", "grafts"), tip+" "+parent+" "+local+"\n")

	st, err := InspectIssue(ws, gitx.Git{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	requireHeld(t, st, "the commit is on no remote; only a local graft says origin/main reaches it")
	if !hasBlocker(st, "git replace --convert-graft-file") {
		t.Fatalf("the hold does not print its fix: %v", st.Blockers())
	}
}

// A clone whose objects/info/alternates leads into the workspace borrows
// objects that go with it.
func TestReapHoldsACloneThatBorrowsObjectsFromTheWorkspace(t *testing.T) {
	ws, clone := issueWorkspace(t)
	store := filepath.Join(ws, "store.git")
	gitOut(t, "", "clone", "-q", "--bare", "--", clone, store)
	writeTestFile(t, filepath.Join(clone, ".git", "objects", "info", "alternates"), filepath.Join(store, "objects")+"\n")

	st, err := InspectIssue(ws, gitx.Git{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !hasBlocker(st, "borrows objects from "+filepath.Join(store, "objects")) {
		t.Fatalf("the clone borrows objects from inside the workspace, yet the hold does not say so: %v", st.Blockers())
	}
}
