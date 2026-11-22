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

	// new-session -d returns before tmux can always report the pane's current
	// path (on Linux it reads the pty's foreground process, which the shell
	// may not have become yet), so poll the way the lsof test does.
	var live []string
	deadline := time.Now().Add(5 * time.Second)
	for {
		live = tmuxPanesUnder(dir, socket)
		if len(live) > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
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

// mustLsof is lsofProcessesUnder, failing the test on a probe error.
func mustLsof(t *testing.T, dir string) []string {
	t.Helper()
	live, err := lsofProcessesUnder(dir)
	if err != nil {
		t.Fatalf("lsofProcessesUnder(%q): %v", dir, err)
	}
	return live
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
		live = mustLsof(t, dir)
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

	elsewhere := mustLsof(t, t.TempDir())
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
		for _, l := range mustLsof(t, dir) {
			if strings.Contains(l, "pid "+pid+")") && strings.Contains(l, "log open") {
				return
			}
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("lsofProcessesUnder(%q) = %v; want pid %s holding log open", dir, mustLsof(t, dir), pid)
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
// only an executable under /System, outside its apps, is left out, and doubt
// holds the workspace.
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
	// Only the user's own processes: the kernel gives no other user's
	// executable, and lsof sees only the user's processes anyway.
	out, err := exec.Command("ps", "-U", strconv.Itoa(os.Getuid()), "-o", "pid=,comm=").Output()
	if err != nil {
		t.Skipf("ps -U: %v", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if pid, comm, ok := strings.Cut(strings.TrimSpace(line), " "); ok && systemExecutable(strings.TrimSpace(comm)) {
			if !isSystemService(pid) {
				t.Errorf("%s (pid %s) was not taken for a system service", strings.TrimSpace(comm), pid)
			}
			return
		}
	}
	t.Skip("no process runs from /System here (not macOS)")
}

// `facet reap` run from inside the workspace is its documented form. Its own
// process, the lsof it runs and its shell must not hold the workspace; any other
// process there still does.
func TestLsofProcessesUnderLeavesOutFacetItself(t *testing.T) {
	if _, err := exec.LookPath("lsof"); err != nil {
		t.Skip("lsof not installed")
	}
	dir := t.TempDir()
	t.Chdir(dir)
	self := strconv.Itoa(os.Getpid())
	for _, l := range mustLsof(t, dir) {
		if strings.Contains(l, "(pid "+self+")") || strings.Contains(l, "process lsof ") {
			t.Errorf("held by facet's own process or its probe: %q", l)
		}
	}
}

func TestWithoutSelf(t *testing.T) {
	procs := []lsofProc{
		{pid: "10", ppid: "9", cmd: "zsh"},     // the shell that ran facet
		{pid: "11", ppid: "10", cmd: "facet"},  // facet
		{pid: "12", ppid: "11", cmd: "lsof"},   // its probe
		{pid: "13", ppid: "12", cmd: "lsof"},   // the probe's helper
		{pid: "14", ppid: "11", cmd: "sleep"},  // something else facet started
		{pid: "15", ppid: "10", cmd: "vim"},    // the user's editor in that shell
		{pid: "16", ppid: "1", cmd: "watcher"}, // unrelated
	}
	var got []string
	for _, p := range withoutSelf(procs, 11, 10) {
		got = append(got, p.pid)
	}
	if strings.Join(got, ",") != "14,15,16" {
		t.Errorf("withoutSelf kept %v, want 14,15,16", got)
	}
}

// lsof ships with macOS, so lsof missing or failing is an error, never "no
// processes": a directory it could not read reads exactly like one nothing
// runs from (facet#303). Only exit 1 with nothing on stdout or stderr is no
// match.
func TestLsofProcessesUnderFailsWhenLsofFails(t *testing.T) {
	for _, tc := range []struct {
		name, script string
		wantErr      bool
	}{
		{name: "lsof not on PATH", wantErr: true},
		{name: "lsof warns and exits 1", script: "#!/bin/sh\necho 'lsof: WARNING: can'\\''t stat() file system' >&2\nexit 1\n", wantErr: true},
		{name: "lsof exits 2", script: "#!/bin/sh\nexit 2\n", wantErr: true},
		{name: "lsof matches nothing", script: "#!/bin/sh\nexit 1\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin := t.TempDir()
			if tc.script != "" {
				if err := os.WriteFile(filepath.Join(bin, "lsof"), []byte(tc.script), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("PATH", bin)
			live, err := lsofProcessesUnder(t.TempDir())
			if (err != nil) != tc.wantErr {
				t.Fatalf("lsofProcessesUnder: err = %v, want an error: %v", err, tc.wantErr)
			}
			if len(live) != 0 {
				t.Fatalf("lsofProcessesUnder = %v, want none", live)
			}
		})
	}
}
