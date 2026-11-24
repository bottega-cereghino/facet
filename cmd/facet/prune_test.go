package main

import (
	"bytes"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bottega-cereghino/facet/internal/config"
	"github.com/bottega-cereghino/facet/internal/gitx"
	"github.com/bottega-cereghino/facet/internal/launchd"
	"github.com/bottega-cereghino/facet/internal/manifest"
)

// pruneWorkspace makes a workspace whose one clone ignores its node_modules and
// tracks the package.json that rebuilds it, and returns the workspace and that
// node_modules.
func pruneWorkspace(t *testing.T, root, name string) (ws, deps string) {
	t.Helper()
	ws, deps = bareNodeModules(t, root, name)
	clone := filepath.Dir(deps)
	if err := os.WriteFile(filepath.Join(clone, "package.json"), []byte(`{"dependencies": {"left-pad": "1.3.0"}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := (gitx.Git{}).Run(clone, nil, "add", "package.json"); err != nil {
		t.Fatal(err)
	}
	return ws, deps
}

// bareNodeModules is pruneWorkspace without the package.json: nothing the clone
// tracks rebuilds its node_modules.
func bareNodeModules(t *testing.T, root, name string) (ws, deps string) {
	t.Helper()
	ws = filepath.Join(root, name)
	clone := filepath.Join(ws, "repo")
	deps = filepath.Join(clone, "node_modules")
	if err := os.MkdirAll(filepath.Join(deps, "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := (&manifest.Manifest{Name: name, Clones: map[string]string{"repo": "git@github.com:acme/repo.git"}}).Write(ws); err != nil {
		t.Fatal(err)
	}
	if _, err := (gitx.Git{}).Run(clone, nil, "init", "-q"); err != nil {
		t.Fatal(err)
	}
	for path, body := range map[string]string{
		filepath.Join(clone, ".gitignore"):            "node_modules/\n",
		filepath.Join(deps, "pkg", "index.js"):        strings.Repeat("facet\n", 2<<20/6),
		filepath.Join(clone, "only-copy-of-notes.md"): "untracked, not ignored\n",
	} {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return ws, deps
}

// busyLive reports a session live for one workspace name.
type busyLive struct{ name string }

func (b busyLive) Live(session, _ string) bool { return session == b.name }

func TestPruneListsWithoutApplyAndDeletesOnlyWhatNothingHolds(t *testing.T) {
	root := t.TempDir()
	idleWS, idleDeps := pruneWorkspace(t, root, "idle")
	busyWS, busyDeps := pruneWorkspace(t, root, "busy")
	dirs := []string{busyWS, idleWS}
	live := busyLive{name: "busy"}
	later := time.Now().Add(60 * 24 * time.Hour)

	var out bytes.Buffer
	if err := runPrune(&out, dirs, gitx.Git{}, live, later, 14*24*time.Hour, false, yesAll); err != nil {
		t.Fatal(err)
	}
	if !exists(t, idleDeps) || !exists(t, busyDeps) {
		t.Fatal("prune without --apply deleted something")
	}
	for _, want := range []string{"idle", "prunable", "busy", "held (1)", "repo/node_modules", "--apply"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("listing lacks %q:\n%s", want, out.String())
		}
	}

	out.Reset()
	if err := runPrune(&out, dirs, gitx.Git{}, live, later, 14*24*time.Hour, true, yesAll); err != nil {
		t.Fatal(err)
	}
	if exists(t, idleDeps) {
		t.Error("the idle workspace's node_modules survived --apply")
	}
	if !strings.Contains(out.String(), "removed "+idleDeps+" (") {
		t.Errorf("--apply did not log the removed cache:\n%s", out.String())
	}
	if !exists(t, busyDeps) {
		t.Error("--apply deleted from a workspace a session runs in")
	}
	if !exists(t, filepath.Join(idleWS, "repo", "only-copy-of-notes.md")) {
		t.Error("--apply deleted an untracked file that git does not ignore")
	}
}

// pruneCommandRoot points facet at root for a test that runs the prune
// command itself: no launchd jobs, and a tmux server of the test's own.
func pruneCommandRoot(t *testing.T, root string) {
	t.Helper()
	t.Setenv("TMUX_TMPDIR", t.TempDir())
	t.Setenv("TMUX", "")
	savedRoots, savedGit, savedJobs := roots, git, jobs
	t.Cleanup(func() { roots, git, jobs = savedRoots, savedGit, savedJobs })
	roots = config.Roots{Workspaces: root}
	git = gitx.Git{}
	jobs = &launchd.Jobs{Dirs: []string{t.TempDir()}, Run: noTools}
}

// age sets every file under dir to have last changed d ago, as a workspace
// nobody has touched for that long reads.
func age(t *testing.T, dir string, d time.Duration) {
	t.Helper()
	then := time.Now().Add(-d)
	err := filepath.WalkDir(dir, func(path string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return os.Chtimes(path, then, then)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func runPruneCmd(args ...string) (string, error) {
	var out bytes.Buffer
	cmd := newPruneCmd()
	cmd.SetArgs(args)
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	err := cmd.Execute()
	return out.String(), err
}

// --safe prunes an idle workspace without asking, holds one used today under
// the default window, and logs each directory it removed by its full path.
func TestPruneSafeCommandAppliesWithoutPrompt(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ws, deps := pruneWorkspace(t, root, "idle")
	_, usedDeps := pruneWorkspace(t, root, "today")
	age(t, ws, 30*24*time.Hour)
	pruneCommandRoot(t, root)
	out := captureStdout(t, func() {
		if _, err := runPruneCmd("--safe"); err != nil {
			t.Fatal(err)
		}
	})
	if exists(t, deps) {
		t.Fatalf("--safe did not prune the idle rebuildable cache:\n%s", out)
	}
	if !exists(t, filepath.Join(ws, "repo", "only-copy-of-notes.md")) {
		t.Fatal("--safe removed a non-cache file")
	}
	if !exists(t, usedDeps) {
		t.Fatalf("--safe pruned a workspace used today:\n%s", out)
	}
	if want := "removed " + deps + " ("; !strings.Contains(out, want) {
		t.Errorf("output lacks %q, the removed directory by its full path:\n%s", want, out)
	}
	if strings.Contains(out, "removed "+usedDeps) {
		t.Errorf("output names a directory that was kept as removed:\n%s", out)
	}
}

// --safe never shortens the idle window: prune's own hold message says "pass
// --idle-days 0", and a session following it must not take a workspace in
// use. A longer window only narrows, so it is accepted.
func TestPruneSafeRefusesAShorterIdleWindow(t *testing.T) {
	root := t.TempDir()
	_, deps := pruneWorkspace(t, root, "today")
	pruneCommandRoot(t, root)
	for _, days := range []string{"0", "13"} {
		out, err := runPruneCmd("--safe", "--idle-days", days)
		if err == nil || !strings.Contains(err.Error(), "--safe keeps the idle window") {
			t.Errorf("prune --safe --idle-days %s: err = %v; want the refusal\n%s", days, err, out)
		}
		if !exists(t, deps) {
			t.Fatalf("prune --safe --idle-days %s deleted from a workspace used today", days)
		}
	}
	if out, err := runPruneCmd("--safe", "--idle-days", "30"); err != nil {
		t.Errorf("prune --safe --idle-days 30: %v\n%s", err, out)
	}
	if !exists(t, deps) {
		t.Fatal("prune --safe --idle-days 30 deleted from a workspace used today")
	}
}

// The log is the record of an unattended run: a directory removed is logged
// and counted even when another in the same workspace could not be deleted.
func TestPruneLogsWhatItRemovedWhenAnotherFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root deletes inside a read-only directory: the failure cannot be planted")
	}
	root := t.TempDir()
	ws, deps := pruneWorkspace(t, root, "idle")
	clone := filepath.Join(ws, "repo")
	web := filepath.Join(clone, "web")
	stuck := filepath.Join(web, "node_modules", "stuck")
	for path, body := range map[string]string{
		filepath.Join(web, "package.json"): `{"dependencies": {"left-pad": "1.3.0"}}` + "\n",
		filepath.Join(stuck, "index.js"):   strings.Repeat("facet\n", 2<<20/6),
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := (gitx.Git{}).Run(clone, nil, "add", "web/package.json"); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(stuck, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(stuck, 0o755) })

	var out bytes.Buffer
	err := runPrune(&out, []string{ws}, gitx.Git{}, nil, time.Now(), 0, true, yesAll)
	if err == nil {
		t.Errorf("runPrune returned no error although web/node_modules could not be deleted:\n%s", out.String())
	}
	if exists(t, deps) {
		t.Fatalf("repo/node_modules survived:\n%s", out.String())
	}
	if want := "removed " + deps + " (2M)"; !strings.Contains(out.String(), want) {
		t.Errorf("output lacks %q although it was deleted:\n%s", want, out.String())
	}
	if strings.Contains(out.String(), "removed "+filepath.Join(web, "node_modules")) {
		t.Errorf("output says the directory it failed to delete was removed:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "freed 2M.") {
		t.Errorf("output should count what was freed:\n%s", out.String())
	}
}

// Used today, a workspace keeps its dependencies under the default window.
func TestPruneHoldsAWorkspaceUsedToday(t *testing.T) {
	ws, deps := pruneWorkspace(t, t.TempDir(), "today")
	var out bytes.Buffer
	if err := runPrune(&out, []string{ws}, gitx.Git{}, nil, time.Now(), 14*24*time.Hour, true, yesAll); err != nil {
		t.Fatal(err)
	}
	if !exists(t, deps) {
		t.Fatalf("a workspace used today lost its node_modules:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "idle window") {
		t.Errorf("output does not say what holds it:\n%s", out.String())
	}
}

func yesAll(string) bool { return true }

// --apply asks once, and an answer of no deletes nothing.
func TestPruneApplyAsksFirst(t *testing.T) {
	ws, deps := pruneWorkspace(t, t.TempDir(), "idle")
	later := time.Now().Add(60 * 24 * time.Hour)
	var asked string
	var out bytes.Buffer
	if err := runPrune(&out, []string{ws}, gitx.Git{}, nil, later, 14*24*time.Hour, true, func(q string) bool { asked = q; return false }); err != nil {
		t.Fatal(err)
	}
	if asked == "" || !exists(t, deps) {
		t.Fatalf("asked %q, node_modules there: %v; want a question and nothing deleted", asked, exists(t, deps))
	}
}

// Work can start while the prompt waits: each workspace is inspected again
// just before deletion, and one held by then is left alone.
func TestPruneChecksAgainBeforeDeleting(t *testing.T) {
	ws, deps := pruneWorkspace(t, t.TempDir(), "idle")
	later := time.Now().Add(60 * 24 * time.Hour)
	live := &switchLive{}
	var out bytes.Buffer
	startWork := func(string) bool { live.name = "idle"; return true }
	if err := runPrune(&out, []string{ws}, gitx.Git{}, live, later, 14*24*time.Hour, true, startWork); err != nil {
		t.Fatal(err)
	}
	if !exists(t, deps) {
		t.Fatalf("deleted from a workspace a session started in while the prompt waited:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "held since the listing") {
		t.Errorf("output does not say why it was left:\n%s", out.String())
	}
}

// One workspace that cannot be read does not stop the rest.
func TestPruneCarriesOnPastAnUnreadableWorkspace(t *testing.T) {
	root := t.TempDir()
	ws, deps := pruneWorkspace(t, root, "idle")
	broken := filepath.Join(root, "broken")
	if err := os.MkdirAll(broken, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifest.Path(broken), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(60 * 24 * time.Hour)
	var out bytes.Buffer
	if err := runPrune(&out, []string{broken, ws}, gitx.Git{}, nil, later, 14*24*time.Hour, true, yesAll); err != nil {
		t.Fatal(err)
	}
	if exists(t, deps) || !strings.Contains(out.String(), "could not check broken") {
		t.Fatalf("want broken named and idle pruned:\n%s", out.String())
	}
}

// switchLive reports a session live once name is set.
type switchLive struct{ name string }

func (s *switchLive) Live(session, _ string) bool { return s.name != "" && session == s.name }

// A node_modules with no tracked package.json beside it has no install to come
// back from: --apply keeps it, and the listing says so even when nothing else
// in the workspace is named.
func TestPruneKeepsWhatNothingTrackedRebuildsAndSaysWhy(t *testing.T) {
	ws, deps := bareNodeModules(t, t.TempDir(), "idle")
	later := time.Now().Add(60 * 24 * time.Hour)
	var out bytes.Buffer
	if err := runPrune(&out, []string{ws}, gitx.Git{}, nil, later, 14*24*time.Hour, true, yesAll); err != nil {
		t.Fatal(err)
	}
	if !exists(t, deps) {
		t.Fatalf("--apply deleted a node_modules nothing tracked rebuilds:\n%s", out.String())
	}
	for _, want := range []string{"kept: repo/node_modules", "package.json", "nothing rebuilds it"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("listing lacks %q:\n%s", want, out.String())
		}
	}
}

// Prune holds a workspace when the probe for what runs from it fails: lsof
// missing from PATH, or failing with a warning, is not "nothing runs", and a
// process whose working directory is the node_modules keeps it (facet#303).
// The control subtest shows the same process holding it when lsof works.
func TestPruneHoldsWhenTheProcessProbeFails(t *testing.T) {
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git")
	}
	for _, tc := range []struct{ name, lsof string }{
		{name: "control: lsof works", lsof: "real"},
		{name: "lsof not on PATH"},
		{name: "lsof fails", lsof: "#!/bin/sh\necho 'lsof: WARNING: can't stat() file system' >&2\nexit 1\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TMUX", "")
			saved := jobs
			t.Cleanup(func() { jobs = saved })
			jobs = &launchd.Jobs{Dirs: []string{t.TempDir()}, Run: noTools}
			ws, deps := pruneWorkspace(t, t.TempDir(), "busy")
			bin := t.TempDir()
			if err := os.Symlink(gitPath, filepath.Join(bin, "git")); err != nil {
				t.Fatal(err)
			}
			switch tc.lsof {
			case "":
			case "real":
				if err := os.Symlink("/usr/sbin/lsof", filepath.Join(bin, "lsof")); err != nil {
					t.Fatal(err)
				}
			default:
				if err := os.WriteFile(filepath.Join(bin, "lsof"), []byte(tc.lsof), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			// A dev server, say, running from the dependencies prune would delete.
			server := exec.Command("/bin/sleep", "60")
			server.Dir = filepath.Join(deps, "pkg")
			if err := server.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = server.Process.Kill(); _ = server.Wait() })
			t.Setenv("PATH", bin)

			var out bytes.Buffer
			later := time.Now().Add(60 * 24 * time.Hour)
			_ = runPrune(&out, []string{ws}, gitx.Git{}, muxLive(), later, 14*24*time.Hour, true, yesAll)
			if !exists(t, deps) {
				t.Fatalf("prune deleted node_modules under a running process (pid %d):\n%s", server.Process.Pid, out.String())
			}
		})
	}
}
