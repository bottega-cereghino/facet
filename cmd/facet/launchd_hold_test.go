package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bottega-cereghino/facet/internal/config"
	"github.com/bottega-cereghino/facet/internal/gitx"
	"github.com/bottega-cereghino/facet/internal/launchd"
	"github.com/bottega-cereghino/facet/internal/manifest"
	"github.com/bottega-cereghino/facet/internal/workspace"
)

// A loaded job whose working directory is a workspace link to a project
// elsewhere still runs from the workspace. Following the link would put the
// job outside, and reap would delete the directory it starts in.
func TestGrillReapHoldsAJobRunningThroughAWorkspaceLink(t *testing.T) {
	_, free, _ := reapableRoot(t)
	proj := filepath.Join(filepath.Dir(free), "projects", "proj")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(free, "proj")
	if err := os.Symlink(proj, link); err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(link)
	if err != nil {
		t.Fatal(err)
	}
	if resolved == link || strings.HasPrefix(resolved, free+string(filepath.Separator)) {
		t.Fatal("the workspace link did not point outside, so this would pass without the fix")
	}
	if err := (&manifest.Manifest{Name: filepath.Base(free), Links: map[string]string{"proj": "proj"}}).Write(free); err != nil {
		t.Fatal(err)
	}
	agents := t.TempDir()
	doc := `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict>
<key>Label</key><string>com.example.link</string>
<key>WorkingDirectory</key><string>` + link + `</string>
</dict></plist>
`
	if err := os.WriteFile(filepath.Join(agents, "com.example.link.plist"), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	jobs = &launchd.Jobs{Dirs: []string{agents}, Run: func(name string, args ...string) ([]byte, error) {
		if name == "launchctl" {
			return []byte("PID\tStatus\tLabel\n-\t0\tcom.example.link\n"), nil
		}
		return os.ReadFile(args[len(args)-1])
	}}
	var out bytes.Buffer
	cmd := newReapCmd()
	cmd.SetArgs([]string{filepath.Base(free), "--yes"})
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	err = cmd.Execute()
	if err == nil || !exists(t, free) {
		t.Fatalf("reap of a workspace a job runs through a link: err=%v, still there=%v\n%s", err, exists(t, free), out.String())
	}
	st, err := workspace.Inspect(free, git, gh, muxLive())
	if err != nil {
		t.Fatal(err)
	}
	b := strings.Join(st.Blockers(), "\n")
	if !strings.Contains(b, "launchd job com.example.link runs from it (WorkingDirectory "+link) {
		t.Fatalf("the hold should name the job and the link:\n%s", b)
	}
}

// facet reap SVC reaches a workspace named svc. The job whose plist names
// svc must still hold it where the disk folds case.
func TestGrillReapByACaseVariantNameKeepsTheLaunchdHold(t *testing.T) {
	t.Setenv("TMUX_TMPDIR", t.TempDir())
	t.Setenv("TMUX", "")
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ws := filepath.Join(root, "svc")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "SVC")); err != nil {
		t.Skip("needs a case-insensitive disk (the macOS job runs it)")
	}
	if err := (&manifest.Manifest{Name: "svc"}).Write(ws); err != nil {
		t.Fatal(err)
	}
	savedRoots, savedGit, savedGH, savedJobs := roots, git, gh, jobs
	t.Cleanup(func() { roots, git, gh, jobs = savedRoots, savedGit, savedGH, savedJobs })
	roots = config.Roots{Workspaces: root}
	git = gitx.Git{}
	agents := t.TempDir()
	doc := `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict>
<key>Label</key><string>com.example.svc</string>
<key>WorkingDirectory</key><string>` + ws + `</string>
</dict></plist>
`
	if err := os.WriteFile(filepath.Join(agents, "com.example.svc.plist"), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	jobs = &launchd.Jobs{Dirs: []string{agents}, Run: func(name string, args ...string) ([]byte, error) {
		if name == "launchctl" {
			return []byte("PID\tStatus\tLabel\n-\t0\tcom.example.svc\n"), nil
		}
		return os.ReadFile(args[len(args)-1])
	}}
	var out bytes.Buffer
	cmd := newReapCmd()
	cmd.SetArgs([]string{"SVC", "--yes"})
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	err = cmd.Execute()
	if err == nil || !exists(t, ws) {
		t.Fatalf("reap SVC deleted svc, or returned no error: %v\n%s", err, out.String())
	}
	st, err := workspace.Inspect(ws, git, gh, muxLive())
	if err != nil {
		t.Fatal(err)
	}
	b := strings.Join(st.Blockers(), "\n")
	if !strings.Contains(b, "launchd job com.example.svc runs from it (WorkingDirectory "+ws) {
		t.Fatalf("the hold should name the job at the other spelling:\n%s", b)
	}
}

// prune deletes a .venv a command rebuilds. It must not, when a loaded job's
// program is the venv's python and that python is a symlink to an interpreter
// outside the workspace.
func TestGrillPruneHoldsAVenvALoadedJobRuns(t *testing.T) {
	t.Setenv("TMUX_TMPDIR", t.TempDir())
	t.Setenv("TMUX", "")
	root := t.TempDir()
	ws := filepath.Join(root, "idle")
	clone := filepath.Join(ws, "repo")
	venv := filepath.Join(clone, ".venv")
	if err := os.MkdirAll(filepath.Join(venv, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(root, "python-real")
	if err := os.WriteFile(outside, []byte("interpreter\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	python := filepath.Join(venv, "bin", "python")
	if err := os.Symlink(outside, python); err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(python)
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(resolved, ws+string(filepath.Separator)) {
		t.Fatal("python did not point outside the workspace, so this would pass without the fix")
	}
	if err := os.MkdirAll(filepath.Join(venv, "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(venv, "lib", "site.py"), bytes.Repeat([]byte("facet\n"), (1<<20)/6+1), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := (&manifest.Manifest{Name: "idle", Clones: map[string]string{"repo": "git@github.com:acme/repo.git"}}).Write(ws); err != nil {
		t.Fatal(err)
	}
	if _, err := (gitx.Git{}).Run(clone, nil, "init", "-q"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(clone, ".gitignore"), []byte(".venv/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(clone, "pyproject.toml"), []byte("[project]\nname = \"app\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := (gitx.Git{}).Run(clone, nil, "add", "pyproject.toml"); err != nil {
		t.Fatal(err)
	}
	agents := t.TempDir()
	doc := `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict>
<key>Label</key><string>com.example.venv</string>
<key>Program</key><string>` + python + `</string>
</dict></plist>
`
	if err := os.WriteFile(filepath.Join(agents, "com.example.venv.plist"), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	saved := jobs
	t.Cleanup(func() { jobs = saved })
	jobs = &launchd.Jobs{Dirs: []string{agents}, Run: func(name string, args ...string) ([]byte, error) {
		if name == "launchctl" {
			return []byte("PID\tStatus\tLabel\n-\t0\tcom.example.venv\n"), nil
		}
		return os.ReadFile(args[len(args)-1])
	}}
	var out bytes.Buffer
	later := time.Now().Add(60 * 24 * time.Hour)
	if err := runPrune(&out, []string{ws}, gitx.Git{}, muxLive(), later, 14*24*time.Hour, true, yesAll); err != nil {
		t.Fatal(err)
	}
	if !exists(t, venv) {
		t.Fatalf("prune deleted a .venv a loaded job runs:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "launchd job com.example.venv runs from it (Program "+python) {
		t.Fatalf("the hold should name the job and the venv python:\n%s", out.String())
	}
}
