package gitx

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// git that a signal ends has no exit status. The error must say "signal:
// killed", not "exit status -1".
func TestRunNamesTheSignalThatKilledGit(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte("#!/bin/sh\nkill -9 $$\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":/bin:/usr/bin")
	_, err := Git{}.Run("", nil, "rev-parse", "--verify", "HEAD")
	if err == nil {
		t.Fatal("a killed git read as success")
	}
	if !strings.Contains(err.Error(), "signal: killed") || strings.Contains(err.Error(), "exit status -1") {
		t.Errorf("err = %q, want the signal named and no exit status -1", err)
	}
}

// git that never starts has no exit status either.
func TestRunNamesWhyGitDidNotStart(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	_, err := Git{}.Run("", nil, "rev-parse", "--verify", "HEAD")
	if err == nil || !strings.Contains(err.Error(), "git did not finish:") || strings.Contains(err.Error(), "exit status -1") {
		t.Errorf("err = %v, want the start failure named", err)
	}
}

// Control: a git that exits with a status of its own keeps saying so.
func TestRunKeepsTheExitStatusOfAGitThatExited(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte("#!/bin/sh\nexit 3\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":/bin:/usr/bin")
	_, err := Git{}.Run("", nil, "status")
	if err == nil || !strings.Contains(err.Error(), "exit status 3") || strings.Contains(err.Error(), "did not finish") {
		t.Errorf("err = %v, want exit status 3", err)
	}
}
