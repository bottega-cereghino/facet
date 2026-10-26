package mux

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
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

// A process whose working directory is elsewhere but that holds a file inside
// dir open -- a watcher tailing a log -- holds the workspace just the same, and
// the working-directory match alone missed it (facet#171).
func TestLsofProcessesUnderFindsAnOpenFileHolder(t *testing.T) {
	if _, err := exec.LookPath("lsof"); err != nil {
		t.Skip("lsof not installed")
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "log")
	if err := os.WriteFile(file, nil, 0o666); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-c", `exec 3<"$1"; exec sleep 30`, "sh", file)
	cmd.Dir = t.TempDir()
	if err := cmd.Start(); err != nil {
		t.Fatalf("start holder: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	pid := strconv.Itoa(cmd.Process.Pid)
	deadline := time.Now().Add(5 * time.Second)
	for {
		for _, l := range lsofProcessesUnder(dir) {
			if strings.Contains(l, "pid "+pid+")") && strings.Contains(l, "log open") {
				return
			}
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("lsofProcessesUnder(%q) = %v; want pid %s holding log open", dir, lsofProcessesUnder(dir), pid)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// One description per process, however many of its files match: the working
// directory when it is among them, otherwise the first file and a count.
func TestParseLsofDescribesEachProcessOnce(t *testing.T) {
	raw := "p10\ncsleep\nfcwd\nn/ws/a\np11\ncpython3\nf3\nn/ws/a/log\nf4\nn/ws/b/log\np12\ncwatch\nftxt\nn/ws/bin/watch\n"
	var got []string
	for _, p := range parseLsof(raw) {
		got = append(got, p.describe())
	}
	want := []string{
		"process sleep (pid 10) at /ws/a",
		"process python3 (pid 11) has /ws/a/log and 1 more file(s) open",
		"process watch (pid 12) has /ws/bin/watch open",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseLsof =\n%q\nwant\n%q", got, want)
	}
}

// A shell is not a system service and neither is a pid that does not exist:
// only an executable under /System is left out, and doubt holds the workspace.
func TestIsSystemService(t *testing.T) {
	if _, err := exec.LookPath("ps"); err != nil {
		t.Skip("ps not installed")
	}
	if isSystemService(strconv.Itoa(os.Getpid())) {
		t.Error("the test binary was taken for a system service")
	}
	if isSystemService("999999999") {
		t.Error("a pid that does not exist was taken for a system service")
	}
	out, err := exec.Command("ps", "-axo", "pid=,comm=").Output()
	if err != nil {
		t.Skipf("ps -ax: %v", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if pid, comm, ok := strings.Cut(strings.TrimSpace(line), " "); ok && strings.HasPrefix(strings.TrimSpace(comm), "/System/") {
			if !isSystemService(pid) {
				t.Errorf("%s (pid %s) was not taken for a system service", strings.TrimSpace(comm), pid)
			}
			return
		}
	}
	t.Skip("no process runs from /System here (not macOS)")
}
