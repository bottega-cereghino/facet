package gitx

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// RunTimeout exists for exactly one reason: a network operation (fetch) must
// degrade under a blackholed connection or captive portal, not hang the
// caller forever. A fake can model a fast failure, but not a real hang -- so
// this test proves the bound against a real, genuinely stalling git
// subprocess, using git's "ext" remote helper to run a script that sleeps
// far longer than the timeout. It is deterministic, not a race against a
// real network: the script's sleep duration is fixed and far exceeds the
// timeout given to RunTimeout.
func TestRunTimeoutKillsAHangingFetch(t *testing.T) {
	dir := t.TempDir()
	g := Git{}
	mustRun := func(args ...string) {
		t.Helper()
		if _, err := g.Run(dir, nil, args...); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}
	mustRun("init", "-q", "-b", "main")
	mustRun("config", "user.email", "t@example.com")
	mustRun("config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte("hi\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	mustRun("add", "-A")
	mustRun("commit", "-qm", "init")

	// A remote-ext helper that hangs far longer than the timeout below --
	// standing in for a blackholed connection or a captive portal, which no
	// fake Runner can reproduce.
	hang := filepath.Join(dir, "hang.sh")
	if err := os.WriteFile(hang, []byte("#!/bin/sh\nsleep 5\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	_, err := g.RunTimeout(dir, nil, 300*time.Millisecond,
		"-c", "protocol.ext.allow=always", "fetch", "ext::"+hang)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("RunTimeout returned no error for a fetch that never completes")
	}
	// Worst case is the 300ms context timeout plus gitx's waitDelay (the
	// grace period Wait() gives a killed process's orphaned grandchild to
	// close the inherited stdout/stderr pipe -- see gitx.go). Comfortably
	// under the script's 5s sleep: if this bound didn't hold, elapsed would
	// land at ~5s instead, which is exactly the bug this test exists to
	// catch (it did, on the first cut of this fix -- WaitDelay was missing).
	if elapsed > 4*time.Second {
		t.Fatalf("RunTimeout did not bound the hang: took %v against a 300ms timeout and a 5s hang", elapsed)
	}
}

func TestRunTimeoutSucceedsWithinBudget(t *testing.T) {
	dir := t.TempDir()
	g := Git{}
	if _, err := g.RunTimeout(dir, nil, 5*time.Second, "init", "-q", "-b", "main"); err != nil {
		t.Fatalf("RunTimeout: %v", err)
	}
	if !IsRepo(dir) {
		t.Error("RunTimeout did not run the command")
	}
}

// Leading whitespace is data: in `status --porcelain` it is the status column.
// Trimming it once cut the first character off a path and hid a modified file
// from reap.
func TestRunKeepsLeadingWhitespace(t *testing.T) {
	dir := t.TempDir()
	g := Git{}
	for _, args := range [][]string{
		{"init", "-q"},
		{"-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "x"},
	} {
		if _, err := g.Run(dir, nil, args...); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "a"), []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Run(dir, nil, "add", "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Run(dir, nil, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "-m", "a"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a"), []byte("2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := g.Run(dir, nil, "status", "--porcelain=v1", "-z")
	if err != nil {
		t.Fatal(err)
	}
	if out != " M a\x00" && out != " M a" {
		t.Errorf("status = %q, want the status column kept: \" M a\"", out)
	}
}

// A manifest URL is user-authored. One that begins with `--` must reach git as
// a repository, never as an option: `--upload-pack=<command>` would run the
// command. With the `--` in Clone, git looks for a repository by that name and
// finds none; without it, git reads the URL as an option and dst as the source.
func TestCloneTakesAURLStartingWithDashesAsARepository(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "ran")
	src := "--upload-pack=touch " + marker + "; git-upload-pack"
	err := Clone(Git{}, src, filepath.Join(dir, "dst"), CloneOptions{})
	if err == nil {
		t.Fatal("Clone succeeded from a URL that names no repository")
	}
	if want := "repository '" + src + "' does not exist"; !strings.Contains(err.Error(), want) {
		t.Errorf("git did not read the URL as a repository\nwant an error containing %q\ngot %v", want, err)
	}
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Errorf("the URL ran as a command: %s exists", marker)
	}
}

// environ drops what points git at another repository, index, config or
// pathspec mode, keeps the rest, and lets a caller's entries come last.
func TestEnvironDropsRepositoryVariables(t *testing.T) {
	base := []string{
		"HOME=/h", "GIT_DIR=/x/.git", "GIT_WORK_TREE=/x", "GIT_INDEX_FILE=/i",
		"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=core.worktree", "GIT_CONFIG_VALUE_0=/x",
		"GIT_GLOB_PATHSPECS=1", "GIT_SSH_COMMAND=ssh -i k", "GIT_ASKPASS=/a",
	}
	got := environ(base, []string{"GIT_LFS_SKIP_SMUDGE=1"})
	want := []string{
		"HOME=/h", "GIT_SSH_COMMAND=ssh -i k", "GIT_ASKPASS=/a",
		"GIT_NO_REPLACE_OBJECTS=1", "GIT_LITERAL_PATHSPECS=1", "GIT_LFS_SKIP_SMUDGE=1",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("environ =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// A real git run under an inherited GIT_DIR still reads the repository it was
// run in.
func TestRunIgnoresAnInheritedGitDir(t *testing.T) {
	g := Git{}
	here, other := t.TempDir(), t.TempDir()
	for _, d := range []string{here, other} {
		if _, err := g.Run(d, nil, "init", "-q", "-b", "main"); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("GIT_DIR", filepath.Join(other, ".git"))
	got, err := g.Run(here, nil, "rev-parse", "--absolute-git-dir")
	if err != nil {
		t.Fatal(err)
	}
	want, _ := filepath.EvalSymlinks(filepath.Join(here, ".git"))
	if got != want {
		t.Fatalf("git dir = %s, want %s", got, want)
	}
}
