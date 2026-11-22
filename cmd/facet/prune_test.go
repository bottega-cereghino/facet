package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	if !exists(t, busyDeps) {
		t.Error("--apply deleted from a workspace a session runs in")
	}
	if !exists(t, filepath.Join(idleWS, "repo", "only-copy-of-notes.md")) {
		t.Error("--apply deleted an untracked file that git does not ignore")
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
