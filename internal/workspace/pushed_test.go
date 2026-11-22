package workspace

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/bottega-cereghino/facet/internal/gitx"
)

// A ref under refs/remotes that no fetch refreshes proves nothing: work
// pushed to a fork whose branch was deleted there since, after the fork's
// refspec was narrowed or its config section removed by hand. Only the refs
// of a configured remote count, which the fetch before the count refreshes
// and prunes whatever the remote's refspec says (facet#309).
func TestReapHoldsWorkOnlyAStaleRemoteRefHas(t *testing.T) {
	for name, orphan := range map[string]func(t *testing.T, clone string){
		"refspec narrowed": func(t *testing.T, clone string) {
			gitOut(t, clone, "remote", "set-branches", "fork", "main")
		},
		"remote section removed by hand": func(t *testing.T, clone string) {
			gitOut(t, clone, "config", "--remove-section", "remote.fork")
		},
	} {
		t.Run(name, func(t *testing.T) {
			ws, clone := issueWorkspace(t)
			fork := filepath.Join(t.TempDir(), "fork.git")
			gitOut(t, "", "init", "-q", "--bare", fork)
			gitOut(t, clone, "remote", "add", "fork", fork)
			gitOut(t, clone, "switch", "-q", "-c", "feature")
			commitFile(t, clone, "idea.txt", "the idea\n", "the idea")
			gitOut(t, clone, "push", "-q", "fork", "feature")

			orphan(t, clone)
			gitOut(t, fork, "branch", "-q", "-D", "feature")

			st, err := InspectIssue(ws, gitx.Git{}, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			if !hasBlocker(st, filepath.Join("repo")+" has 1 unpushed commit") {
				t.Fatalf("the branch is gone from the fork, and reap would delete the only copy: %v", st.Blockers())
			}
		})
	}
}

// A remote whose repository reap deletes with the workspace is no copy: the
// clone itself as a remote, a bare backup inside it, a sibling clone. Its
// refs never count as pushed (facet#309).
func TestReapHoldsWorkWhoseOnlyRemoteIsInsideTheWorkspace(t *testing.T) {
	for name, addRemote := range map[string]func(t *testing.T, ws, clone string) string{
		"the clone itself": func(t *testing.T, _, clone string) string {
			gitOut(t, clone, "remote", "add", "self", ".")
			return "self"
		},
		"a bare repository in the clone, by a relative path": func(t *testing.T, _, clone string) string {
			gitOut(t, "", "init", "-q", "--bare", filepath.Join(clone, "backup.git"))
			writeTestFile(t, filepath.Join(clone, ".git", "info", "exclude"), "backup.git/\n")
			gitOut(t, clone, "remote", "add", "backup", "backup.git")
			return "backup"
		},
		"a bare repository beside the clones, by a file URL": func(t *testing.T, ws, clone string) string {
			gitOut(t, "", "init", "-q", "--bare", filepath.Join(ws, "backup.git"))
			gitOut(t, clone, "remote", "add", "backup", "file://"+filepath.Join(ws, "backup.git"))
			return "backup"
		},
	} {
		t.Run(name, func(t *testing.T) {
			ws, clone := issueWorkspace(t)
			remote := addRemote(t, ws, clone)
			gitOut(t, clone, "switch", "-q", "-c", "feature")
			commitFile(t, clone, "idea.txt", "the idea\n", "the idea")
			gitOut(t, clone, "push", "-q", remote, "feature")
			gitOut(t, clone, "fetch", "-q", remote)

			st, err := InspectIssue(ws, gitx.Git{}, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			if !hasBlocker(st, "repo has 1 unpushed commit") {
				t.Fatalf("the commit's only other copy is inside the workspace, and reap would delete both: %v", st.Blockers())
			}
		})
	}
}

// Pushed to a remote outside the workspace, by a local path, the same work
// counts as pushed.
func TestReapCountsWorkOnARemoteOutsideTheWorkspaceAsPushed(t *testing.T) {
	ws, clone := issueWorkspace(t)
	backup := filepath.Join(t.TempDir(), "backup.git")
	gitOut(t, "", "init", "-q", "--bare", backup)
	gitOut(t, clone, "remote", "add", "backup", backup)
	gitOut(t, clone, "switch", "-q", "-c", "feature")
	commitFile(t, clone, "idea.txt", "the idea\n", "the idea")
	gitOut(t, clone, "push", "-q", "backup", "feature")

	st, err := InspectIssue(ws, gitx.Git{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if hasBlocker(st, "unpushed") {
		t.Fatalf("work pushed outside the workspace holds: %v", st.Blockers())
	}
}

// localPath names a local URL's path, read as git reads it, so a remote
// inside the workspace is found by its path.
func TestLocalPathInside(t *testing.T) {
	root := t.TempDir()
	gone := filepath.Join(root, "ws")
	clone := filepath.Join(gone, "repo")
	if err := os.MkdirAll(filepath.Join(clone, "tmp"), 0o777); err != nil {
		t.Fatal(err)
	}
	for url, want := range map[string]bool{
		".":                                   true,
		"tmp/backup.git":                      true,
		"../sibling":                          true,
		"../../elsewhere":                     false,
		clone:                                 true,
		filepath.Join(root, "other"):          false,
		"file://" + filepath.Join(clone, "x"): true,
		"file://" + filepath.Join(root, "x"):  false,
		"https://github.com/o/r":              false,
		"ssh://host/" + clone:                 false,
		"git@github.com:o/r.git":              false,
		"host:tmp/backup.git":                 false,
		"./a:b":                               true,
	} {
		p, local := localPath(url, clone)
		if got := local && isUnder(realPath(gone), resolvePath(p)); got != want {
			t.Errorf("localPath(%q) inside = %v, want %v", url, got, want)
		}
	}
}

// A remote URL may name a file. git fetches a bundle from one, but upload-pack
// also follows a gitfile, so a file reading `gitdir: <clone>/.git` fetches the
// clone itself: only a bundle is a copy (#380).
func TestReapHoldsWorkWhoseRemoteURLIsAGitfileIntoTheClone(t *testing.T) {
	ws, clone := issueWorkspace(t)
	gitfile := filepath.Join(t.TempDir(), "backup")
	writeTestFile(t, gitfile, "gitdir: "+filepath.Join(clone, ".git")+"\n")
	gitOut(t, clone, "remote", "add", "backup", gitfile)
	gitOut(t, clone, "switch", "-q", "-c", "feature")
	commitFile(t, clone, "idea.txt", "the idea\n", "the idea")
	gitOut(t, clone, "fetch", "-q", "backup")
	if got := gitOut(t, clone, "rev-parse", "refs/remotes/backup/feature"); got != gitOut(t, clone, "rev-parse", "HEAD") {
		t.Fatalf("setup: backup/feature = %s, want HEAD", got)
	}
	st, err := InspectIssue(ws, gitx.Git{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !hasBlocker(st, "unpushed") {
		t.Fatalf("the commit's only repository is the clone, fetched through %s; blockers = %v", gitfile, st.Blockers())
	}
}

// A bundle outside the workspace is a copy of what it holds.
func TestReapCountsWorkInABundleOutsideTheWorkspaceAsPushed(t *testing.T) {
	ws, clone := issueWorkspace(t)
	gitOut(t, clone, "switch", "-q", "-c", "feature")
	commitFile(t, clone, "idea.txt", "the idea\n", "the idea")
	bundle := filepath.Join(t.TempDir(), "feature.bundle")
	gitOut(t, clone, "bundle", "create", "-q", bundle, "feature")
	gitOut(t, clone, "remote", "add", "backup", bundle)
	gitOut(t, clone, "fetch", "-q", "backup")
	st, err := InspectIssue(ws, gitx.Git{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if hasBlocker(st, "unpushed") {
		t.Fatalf("work in a bundle outside the workspace holds: %v", st.Blockers())
	}
}
