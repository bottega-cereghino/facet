package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bottega-cereghino/facet/internal/gitx"
	"github.com/bottega-cereghino/facet/internal/launchd"
)

// jobsLoadedOnCue is a launchd whose one job, running from dir, is loaded only
// once load has been called, as `launchctl load` during a prompt would.
func jobsLoadedOnCue(t *testing.T, dir string) (j *launchd.Jobs, load func()) {
	t.Helper()
	agents := t.TempDir()
	doc := `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict>
<key>Label</key><string>com.example.recheck</string>
<key>WorkingDirectory</key><string>` + dir + `</string>
</dict></plist>
`
	if err := os.WriteFile(filepath.Join(agents, "com.example.recheck.plist"), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	loaded := false
	j = &launchd.Jobs{Dirs: []string{agents}, Run: func(name string, args ...string) ([]byte, error) {
		if name == "launchctl" {
			if loaded {
				return []byte("PID\tStatus\tLabel\n-\t0\tcom.example.recheck\n"), nil
			}
			return []byte("PID\tStatus\tLabel\n"), nil
		}
		return os.ReadFile(args[len(args)-1]) // plutil -convert xml1 of an XML plist
	}}
	return j, func() { loaded = true }
}

// A launchd job loaded while reap's prompt waits holds the workspace, as work
// started then does: the jobs are read again for the check after the prompt
// (facet#324).
func TestReapRecheckSeesALaunchdJobLoadedDuringThePrompt(t *testing.T) {
	_, free, _ := reapableRoot(t)
	var load func()
	jobs, load = jobsLoadedOnCue(t, free)
	asked := false
	answer := func(string) bool { asked = true; load(); return true }
	err := runReap(t, answer, filepath.Base(free))
	if !asked {
		t.Fatalf("reap never asked, so the recheck was not reached (err=%v)", err)
	}
	if !exists(t, free) {
		t.Fatal("reap deleted a workspace a launchd job loaded during its prompt runs from")
	}
}

// The same for --reapable, which checks each workspace again right before
// deleting it.
func TestReapableRecheckSeesALaunchdJobLoadedDuringThePrompt(t *testing.T) {
	_, free, _ := reapableRoot(t)
	var load func()
	jobs, load = jobsLoadedOnCue(t, free)
	asked := false
	var out bytes.Buffer
	ask := func(string) bool { asked = true; load(); return true }
	if err := reapReapable(&out, []string{free}, false, false, ask); err != nil {
		t.Fatal(err)
	}
	if !asked {
		t.Fatalf("--reapable never asked, so the recheck was not reached:\n%s", out.String())
	}
	if !exists(t, free) {
		t.Fatalf("--reapable deleted a workspace a launchd job loaded during its prompt runs from:\n%s", out.String())
	}
}

// The same for prune, which inspects each workspace again before deleting.
func TestPruneRecheckSeesALaunchdJobLoadedDuringThePrompt(t *testing.T) {
	t.Setenv("TMUX_TMPDIR", t.TempDir())
	t.Setenv("TMUX", "")
	ws, deps := pruneWorkspace(t, t.TempDir(), "idle")
	saved := jobs
	t.Cleanup(func() { jobs = saved })
	var load func()
	jobs, load = jobsLoadedOnCue(t, ws)
	later := time.Now().Add(60 * 24 * time.Hour)
	asked := false
	var out bytes.Buffer
	ask := func(string) bool { asked = true; load(); return true }
	if err := runPrune(&out, []string{ws}, gitx.Git{}, muxLive(), later, 14*24*time.Hour, true, ask); err != nil {
		t.Fatal(err)
	}
	if !asked {
		t.Fatalf("prune never asked, so the recheck was not reached:\n%s", out.String())
	}
	if !exists(t, deps) {
		t.Fatalf("prune deleted from a workspace a launchd job loaded during its prompt runs from:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "held since the listing") {
		t.Errorf("output does not say why it was left:\n%s", out.String())
	}
}
