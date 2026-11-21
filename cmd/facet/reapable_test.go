package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bottega-cereghino/facet/internal/config"
	"github.com/bottega-cereghino/facet/internal/ghx"
	"github.com/bottega-cereghino/facet/internal/gitx"
	"github.com/bottega-cereghino/facet/internal/launchd"
	"github.com/bottega-cereghino/facet/internal/manifest"
	"github.com/bottega-cereghino/facet/internal/workspace"
)

// reapableRoot makes a workspaces root with two long-lived workspaces: one that
// holds nothing of its own, and one holding the only copy of a file. It points
// facet at that root and tmux at a server of its own, because reap kills the
// session named after each workspace it deletes.
func reapableRoot(t *testing.T) (root, free, held string) {
	t.Helper()
	t.Setenv("TMUX_TMPDIR", t.TempDir())
	t.Setenv("TMUX", "")
	root = t.TempDir()
	free = filepath.Join(root, "facet-test-reapable-free")
	held = filepath.Join(root, "facet-test-reapable-held")
	for _, dir := range []string{free, held} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := (&manifest.Manifest{Name: filepath.Base(dir)}).Write(dir); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(held, "plan.md"), []byte("the only copy\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	savedRoots, savedGit, savedGH, savedJobs := roots, git, gh, jobs
	t.Cleanup(func() { roots, git, gh, jobs = savedRoots, savedGit, savedGH, savedJobs })
	jobs = &launchd.Jobs{Dirs: []string{t.TempDir()}, Run: noTools}
	roots = config.Roots{Workspaces: root}
	git = gitx.Git{}
	return root, free, held
}

func exists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Stat(path)
	return err == nil
}

func TestReapReapableDeletesOnlyWhatNothingHolds(t *testing.T) {
	_, free, held := reapableRoot(t)
	var out bytes.Buffer
	var asked string
	err := reapReapable(&out, []string{free, held}, false, func(q string) bool { asked = q; return true })
	if err != nil {
		t.Fatalf("reapReapable: %v\n%s", err, out.String())
	}
	if asked != "Delete this workspace?" {
		t.Errorf("asked %q, want one question naming the count", asked)
	}
	if exists(t, free) {
		t.Errorf("%s holds nothing and should be gone:\n%s", free, out.String())
	}
	if !exists(t, filepath.Join(held, "plan.md")) {
		t.Errorf("%s holds the only copy of a file and must be left alone:\n%s", held, out.String())
	}
	for _, want := range []string{"1 reapable", "1 held left alone", "removed " + free, "shared mirror was not touched"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}

func TestReapReapableDeletesNothingWhenTheAnswerIsNo(t *testing.T) {
	_, free, held := reapableRoot(t)
	var out bytes.Buffer
	if err := reapReapable(&out, []string{free, held}, false, func(string) bool { return false }); err != nil {
		t.Fatal(err)
	}
	if !exists(t, free) || !exists(t, held) {
		t.Errorf("answering no deleted something:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "aborted.") {
		t.Errorf("output should say it aborted:\n%s", out.String())
	}
}

// Work can start in a workspace while the prompt waits. The check before each
// deletion must see it.
func TestReapReapableChecksAgainBeforeDeleting(t *testing.T) {
	_, free, held := reapableRoot(t)
	var out bytes.Buffer
	startWork := func(string) bool {
		if err := os.WriteFile(filepath.Join(free, "draft.md"), []byte("new work\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return true
	}
	if err := reapReapable(&out, []string{free, held}, false, startWork); err != nil {
		t.Fatal(err)
	}
	if !exists(t, filepath.Join(free, "draft.md")) {
		t.Fatalf("a workspace that became held during the prompt was deleted:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "skipped facet-test-reapable-free") {
		t.Errorf("output should say it skipped the workspace:\n%s", out.String())
	}
}

func TestReapReapableSaysSoWhenNothingIsReapable(t *testing.T) {
	_, _, held := reapableRoot(t)
	var out bytes.Buffer
	ask := func(string) bool { t.Fatal("asked although there is nothing to delete"); return false }
	if err := reapReapable(&out, []string{held}, false, ask); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "nothing to reap: 1 held") {
		t.Errorf("output:\n%s", out.String())
	}
}

func TestReapReapableRefusesForceAndAWorkspace(t *testing.T) {
	_, free, _ := reapableRoot(t)
	for _, args := range [][]string{
		{"--reapable", "--force"},
		{"--reapable", "facet-test-reapable-free"},
		{"--reapable", "--path", free},
	} {
		cmd := newReapCmd()
		cmd.SetArgs(args)
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		if err := cmd.Execute(); err == nil {
			t.Errorf("reap %v: no error", args)
		}
		if !exists(t, free) {
			t.Fatalf("reap %v deleted %s", args, free)
		}
	}
}

// prGH answers the pull-request lookup per issue branch, from maps a test may
// change while reap waits at the prompt. Everything else is fakeGH.
type prGH struct {
	*fakeGH
	prs  map[string]*ghx.PR
	errs map[string]error
}

func (f *prGH) ViewPR(_, branch string) (*ghx.PR, error) { return f.prs[branch], f.errs[branch] }

// issueWorkspace adds an issue workspace with no clones to root, so the only
// thing that can hold it is its pull request (or a session). Its branch is its
// directory name.
func issueWorkspace(t *testing.T, root string, n int) (dir, branch string) {
	t.Helper()
	branch = fmt.Sprintf("%d-reapable-test", n)
	dir = filepath.Join(root, config.IssuePrefix+branch)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	m := &manifest.Manifest{Name: filepath.Base(dir), Issue: &manifest.Issue{Repo: "o/r", Number: n, Branch: branch}}
	if err := m.Write(dir); err != nil {
		t.Fatal(err)
	}
	return dir, branch
}

// The batch decides with reap's own checks, so the pull-request guard holds in
// it too: an open PR holds, and a lookup that fails holds, as it does for reap
// <name>.
func TestReapReapableLeavesAnOpenOrUnknownPullRequestAlone(t *testing.T) {
	root, _, _ := reapableRoot(t)
	merged, mb := issueWorkspace(t, root, 1)
	open, ob := issueWorkspace(t, root, 2)
	unknown, ub := issueWorkspace(t, root, 3)
	gh = &prGH{
		fakeGH: &fakeGH{},
		prs:    map[string]*ghx.PR{mb: {Number: 10, State: "MERGED"}, ob: {Number: 11, State: "OPEN"}},
		errs:   map[string]error{ub: errors.New("HTTP 502")},
	}
	var out bytes.Buffer
	if err := reapReapable(&out, []string{merged, open, unknown}, false, func(string) bool { return true }); err != nil {
		t.Fatalf("reapReapable: %v\n%s", err, out.String())
	}
	if exists(t, merged) {
		t.Errorf("%s has a merged PR and nothing else and should be gone:\n%s", merged, out.String())
	}
	for _, dir := range []string{open, unknown} {
		if !exists(t, dir) {
			t.Errorf("%s was deleted although its pull request is open or unknown:\n%s", dir, out.String())
		}
	}
	if !strings.Contains(out.String(), "2 held left alone") {
		t.Errorf("output should count the two held workspaces:\n%s", out.String())
	}
}

// The check before each deletion runs every guard again, not only the files: a
// PR opened, or a lookup that starts failing, while the prompt waits holds the
// workspace.
func TestReapReapableChecksThePullRequestAgain(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(f *prGH, branch string)
	}{
		{"a PR opened", func(f *prGH, b string) { f.prs[b] = &ghx.PR{Number: 12, State: "OPEN"} }},
		{"the lookup fails", func(f *prGH, b string) { f.errs[b] = errors.New("HTTP 401") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, _, _ := reapableRoot(t)
			dir, branch := issueWorkspace(t, root, 4)
			f := &prGH{fakeGH: &fakeGH{}, prs: map[string]*ghx.PR{}, errs: map[string]error{}}
			gh = f
			var out bytes.Buffer
			ask := func(string) bool { tc.change(f, branch); return true }
			if err := reapReapable(&out, []string{dir}, false, ask); err != nil {
				t.Fatal(err)
			}
			if !exists(t, dir) {
				t.Fatalf("deleted although %s during the prompt:\n%s", tc.name, out.String())
			}
			if !strings.Contains(out.String(), "skipped "+filepath.Base(dir)) {
				t.Errorf("output should say it skipped the workspace:\n%s", out.String())
			}
		})
	}
}

func TestReapReapableSaysWhatItCouldNotCheck(t *testing.T) {
	root, free, _ := reapableRoot(t)
	broken := filepath.Join(root, "facet-test-reapable-broken")
	if err := os.MkdirAll(broken, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifest.Path(broken), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := reapReapable(&out, []string{free, broken}, false, func(string) bool { return true }); err != nil {
		t.Fatal(err)
	}
	if !exists(t, broken) {
		t.Fatalf("a workspace that could not be checked was deleted:\n%s", out.String())
	}
	for _, want := range []string{"could not check facet-test-reapable-broken, left alone: ", "1 that could not be checked", "removed " + free} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}

// A copied workspace keeps its manifest's name. The list names directories, so
// it says which ones go.
func TestReapReapableListsDirectories(t *testing.T) {
	root, free, _ := reapableRoot(t)
	copied := filepath.Join(root, "facet-test-reapable-copy")
	if err := os.MkdirAll(copied, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := (&manifest.Manifest{Name: filepath.Base(free)}).Write(copied); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := reapReapable(&out, []string{free, copied}, false, func(string) bool { return false }); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"facet-test-reapable-free ", "facet-test-reapable-copy "} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the list lacks %q:\n%s", want, out.String())
		}
	}
}

// Through the command: the root comes from FACET_WORKSPACES, relative or not,
// and --yes skips the prompt.
func TestReapReapableCommandReadsTheRoot(t *testing.T) {
	root, free, held := reapableRoot(t)
	t.Chdir(filepath.Dir(root))
	roots = config.Roots{Workspaces: filepath.Base(root)}
	cmd := newReapCmd()
	cmd.SetArgs([]string{"--reapable", "--yes"})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if exists(t, free) {
		t.Errorf("%s holds nothing and should be gone", free)
	}
	if !exists(t, filepath.Join(held, "plan.md")) {
		t.Errorf("%s holds the only copy of a file and must be left alone", held)
	}
}

func noTools(string, ...string) ([]byte, error) { return nil, errors.New("not on this test's path") }

// A loaded launchd job that runs from a workspace holds it, running or not
// (facet#234). Only launchctl and plutil are faked: the job is "loaded" and its
// plist is already XML.
func TestReapReapableLeavesAWorkspaceALaunchdJobRunsFrom(t *testing.T) {
	_, free, held := reapableRoot(t)
	agents := t.TempDir()
	p := filepath.Join(agents, "com.example.svc.plist")
	doc := `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict>
<key>Label</key><string>com.example.svc</string>
<key>WorkingDirectory</key><string>` + free + `</string>
</dict></plist>
`
	if err := os.WriteFile(p, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	jobs = &launchd.Jobs{Dirs: []string{agents}, Run: func(name string, args ...string) ([]byte, error) {
		if name == "launchctl" {
			return []byte("PID\tStatus\tLabel\n-\t0\tcom.example.svc\n"), nil
		}
		return os.ReadFile(args[len(args)-1])
	}}
	var out bytes.Buffer
	ask := func(string) bool { t.Fatal("asked although nothing is reapable"); return false }
	if err := reapReapable(&out, []string{free, held}, false, ask); err != nil {
		t.Fatal(err)
	}
	if !exists(t, free) {
		t.Fatalf("deleted the directory a loaded launchd job runs from:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "nothing to reap: 2 held") {
		t.Errorf("output:\n%s", out.String())
	}
	st, err := workspace.Inspect(free, git, gh, muxLive())
	if err != nil {
		t.Fatal(err)
	}
	b := strings.Join(st.Blockers(), "\n")
	if !strings.Contains(b, "launchd job com.example.svc runs from it (WorkingDirectory ") || !strings.Contains(b, "unload the job") {
		t.Errorf("the held reason should name the job and the fix:\n%s", b)
	}
}
