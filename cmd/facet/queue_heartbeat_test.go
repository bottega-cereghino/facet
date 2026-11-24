package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bottega-cereghino/facet/internal/ghx"
)

func statusExitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr exitCodeError
	if errors.As(err, &exitErr) {
		return exitErr.code
	}
	return -1
}

func TestQueueStatusFreshHeartbeatExitsZero(t *testing.T) {
	dir := t.TempDir()
	hbPath := filepath.Join(dir, "heartbeat.json")
	now := time.Date(2026, 11, 23, 22, 0, 0, 0, time.UTC)
	hb := queueHeartbeat{
		At:      now.Add(-20 * time.Second),
		PID:     4242,
		Version: "v1.2.3",
		Running: []heartbeatRun{{Issue: "acme/widgets#7", Agent: "gemini", Since: now.Add(-2 * time.Minute)}},
		Held:    []string{},
	}
	if err := writeHeartbeatFile(hbPath, hb); err != nil {
		t.Fatal(err)
	}
	cfg := queueConfig{
		Wait:      30 * time.Second,
		Heartbeat: hbPath,
		Hold:      []string{filepath.Join(dir, "queue.hold")},
	}

	var text bytes.Buffer
	err := runQueueStatus(cfg, now, false, &text)
	if got := statusExitCode(err); got != 0 {
		t.Fatalf("fresh heartbeat exit = %d (err %v), want 0\noutput:\n%s", got, err, text.String())
	}
	for _, want := range []string{"state: ok", "pid: 4242", "version: v1.2.3", "running: 1", "acme/widgets#7 (gemini", "held: none", "last_error: none"} {
		if !strings.Contains(text.String(), want) {
			t.Errorf("text output missing %q:\n%s", want, text.String())
		}
	}

	var js bytes.Buffer
	err = runQueueStatus(cfg, now, true, &js)
	if got := statusExitCode(err); got != 0 {
		t.Fatalf("fresh heartbeat --json exit = %d (err %v), want 0", got, err)
	}
	var out queueStatusJSON
	if err := json.Unmarshal(js.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal --json: %v\n%s", err, js.String())
	}
	if out.State != "ok" || out.PID != 4242 || out.Version != "v1.2.3" || len(out.Running) != 1 {
		t.Fatalf("unexpected --json payload: %+v", out)
	}
	if out.AgeSeconds == nil || *out.AgeSeconds != 20 {
		t.Fatalf("age_seconds = %v, want 20", out.AgeSeconds)
	}
}

func TestQueueStatusOldHeartbeatExitsOne(t *testing.T) {
	dir := t.TempDir()
	hbPath := filepath.Join(dir, "heartbeat.json")
	now := time.Date(2026, 11, 23, 22, 0, 0, 0, time.UTC)
	hb := queueHeartbeat{
		At:        now.Add(-90 * time.Second), // exactly 3 * 30s -> not younger than 3 * wait
		PID:       4242,
		Version:   "dev",
		Running:   []heartbeatRun{},
		Held:      []string{},
		LastError: "list issues: boom",
	}
	if err := writeHeartbeatFile(hbPath, hb); err != nil {
		t.Fatal(err)
	}
	cfg := queueConfig{Wait: 30 * time.Second, Heartbeat: hbPath}

	var js bytes.Buffer
	err := runQueueStatus(cfg, now, true, &js)
	if got := statusExitCode(err); got != 1 {
		t.Fatalf("old heartbeat exit = %d (err %v), want 1", got, err)
	}
	var out queueStatusJSON
	if err := json.Unmarshal(js.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.State != "stale" || out.AgeSeconds == nil || *out.AgeSeconds != 90 || out.LastError != "list issues: boom" {
		t.Fatalf("unexpected stale payload: %+v", out)
	}
}

func TestQueueStatusHoldFileExitsTwoEvenWhenFresh(t *testing.T) {
	dir := t.TempDir()
	hbPath := filepath.Join(dir, "heartbeat.json")
	holdPath := filepath.Join(dir, "queue.hold")
	if err := os.WriteFile(holdPath, []byte("paused\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 23, 22, 0, 0, 0, time.UTC)
	hb := queueHeartbeat{
		At:      now.Add(-5 * time.Second),
		PID:     4242,
		Version: "dev",
		Running: []heartbeatRun{},
		Held:    []string{},
	}
	if err := writeHeartbeatFile(hbPath, hb); err != nil {
		t.Fatal(err)
	}
	cfg := queueConfig{
		Wait:      30 * time.Second,
		Heartbeat: hbPath,
		Hold:      []string{holdPath},
	}

	var js bytes.Buffer
	err := runQueueStatus(cfg, now, true, &js)
	if got := statusExitCode(err); got != 2 {
		t.Fatalf("held fresh heartbeat exit = %d (err %v), want 2", got, err)
	}
	var out queueStatusJSON
	if err := json.Unmarshal(js.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.State != "held" || len(out.Held) != 1 || out.Held[0] != holdPath {
		t.Fatalf("unexpected held payload: %+v", out)
	}
	if out.AgeSeconds == nil || *out.AgeSeconds != 5 {
		t.Fatalf("age_seconds = %v, want 5", out.AgeSeconds)
	}
}

func TestQueueStatusMissingFileExitsOne(t *testing.T) {
	dir := t.TempDir()
	hbPath := filepath.Join(dir, "missing.json")
	now := time.Date(2026, 11, 23, 22, 0, 0, 0, time.UTC)
	cfg := queueConfig{Wait: 30 * time.Second, Heartbeat: hbPath}

	var text bytes.Buffer
	err := runQueueStatus(cfg, now, false, &text)
	if got := statusExitCode(err); got != 1 {
		t.Fatalf("missing heartbeat exit = %d (err %v), want 1", got, err)
	}
	if !strings.Contains(text.String(), "state: missing") || !strings.Contains(text.String(), "(not found)") {
		t.Fatalf("unexpected missing output:\n%s", text.String())
	}

	var js bytes.Buffer
	err = runQueueStatus(cfg, now, true, &js)
	if got := statusExitCode(err); got != 1 {
		t.Fatalf("missing heartbeat --json exit = %d (err %v), want 1", got, err)
	}
	var out queueStatusJSON
	if err := json.Unmarshal(js.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.State != "missing" || out.AgeSeconds != nil {
		t.Fatalf("unexpected missing --json payload: %+v", out)
	}
}

func TestQueueWritesHeartbeatAtomicallyOnEachTickAndRunLifecycle(t *testing.T) {
	dir := t.TempDir()
	hbPath := filepath.Join(dir, "heartbeat.json")

	var synced atomic.Int32
	prevSync := syncFile
	syncFile = func(f *os.File) error {
		synced.Add(1)
		return prevSync(f)
	}
	t.Cleanup(func() { syncFile = prevSync })

	cfg := queueConfig{
		Max:       1,
		PerRepo:   1,
		Wait:      15 * time.Millisecond,
		Label:     "next",
		Repos:     []string{"acme/widgets"},
		Prompt:    "{issues}",
		Agents:    []queueAgent{{Label: "", Command: []string{"agent", "{prompt}"}, Bundle: 1}},
		Effort:    "medium",
		Heartbeat: hbPath,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var (
		mu         sync.Mutex
		listedOnce bool
		started    = make(chan struct{})
		release    = make(chan struct{})
		sawRunning bool
	)

	env := queueEnv{
		list: func(string, string) ([]ghx.Issue, error) {
			mu.Lock()
			first := !listedOnce
			listedOnce = true
			mu.Unlock()
			if first {
				return []ghx.Issue{{Number: 12, Title: "heartbeat check", State: "OPEN", CreatedAt: "2026-11-23T20:00:00Z"}}, nil
			}
			return nil, nil
		},
		spawn: func(string, int) (string, error) { return dir, nil },
		run: func(_ context.Context, _ string, _ []string) error {
			close(started)
			<-release
			return nil
		},
		out: &bytes.Buffer{},
	}

	done := make(chan error, 1)
	go func() {
		done <- runQueue(ctx, cfg, env)
	}()

	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("agent did not start")
	}

	hb, found, err := readHeartbeatFile(hbPath)
	if err != nil || !found {
		t.Fatalf("heartbeat while running: found=%v err=%v", found, err)
	}
	if hb.PID != os.Getpid() || hb.Version == "" {
		t.Fatalf("unexpected pid/version: %+v", hb)
	}
	if len(hb.Running) == 1 && hb.Running[0].Issue == "acme/widgets#12" && hb.Running[0].Agent == "default" && !hb.Running[0].Since.IsZero() {
		sawRunning = true
	}
	if !sawRunning {
		t.Fatalf("expected running entry for acme/widgets#12, got %+v", hb.Running)
	}

	close(release)
	waitFor(t, "heartbeat clears running after exit", func() bool {
		got, ok, readErr := readHeartbeatFile(hbPath)
		return readErr == nil && ok && len(got.Running) == 0
	})

	firstAt := func() time.Time {
		got, _, _ := readHeartbeatFile(hbPath)
		return got.At
	}()
	waitFor(t, "heartbeat advances on subsequent tick", func() bool {
		got, ok, readErr := readHeartbeatFile(hbPath)
		return readErr == nil && ok && got.At.After(firstAt)
	})

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("runQueue returned %v", err)
	}

	if synced.Load() < 2 {
		t.Fatalf("expected atomic syncFile calls, got %d", synced.Load())
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") {
			t.Errorf("leftover temp file %q after atomic heartbeat write", e.Name())
		}
	}
}

func TestQueueHeartbeatPulsesWhileGateRuns(t *testing.T) {
	dir := t.TempDir()
	hbPath := filepath.Join(dir, "heartbeat.json")
	cfg := queueConfig{
		Max:       1,
		PerRepo:   1,
		Wait:      15 * time.Millisecond,
		Label:     "next",
		Repos:     []string{"acme/widgets"},
		Prompt:    "{issues}",
		Gate:      []string{"gate"},
		Agents:    []queueAgent{{Label: "", Command: []string{"agent", "{prompt}"}, Bundle: 1}},
		Effort:    "medium",
		Heartbeat: hbPath,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	gateEntered := make(chan struct{})
	gateRelease := make(chan struct{})
	env := queueEnv{
		list: func(string, string) ([]ghx.Issue, error) {
			return []ghx.Issue{{Number: 9, Title: "slow gate", State: "OPEN", CreatedAt: "2026-11-23T20:00:00Z"}}, nil
		},
		spawn: func(string, int) (string, error) { return dir, nil },
		run: func(_ context.Context, _ string, argv []string) error {
			if argv[0] == "gate" {
				close(gateEntered)
				<-gateRelease
				cancel()
				return errors.New("cancelled")
			}
			return nil
		},
		out: &bytes.Buffer{},
	}

	done := make(chan error, 1)
	go func() {
		done <- runQueue(ctx, cfg, env)
	}()

	select {
	case <-gateEntered:
	case <-time.After(3 * time.Second):
		t.Fatal("gate did not start")
	}

	initial, found, err := readHeartbeatFile(hbPath)
	if err != nil || !found {
		t.Fatalf("heartbeat before gate pulse: found=%v err=%v", found, err)
	}
	waitFor(t, "heartbeat refreshed while gate runs", func() bool {
		got, ok, readErr := readHeartbeatFile(hbPath)
		return readErr == nil && ok && got.At.After(initial.At)
	})

	close(gateRelease)
	if err := <-done; err != nil {
		t.Fatalf("runQueue returned %v", err)
	}
}

type panicGH struct{ ghx.Client }

func (panicGH) OpenIssues(string, string) ([]ghx.Issue, error) {
	panic("status must not call GitHub")
}

func TestQueueStatusMakesNoNetworkCallAndRunsNoGate(t *testing.T) {
	dir := t.TempDir()
	hbPath := filepath.Join(dir, "heartbeat.json")
	cfgPath := filepath.Join(dir, "queue.json")
	marker := filepath.Join(dir, "gate-ran")

	if err := writeHeartbeatFile(hbPath, queueHeartbeat{
		At:      time.Now().UTC(),
		PID:     os.Getpid(),
		Version: "dev",
		Running: []heartbeatRun{},
		Held:    []string{},
	}); err != nil {
		t.Fatal(err)
	}
	rawCfg := `{
  "wait": "30s",
  "heartbeat": "heartbeat.json",
  "gate": ["sh", "-c", "touch ` + marker + ` && exit 1"],
  "repos": ["acme/widgets"],
  "agents": [{"label": "", "command": ["agent", "{prompt}"]}]
}`
	if err := os.WriteFile(cfgPath, []byte(rawCfg), 0o644); err != nil {
		t.Fatal(err)
	}

	prevGH := gh
	gh = panicGH{}
	t.Cleanup(func() { gh = prevGH })

	cmd := newQueueStatusCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"--config", cfgPath, "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("facet queue status failed: %v\noutput:\n%s", err, out.String())
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("gate command ran during facet queue status")
	}
	var payload queueStatusJSON
	if err := json.Unmarshal(out.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal status json: %v\n%s", err, out.String())
	}
	if payload.State != "ok" {
		t.Fatalf("state = %q, want ok", payload.State)
	}
}
