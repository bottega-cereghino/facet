package mux

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func holderGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+filepath.Join(t.TempDir(), "gitconfig"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// startHolder starts sh with cwd in cwd, holding file open on fd 3, and waits
// until lsof sees it.
func startHolder(t *testing.T, cwd, file string, argv0 string) int {
	t.Helper()
	script := `exec 3<"$1"; exec sleep 30`
	if argv0 != "" {
		script = `exec 3<"$1"; exec -a "$2" sleep 30`
	}
	cmd := exec.Command("bash", "-c", script, "bash", file, argv0)
	cmd.Dir = cwd
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	pid := cmd.Process.Pid
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		out, _ := exec.Command("lsof", "-a", "-p", strconv.Itoa(pid), "-d", "3", "-Fn").Output()
		if strings.Contains(string(out), filepath.Base(file)) {
			if argv0 == "" {
				return pid
			}
			if comm, _ := exec.Command("ps", "-o", "comm=", "-p", strconv.Itoa(pid)).Output(); strings.TrimSpace(string(comm)) == argv0 {
				return pid
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("holder %d never showed %s open", pid, file)
	return 0
}

// lsof +D matches by device and inode, and a clone's objects are hardlinks
// shared with the mirror and sibling clones. A process in workspace B holding
// B's own pack is reported under A's path; deleting A's name for that pack
// breaks nothing, so it does not hold A (#331).
func TestLsofProcessesUnderSkipsAHardlinkedObjectHeldElsewhere(t *testing.T) {
	if _, err := exec.LookPath("lsof"); err != nil {
		t.Skip("lsof not installed")
	}
	root := t.TempDir()
	origin := filepath.Join(root, "mirror")
	if err := os.MkdirAll(origin, 0o755); err != nil {
		t.Fatal(err)
	}
	holderGit(t, origin, "init", "-q")
	if err := os.WriteFile(filepath.Join(origin, "f"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	holderGit(t, origin, "add", "f")
	holderGit(t, origin, "-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-qm", "c")
	holderGit(t, origin, "gc", "-q")
	a, b := filepath.Join(root, "ws-a", "repo"), filepath.Join(root, "ws-b", "repo")
	holderGit(t, root, "clone", "-q", "--local", origin, a)
	holderGit(t, root, "clone", "-q", "--local", origin, b)
	packs, _ := filepath.Glob(filepath.Join(b, ".git", "objects", "pack", "*.pack"))
	if len(packs) != 1 {
		t.Fatalf("packs in b = %v", packs)
	}
	pid := startHolder(t, filepath.Dir(b), packs[0], "")
	tag := "(pid " + strconv.Itoa(pid) + ")"

	for _, l := range mustLsof(t, filepath.Join(root, "ws-a")) {
		if strings.Contains(l, tag) {
			t.Fatalf("ws-a is held by %q, a process in ws-b holding ws-b's own pack (a hardlink of ws-a's)", l)
		}
	}
	// It still holds its own workspace, by its working directory.
	found := false
	for _, l := range mustLsof(t, filepath.Join(root, "ws-b")) {
		found = found || strings.Contains(l, tag)
	}
	if !found {
		t.Error("the process in ws-b no longer holds ws-b")
	}
}

// A process is let off as a system service by the executable it runs, never
// by the argv[0] it gives itself (#331).
func TestLsofProcessesUnderJudgesASystemServiceByItsExecutable(t *testing.T) {
	if _, err := exec.LookPath("lsof"); err != nil {
		t.Skip("lsof not installed")
	}
	ws := t.TempDir()
	log := filepath.Join(ws, "build.log")
	if err := os.WriteFile(log, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	pid := startHolder(t, t.TempDir(), log, "/System/Library/CoreServices/fake")
	for _, l := range mustLsof(t, ws) {
		if strings.Contains(l, "(pid "+strconv.Itoa(pid)+")") {
			return
		}
	}
	t.Fatalf("the sleep (pid %d, executable /bin/sleep, argv[0] under /System) with %s open was dropped as a system service", pid, log)
}

// An app Apple ships under /System/Applications (TextEdit, Preview) is the
// user's, not a service: one with a file in the workspace open holds it.
func TestSystemExecutableLeavesOutApps(t *testing.T) {
	cases := map[string]bool{
		"/System/Library/Frameworks/CoreServices.framework/Frameworks/Metadata.framework/Support/mds": true,
		"/System/Library/Frameworks/Virtualization.framework/XPCServices/x.xpc/Contents/MacOS/x":      true,
		"/System/Applications/TextEdit.app/Contents/MacOS/TextEdit":                                   false,
		"/System/Volumes/Data/Users/u/bin/tool":                                                       false,
		"/usr/bin/sleep":                                                                              false,
		"":                                                                                            false,
	}
	for p, want := range cases {
		if got := systemExecutable(p); got != want {
			t.Errorf("systemExecutable(%q) = %v, want %v", p, got, want)
		}
	}
}
