package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bottega-cereghino/facet/internal/config"
	"github.com/bottega-cereghino/facet/internal/ghx"
	"github.com/bottega-cereghino/facet/internal/manifest"
)

func TestQueueConfigPathPrefersTheFlag(t *testing.T) {
	t.Setenv("FACET_QUEUE", "from-env")
	got, err := queueConfigPath("from-flag")
	if err != nil || got != "from-flag" {
		t.Fatalf("flag: got %q, %v", got, err)
	}
	got, err = queueConfigPath("")
	if err != nil || got != "from-env" {
		t.Fatalf("env: got %q, %v", got, err)
	}
	t.Setenv("FACET_QUEUE", "")
	_, err = queueConfigPath("")
	if err == nil || !strings.Contains(err.Error(), "fix:") {
		t.Fatalf("missing config: %v", err)
	}
}

func TestQueueExampleConfigLoads(t *testing.T) {
	cfg, err := loadQueueConfig(filepath.Join("..", "..", "docs", "queue.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Max != 6 || cfg.Order != queueOrderOldest || cfg.Label != "next" || cfg.Wait != 30*time.Second {
		t.Fatalf("defaults from the example: %+v", cfg)
	}
	if cfg.Hold == "" || len(cfg.Gate) == 0 || len(cfg.Repos) != 1 || len(cfg.Agents) != 3 {
		t.Fatalf("example is missing a field: %+v", cfg)
	}
	if cfg.Agents[0].Label != "codex" || cfg.Agents[1].Label != "gemini" || cfg.Agents[2].Label != "" {
		t.Fatalf("agents = %+v", cfg.Agents)
	}
	for _, a := range cfg.Agents {
		if !commandHasPrompt(a.Command) {
			t.Errorf("agent %q has no {prompt}: %q", a.Label, a.Command)
		}
	}
}

func TestQueueConfigDefaultsAndRefusals(t *testing.T) {
	ok, err := loadTempConfig(t, `{
		"repos": ["acme/widgets"],
		"agents": [{"label": "", "command": ["agent", "{prompt}"]}]
	}`)
	if err != nil {
		t.Fatal(err)
	}
	if ok.Max != defaultQueueMax || ok.Wait != defaultQueueWait || ok.Label != defaultQueueLabel || ok.Order != queueOrderOldest {
		t.Fatalf("defaults: %+v", ok)
	}
	if !strings.Contains(ok.Prompt, "Merge when the checks are green") {
		t.Fatalf("default prompt = %q", ok.Prompt)
	}

	cases := []struct {
		name string
		body string
		want string
	}{
		{"unknown field", `{"maxAgents": 1, "repos": ["acme/widgets"], "agents": [{"command": ["a", "{prompt}"]}]}`, "maxAgents"},
		{"max zero", `{"max": 0, "repos": ["acme/widgets"], "agents": [{"command": ["a", "{prompt}"]}]}`, "at least 1"},
		{"bad order", `{"order": "newest", "repos": ["acme/widgets"], "agents": [{"command": ["a", "{prompt}"]}]}`, "oldest"},
		{"bad repo", `{"repos": ["widgets"], "agents": [{"command": ["a", "{prompt}"]}]}`, "owner/name"},
		{"duplicate repo", `{"repos": ["acme/widgets", "acme/widgets"], "agents": [{"command": ["a", "{prompt}"]}]}`, "twice"},
		{"no repos", `{"repos": [], "agents": [{"command": ["a", "{prompt}"]}]}`, "no repos"},
		{"no agents", `{"repos": ["acme/widgets"], "agents": []}`, "no agents"},
		{"two defaults", `{"repos": ["acme/widgets"], "agents": [{"command": ["a", "{prompt}"]}, {"command": ["b", "{prompt}"]}]}`, "empty label"},
		{"no prompt slot", `{"repos": ["acme/widgets"], "agents": [{"command": ["agent"]}]}`, "{prompt}"},
		{"agent label is the queue label", `{"label": "next", "repos": ["acme/widgets"], "agents": [{"label": "next", "command": ["a", "{prompt}"]}, {"command": ["b", "{prompt}"]}]}`, "queue label"},
		{"duplicate agent label", `{"repos": ["acme/widgets"], "agents": [{"label": "Codex", "command": ["a", "{prompt}"]}, {"label": "codex", "command": ["b", "{prompt}"]}, {"command": ["c", "{prompt}"]}]}`, "two agents"},
		{"bad wait", `{"wait": "soon", "repos": ["acme/widgets"], "agents": [{"command": ["a", "{prompt}"]}]}`, "duration"},
		{"extra value", `{"repos": ["acme/widgets"], "agents": [{"command": ["a", "{prompt}"]}]} {}`, "extra data"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadTempConfig(t, tc.body)
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "fix:") {
				t.Fatalf("error %v, want it to mention %q and the fix", err, tc.want)
			}
		})
	}
}

func TestExpandDoesNotRescanAValue(t *testing.T) {
	got := expand("T={title} W={workspace} P={prompt}", map[string]string{
		"title":     "{workspace}",
		"workspace": "/ws",
		"prompt":    "kept {title}",
	})
	want := "T={workspace} W=/ws P=kept {title}"
	if got != want {
		t.Fatalf("expand = %q, want %q", got, want)
	}
}

func TestQueueCommandFor(t *testing.T) {
	cfg := queueConfig{Agents: []queueAgent{
		{Label: "codex", Command: []string{"codex", "{prompt}"}},
		{Label: "gemini", Command: []string{"gemini", "{prompt}"}},
		{Label: "", Command: []string{"default", "{prompt}"}},
	}}
	cases := []struct {
		labels []string
		want   string
	}{
		{nil, "default"},
		{[]string{"next"}, "default"},
		{[]string{"Codex"}, "codex"},
		{[]string{"gemini", "codex"}, "codex"},
		{[]string{"gemini"}, "gemini"},
	}
	for _, tc := range cases {
		_, cmd, err := cfg.commandFor(tc.labels)
		if err != nil || cmd[0] != tc.want {
			t.Errorf("labels %q: command %q, %v; want %s", tc.labels, cmd, err, tc.want)
		}
	}
}

func TestQueueOrdersIssues(t *testing.T) {
	early := "2026-02-01T00:00:00Z"
	late := "2026-06-01T00:00:00Z"
	issues := []queuedIssue{
		{Repo: "acme/widgets", Issue: openIssue(1, "2026-03-01T00:00:00Z", milestone(2, "Late", late))},
		{Repo: "acme/widgets", Issue: openIssue(2, "2026-01-03T00:00:00Z", milestone(1, "Early", early))},
		{Repo: "acme/other", Issue: openIssue(3, "2026-01-01T00:00:00Z", nil)},
		{Repo: "acme/widgets", Issue: openIssue(4, "2026-01-02T00:00:00Z", &ghx.Milestone{Number: 9, Title: "Undated"})},
		{Repo: "acme/widgets", Issue: openIssue(5, "not-a-date", nil)},
	}
	if got, want := keys(orderQueued(issues, queueOrderMilestone)), []string{
		"acme/widgets#2", "acme/widgets#1", "acme/widgets#4", "acme/other#3", "acme/widgets#5",
	}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("milestone order = %v, want %v", got, want)
	}
	if got, want := keys(orderQueued(issues, queueOrderOldest)), []string{
		"acme/other#3", "acme/widgets#4", "acme/widgets#2", "acme/widgets#1", "acme/widgets#5",
	}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("oldest order = %v, want %v", got, want)
	}

	same := []queuedIssue{
		{Repo: "acme/widgets", Issue: openIssue(8, "2026-01-01T00:00:00Z", nil)},
		{Repo: "acme/other", Issue: openIssue(9, "2026-01-01T00:00:00Z", nil)},
		{Repo: "acme/widgets", Issue: openIssue(7, "2026-01-01T00:00:00Z", nil)},
	}
	if got, want := keys(orderQueued(same, queueOrderOldest)), []string{
		"acme/other#9", "acme/widgets#7", "acme/widgets#8",
	}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("tie break = %v, want %v", got, want)
	}
}

func TestRunArgvDoesNotPassThePromptThroughAShell(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "pwned")
	out := filepath.Join(dir, "args")
	script := writeExe(t, "record", "#!/bin/sh\nout=$1\nshift\nfor a in \"$@\"; do printf '%s\\0' \"$a\"; done > \"$out\"\npwd > \"$out.cwd\"\n")
	nasty := "x $(touch " + marker + ") `touch " + marker + "`"
	if err := runArgv(context.Background(), dir, []string{script, out, nasty}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("the argument was passed through a shell")
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if string(bytes.TrimRight(got, "\x00")) != nasty {
		t.Fatalf("arg = %q, want %q", got, nasty)
	}
	cwd, err := os.ReadFile(out + ".cwd")
	if err != nil {
		t.Fatal(err)
	}
	if !sameDir(strings.TrimSpace(string(cwd)), dir) {
		t.Fatalf("cwd = %q, want %q", cwd, dir)
	}
}

func TestQueueCommandStopsOnTheHoldFileWithoutListing(t *testing.T) {
	dir := t.TempDir()
	hold := filepath.Join(dir, "hold")
	if err := os.WriteFile(hold, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(queueFile{
		Hold:   hold,
		Wait:   "20ms",
		Repos:  []string{"acme/widgets"},
		Agents: []queueAgent{{Command: []string{"agent", "{prompt}"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "q.json")
	if err := os.WriteFile(cfgPath, body, 0o644); err != nil {
		t.Fatal(err)
	}
	prevGH := gh
	fake := &fakeGH{openErr: errors.New("should not list")}
	gh = fake
	prevRoots := roots
	roots.Workspaces = dir
	t.Cleanup(func() {
		gh = prevGH
		roots = prevRoots
	})
	t.Setenv("FACET_QUEUE", cfgPath)

	cmd := newQueueCmd()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if len(fake.openCalls) != 0 {
		t.Fatalf("listed issues despite the hold file: %v", fake.openCalls)
	}
}

func TestQueueStopsWhenTheHoldFileExists(t *testing.T) {
	dir := t.TempDir()
	hold := filepath.Join(dir, "hold")
	if err := os.WriteFile(hold, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	var listed atomic.Int32
	var buf bytes.Buffer
	cfg := oneAgentConfig(nil)
	cfg.Hold = hold
	err := runQueue(context.Background(), cfg, queueEnv{
		out: &buf,
		list: func(string, string) ([]ghx.Issue, error) {
			listed.Add(1)
			return nil, nil
		},
		spawn: func(string, int) (string, error) {
			t.Error("spawned while the hold file exists")
			return "", errors.New("spawned")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if listed.Load() != 0 {
		t.Fatalf("listed %d time(s)", listed.Load())
	}
	if !strings.Contains(buf.String(), "starting nothing") {
		t.Fatalf("output = %q", buf.String())
	}
}

func TestQueueDoesNotStartWhenARepositoryCannotBeListed(t *testing.T) {
	var spawned atomic.Int32
	cfg := oneAgentConfig([]string{"acme/widgets", "acme/other"})
	s := startQueue(t, cfg, queueEnv{
		list: func(repo, _ string) ([]ghx.Issue, error) {
			if repo == "acme/other" {
				return nil, errors.New("temporary failure")
			}
			return []ghx.Issue{openIssue(1, "2026-01-01T00:00:00Z", nil)}, nil
		},
		spawn: func(string, int) (string, error) {
			spawned.Add(1)
			return "", errors.New("spawned")
		},
	})
	time.Sleep(150 * time.Millisecond)
	s.stop(t)
	if spawned.Load() != 0 {
		t.Fatalf("spawned %d time(s) while a repository could not be listed", spawned.Load())
	}
	if !strings.Contains(s.buf.String(), "cannot be listed") {
		t.Fatalf("output = %q", s.buf.String())
	}
}

func TestQueueTakesTheOldestIssueFirst(t *testing.T) {
	out := filepath.Join(t.TempDir(), "out")
	script := appendScript(t)
	cfg := oneAgentConfig([]string{"acme/widgets", "acme/other"})
	cfg.Agents = []queueAgent{{Command: []string{script, out, "{number}", "{prompt}"}}}
	s := startQueue(t, cfg, queueEnv{
		list: func(repo, label string) ([]ghx.Issue, error) {
			if label != "next" {
				return nil, fmt.Errorf("label %q", label)
			}
			switch repo {
			case "acme/widgets":
				return []ghx.Issue{
					openIssue(9, "2026-03-01T00:00:00Z", nil),
					closedIssue(1, "2020-01-01T00:00:00Z"),
					openIssue(2, "2026-02-01T00:00:00Z", nil),
				}, nil
			case "acme/other":
				return []ghx.Issue{openIssue(5, "2026-01-01T00:00:00Z", nil)}, nil
			default:
				return nil, fmt.Errorf("unexpected repo %s", repo)
			}
		},
	})
	waitFor(t, "three agents", func() bool { return len(readLines(t, out)) >= 3 })
	s.stop(t)
	if got, want := readLines(t, out), []string{"5", "2", "9"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("start order = %v, want %v", got, want)
	}
}

func TestQueueOrdersAMilestoneBeforeAnUnscheduledIssue(t *testing.T) {
	out := filepath.Join(t.TempDir(), "out")
	script := appendScript(t)
	cfg := oneAgentConfig([]string{"acme/widgets"})
	cfg.Order = queueOrderMilestone
	cfg.Agents = []queueAgent{{Command: []string{script, out, "{number}", "{prompt}"}}}
	early := milestone(1, "Early", "2026-02-01T00:00:00Z")
	late := milestone(2, "Late", "2026-06-01T00:00:00Z")
	s := startQueue(t, cfg, queueEnv{
		list: func(string, string) ([]ghx.Issue, error) {
			a := openIssue(1, "2026-01-01T00:00:00Z", late)
			b := openIssue(2, "2026-01-03T00:00:00Z", early)
			c := openIssue(3, "2026-01-01T00:00:00Z", nil)
			return []ghx.Issue{a, b, c}, nil
		},
	})
	waitFor(t, "three agents", func() bool { return len(readLines(t, out)) >= 3 })
	s.stop(t)
	if got, want := readLines(t, out), []string{"2", "1", "3"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("start order = %v, want %v", got, want)
	}
}

func TestQueuePicksTheCommandForTheIssueLabel(t *testing.T) {
	root := t.TempDir()
	out := filepath.Join(root, "out")
	script := recordScript(t)
	cfg := oneAgentConfig([]string{"acme/widgets"})
	cfg.Prompt = "Do {repo}#{number} {title} ws={workspace}"
	cfg.Agents = []queueAgent{
		{Label: "codex", Command: []string{script, out, "codex", "{number}", "{workspace}", "{prompt}"}},
		{Label: "gemini", Command: []string{script, out, "gemini", "{number}", "{workspace}", "{prompt}"}},
		{Command: []string{script, out, "default", "{number}", "{workspace}", "{prompt}"}},
	}
	ws := map[int]string{}
	var mu sync.Mutex
	s := startQueue(t, cfg, queueEnv{
		list: func(string, string) ([]ghx.Issue, error) {
			return []ghx.Issue{
				labelled(1, "2026-01-01T00:00:00Z", "Bell", "next"),
				labelled(2, "2026-01-02T00:00:00Z", "Codex task", "next", "codex"),
				labelled(3, "2026-01-03T00:00:00Z", "Both", "next", "gemini", "codex"),
				labelled(4, "2026-01-04T00:00:00Z", "Gemini", "next", "Gemini"),
			}, nil
		},
		spawn: func(_ string, n int) (string, error) {
			d, err := os.MkdirTemp(root, "ws")
			if err != nil {
				return "", err
			}
			mu.Lock()
			ws[n] = d
			mu.Unlock()
			return d, nil
		},
	})
	waitFor(t, "four agents", func() bool { return strings.Count(readFile(t, out), "\n---\n") >= 4 })
	s.stop(t)

	recs := parseRecords(t, readFile(t, out))
	if len(recs) != 4 {
		t.Fatalf("records = %+v", recs)
	}
	wantID := []string{"default", "codex", "codex", "gemini"}
	for i, rec := range recs {
		if rec.id != wantID[i] {
			t.Errorf("record %d id = %s, want %s", i, rec.id, wantID[i])
		}
		mu.Lock()
		dir := ws[rec.n]
		mu.Unlock()
		if !sameDir(rec.cwd, dir) || !sameDir(rec.ws, dir) {
			t.Errorf("record %d cwd %q ws %q, workspace %q", i, rec.cwd, rec.ws, dir)
		}
		if !strings.Contains(rec.prompt, fmt.Sprintf("acme/widgets#%d", rec.n)) {
			t.Errorf("record %d prompt = %q", i, rec.prompt)
		}
	}
}

func TestQueueRunsAtMostMaxAgents(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "out")
	script := blockScript(t)
	cfg := oneAgentConfig([]string{"acme/widgets"})
	cfg.Max = 2
	cfg.Agents = []queueAgent{{Command: []string{script, out, "{number}", "{prompt}"}}}
	s := startQueue(t, cfg, queueEnv{
		list: func(string, string) ([]ghx.Issue, error) {
			return []ghx.Issue{
				openIssue(1, "2026-01-01T00:00:00Z", nil),
				openIssue(2, "2026-01-02T00:00:00Z", nil),
				openIssue(3, "2026-01-03T00:00:00Z", nil),
			}, nil
		},
		spawn: func(string, int) (string, error) {
			return os.MkdirTemp(dir, "ws")
		},
	})
	waitFor(t, "two agents", func() bool { return len(readLines(t, out)) >= 2 })
	time.Sleep(250 * time.Millisecond)
	got := readLines(t, out)
	if len(got) != 2 {
		t.Fatalf("started %v, want exactly two while neither has exited", got)
	}
	s.stop(t)
}

func TestQueueWaitsWhenTheGateFails(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "out")
	count := filepath.Join(dir, "gate-count")
	cfg := oneAgentConfig([]string{"acme/widgets"})
	cfg.Gate = []string{gateScript(t, count, 3)}
	cfg.Agents = []queueAgent{{Command: []string{appendScript(t), out, "{number}", "{prompt}"}}}
	var spawned atomic.Int32
	s := startQueue(t, cfg, queueEnv{
		list: func(string, string) ([]ghx.Issue, error) {
			return []ghx.Issue{openIssue(4, "2026-01-01T00:00:00Z", nil)}, nil
		},
		spawn: func(string, int) (string, error) {
			spawned.Add(1)
			return os.MkdirTemp(dir, "ws")
		},
	})
	waitFor(t, "the agent", func() bool { return len(readLines(t, out)) >= 1 })
	s.stop(t)
	if spawned.Load() != 1 {
		t.Fatalf("spawned %d time(s), want 1", spawned.Load())
	}
	if got := strings.TrimSpace(readFile(t, count)); got != "3" {
		t.Fatalf("gate ran until count %q, want 3", got)
	}
}

func TestQueueDoesNotHandTheSameIssueOutTwice(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "out")
	cfg := oneAgentConfig([]string{"acme/widgets"})
	cfg.Agents = []queueAgent{{Command: []string{appendScript(t), out, "{number}", "{prompt}"}}}
	var spawned atomic.Int32
	s := startQueue(t, cfg, queueEnv{
		list: func(string, string) ([]ghx.Issue, error) {
			return []ghx.Issue{openIssue(6, "2026-01-01T00:00:00Z", nil)}, nil
		},
		spawn: func(string, int) (string, error) {
			spawned.Add(1)
			return os.MkdirTemp(dir, "ws")
		},
	})
	waitFor(t, "the agent", func() bool { return len(readLines(t, out)) >= 1 })
	time.Sleep(200 * time.Millisecond)
	s.stop(t)
	if spawned.Load() != 1 {
		t.Fatalf("spawned %d time(s), want 1", spawned.Load())
	}
}

func TestQueueDoesNotRetryAnAgentThatExitsNonZero(t *testing.T) {
	dir := t.TempDir()
	cfg := oneAgentConfig([]string{"acme/widgets"})
	cfg.Agents = []queueAgent{{Command: []string{writeExe(t, "fail", "#!/bin/sh\nexit 1\n"), "{prompt}"}}}
	var spawned atomic.Int32
	s := startQueue(t, cfg, queueEnv{
		list: func(string, string) ([]ghx.Issue, error) {
			return []ghx.Issue{openIssue(10, "2026-01-01T00:00:00Z", nil)}, nil
		},
		spawn: func(string, int) (string, error) {
			spawned.Add(1)
			return os.MkdirTemp(dir, "ws")
		},
	})
	waitFor(t, "the agent", func() bool { return spawned.Load() >= 1 })
	time.Sleep(200 * time.Millisecond)
	s.stop(t)
	if spawned.Load() != 1 {
		t.Fatalf("spawned %d time(s), want 1", spawned.Load())
	}
}

func TestQueueRetriesWhenTheAgentCannotStart(t *testing.T) {
	dir := t.TempDir()
	cfg := oneAgentConfig([]string{"acme/widgets"})
	cfg.Agents = []queueAgent{{Command: []string{filepath.Join(dir, "missing-agent"), "{prompt}"}}}
	var spawned atomic.Int32
	s := startQueue(t, cfg, queueEnv{
		list: func(string, string) ([]ghx.Issue, error) {
			return []ghx.Issue{openIssue(9, "2026-01-01T00:00:00Z", nil)}, nil
		},
		spawn: func(string, int) (string, error) {
			spawned.Add(1)
			return os.MkdirTemp(dir, "ws")
		},
	})
	waitFor(t, "a second attempt", func() bool { return spawned.Load() >= 2 })
	s.stop(t)
}

func TestQueueRetriesASpawnThatFailed(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "out")
	cfg := oneAgentConfig([]string{"acme/widgets"})
	cfg.Agents = []queueAgent{{Command: []string{appendScript(t), out, "{number}", "{prompt}"}}}
	var calls atomic.Int32
	s := startQueue(t, cfg, queueEnv{
		list: func(string, string) ([]ghx.Issue, error) {
			return []ghx.Issue{openIssue(8, "2026-01-01T00:00:00Z", nil)}, nil
		},
		spawn: func(string, int) (string, error) {
			if calls.Add(1) == 1 {
				return "", errors.New("mirror missing")
			}
			return os.MkdirTemp(dir, "ws")
		},
	})
	waitFor(t, "the agent", func() bool { return len(readLines(t, out)) >= 1 })
	s.stop(t)
	if calls.Load() != 2 {
		t.Fatalf("spawn calls = %d, want 2", calls.Load())
	}
}

func TestQueueUsesAWorkspaceThatAlreadyExists(t *testing.T) {
	root := withWorkspaces(t)
	ws := writeIssueWorkspace(t, root, "iss-widgets-7-bell", "acme/widgets", 7)
	keep := filepath.Join(ws, "keep")
	if err := os.WriteFile(keep, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	spawned := false
	got, err := prepareWorkspaceSpawn(root, "acme/widgets", 7, func() error {
		spawned = true
		return errors.New("should not spawn")
	})
	if err != nil {
		t.Fatal(err)
	}
	if spawned {
		t.Fatal("spawned even though a workspace already exists")
	}
	if got != ws {
		t.Fatalf("workspace = %s, want %s", got, ws)
	}
	if b, err := os.ReadFile(keep); err != nil || string(b) != "keep" {
		t.Fatalf("workspace contents after prepare: %q, %v", b, err)
	}

	// A long-lived workspace is not an issue workspace, whatever it records.
	writeIssueWorkspace(t, root, "notes", "acme/widgets", 8)
	if got, err := prepareWorkspaceSpawn(root, "acme/widgets", 8, func() error {
		d := writeIssueWorkspace(t, root, "iss-widgets-8-new", "acme/widgets", 8)
		if d == "" {
			return errors.New("did not write")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	} else if !strings.HasPrefix(filepath.Base(got), config.IssuePrefix) {
		t.Fatalf("started from %s", got)
	}
}

func TestQueueSpawnsWhenNoWorkspaceExists(t *testing.T) {
	root := withWorkspaces(t)
	got, err := prepareWorkspaceSpawn(root, "acme/widgets", 3, func() error {
		writeIssueWorkspace(t, root, "iss-widgets-3-bell", "acme/widgets", 3)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(got) != "iss-widgets-3-bell" {
		t.Fatalf("workspace = %s", got)
	}
}

func TestQueueRefusesTwoWorkspacesForOneIssue(t *testing.T) {
	root := withWorkspaces(t)
	writeIssueWorkspace(t, root, "iss-a", "acme/widgets", 3)
	writeIssueWorkspace(t, root, "iss-b", "acme/widgets", 3)
	_, err := prepareWorkspaceSpawn(root, "acme/widgets", 3, func() error {
		t.Error("spawned despite two workspaces")
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "two workspaces") || !strings.Contains(err.Error(), "fix:") {
		t.Fatalf("error = %v", err)
	}
}

func TestQueueRefusesAnUnreadableIssueWorkspace(t *testing.T) {
	root := withWorkspaces(t)
	dir := filepath.Join(root, "iss-broken")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifest.Path(dir), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := prepareWorkspaceSpawn(root, "acme/widgets", 1, func() error {
		t.Error("spawned despite an unreadable issue workspace")
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "fix:") {
		t.Fatalf("error = %v", err)
	}
}

func TestQueueKeepsAWorkspaceAFailedSpawnLeftBehind(t *testing.T) {
	root := withWorkspaces(t)
	var ws string
	got, err := prepareWorkspaceSpawn(root, "acme/widgets", 4, func() error {
		ws = writeIssueWorkspace(t, root, "iss-widgets-4-bell", "acme/widgets", 4)
		return errors.New("already exists")
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != ws {
		t.Fatalf("got %s, want %s", got, ws)
	}
}

type queueRun struct {
	cancel context.CancelFunc
	errCh  chan error
	buf    *bytes.Buffer
	once   sync.Once
	err    error
}

func startQueue(t *testing.T, cfg queueConfig, env queueEnv) *queueRun {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	q := &queueRun{cancel: cancel, errCh: make(chan error, 1), buf: &bytes.Buffer{}}
	if env.out == nil {
		env.out = q.buf
	} else if b, ok := env.out.(*bytes.Buffer); ok {
		q.buf = b
	}
	if env.spawn == nil {
		// MkdirTemp, not t.TempDir: spawn runs on the queue's goroutine.
		root := t.TempDir()
		env.spawn = func(string, int) (string, error) {
			return os.MkdirTemp(root, "ws")
		}
	}
	if env.list == nil {
		t.Fatal("queue test left list unset")
	}
	go func() { q.errCh <- runQueue(ctx, cfg, env) }()
	t.Cleanup(func() { q.stop(t) })
	return q
}

func (q *queueRun) stop(t *testing.T) {
	t.Helper()
	q.once.Do(func() {
		q.cancel()
		select {
		case q.err = <-q.errCh:
		case <-time.After(5 * time.Second):
			q.err = errors.New("queue did not stop")
		}
	})
	if q.err != nil {
		t.Fatalf("queue: %v\n%s", q.err, q.buf.String())
	}
}

func oneAgentConfig(repos []string) queueConfig {
	if repos == nil {
		repos = []string{"acme/widgets"}
	}
	return queueConfig{
		Max:    1,
		Wait:   20 * time.Millisecond,
		Order:  queueOrderOldest,
		Label:  "next",
		Repos:  repos,
		Prompt: defaultQueuePrompt,
		Agents: []queueAgent{{Command: []string{"agent", "{prompt}"}}},
	}
}

func openIssue(number int, created string, m *ghx.Milestone) ghx.Issue {
	return ghx.Issue{
		Number: number, Title: fmt.Sprintf("Issue %d", number), State: "OPEN",
		CreatedAt: created, Milestone: m,
		URL: fmt.Sprintf("https://github.com/acme/widgets/issues/%d", number),
	}
}

func closedIssue(number int, created string) ghx.Issue {
	iss := openIssue(number, created, nil)
	iss.State = "CLOSED"
	return iss
}

func labelled(number int, created, title string, labels ...string) ghx.Issue {
	iss := openIssue(number, created, nil)
	iss.Title = title
	for _, l := range labels {
		iss.Labels = append(iss.Labels, ghx.Label{Name: l})
	}
	return iss
}

func milestone(number int, title, due string) *ghx.Milestone {
	m := &ghx.Milestone{Number: number, Title: title, DueOn: &due}
	return m
}

func keys(issues []queuedIssue) []string {
	out := make([]string, len(issues))
	for i, iss := range issues {
		out[i] = iss.key()
	}
	return out
}

func withWorkspaces(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	prev := roots
	roots.Workspaces = dir
	t.Cleanup(func() { roots = prev })
	return dir
}

func writeIssueWorkspace(t *testing.T, root, name, repo string, number int) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	m := &manifest.Manifest{
		Name:        name,
		Description: "test",
		Issue:       &manifest.Issue{Repo: repo, Number: number},
	}
	if err := m.Write(dir); err != nil {
		t.Fatal(err)
	}
	return dir
}

func loadTempConfig(t *testing.T, body string) (queueConfig, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "q.json")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return loadQueueConfig(path)
}

func writeExe(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func appendScript(t *testing.T) string {
	t.Helper()
	return writeExe(t, "append", "#!/bin/sh\nprintf '%s\\n' \"$2\" >> \"$1\"\n")
}

func blockScript(t *testing.T) string {
	t.Helper()
	return writeExe(t, "block", "#!/bin/sh\nprintf '%s\\n' \"$2\" >> \"$1\"\ndir=$(dirname \"$1\")\nwhile [ ! -f \"$dir/release\" ]; do sleep 0.02; done\n")
}

func recordScript(t *testing.T) string {
	t.Helper()
	return writeExe(t, "record", "#!/bin/sh\nout=$1\nshift\n{\nprintf 'cwd=%s\\n' \"$(pwd)\"\nfor a in \"$@\"; do printf 'arg=%s\\n' \"$a\"; done\nprintf '%s\\n' ---\n} >> \"$out\"\n")
}

func gateScript(t *testing.T, count string, passOn int) string {
	t.Helper()
	body := fmt.Sprintf("#!/bin/sh\nf=%q\nn=0\nif [ -f \"$f\" ]; then n=$(cat \"$f\"); fi\nn=$((n + 1))\nprintf '%%s\\n' \"$n\" > \"$f\"\nif [ \"$n\" -lt %d ]; then exit 1; fi\nexit 0\n", count, passOn)
	return writeExe(t, "gate", body)
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	s := strings.TrimSuffix(readFile(t, path), "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatal(err)
	}
	return string(b)
}

type agentRecord struct {
	id, cwd, ws, prompt string
	n                   int
}

func parseRecords(t *testing.T, s string) []agentRecord {
	t.Helper()
	var out []agentRecord
	for _, block := range strings.Split(s, "---\n") {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		var rec agentRecord
		var args []string
		for _, line := range strings.Split(block, "\n") {
			if v, ok := strings.CutPrefix(line, "cwd="); ok {
				rec.cwd = v
				continue
			}
			if v, ok := strings.CutPrefix(line, "arg="); ok {
				args = append(args, v)
			}
		}
		if len(args) < 4 {
			t.Fatalf("record %q", block)
		}
		rec.id = args[0]
		if _, err := fmt.Sscan(args[1], &rec.n); err != nil {
			t.Fatal(err)
		}
		rec.ws = args[2]
		rec.prompt = args[3]
		out = append(out, rec)
	}
	return out
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func sameDir(a, b string) bool {
	aa, errA := filepath.EvalSymlinks(a)
	bb, errB := filepath.EvalSymlinks(b)
	if errA != nil || errB != nil {
		return a == b
	}
	return aa == bb
}
