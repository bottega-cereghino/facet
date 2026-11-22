package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// realPathOf is p with its symlinks resolved, as tmux reports a path.
func realPathOf(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// shortTmux points tmux at a server of its own under /tmp: macOS caps a
// socket's path near 104 bytes, which t.TempDir() can pass.
func shortTmux(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	dir, err := os.MkdirTemp("/tmp", "fts")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMUX_TMPDIR", dir)
	t.Setenv("TMUX", "")
	t.Cleanup(func() {
		// Still under this test's TMUX_TMPDIR: cleanups run before Setenv restores.
		_ = exec.Command("tmux", "kill-server").Run()
		_ = os.RemoveAll(dir)
	})
}

func tmuxSession(t *testing.T, name, dir string) (alive func() bool) {
	t.Helper()
	if out, err := exec.Command("tmux", "new-session", "-d", "-s", name, "-c", dir, "sleep 300").CombinedOutput(); err != nil {
		t.Fatalf("new-session: %v\n%s", err, out)
	}
	alive = func() bool { return exec.Command("tmux", "has-session", "-t", "="+name).Run() == nil }
	if !alive() {
		t.Fatalf("session %s did not start", name)
	}
	return alive
}

func reapCmd(args ...string) error {
	cmd := newReapCmd()
	cmd.SetArgs(args)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	return cmd.Execute()
}

// A tmux session that shares the workspace's name but runs nothing in it is
// someone else's: it neither holds the reap nor dies with it (#330).
func TestReapLeavesAnUnrelatedSessionOfTheSameNameAlone(t *testing.T) {
	root, free, _ := reapableRoot(t)
	shortTmux(t)
	ws := filepath.Join(root, "api")
	if err := os.Rename(free, ws); err != nil {
		t.Fatal(err)
	}
	alive := tmuxSession(t, "api", t.TempDir())

	if err := reapCmd("api", "--yes", "--force"); err != nil {
		t.Fatalf("reap: %v", err)
	}
	if !alive() {
		t.Error("reap --force killed the tmux session api, whose only pane runs outside the workspace")
	}
	if exists(t, ws) {
		t.Errorf("reap left %s, which holds nothing", ws)
	}

	ws2 := filepath.Join(root, "facet-test-reapable-held")
	if err := os.Remove(filepath.Join(ws2, "plan.md")); err != nil {
		t.Fatal(err)
	}
	tmuxSession(t, filepath.Base(ws2), t.TempDir())
	if err := reapCmd(filepath.Base(ws2), "--yes"); err != nil {
		t.Errorf("reap refused %s, which holds nothing, because an unrelated session shares its name: %v", ws2, err)
	}
}

// A session of the workspace's name with a pane inside it holds the reap, and
// --force ends it before the delete.
func TestReapForceEndsTheWorkspacesSession(t *testing.T) {
	root, free, _ := reapableRoot(t)
	shortTmux(t)
	ws := filepath.Join(root, "api")
	if err := os.Rename(free, ws); err != nil {
		t.Fatal(err)
	}
	alive := tmuxSession(t, "api", ws)
	// tmux reports a new pane's path only once its process has started (on
	// Linux, from the pty's foreground process). Wait for that, not for a
	// refusal: a reap that does not refuse deletes the workspace.
	want := realPathOf(t, ws)
	deadline := time.Now().Add(5 * time.Second)
	for {
		out, _ := exec.Command("tmux", "list-panes", "-t", "=api", "-F", "#{pane_current_path}").Output()
		if strings.TrimSpace(string(out)) == want {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("tmux never reported the pane at %s (last: %q)", want, out)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := reapCmd("api", "--yes"); err == nil {
		t.Fatal("reap did not refuse a workspace a session of its name runs in")
	}
	if err := reapCmd("api", "--yes", "--force"); err != nil {
		t.Fatalf("reap --force: %v", err)
	}
	if alive() {
		t.Error("reap --force left the workspace's own session running")
	}
	if exists(t, ws) {
		t.Errorf("reap --force left %s", ws)
	}
}
