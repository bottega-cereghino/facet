package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bottega-cereghino/facet/internal/ghx"
	"github.com/bottega-cereghino/facet/internal/gitx"
	"github.com/bottega-cereghino/facet/internal/gitx/gitxtest"
	"github.com/bottega-cereghino/facet/internal/launchd"
	"github.com/bottega-cereghino/facet/internal/manifest"
	"github.com/bottega-cereghino/facet/internal/mux"
)

// runReapCmd runs `facet reap` with args and returns what it printed. Stdin
// is closed, as it is for a scheduled job: a prompt would read no answer.
func runReapCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = devnull.Close() }()
	saved := os.Stdin
	os.Stdin = devnull
	defer func() { os.Stdin = saved }()
	var runErr error
	out := captureStdout(t, func() {
		cmd := newReapCmd()
		cmd.SetArgs(args)
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		runErr = cmd.Execute()
	})
	return out, runErr
}

// pushedClone adds a long-lived workspace to root with one clone of a local
// origin, everything in it pushed, and returns the workspace and the clone.
func pushedClone(t *testing.T, root, name string) (ws, clone string) {
	t.Helper()
	origin := filepath.Join(t.TempDir(), "origin.git")
	gitIn(t, "", "init", "-q", "--bare", "-b", "main", origin)
	seed := filepath.Join(t.TempDir(), "seed")
	gitIn(t, "", "clone", "-q", origin, seed)
	putFile(t, filepath.Join(seed, "README"), "seed\n")
	gitIn(t, seed, "add", "README")
	gitIn(t, seed, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "-m", "init")
	gitIn(t, seed, "push", "-q", "origin", "HEAD:main")
	ws = filepath.Join(root, name)
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := (&manifest.Manifest{Name: name, Clones: map[string]string{"app": origin}}).Write(ws); err != nil {
		t.Fatal(err)
	}
	clone = filepath.Join(ws, "app")
	gitIn(t, "", "clone", "-q", origin, clone)
	return ws, clone
}

// --safe is reap --reapable without the prompt, so every hold reap knows
// keeps a workspace through it: each case plants one and runs the command as
// a scheduled job would, with stdin closed. The free workspace beside it is
// deleted, so a pass is not a sweep that did nothing.
func TestReapSafeKeepsEveryHeldWorkspace(t *testing.T) {
	cases := []struct {
		name string
		// plant makes the held workspace in root and returns it and the
		// path in it that must survive.
		plant func(t *testing.T, root string) (ws, keep string)
		// allHeld: the hold is machine-wide, so the free workspace stays too.
		allHeld bool
	}{
		{name: "the only copy of a file", plant: func(_ *testing.T, root string) (string, string) {
			held := filepath.Join(root, "facet-test-reapable-held")
			return held, filepath.Join(held, "plan.md")
		}},
		{name: "an unpushed commit", plant: func(t *testing.T, root string) (string, string) {
			ws, clone := pushedClone(t, root, "facet-test-unpushed")
			gitIn(t, clone, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "--allow-empty", "-m", "local")
			return ws, clone
		}},
		{name: "an uncommitted change", plant: func(t *testing.T, root string) (string, string) {
			ws, clone := pushedClone(t, root, "facet-test-dirty")
			putFile(t, filepath.Join(clone, "README"), "edited\n")
			return ws, clone
		}},
		{name: "a stash", plant: func(t *testing.T, root string) (string, string) {
			ws, clone := pushedClone(t, root, "facet-test-stash")
			putFile(t, filepath.Join(clone, "README"), "edited\n")
			gitIn(t, clone, "-c", "user.name=t", "-c", "user.email=t@example.com", "stash", "-q")
			return ws, clone
		}},
		{name: "an open pull request", plant: func(t *testing.T, root string) (string, string) {
			dir, branch := issueWorkspace(t, root, 2)
			gh = &prGH{fakeGH: &fakeGH{}, prs: map[string]*ghx.PR{branch: {Number: 11, State: "OPEN"}}}
			return dir, dir
		}},
		{name: "a pull request lookup that fails", plant: func(t *testing.T, root string) (string, string) {
			dir, branch := issueWorkspace(t, root, 3)
			gh = &prGH{fakeGH: &fakeGH{}, errs: map[string]error{branch: errors.New("HTTP 502")}}
			return dir, dir
		}},
		{name: "a remote it cannot fetch", plant: func(t *testing.T, root string) (string, string) {
			ws, clone := pushedClone(t, root, "facet-test-stale")
			git = &gitxtest.Runner{
				Real: gitx.Git{},
				Fail: func(args []string) bool { return len(args) > 0 && args[0] == "fetch" },
				Err:  errors.New("could not resolve host"),
			}
			return ws, clone
		}},
		{name: "a process running in it", plant: func(t *testing.T, root string) (string, string) {
			if _, err := exec.LookPath("lsof"); err != nil {
				t.Skip("lsof not installed: the process probe cannot run here")
			}
			dir := filepath.Join(root, "facet-test-busy")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := (&manifest.Manifest{Name: "busy"}).Write(dir); err != nil {
				t.Fatal(err)
			}
			sleeper := exec.Command("sleep", "30")
			sleeper.Dir = dir
			if err := sleeper.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = sleeper.Process.Kill(); _ = sleeper.Wait() })
			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				if procs, _ := mux.ProcessesUnder(dir); len(procs) > 0 {
					break
				}
				time.Sleep(100 * time.Millisecond)
			}
			return dir, dir
		}},
		{name: "a loaded launchd job running from it", plant: func(t *testing.T, root string) (string, string) {
			dir := filepath.Join(root, "facet-test-job")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := (&manifest.Manifest{Name: "job"}).Write(dir); err != nil {
				t.Fatal(err)
			}
			agents := t.TempDir()
			putFile(t, filepath.Join(agents, "com.example.svc.plist"), `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict>
<key>Label</key><string>com.example.svc</string>
<key>WorkingDirectory</key><string>`+dir+`</string>
</dict></plist>
`)
			jobs = &launchd.Jobs{Dirs: []string{agents}, Run: func(name string, args ...string) ([]byte, error) {
				if name == "launchctl" {
					return []byte("PID\tStatus\tLabel\n-\t0\tcom.example.svc\n"), nil
				}
				return os.ReadFile(args[len(args)-1])
			}}
			return dir, dir
		}},
		{name: "launchd jobs it cannot read", allHeld: true, plant: func(t *testing.T, root string) (string, string) {
			jobs = &launchd.Jobs{Dirs: []string{t.TempDir()}, Run: func(string, ...string) ([]byte, error) {
				return nil, errors.New("exit status 5")
			}}
			free := filepath.Join(root, "facet-test-reapable-free")
			return free, free
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root, free, _ := reapableRoot(t)
			ws, keep := c.plant(t, root)
			out, err := runReapCmd(t, "--safe")
			if err != nil {
				t.Fatalf("reap --safe: %v\n%s", err, out)
			}
			if !exists(t, keep) {
				t.Fatalf("reap --safe deleted %s, held by %s:\n%s", keep, c.name, out)
			}
			if strings.Contains(out, "removed "+ws+" (") {
				t.Errorf("output says the held workspace was removed:\n%s", out)
			}
			if !c.allHeld && exists(t, free) {
				t.Errorf("reap --safe left the free workspace; the sweep did not run:\n%s", out)
			}
		})
	}
}

// --safe deletes without asking, and logs each workspace it removed by its
// path with the disk it freed.
func TestReapSafeCommandReapsOnlyWhatIsReapableWithoutPrompt(t *testing.T) {
	_, free, held := reapableRoot(t)
	out, err := runReapCmd(t, "--safe")
	if err != nil {
		t.Fatalf("reap --safe: %v\n%s", err, out)
	}
	if exists(t, free) || !exists(t, filepath.Join(held, "plan.md")) {
		t.Fatalf("--safe must delete the reapable workspace and keep the held one:\n%s", out)
	}
	if want := "removed " + free + " ("; !strings.Contains(out, want) {
		t.Errorf("output lacks %q, the removed workspace with its size:\n%s", want, out)
	}
	if strings.Contains(out, "aborted") {
		t.Errorf("--safe asked:\n%s", out)
	}
}

// --safe takes no flag that widens it: no --force, and no named workspace,
// which plain reap may delete with --force. It refuses before reading the
// root, so nothing is deleted, the free workspace included.
func TestReapSafeRejectsNamedAndForcedTargets(t *testing.T) {
	_, free, held := reapableRoot(t)
	for _, args := range [][]string{
		{"--safe", "--force"},
		{"--safe", filepath.Base(held)},
		{"--safe", "--path", held},
		{"--safe", "--force", filepath.Base(held)},
	} {
		out, err := runReapCmd(t, args...)
		if err == nil || !strings.Contains(err.Error(), "--safe") {
			t.Errorf("reap %v: err = %v; want the --safe refusal\n%s", args, err, out)
		}
		if !exists(t, free) || !exists(t, filepath.Join(held, "plan.md")) {
			t.Fatalf("reap %v deleted something:\n%s", args, out)
		}
	}
}

// --safe --dry-run lists and deletes nothing.
func TestReapSafeDryRunDeletesNothing(t *testing.T) {
	_, free, _ := reapableRoot(t)
	out, err := runReapCmd(t, "--safe", "--dry-run")
	if err != nil {
		t.Fatalf("reap --safe --dry-run: %v\n%s", err, out)
	}
	if !exists(t, free) || !strings.Contains(out, "dry run: nothing was deleted") {
		t.Fatalf("--safe --dry-run deleted or did not say so:\n%s", out)
	}
}
