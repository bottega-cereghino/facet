package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bottega-cereghino/facet/internal/ghx"
	"github.com/bottega-cereghino/facet/internal/manifest"
)

func TestQueueDiskLowFreeSpaceStartsNoNewRunButFixes(t *testing.T) {
	root := withWorkspaces(t)
	writeIssueWorkspace(t, root, "iss-1", "acme/widgets", 1)
	log := filepath.Join(t.TempDir(), "log")
	cfg := fixQueue(t, log)
	cfg.Disk = &queueDisk{Path: root, MinFreeGB: 20}
	gateLog := filepath.Join(t.TempDir(), "gate")
	cfg.Gate = []string{writeExe(t, "gate-mark", "#!/bin/sh\nprintf '%s\\n' \"$2\" >> \"$1\"\n"), gateLog, "{number}"}
	seedState(t, cfg.State, []int{1}, nil)
	wsRoot := t.TempDir()
	var other atomic.Int32
	s := startQueue(t, cfg, queueEnv{
		list: func(string, string) ([]ghx.Issue, error) {
			return []ghx.Issue{openIssue(5, "2026-01-02T00:00:00Z")}, nil
		},
		prs: staticPRs(redPR(11, "feature/1-bell")),
		spawn: func(_ string, n int) (string, error) {
			if n != 1 {
				other.Add(1)
			}
			return os.MkdirTemp(wsRoot, "ws")
		},
		diskFree: func(string) (int64, error) { return 18 * diskGB, nil },
	})
	waitFor(t, "the fix", func() bool {
		for _, line := range readLines(t, log) {
			if strings.HasPrefix(line, "fix 1 ") {
				return true
			}
		}
		return false
	})
	time.Sleep(300 * time.Millisecond)
	s.stop(t)
	if other.Load() != 0 {
		t.Fatalf("spawned an issue other than the fix")
	}
	for _, line := range readLines(t, log) {
		if strings.HasPrefix(line, "start ") {
			t.Fatalf("new run started: %q", readLines(t, log))
		}
	}
	gateLines := readLines(t, gateLog)
	if len(gateLines) == 0 {
		t.Fatal("fix gate did not run")
	}
	for _, line := range gateLines {
		if line != "1" {
			t.Fatalf("gate saw %q, want only the fix's issue", gateLines)
		}
	}
	if got := s.buf.String(); strings.Count(got, "disk: 18 GB free, below 20: starting no new work") != 1 {
		t.Fatalf("log =\n%s", got)
	}
}

func TestQueueDiskAtTheWorkspaceLimitStartsNoNewRunButFixes(t *testing.T) {
	root := withWorkspaces(t)
	writeIssueWorkspace(t, root, "iss-1", "acme/widgets", 1)
	if err := os.MkdirAll(filepath.Join(root, "iss-empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	plain := filepath.Join(root, "notes")
	if err := os.MkdirAll(plain, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := (&manifest.Manifest{Name: "notes", Description: "not an issue"}).Write(plain); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(t.TempDir(), "log")
	cfg := fixQueue(t, log)
	limit := 1
	cfg.Disk = &queueDisk{Path: root, MinFreeGB: 0, MaxWorkspaces: &limit}
	seedState(t, cfg.State, []int{1}, nil)
	wsRoot := t.TempDir()
	var other atomic.Int32
	s := startQueue(t, cfg, queueEnv{
		list: func(string, string) ([]ghx.Issue, error) {
			return []ghx.Issue{openIssue(5, "2026-01-02T00:00:00Z")}, nil
		},
		prs: staticPRs(redPR(11, "feature/1-bell")),
		spawn: func(_ string, n int) (string, error) {
			if n != 1 {
				other.Add(1)
			}
			return os.MkdirTemp(wsRoot, "ws")
		},
		diskFree: func(string) (int64, error) { return 0, nil },
	})
	waitFor(t, "the fix", func() bool {
		for _, line := range readLines(t, log) {
			if strings.HasPrefix(line, "fix 1 ") {
				return true
			}
		}
		return false
	})
	time.Sleep(300 * time.Millisecond)
	s.stop(t)
	if other.Load() != 0 {
		t.Fatalf("spawned an issue other than the fix")
	}
	for _, line := range readLines(t, log) {
		if strings.HasPrefix(line, "start ") {
			t.Fatalf("new run started: %q", readLines(t, log))
		}
	}
	got := s.buf.String()
	if strings.Count(got, "disk: 1 issue workspace, limit 1: starting no new work") != 1 || strings.Contains(got, "GB free") {
		t.Fatalf("log =\n%s", got)
	}
}

func TestQueueDiskDoesNotSpawnAMissingFixWorkspace(t *testing.T) {
	root := withWorkspaces(t)
	log := filepath.Join(t.TempDir(), "log")
	cfg := fixQueue(t, log)
	cfg.Disk = &queueDisk{Path: root, MinFreeGB: 20}
	seedState(t, cfg.State, []int{1}, nil)
	wsRoot := t.TempDir()
	var spawned atomic.Int32
	s := startQueue(t, cfg, queueEnv{
		list: func(string, string) ([]ghx.Issue, error) {
			return []ghx.Issue{openIssue(5, "2026-01-02T00:00:00Z")}, nil
		},
		prs:      staticPRs(redPR(11, "feature/1-bell")),
		spawn:    func(string, int) (string, error) { spawned.Add(1); return os.MkdirTemp(wsRoot, "ws") },
		diskFree: func(string) (int64, error) { return 18 * diskGB, nil },
	})
	waitFor(t, "the disk refusal", func() bool {
		return strings.Contains(s.buf.String(), "would spawn a workspace")
	})
	time.Sleep(300 * time.Millisecond)
	s.stop(t)
	if spawned.Load() != 0 || len(readLines(t, log)) != 0 {
		t.Fatalf("spawned %d, log %q, queue =\n%s", spawned.Load(), readLines(t, log), s.buf.String())
	}
	if strings.Count(s.buf.String(), "disk: fix acme/widgets#1 would spawn a workspace; leaving it") != 1 {
		t.Fatalf("log =\n%s", s.buf.String())
	}
}

func TestQueueDiskClearingResumesStarts(t *testing.T) {
	root := withWorkspaces(t)
	dir := t.TempDir()
	log := filepath.Join(dir, "ran")
	decisions := filepath.Join(dir, "decisions.jsonl")
	cfg := oneAgentConfig([]string{"acme/widgets"})
	cfg.Agents = []queueAgent{{Command: []string{appendScript(t), log, "{number}"}}}
	cfg.Disk = &queueDisk{Path: root, MinFreeGB: 20}
	cfg.Decisions = decisions
	free := &atomic.Int64{}
	free.Store(18)
	fixed := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	var spawned atomic.Int32
	s := startQueue(t, cfg, queueEnv{
		list: func(string, string) ([]ghx.Issue, error) {
			return []ghx.Issue{openIssue(7, "2026-01-01T00:00:00Z")}, nil
		},
		spawn:    func(string, int) (string, error) { spawned.Add(1); return os.MkdirTemp(dir, "ws") },
		diskFree: func(string) (int64, error) { return free.Load() * diskGB, nil },
		now:      func() time.Time { return fixed },
	})
	low := "disk: 18 GB free, below 20: starting no new work"
	waitFor(t, "the refusal", func() bool { return strings.Contains(s.buf.String(), low) })
	time.Sleep(300 * time.Millisecond)
	if spawned.Load() != 0 || strings.Count(s.buf.String(), low) != 1 {
		t.Fatalf("spawned %d, log =\n%s", spawned.Load(), s.buf.String())
	}
	free.Store(40)
	waitFor(t, "the start", func() bool {
		return len(readLines(t, log)) >= 1 && strings.Contains(s.buf.String(), "disk: clear")
	})
	time.Sleep(300 * time.Millisecond)
	if spawned.Load() != 1 || strings.Count(s.buf.String(), "disk: clear") != 1 || strings.Count(s.buf.String(), low) != 1 {
		t.Fatalf("spawned %d, log =\n%s", spawned.Load(), s.buf.String())
	}
	free.Store(10)
	again := "disk: 10 GB free, below 20: starting no new work"
	waitFor(t, "the second refusal", func() bool { return strings.Contains(s.buf.String(), again) })
	time.Sleep(300 * time.Millisecond)
	s.stop(t)
	got := s.buf.String()
	if strings.Count(got, low) != 1 || strings.Count(got, "disk: clear") != 1 || strings.Count(got, again) != 1 || spawned.Load() != 1 {
		t.Fatalf("spawned %d, log =\n%s", spawned.Load(), got)
	}
	rows, raws := readDecisionLines(t, decisions)
	var diskRows []queueDecision
	var diskRaw []map[string]any
	for i, row := range rows {
		if row.Decision == "disk" || row.Decision == "disk_clear" {
			diskRows = append(diskRows, row)
			diskRaw = append(diskRaw, raws[i])
		}
	}
	want := []string{"disk", "disk_clear", "disk"}
	why := []string{low, "disk: clear", again}
	if len(diskRows) != len(want) {
		t.Fatalf("disk decisions = %+v", diskRows)
	}
	for i := range want {
		if diskRows[i].Decision != want[i] || diskRows[i].Why != why[i] || diskRows[i].Source != "queue" || diskRows[i].At != "2026-11-24T12:00:00Z" || diskRows[i].Repo != "" || diskRows[i].Issue != "" || diskRows[i].Lane != "" {
			t.Fatalf("row %d = %+v", i, diskRows[i])
		}
		for _, key := range []string{"at", "repo", "issue", "lane", "decision", "why", "source"} {
			if _, ok := diskRaw[i][key]; !ok {
				t.Errorf("row %d missing %s", i, key)
			}
		}
	}
}

func TestQueueDiskOmittedChangesNothing(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "ran")
	cfg := oneAgentConfig([]string{"acme/widgets"})
	cfg.Agents = []queueAgent{{Command: []string{appendScript(t), log, "{number}"}}}
	var spawned atomic.Int32
	s := startQueue(t, cfg, queueEnv{
		list: func(string, string) ([]ghx.Issue, error) {
			return []ghx.Issue{openIssue(3, "2026-01-01T00:00:00Z")}, nil
		},
		spawn: func(string, int) (string, error) { spawned.Add(1); return os.MkdirTemp(dir, "ws") },
	})
	waitFor(t, "the start", func() bool { return len(readLines(t, log)) >= 1 })
	s.stop(t)
	if spawned.Load() != 1 || strings.Contains(s.buf.String(), "disk:") {
		t.Fatalf("spawned %d, log =\n%s", spawned.Load(), s.buf.String())
	}

	plain := oneAgentConfig([]string{"acme/widgets"})
	plain.Max = 1
	list := func(string, string) ([]ghx.Issue, error) {
		return []ghx.Issue{openIssue(1, "2026-01-01T00:00:00Z")}, nil
	}
	// Both runs share one clock, so no line differs by its timestamp.
	fixed := func() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) }
	var without, with bytes.Buffer
	if err := testDryQueue(context.Background(), plain, queueEnv{list: list, out: &without, now: fixed}); err != nil {
		t.Fatal(err)
	}
	watched := plain
	watched.Disk = &queueDisk{Path: t.TempDir(), MinFreeGB: 20}
	if err := testDryQueue(context.Background(), watched, queueEnv{
		list:     list,
		out:      &with,
		now:      fixed,
		diskFree: func(string) (int64, error) { return 80 * diskGB, nil },
	}); err != nil {
		t.Fatal(err)
	}
	if without.String() != with.String() || !strings.Contains(without.String(), "2026-01-02T03:04:05Z") || !strings.Contains(without.String(), "would start acme/widgets#1") || strings.Contains(with.String(), "disk:") {
		t.Fatalf("without =\n%s\nwith =\n%s", without.String(), with.String())
	}
}

func TestQueueDiskDryRunPrintsTheRefusalAndTheStarts(t *testing.T) {
	root := withWorkspaces(t)
	writeIssueWorkspace(t, root, "iss-1", "acme/widgets", 1)
	cfg := fixQueue(t, filepath.Join(t.TempDir(), "unused"))
	cfg.Max = 4
	cfg.Disk = &queueDisk{Path: root, MinFreeGB: 20}
	cfg.Decisions = filepath.Join(t.TempDir(), "decisions.jsonl")
	seedState(t, cfg.State, []int{1, 2, 3}, nil)
	var buf bytes.Buffer
	var spawned atomic.Int32
	err := testDryQueue(context.Background(), cfg, queueEnv{
		list: func(string, string) ([]ghx.Issue, error) {
			return []ghx.Issue{openIssue(9, "2026-01-04T00:00:00Z")}, nil
		},
		prs: staticPRs(
			redPR(11, "feature/1-bell"),
			redPR(12, "feature/2-horn"),
			landPR(13, "feature/3-bell", "BEHIND"),
		),
		spawn:    func(string, int) (string, error) { spawned.Add(1); return "", nil },
		diskFree: func(string) (int64, error) { return 18 * diskGB, nil },
		out:      &buf,
	})
	got := buf.String()
	if err != nil || spawned.Load() != 0 {
		t.Fatalf("err %v, spawned %d, dry-run =\n%s", err, spawned.Load(), got)
	}
	for _, want := range []string{
		"disk: 18 GB free, below 20: starting no new work",
		"dry-run: would update acme/widgets#3 PR #13: merge main into it",
		"dry-run: would fix acme/widgets#1 PR #11:",
		"dry-run: would not fix acme/widgets#2 PR #12: it would spawn a workspace",
		"dry-run: would have started acme/widgets#9",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q, dry-run =\n%s", want, got)
		}
	}
	if strings.Contains(got, "would start ") {
		t.Fatalf("listed a start that would happen, dry-run =\n%s", got)
	}
	if _, err := os.Stat(cfg.Decisions); !os.IsNotExist(err) {
		t.Fatalf("dry-run wrote the decision log: %v", err)
	}
}

func TestQueueDiskCountsIssueWorkspacesOnly(t *testing.T) {
	root := withWorkspaces(t)
	writeIssueWorkspace(t, root, "iss-a", "acme/widgets", 1)
	writeIssueWorkspace(t, root, "iss-b", "acme/other", 2)
	if err := os.MkdirAll(filepath.Join(root, "iss-empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	plain := filepath.Join(root, "notes")
	if err := os.MkdirAll(plain, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := (&manifest.Manifest{Name: "notes", Description: "not an issue"}).Write(plain); err != nil {
		t.Fatal(err)
	}
	topical := filepath.Join(root, "topical")
	if err := os.MkdirAll(topical, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := (&manifest.Manifest{Name: "topical", Description: "x", Issue: &manifest.Issue{Repo: "acme/widgets", Number: 9}}).Write(topical); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "iss-a"), filepath.Join(root, "iss-link")); err != nil {
		t.Fatal(err)
	}
	n, have, err := issueWorkspaces(root)
	if err != nil || n != 2 || !have["acme/widgets#1"] || !have["acme/other#2"] || have["acme/widgets#9"] {
		t.Fatalf("n=%d have=%v err=%v", n, have, err)
	}

	volume := t.TempDir()
	limit := 2
	var saw string
	r := &queueRunner{cfg: queueConfig{Disk: &queueDisk{Path: volume, MinFreeGB: 1, MaxWorkspaces: &limit}}, env: queueEnv{
		diskFree: func(path string) (int64, error) {
			saw = path
			return 80 * diskGB, nil
		},
		out: &bytes.Buffer{},
	}}
	view := r.measureDisk()
	if saw != volume || !view.block || view.line != "disk: 2 issue workspaces, limit 2: starting no new work" || !view.have["acme/widgets#1"] {
		t.Fatalf("path %q, view %+v", saw, view)
	}
	r.cfg.Disk.MinFreeGB = 100
	r.env.diskFree = func(string) (int64, error) { return 18 * diskGB, nil }
	both := r.measureDisk()
	if both.line != "disk: 18 GB free, below 100, and 2 issue workspaces, limit 2: starting no new work" {
		t.Fatalf("both = %q", both.line)
	}
}

func TestQueueDiskConfig(t *testing.T) {
	base := `"repos": ["acme/widgets"], "agents": [{"command": ["agent", "{prompt}"]}]`
	none, err := loadTempConfig(t, `{`+base+`}`)
	if err != nil || none.Disk != nil {
		t.Fatalf("omitted disk = %+v, %v", none.Disk, err)
	}
	def, err := loadTempConfig(t, `{"disk": {}, `+base+`}`)
	if err != nil || def.Disk == nil || def.Disk.MinFreeGB != 20 || def.Disk.MaxWorkspaces != nil || def.Disk.Path != "" {
		t.Fatalf("defaults = %+v, %v", def.Disk, err)
	}
	set, err := loadTempConfig(t, `{"disk": {"path": "vol", "minFreeGB": 0, "maxWorkspaces": 4}, `+base+`}`)
	if err != nil || set.Disk == nil || set.Disk.MinFreeGB != 0 || set.Disk.MaxWorkspaces == nil || *set.Disk.MaxWorkspaces != 4 || !strings.HasSuffix(set.Disk.Path, string(filepath.Separator)+"vol") {
		t.Fatalf("set = %+v, %v", set.Disk, err)
	}
	for _, tc := range []struct{ body, want string }{
		{`{"disk": {"minFreeGB": -1}, ` + base + `}`, "minFreeGB"},
		{`{"disk": {"maxWorkspaces": 0}, ` + base + `}`, "maxWorkspaces"},
		{`{"disk": {"nope": 1}, ` + base + `}`, "nope"},
	} {
		_, err := loadTempConfig(t, tc.body)
		if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "fix:") {
			t.Errorf("%s: %v", tc.want, err)
		}
	}
}

func TestDiskFreeBytesReadsADirectory(t *testing.T) {
	n, err := diskFreeBytes(t.TempDir())
	if err != nil || n < 0 {
		t.Fatalf("free = %d, %v", n, err)
	}
	if _, err := diskFreeBytes(filepath.Join(t.TempDir(), "missing")); err == nil || !strings.Contains(err.Error(), "fix:") {
		t.Fatalf("missing path: %v", err)
	}
}
