package workspace

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bottega-cereghino/facet/internal/gitx"
	"github.com/bottega-cereghino/facet/internal/gitx/gitxtest"
	"github.com/bottega-cereghino/facet/internal/manifest"
)

// prunableWorkspace builds a long-lived workspace whose clone holds, beside its
// tracked files, every kind of directory prune must tell apart.
func prunableWorkspace(t *testing.T) (ws, clone string) {
	t.Helper()
	ws, clone = longLivedWorkspace(t, t.TempDir())
	// Only the root node_modules is ignored: sub/node_modules is untracked
	// work as far as git knows.
	writeTestFile(t, filepath.Join(clone, ".gitignore"), "/node_modules/\n.venv/\n.secrets/\n")
	mustGit(t, clone, "-c", "user.email=t@example.com", "-c", "user.name=t", "add", ".gitignore")
	// A directory named like a cache but committed: it is source here.
	writeMiB(t, filepath.Join(clone, ".terraform", "committed"))
	mustGit(t, clone, "add", ".terraform")
	// What rebuilds the two that prune may delete, tracked beside each.
	writeTestFile(t, filepath.Join(clone, "package.json"), rebuildBody("package.json"))
	if err := os.MkdirAll(filepath.Join(clone, "api"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(clone, "api", "pyproject.toml"), rebuildBody("pyproject.toml"))
	mustGit(t, clone, "add", "package.json", "api/pyproject.toml")
	mustGit(t, clone, "-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-qm", "tracked")

	writeMiB(t, filepath.Join(clone, "node_modules", "pkg", "index.js"))
	writeMiB(t, filepath.Join(clone, "api", ".venv", "lib", "site.py"))
	writeMiB(t, filepath.Join(clone, "sub", "node_modules", "mine.js"))
	writeMiB(t, filepath.Join(clone, ".secrets", "key"))
	// A worktree or nested repository has its own checkout to answer for.
	writeMiB(t, filepath.Join(clone, "nested", "node_modules", "x.js"))
	writeTestFile(t, filepath.Join(clone, "nested", ".git"), "gitdir: elsewhere\n")
	return ws, clone
}

func foundRels(s *PruneState) []string {
	var out []string
	for _, r := range s.Found {
		out = append(out, r.Clone+"/"+r.Rel)
	}
	return out
}

// Prune names only directories that are regenerable by name, ignored by git,
// free of tracked files, and outside nested checkouts. Anything else in the
// tree might be the only copy of something.
func TestInspectPruneFindsOnlyIgnoredRegenerableDirectories(t *testing.T) {
	ws, _ := prunableWorkspace(t)
	st, err := InspectPrune(ws, gitx.Git{}, nil, time.Now(), 0)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(foundRels(st), ",")
	if got != "repo/api/.venv,repo/node_modules" {
		t.Fatalf("found %q; want repo/api/.venv and repo/node_modules only", got)
	}
	if len(st.Held) != 0 {
		t.Errorf("held %v; want nothing holding a workspace with no idle window", st.Held)
	}
	if st.Frees() < 2*mib {
		t.Errorf("Frees() = %d; want at least the two MiB found", st.Frees())
	}
}

func TestPruneDeletesWhatWasFoundAndNothingElse(t *testing.T) {
	ws, clone := prunableWorkspace(t)
	st, err := InspectPrune(ws, gitx.Git{}, nil, time.Now(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := Prune(st); err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{"node_modules", "api/.venv"} {
		if _, err := os.Stat(filepath.Join(clone, gone)); !os.IsNotExist(err) {
			t.Errorf("%s survived prune (stat err %v)", gone, err)
		}
	}
	for _, kept := range []string{".terraform/committed", "sub/node_modules/mine.js", ".secrets/key", "nested/node_modules/x.js", "README"} {
		if _, err := os.Stat(filepath.Join(clone, kept)); err != nil {
			t.Errorf("%s is gone; prune must leave it: %v", kept, err)
		}
	}
	// What git tracks is untouched, so the clone reads exactly as before.
	if out, err := (gitx.Git{}).Run(clone, nil, "status", "--porcelain", "--untracked-files=no"); err != nil || out != "" {
		t.Errorf("git status after prune = %q, %v; want a clean tracked tree", out, err)
	}
}

// A workspace used recently is somebody's: its dependencies stay installed.
func TestInspectPruneHoldsAWorkspaceUsedWithinTheIdleWindow(t *testing.T) {
	ws, _ := prunableWorkspace(t)
	now := time.Now()
	st, err := InspectPrune(ws, gitx.Git{}, nil, now, 14*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Held) != 1 || !strings.Contains(st.Held[0], "idle window of 14 days") {
		t.Fatalf("held %v; want the idle window to hold a workspace used just now", st.Held)
	}
	if err := Prune(st); err == nil {
		t.Fatal("Prune deleted from a held workspace")
	}

	st, err = InspectPrune(ws, gitx.Git{}, nil, now.Add(30*24*time.Hour), 14*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Held) != 0 {
		t.Errorf("held %v thirty days on; want it prunable", st.Held)
	}
}

func TestInspectPruneHoldsAWorkspaceSomethingRunsFrom(t *testing.T) {
	ws, _ := prunableWorkspace(t)
	st, err := InspectPrune(ws, gitx.Git{}, fakeLiveRoots{roots: []string{"pid 42 (node)"}}, time.Now(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Held) != 1 || !strings.Contains(st.Held[0], "pid 42") {
		t.Fatalf("held %v; want the running process named", st.Held)
	}
}

// "Could not list what git ignores" must never read as "nothing is tracked".
func TestInspectPruneHoldsWhenIgnoredFilesCannotBeListed(t *testing.T) {
	ws, _ := prunableWorkspace(t)
	g := &gitxtest.Runner{
		Real: gitx.Git{},
		Fail: func(args []string) bool { return len(args) > 0 && args[0] == "ls-files" },
		Err:  errors.New("index file corrupt"),
	}
	st, err := InspectPrune(ws, g, nil, time.Now(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Found) != 0 || len(st.Held) != 1 || !strings.Contains(st.Held[0], "index file corrupt") {
		t.Fatalf("found %v, held %v; want nothing found and the failure holding it", foundRels(st), st.Held)
	}
}

// `pip install -e git+...` checks a repository out inside the .venv, and it may
// hold commits: a directory with a .git anywhere below it is kept.
func TestInspectPruneKeepsADirectoryHoldingARepository(t *testing.T) {
	ws, clone := prunableWorkspace(t)
	src := filepath.Join(clone, "api", ".venv", "src", "mylib")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	mustGit(t, src, "init", "-q")
	st, err := InspectPrune(ws, gitx.Git{}, nil, time.Now(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(foundRels(st), ","); got != "repo/node_modules" {
		t.Fatalf("found %q; want the .venv holding a repository left out", got)
	}
	if len(st.Kept) != 1 || !strings.Contains(st.Kept[0], "mylib") {
		t.Errorf("kept %v; want the .venv named with its repository", st.Kept)
	}
}

// git is not the only repository a rebuildable name can hold. pip's editable
// installs check Mercurial, Subversion and Bazaar repositories out into
// .venv/src as readily as git ones, a Jujutsu repository that is not colocated
// has no .git at all, and a bare git repository or a Fossil repository file
// has no checkout around it. Each may hold commits or changes that exist
// nowhere else, so the directory stays.
func TestPruneKeepsADirectoryHoldingANonGitRepository(t *testing.T) {
	for _, tc := range []struct {
		name, rel string // rel is the repository's only copy, inside the .venv
		bare      bool   // rel is made by git init --bare instead of written
	}{
		{name: "mercurial", rel: "src/mylib/.hg/store/data"},
		{name: "jujutsu", rel: "src/mylib/.jj/repo/store/data"},
		{name: "subversion", rel: "src/mylib/.svn/wc.db"},
		{name: "sapling", rel: "src/mylib/.sl/store/data"},
		{name: "bazaar", rel: "src/mylib/.bzr/branch/data"},
		{name: "darcs", rel: "src/mylib/_darcs/patches/data"},
		{name: "pijul", rel: "src/mylib/.pijul/pristine/db"},
		{name: "cvs", rel: "src/mylib/CVS/Entries"},
		{name: "fossil checkout", rel: "src/mylib/.fslckout"},
		{name: "fossil repository", rel: "src/mylib.fossil"},
		{name: "bare git", rel: "src/mylib.git", bare: true},
		{name: "the venv itself a checkout", rel: ".hg/store/data"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws, clone := rebuildClone(t, ".venv", ".", "pyproject.toml", "")
			only := filepath.Join(clone, ".venv", filepath.FromSlash(tc.rel))
			if tc.bare {
				mustGit(t, clone, "init", "-q", "--bare", only)
				writeMiB(t, filepath.Join(only, "objects", "pack", "only-copy.pack"))
			} else {
				writeMiB(t, only)
			}
			st, err := InspectPrune(ws, gitx.Git{}, nil, time.Now(), 0)
			if err != nil {
				t.Fatal(err)
			}
			if err := Prune(st); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(only); err != nil {
				t.Fatalf("prune deleted a .venv holding a repository (%s): %v (found %v, kept %v)",
					tc.name, err, foundRels(st), st.Kept)
			}
			if len(st.Found) != 0 {
				t.Errorf("found %v; want the .venv left out", foundRels(st))
			}
		})
	}
}

// A wholly ignored name holding a file someone force-added is not wholly
// ignored, so git lists its files one by one and prune leaves it.
func TestInspectPruneLeavesAnIgnoredDirectoryHoldingATrackedFile(t *testing.T) {
	ws, clone := prunableWorkspace(t)
	mustGit(t, clone, "add", "-f", "node_modules/pkg/index.js")
	mustGit(t, clone, "-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-qm", "vendored")
	st, err := InspectPrune(ws, gitx.Git{}, nil, time.Now(), 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range foundRels(st) {
		if r == "repo/node_modules" {
			t.Fatal("named a node_modules holding a tracked file")
		}
	}
}

func TestInspectPruneNeverNamesTerraform(t *testing.T) {
	ws, clone := prunableWorkspace(t)
	if err := os.MkdirAll(filepath.Join(clone, "infra"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(clone, "infra", ".gitignore"), ".terraform/\n")
	writeMiB(t, filepath.Join(clone, "infra", ".terraform", "environment"))
	st, err := InspectPrune(ws, gitx.Git{}, nil, time.Now(), 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range foundRels(st) {
		if strings.Contains(r, ".terraform") {
			t.Fatalf("named %s; .terraform keeps the selected workspace", r)
		}
	}
}

// A worktree clone keeps its index and HEAD outside <clone>/.git. Its use must
// still be seen, or a workspace used a minute ago reads as idle.
func TestInspectPruneSeesTheUseOfAWorktreeClone(t *testing.T) {
	ws, clone := prunableWorkspace(t)
	wt := filepath.Join(ws, "wt")
	mustGit(t, clone, "worktree", "add", "-q", "--detach", wt)
	writeTestFile(t, filepath.Join(wt, ".gitignore"), "/node_modules/\n")
	writeMiB(t, filepath.Join(wt, "node_modules", "x.js"))
	m, err := manifest.Read(ws)
	if err != nil {
		t.Fatal(err)
	}
	m.Clones = map[string]string{"wt": m.Clones["repo"]}
	if err := m.Write(ws); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-60 * 24 * time.Hour)
	if err := os.Chtimes(filepath.Join(wt, "node_modules"), old, old); err != nil {
		t.Fatal(err)
	}
	st, err := InspectPrune(ws, gitx.Git{}, nil, time.Now(), 14*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Held) == 0 {
		t.Fatalf("a worktree clone used just now reads as idle (last used %v)", st.LastUsed)
	}
}

// A probe that errors is not "nothing runs here".
func TestInspectPruneHoldsWhenWhatRunsCannotBeListed(t *testing.T) {
	ws, _ := prunableWorkspace(t)
	st, err := InspectPrune(ws, gitx.Git{}, failingLiveRoots{}, time.Now(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Held) != 1 || !strings.Contains(st.Held[0], "could not list") {
		t.Fatalf("held %v; want the failed probe to hold", st.Held)
	}
}

type failingLiveRoots struct{}

func (failingLiveRoots) Live(string, string) bool { return false }
func (failingLiveRoots) LiveRoots(string) ([]string, error) {
	return nil, errors.New("lsof crashed")
}

// No step of the deleted path may be a symlink: a parent swapped for one after the
// listing cannot point it outside the clone.
func TestPruneDoesNotFollowASwappedParent(t *testing.T) {
	ws, clone := prunableWorkspace(t)
	st, err := InspectPrune(ws, gitx.Git{}, nil, time.Now(), 0)
	if err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	writeMiB(t, filepath.Join(outside, ".venv", "precious"))
	if err := os.RemoveAll(filepath.Join(clone, "api")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(clone, "api")); err != nil {
		t.Fatal(err)
	}
	_ = Prune(st)
	if _, err := os.Stat(filepath.Join(outside, ".venv", "precious")); err != nil {
		t.Fatalf("prune followed a symlinked parent out of the clone: %v", err)
	}
}

// rebuildClone makes a workspace whose one clone ignores dir and holds a MiB in
// it, under parent. With tracked set, the file named tracked is committed in
// parent; untracked is written beside it without being added.
func rebuildClone(t *testing.T, dir, parent, tracked, untracked string) (ws, clone string) {
	t.Helper()
	ws, clone = longLivedWorkspace(t, t.TempDir())
	writeTestFile(t, filepath.Join(clone, ".gitignore"), dir+"/\n")
	mustGit(t, clone, "add", ".gitignore")
	if err := os.MkdirAll(filepath.Join(clone, parent), 0o755); err != nil {
		t.Fatal(err)
	}
	if tracked != "" {
		writeTestFile(t, filepath.Join(clone, parent, tracked), rebuildBody(filepath.Base(tracked)))
		mustGit(t, clone, "add", filepath.ToSlash(filepath.Join(parent, tracked)))
	}
	mustGit(t, clone, "-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-qm", "rebuild files")
	if untracked != "" {
		writeTestFile(t, filepath.Join(clone, parent, untracked), rebuildBody(filepath.Base(untracked)))
	}
	writeMiB(t, filepath.Join(clone, parent, dir, "inside", "file"))
	return ws, clone
}

// rebuildBody is the text of a rebuild file that does declare a package, so a
// test about something else is not kept for want of one.
func rebuildBody(name string) string {
	switch name {
	case "package.json":
		return "{\"dependencies\": {\"left-pad\": \"1.3.0\"}}\n"
	case "pyproject.toml":
		return "[project]\nname = \"app\"\n"
	default: // requirements*.txt
		return "requests==2.32.3\n"
	}
}

// Each directory prune deletes is rebuilt from a file the clone tracks beside
// it. Without one, "rebuild: uv sync" names a command that cannot bring the
// directory back, so it is kept and the listing says why. A file that is there
// but untracked, or tracked in another directory, rebuilds nothing here.
func TestPruneKeepsADirectoryNothingTrackedBesideItRebuilds(t *testing.T) {
	for _, tc := range []struct {
		name, dir, parent, tracked, untracked string
		pruned                                bool
	}{
		{name: "venv alone", dir: ".venv", parent: "."},
		{name: "venv beside an untracked pyproject", dir: ".venv", parent: ".", untracked: "pyproject.toml"},
		{name: "venv below a tracked pyproject", dir: ".venv", parent: "api", tracked: "../pyproject.toml"},
		{name: "venv beside a tracked pyproject", dir: ".venv", parent: ".", tracked: "pyproject.toml", pruned: true},
		{name: "venv beside tracked requirements", dir: ".venv", parent: "api", tracked: "requirements-dev.txt", pruned: true},
		{name: "node_modules alone", dir: "node_modules", parent: "."},
		{name: "node_modules beside an untracked package.json", dir: "node_modules", parent: "web", untracked: "package.json"},
		{name: "node_modules beside a tracked package.json", dir: "node_modules", parent: "web", tracked: "package.json", pruned: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws, clone := rebuildClone(t, tc.dir, tc.parent, tc.tracked, tc.untracked)
			rel := filepath.ToSlash(filepath.Join(tc.parent, tc.dir))
			st, err := InspectPrune(ws, gitx.Git{}, nil, time.Now(), 0)
			if err != nil {
				t.Fatal(err)
			}
			if len(st.Held) != 0 {
				t.Fatalf("held %v; want nothing held", st.Held)
			}
			if err := Prune(st); err != nil {
				t.Fatal(err)
			}
			_, statErr := os.Stat(filepath.Join(clone, tc.parent, tc.dir, "inside", "file"))
			if tc.pruned {
				if got := strings.Join(foundRels(st), ","); got != "repo/"+rel {
					t.Errorf("found %q; want repo/%s", got, rel)
				}
				if !os.IsNotExist(statErr) {
					t.Errorf("%s survived prune with a tracked rebuild file beside it (stat err %v)", rel, statErr)
				}
				return
			}
			if len(st.Found) != 0 {
				t.Errorf("found %v; want nothing with no tracked rebuild file", foundRels(st))
			}
			if statErr != nil {
				t.Errorf("%s is gone though nothing tracked rebuilds it: %v", rel, statErr)
			}
			if len(st.Kept) != 1 || !strings.Contains(st.Kept[0], "repo/"+rel) || !strings.Contains(st.Kept[0], "nothing rebuilds it") {
				t.Errorf("kept %v; want repo/%s named with why it was kept", st.Kept, rel)
			}
		})
	}
}

// installClone makes a workspace whose one clone commits files at its root,
// ignores dir there and holds a MiB in it. edits are then written over the
// committed files without being staged.
func installClone(t *testing.T, dir string, files, edits map[string]string) (ws, clone string) {
	t.Helper()
	ws, clone = longLivedWorkspace(t, t.TempDir())
	writeTestFile(t, filepath.Join(clone, ".gitignore"), dir+"/\n")
	mustGit(t, clone, "add", ".gitignore")
	for name, body := range files {
		writeTestFile(t, filepath.Join(clone, name), body)
		mustGit(t, clone, "add", name)
	}
	mustGit(t, clone, "-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-qm", "project files")
	for name, body := range edits {
		writeTestFile(t, filepath.Join(clone, name), body)
	}
	writeMiB(t, filepath.Join(clone, dir, "lib", "site-packages", "hand_built", "__init__.py"))
	return ws, clone
}

// A file is not a rebuild by its name alone. A setup.cfg holding only flake8's
// settings, a pyproject.toml holding only ruff's, or a package.json that only
// marks a directory as ES modules is common in a repository that is not a
// package of that kind: `uv sync`, `pip install .` and `npm install` install
// nothing from it, so a .venv or node_modules filled by hand beside one is
// gone for good once deleted. It is kept, and the listing says why.
func TestPruneKeepsADirectoryBesideAFileThatInstallsNothing(t *testing.T) {
	for _, tc := range []struct {
		name, dir    string
		files, edits map[string]string
	}{
		{name: "setup.cfg with only flake8", dir: ".venv",
			files: map[string]string{"setup.cfg": "[flake8]\nmax-line-length = 100\n"}},
		{name: "pyproject.toml with only ruff", dir: ".venv",
			files: map[string]string{"pyproject.toml": "[tool.ruff]\nline-length = 100\n"}},
		{name: "pyproject.toml with only a build system", dir: ".venv",
			files: map[string]string{"pyproject.toml": "[build-system]\nrequires = [\"setuptools\"]\n"}},
		{name: "a project table only quoted in a multi-line string", dir: ".venv",
			files: map[string]string{"pyproject.toml": "[tool.template]\nbody = \"\"\"\n[project]\nname = \"x\"\n\"\"\"\n"}},
		// pip install . needs a setup.py or a pyproject.toml beside it.
		{name: "setup.cfg with metadata and nothing to build it", dir: ".venv",
			files: map[string]string{"setup.cfg": "[metadata]\nname = app\n"}},
		{name: "setup.cfg with metadata only in a continuation line", dir: ".venv",
			files: map[string]string{"setup.cfg": "[flake8]\nexclude =\n  [metadata]\n", "pyproject.toml": "[tool.ruff]\n"}},
		{name: "requirements.txt of comments", dir: ".venv",
			files: map[string]string{"requirements.txt": "# nothing pinned yet\n\n"}},
		// What a fresh clone rebuilds from is what the index holds.
		{name: "pyproject.toml declaring a project only in an uncommitted edit", dir: ".venv",
			files: map[string]string{"pyproject.toml": "[tool.ruff]\n"},
			edits: map[string]string{"pyproject.toml": "[project]\nname = \"app\"\n"}},
		// And what an install now reads is what is on disk.
		{name: "pyproject.toml whose project an uncommitted edit removed", dir: ".venv",
			files: map[string]string{"pyproject.toml": "[project]\nname = \"app\"\n"},
			edits: map[string]string{"pyproject.toml": "[tool.ruff]\n"}},
		{name: "package.json marking ES modules", dir: "node_modules",
			files: map[string]string{"package.json": "{\"type\": \"module\"}\n"}},
		{name: "package.json with empty dependencies", dir: "node_modules",
			files: map[string]string{"package.json": "{\"dependencies\": {}, \"devDependencies\": {}}\n"}},
		{name: "package.json that is not JSON", dir: "node_modules",
			files: map[string]string{"package.json": "{\"dependencies\": {\"left-pad\": \"1.3.0\"}\n"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws, clone := installClone(t, tc.dir, tc.files, tc.edits)
			handBuilt := filepath.Join(clone, tc.dir, "lib", "site-packages", "hand_built", "__init__.py")
			st, err := InspectPrune(ws, gitx.Git{}, nil, time.Now(), 0)
			if err != nil {
				t.Fatal(err)
			}
			if err := Prune(st); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(handBuilt); err != nil {
				t.Fatalf("prune deleted a %s beside a file that installs nothing: %v (found %v)", tc.dir, err, foundRels(st))
			}
			if len(st.Kept) != 1 || !strings.Contains(st.Kept[0], "repo/"+tc.dir) || !strings.Contains(st.Kept[0], "nothing rebuilds it") {
				t.Errorf("kept %v; want repo/%s named with why it was kept", st.Kept, tc.dir)
			}
		})
	}
}

// A file that does declare a package still rebuilds the directory beside it,
// and a tool's cache still goes beside the configuration alone: the cache holds
// only what the tool computes again, and that configuration is where ruff,
// mypy and pytest write it.
func TestPruneDeletesADirectoryBesideAFileThatInstallsIt(t *testing.T) {
	for _, tc := range []struct {
		name, dir string
		files     map[string]string
	}{
		{name: "pyproject.toml with a project", dir: ".venv",
			files: map[string]string{"pyproject.toml": "[project]\nname = \"app\"\ndependencies = [\n  \"requests\",\n]\n"}},
		{name: "pyproject.toml with only a project subtable, spaced", dir: ".venv",
			files: map[string]string{"pyproject.toml": "[tool.ruff]\n[ project . optional-dependencies ] # extras\ndev = []\n"}},
		{name: "pyproject.toml with poetry", dir: ".venv",
			files: map[string]string{"pyproject.toml": "[tool.poetry]\nname = \"app\"\n"}},
		{name: "pyproject.toml with a project after a multi-line string", dir: ".venv",
			files: map[string]string{"pyproject.toml": "[tool.black]\nextend-exclude = '''\n/(build)/\n'''\n[project]\nname = \"app\"\n"}},
		{name: "pyproject.toml of a uv workspace root, with its uv.lock", dir: ".venv",
			files: map[string]string{"pyproject.toml": "[tool.uv.workspace]\nmembers = [\"packages/*\"]\n", "uv.lock": "version = 1\n"}},
		{name: "setup.cfg with metadata beside a pyproject.toml", dir: ".venv",
			files: map[string]string{"setup.cfg": "[metadata]\nname = app\n", "pyproject.toml": "[build-system]\nrequires = [\"setuptools\"]\n"}},
		{name: "setup.cfg with options beside a setup.py", dir: ".venv",
			files: map[string]string{"setup.cfg": "[options]\ninstall_requires = requests\n", "setup.py": "from setuptools import setup\nsetup()\n"}},
		{name: "setup.py", dir: ".venv",
			files: map[string]string{"setup.py": "from setuptools import setup\nsetup(name=\"app\")\n"}},
		{name: "Pipfile", dir: ".venv",
			files: map[string]string{"Pipfile": "[packages]\nrequests = \"*\"\n"}},
		{name: "requirements file", dir: ".venv",
			files: map[string]string{"requirements-dev.txt": "# pinned\nrequests==2.32.3\n"}},
		{name: "package.json with dependencies", dir: "node_modules",
			files: map[string]string{"package.json": "{\"devDependencies\": {\"left-pad\": \"1.3.0\"}}\n"}},
		{name: "package.json of npm workspaces", dir: "node_modules",
			files: map[string]string{"package.json": "{\"private\": true, \"workspaces\": [\"packages/*\"]}\n"}},
		{name: "package.json of a pnpm workspace", dir: "node_modules",
			files: map[string]string{"package.json": "{\"private\": true}\n", "pnpm-workspace.yaml": "packages:\n  - packages/*\n"}},
		{name: "ruff cache beside ruff's settings", dir: ".ruff_cache",
			files: map[string]string{"pyproject.toml": "[tool.ruff]\nline-length = 100\n"}},
		{name: "pytest cache beside pytest's settings", dir: ".pytest_cache",
			files: map[string]string{"setup.cfg": "[tool:pytest]\naddopts = -q\n"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws, clone := installClone(t, tc.dir, tc.files, nil)
			st, err := InspectPrune(ws, gitx.Git{}, nil, time.Now(), 0)
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Join(foundRels(st), ","); got != "repo/"+tc.dir {
				t.Fatalf("found %q, kept %v; want repo/%s", got, st.Kept, tc.dir)
			}
			if err := Prune(st); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(clone, tc.dir)); !os.IsNotExist(err) {
				t.Errorf("%s survived prune beside a file that rebuilds it (stat err %v)", tc.dir, err)
			}
		})
	}
}

// pip writes into .venv/lib/.../site-packages, and an edit to an installed
// package (the patch-package workflow, where that edit is the only copy until
// patch-package runs) writes deep inside node_modules. Neither touches the top
// directory, which was all prune dated: a directory installed into or edited
// yesterday read as idle (#304).
func TestPruneSeesAnInstallOrEditInsideTheDirectory(t *testing.T) {
	for _, tc := range []struct{ dir, from, inner string }{
		{".venv", "pyproject.toml", "lib/python3.12/site-packages/newpkg/__init__.py"},
		{"node_modules", "package.json", "left-pad/index.js"},
	} {
		t.Run(tc.dir, func(t *testing.T) {
			ws, clone := rebuildClone(t, tc.dir, ".", tc.from, "")
			now := time.Now().Add(60 * 24 * time.Hour)
			yesterday := now.Add(-24 * time.Hour)
			inner := filepath.Join(clone, tc.dir, filepath.FromSlash(tc.inner))
			writeMiB(t, inner)
			for p := inner; p != filepath.Join(clone, tc.dir); p = filepath.Dir(p) {
				if err := os.Chtimes(p, yesterday, yesterday); err != nil {
					t.Fatal(err)
				}
			}
			st, err := InspectPrune(ws, gitx.Git{}, nil, now, 14*24*time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			if len(st.Held) == 0 {
				t.Fatalf("a %s written into a day ago reads as idle (last used %v, found %v)",
					tc.dir, st.LastUsed.Sub(now).Round(time.Hour), foundRels(st))
			}
		})
	}
}
