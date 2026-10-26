package mux

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestTmuxPanesUnderFindsPaneRootedInDir is the real-tmux integration test for
// the reap liveness check: a pane whose cwd is inside dir must be found, in
// any session, and one whose cwd is elsewhere must not.
func TestTmuxPanesUnderFindsPaneRootedInDir(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	socket := fmt.Sprintf("facet-liveroots-%d", os.Getpid())
	session := fmt.Sprintf("facet-liveroots-%d", os.Getpid())
	t.Cleanup(func() { _ = exec.Command("tmux", "-L", socket, "kill-server").Run() })

	dir := t.TempDir()
	if out, err := exec.Command("tmux", "-L", socket, "new-session", "-d", "-s", session, "-c", dir).CombinedOutput(); err != nil {
		t.Fatalf("new-session: %v\n%s", err, out)
	}

	live := tmuxPanesUnder(dir, socket)
	if len(live) != 1 {
		t.Fatalf("tmuxPanesUnder(%q) = %v; want exactly one pane", dir, live)
	}
	if !strings.Contains(live[0], session) {
		t.Errorf("description %q does not name the session, so an operator could not find it", live[0])
	}

	elsewhere := tmuxPanesUnder(t.TempDir(), socket)
	if len(elsewhere) != 0 {
		t.Errorf("tmuxPanesUnder(unrelated dir) = %v; want none", elsewhere)
	}
}

// TestLsofProcessesUnderFindsProcessRootedInDir covers the fallback probe: a
// process with no tmux pane at all (started outside any session) whose cwd is
// inside dir must still be found.
func TestLsofProcessesUnderFindsProcessRootedInDir(t *testing.T) {
	if _, err := exec.LookPath("lsof"); err != nil {
		t.Skip("lsof not installed")
	}
	dir := t.TempDir()
	cmd := exec.Command("sleep", "30")
	cmd.Dir = dir
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	deadline := time.Now().Add(5 * time.Second)
	var live []string
	for {
		live = lsofProcessesUnder(dir)
		if len(live) > 0 || !time.Now().Before(deadline) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if len(live) == 0 {
		t.Fatalf("lsofProcessesUnder(%q) found nothing; want the sleeping process", dir)
	}
	pid := strconv.Itoa(cmd.Process.Pid)
	found := false
	for _, l := range live {
		if strings.Contains(l, pid) {
			found = true
		}
	}
	if !found {
		t.Errorf("lsofProcessesUnder(%q) = %v; want an entry naming pid %s", dir, live, pid)
	}

	elsewhere := lsofProcessesUnder(t.TempDir())
	if len(elsewhere) != 0 {
		t.Errorf("lsofProcessesUnder(unrelated dir) = %v; want none", elsewhere)
	}
}
