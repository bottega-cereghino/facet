package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bottega-cereghino/facet/internal/config"
	"github.com/bottega-cereghino/facet/internal/ghx"
	"github.com/bottega-cereghino/facet/internal/gitx"
	"github.com/bottega-cereghino/facet/internal/gitx/gitxtest"
	"github.com/bottega-cereghino/facet/internal/launchd"
	"github.com/bottega-cereghino/facet/internal/manifest"
	"github.com/bottega-cereghino/facet/internal/mux"
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
	err := reapReapable(&out, []string{free, held}, false, false, func(q string) bool { asked = q; return true })
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
	if err := reapReapable(&out, []string{free, held}, false, false, func(string) bool { return false }); err != nil {
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
	if err := reapReapable(&out, []string{free, held}, false, false, startWork); err != nil {
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
	if err := reapReapable(&out, []string{held}, false, false, ask); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "nothing to reap: 1 held") {
		t.Errorf("output:\n%s", out.String())
	}
}

// --dry-run prints what --reapable would delete, asks nothing and deletes
// nothing, even with --yes.
func TestReapReapableDryRunDeletesNothing(t *testing.T) {
	_, free, held := reapableRoot(t)
	var out bytes.Buffer
	ask := func(string) bool { t.Fatal("a dry run asked"); return true }
	if err := reapReapable(&out, []string{free, held}, true, true, ask); err != nil {
		t.Fatal(err)
	}
	if !exists(t, free) {
		t.Fatalf("a dry run deleted %s:\n%s", free, out.String())
	}
	for _, want := range []string{"facet-test-reapable-free ", "1 reapable", "1 held left alone", "dry run: nothing was deleted."} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "removed ") || strings.Contains(out.String(), "freed ") {
		t.Errorf("a dry run reported a deletion:\n%s", out.String())
	}
}

func TestReapReapableRefusesForceAndAWorkspace(t *testing.T) {
	_, free, _ := reapableRoot(t)
	for _, args := range [][]string{
		{"--reapable", "--force"},
		{"--reapable", "facet-test-reapable-free"},
		{"--reapable", "--path", free},
		{"--dry-run"},
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
	if err := reapReapable(&out, []string{merged, open, unknown}, false, false, func(string) bool { return true }); err != nil {
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
			if err := reapReapable(&out, []string{dir}, false, false, ask); err != nil {
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
	if err := reapReapable(&out, []string{free, broken}, false, false, func(string) bool { return true }); err != nil {
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
	if err := reapReapable(&out, []string{free, copied}, false, false, func(string) bool { return false }); err != nil {
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

// sweepProjects makes a projects root holding the two layouts facet#275 lost
// data in, and one worktree the sweep is for:
//   - app, a bare repository in app/.bare with the everyday checkout at
//     app/main, a linked worktree whose ignored .env is the only copy;
//   - svc, a checkout with a worktree outside it, a service run from a tag with
//     its ignored database beside the code;
//   - an agent's worktree in svc/.claude/worktrees, clean and pushed.
func sweepProjects(t *testing.T) (projects, env, db, agent string) {
	t.Helper()
	root := t.TempDir()
	origin := filepath.Join(root, "origin")
	if err := os.MkdirAll(origin, 0o777); err != nil {
		t.Fatal(err)
	}
	gitIn(t, origin, "init", "-q", "-b", "main")
	gitIn(t, origin, "-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "init")
	projects = filepath.Join(root, "Projects")

	app := filepath.Join(projects, "app")
	gitIn(t, "", "clone", "-q", "--bare", origin, filepath.Join(app, ".bare"))
	putFile(t, filepath.Join(app, ".git"), "gitdir: ./.bare\n")
	gitIn(t, app, "config", "remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*")
	gitIn(t, app, "fetch", "-q", "origin")
	everyday := filepath.Join(app, "main")
	gitIn(t, app, "worktree", "add", "-q", everyday, "main")
	gitIn(t, everyday, "branch", "-q", "-u", "origin/main")
	putFile(t, filepath.Join(app, ".bare", "info", "exclude"), ".env\n")
	env = filepath.Join(everyday, ".env")
	putFile(t, env, "API_KEY=only-copy\n")

	svc := filepath.Join(projects, "svc")
	gitIn(t, "", "clone", "-q", origin, svc)
	putFile(t, filepath.Join(svc, ".git", "info", "exclude"), "*.db\n.claude/\n")
	service := filepath.Join(root, "Services", "svc")
	gitIn(t, svc, "worktree", "add", "-q", "--detach", service, "origin/main")
	db = filepath.Join(service, "prod.db")
	putFile(t, db, "rows\n")
	agent = filepath.Join(svc, ".claude", "worktrees", "agent")
	gitIn(t, svc, "worktree", "add", "-q", "--detach", agent, "origin/main")
	return projects, env, db, agent
}

func putFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o666); err != nil {
		t.Fatal(err)
	}
}

// --reapable sweeps only worktrees inside their checkout, never a bare
// layout's: the everyday checkout of a bare repository and a service run from
// a worktree elsewhere both keep their ignored files (facet#275).
func TestReapReapableSweepsOnlyWorktreesInsideTheirCheckout(t *testing.T) {
	root, _, _ := reapableRoot(t)
	projects, env, db, agent := sweepProjects(t)
	roots.Projects = projects
	cmd := newReapCmd()
	cmd.SetArgs([]string{"--reapable", "--yes"})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !exists(t, env) {
		t.Errorf("%s went: the bare layout's everyday checkout was swept", env)
	}
	if !exists(t, db) {
		t.Errorf("%s went: a worktree outside its checkout was swept", db)
	}
	if exists(t, agent) {
		t.Errorf("%s holds nothing, is inside its checkout and should be gone", agent)
	}
	if exists(t, filepath.Join(root, "facet-test-reapable-free")) {
		t.Errorf("the free workspace should be gone too")
	}
}

// noTools is a machine without launchctl or plutil: off macOS, which is the
// one failure read as no jobs loaded.
func noTools(name string, _ ...string) ([]byte, error) {
	return nil, &exec.Error{Name: name, Err: exec.ErrNotFound}
}

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
	if err := reapReapable(&out, []string{free, held}, false, false, ask); err != nil {
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

// A launchctl list that fails cannot say no job runs from a workspace, so it
// holds the workspace, in reap <name> and in --reapable, and the reason says how
// to check (facet#273). Only a launchctl that is not installed reads as none.
func TestReapHoldsAWorkspaceWhenLaunchctlListFails(t *testing.T) {
	_, free, held := reapableRoot(t)
	jobs = &launchd.Jobs{Dirs: []string{t.TempDir()}, Run: func(string, ...string) ([]byte, error) {
		return nil, errors.New("exit status 5")
	}}
	var out bytes.Buffer
	ask := func(string) bool { t.Fatal("asked although nothing is reapable"); return false }
	if err := reapReapable(&out, []string{free, held}, false, false, ask); err != nil {
		t.Fatal(err)
	}
	if !exists(t, free) {
		t.Fatalf("--reapable deleted a workspace whose launchd jobs could not be read:\n%s", out.String())
	}
	cmd := newReapCmd()
	cmd.SetArgs([]string{filepath.Base(free), "--yes"})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err == nil {
		t.Error("reap returned no error although the loaded jobs could not be read")
	}
	if !exists(t, free) {
		t.Fatal("reap deleted a workspace whose launchd jobs could not be read")
	}
	st, err := workspace.Inspect(free, git, gh, muxLive())
	if err != nil {
		t.Fatal(err)
	}
	b := strings.Join(st.Blockers(), "\n")
	if !strings.Contains(b, "could not read the loaded launchd jobs") || !strings.Contains(b, "exit status 5") || !strings.Contains(b, "`launchctl list`") {
		t.Errorf("the held reason should say the jobs could not be read, why, and how to check:\n%s", b)
	}
}

// reap <name> refuses while something holds the workspace, and deletes nothing:
// the guarantee the rest of reap exists for, through the command itself.
func TestReapRefusesAHeldWorkspace(t *testing.T) {
	_, _, held := reapableRoot(t)
	cmd := newReapCmd()
	cmd.SetArgs([]string{filepath.Base(held), "--yes"})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err == nil {
		t.Error("reap of a held workspace returned no error")
	}
	if !exists(t, filepath.Join(held, "plan.md")) {
		t.Fatal("reap deleted a workspace holding the only copy of a file")
	}
}

// A process whose working directory is in the workspace holds it, through
// the command and with whatever multiplexer the machine has, tmux or none.
func TestReapRefusesWhileAProcessIsInTheWorkspace(t *testing.T) {
	if _, err := exec.LookPath("lsof"); err != nil {
		t.Skip("lsof not installed: the process probe cannot run here")
	}
	_, free, _ := reapableRoot(t)
	sleeper := exec.Command("sleep", "30")
	sleeper.Dir = free
	if err := sleeper.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sleeper.Process.Kill(); _ = sleeper.Wait() })
	deadline := time.Now().Add(5 * time.Second)
	running := func() bool { procs, _ := mux.ProcessesUnder(free); return len(procs) > 0 }
	for !running() && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	cmd := newReapCmd()
	cmd.SetArgs([]string{filepath.Base(free), "--yes"})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err == nil {
		t.Error("reap returned no error with a process in the workspace")
	}
	if !exists(t, free) {
		t.Fatal("reap deleted a workspace a process is running in")
	}
}

// A pushed branch deleted upstream since, then a sweep while origin cannot be
// reached: "nothing unpushed" was judged against refs on disk that still name
// the branch. A single reap says so in a note; --reapable never guesses, so it
// leaves the workspace alone and says why (facet#267).
func TestReapReapableHoldsAWorkspaceJudgedAgainstAFailedFetch(t *testing.T) {
	root, _, _ := reapableRoot(t)
	origin := filepath.Join(t.TempDir(), "origin.git")
	gitIn(t, "", "init", "-q", "--bare", "-b", "main", origin)
	seed := filepath.Join(t.TempDir(), "seed")
	gitIn(t, "", "clone", "-q", origin, seed)
	gitIn(t, seed, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "--allow-empty", "-m", "init")
	gitIn(t, seed, "push", "-q", "origin", "HEAD:main")
	ws := filepath.Join(root, "facet-test-reapable-stale")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := (&manifest.Manifest{Name: "stale", Clones: map[string]string{"app": origin}}).Write(ws); err != nil {
		t.Fatal(err)
	}
	gitIn(t, "", "clone", "-q", origin, filepath.Join(ws, "app"))
	git = &gitxtest.Runner{
		Real: gitx.Git{},
		Fail: func(args []string) bool { return len(args) > 0 && args[0] == "fetch" },
		Err:  errors.New("could not resolve host"),
	}
	var out bytes.Buffer
	if err := reapReapable(&out, []string{ws}, true, false, nil); err != nil {
		t.Fatalf("reapReapable: %v\n%s", err, out.String())
	}
	if !exists(t, filepath.Join(ws, "app")) {
		t.Fatalf("a workspace judged against a failed fetch was deleted:\n%s", out.String())
	}
	for _, want := range []string{"could not fetch origin", "facet reap facet-test-reapable-stale"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}

// The dry run lists the held workspaces grouped by what holds them, one line
// each, so the list reads at a glance.
func TestReapReapableDryRunGroupsTheHeldByWhatHoldsThem(t *testing.T) {
	root, free, held := reapableRoot(t)
	second := filepath.Join(root, "facet-test-reapable-held-2")
	if err := os.MkdirAll(second, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := (&manifest.Manifest{Name: filepath.Base(second)}).Write(second); err != nil {
		t.Fatal(err)
	}
	putFile(t, filepath.Join(second, "notes.md"), "only copy\n")
	var out bytes.Buffer
	if err := reapReapable(&out, []string{free, held, second}, false, true, nil); err != nil {
		t.Fatal(err)
	}
	want := "\nheld (2), by what holds them:\n" +
		"  files beside the clones (2)\n" +
		"    facet-test-reapable-held\n" +
		"    facet-test-reapable-held-2\n"
	if !strings.Contains(out.String(), want) {
		t.Errorf("output lacks the grouped held list %q:\n%s", want, out.String())
	}
	if !strings.Contains(out.String(), "2 held left alone") {
		t.Errorf("output lacks the count:\n%s", out.String())
	}
}
