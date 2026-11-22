package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"time"

	"github.com/bottega-cereghino/facet/internal/ghx"
)

// queueEnv is the seams the loop needs. list and spawn are the only fakes a
// test has to supply: the agent and the gate are commands, run by runArgv.
type queueEnv struct {
	list  func(repo, label string) ([]ghx.Issue, error)
	spawn func(repo string, number int) (string, error)
	run   func(ctx context.Context, dir string, argv []string) error
	now   func() time.Time
	out   io.Writer
}

func realQueueEnv() queueEnv {
	return queueEnv{
		list: func(repo, label string) ([]ghx.Issue, error) {
			return gh.OpenIssues(repo, label)
		},
		spawn: prepareWorkspace,
		run:   runArgv,
	}
}

func (e *queueEnv) fill() {
	if e.now == nil {
		e.now = time.Now
	}
	if e.out == nil {
		e.out = os.Stdout
	}
	if e.run == nil {
		e.run = runArgv
	}
}

// runArgv starts argv[0] with the remaining arguments and waits for it.
// It does not use a shell. The issue title is substituted into the prompt
// before this is called, and a title is someone else's text: a shell would
// turn it into a command. An empty dir keeps the current working directory,
// which is what the gate wants; an agent is given its workspace.
func runArgv(ctx context.Context, dir string, argv []string) error {
	if len(argv) == 0 || argv[0] == "" {
		return fmt.Errorf("empty command\nfix: set the command in the queue config")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

type queueRunner struct {
	cfg      queueConfig
	env      queueEnv
	mu       sync.Mutex
	inflight map[string]bool
	done     map[string]bool
	retryAt  map[string]time.Time
	wake     chan struct{}
	wg       sync.WaitGroup
}

func runQueue(ctx context.Context, cfg queueConfig, env queueEnv) error {
	env.fill()
	r := &queueRunner{
		cfg:      cfg,
		env:      env,
		inflight: map[string]bool{},
		done:     map[string]bool{},
		retryAt:  map[string]time.Time{},
		wake:     make(chan struct{}, 1),
	}
	r.logf("queue: up to %d agent(s), label %s, order p1 then p2 then p3 then oldest", cfg.Max, cfg.Label)
	err := r.loop(ctx)
	r.wg.Wait()
	return err
}

func (r *queueRunner) loop(ctx context.Context) error {
	for {
		stop, err := r.tick(ctx)
		if err != nil || stop {
			return err
		}
	}
}

// tick starts at most one agent, or waits. stop means the queue should exit.
func (r *queueRunner) tick(ctx context.Context) (bool, error) {
	if stop, err := r.finishIfStopped(ctx); stop || err != nil {
		return true, err
	}
	if r.full() {
		r.pause(ctx)
		return false, nil
	}
	iss, err := r.candidate()
	if err != nil {
		r.logf("queue: %s", err.Error())
		r.pause(ctx)
		return false, nil
	}
	if iss == nil {
		r.logf("queue: no open issue labelled %s; waiting %s", r.cfg.Label, r.cfg.Wait)
		r.pause(ctx)
		return false, nil
	}
	if err := r.gate(ctx); err != nil {
		if ctx.Err() != nil {
			r.logf("queue: stopping")
			return true, nil
		}
		r.logf("queue: gate: %s; waiting %s", err.Error(), r.cfg.Wait)
		r.pause(ctx)
		return false, nil
	}
	if stop, err := r.finishIfStopped(ctx); stop || err != nil {
		return true, err
	}
	r.start(ctx, *iss)
	return false, nil
}

func (r *queueRunner) finishIfStopped(ctx context.Context) (bool, error) {
	if ctx.Err() != nil {
		r.logf("queue: stopping")
		return true, nil
	}
	held, err := holdFile(r.cfg.Hold)
	if err != nil {
		return true, err
	}
	if held {
		r.logf("queue: hold file %s exists; starting nothing", r.cfg.Hold)
		return true, nil
	}
	return false, nil
}

func holdFile(path string) (bool, error) {
	if path == "" {
		return false, nil
	}
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, fmt.Errorf("hold file %s: %w\nfix: the queue stops when it cannot tell whether the hold file exists", path, err)
}

func (r *queueRunner) full() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.inflight) >= r.cfg.Max
}

func (r *queueRunner) candidate() (*queuedIssue, error) {
	issues, err := r.collect()
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.env.now()
	for _, iss := range orderQueued(issues) {
		if r.blocked(iss, now) {
			continue
		}
		picked := iss
		return &picked, nil
	}
	return nil, nil
}

// collect asks every repository. One failure fails the round: starting while
// a repository could not be listed would skip an older issue the queue
// cannot see.
func (r *queueRunner) collect() ([]queuedIssue, error) {
	var (
		all  []queuedIssue
		errs []error
	)
	for _, repo := range r.cfg.Repos {
		got, err := r.env.list(repo, r.cfg.Label)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", repo, err))
			continue
		}
		if len(got) == ghx.OpenIssueLimit {
			r.logf("queue: %s returned %d open issues labelled %s, the cap, so any beyond it are unseen", repo, len(got), r.cfg.Label)
		}
		for _, iss := range got {
			if !iss.IsOpen() {
				continue
			}
			all = append(all, queuedIssue{Repo: repo, Issue: iss})
		}
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("list issues: %w\nfix: the queue will not start while a repository cannot be listed", errors.Join(errs...))
	}
	return all, nil
}

// blocked reads the runner maps. The caller holds r.mu.
func (r *queueRunner) blocked(iss queuedIssue, now time.Time) bool {
	k := iss.key()
	if r.done[k] || r.inflight[k] {
		return true
	}
	until, ok := r.retryAt[k]
	return ok && now.Before(until)
}

func (r *queueRunner) gate(ctx context.Context) error {
	if len(r.cfg.Gate) == 0 {
		return nil
	}
	return r.env.run(ctx, "", r.cfg.Gate)
}

func (r *queueRunner) start(ctx context.Context, iss queuedIssue) {
	key := iss.key()
	r.mu.Lock()
	r.inflight[key] = true
	r.mu.Unlock()
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		defer r.poke()
		defer func() {
			r.mu.Lock()
			delete(r.inflight, key)
			r.mu.Unlock()
		}()
		r.runOne(ctx, iss)
	}()
}

func (r *queueRunner) runOne(ctx context.Context, iss queuedIssue) {
	if ctx.Err() != nil {
		return
	}
	ws, err := r.env.spawn(iss.Repo, iss.Issue.Number)
	if err != nil {
		r.logf("queue: spawn %s#%d: %v", iss.Repo, iss.Issue.Number, err)
		r.deferRetry(iss)
		return
	}
	if ctx.Err() != nil {
		return
	}
	held, err := holdFile(r.cfg.Hold)
	if err != nil || held {
		return
	}
	argv, who, err := r.argvFor(iss, ws)
	if err != nil {
		r.logf("queue: %s#%d: %v", iss.Repo, iss.Issue.Number, err)
		r.deferRetry(iss)
		return
	}
	r.logf("queue: %s#%d agent %s in %s", iss.Repo, iss.Issue.Number, who, ws)
	err = r.env.run(ctx, ws, argv)
	if err == nil {
		r.logf("queue: %s#%d agent exited", iss.Repo, iss.Issue.Number)
		r.markDone(iss)
		return
	}
	// An ExitError means the process ran and finished, whatever its status.
	// This process does not run that issue again. Anything else means the
	// command never started (the binary is missing, or it cannot be executed),
	// which is not the agent finishing the issue.
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		r.logf("queue: %s#%d agent exited: %v", iss.Repo, iss.Issue.Number, err)
		r.markDone(iss)
		return
	}
	r.logf("queue: %s#%d did not start: %v", iss.Repo, iss.Issue.Number, err)
	if ctx.Err() != nil {
		return
	}
	r.deferRetry(iss)
}

func (r *queueRunner) argvFor(iss queuedIssue, ws string) ([]string, string, error) {
	label, cmd, err := r.cfg.commandFor(iss.Issue.LabelNames())
	if err != nil {
		return nil, "", err
	}
	vars := map[string]string{
		"repo":      iss.Repo,
		"number":    strconv.Itoa(iss.Issue.Number),
		"title":     iss.Issue.Title,
		"workspace": ws,
		"label":     r.cfg.Label,
		"url":       iss.Issue.URL,
	}
	// prompt is expanded first, then dropped in as a single value, so a title
	// is not scanned for further placeholders.
	prompt := expand(r.cfg.Prompt, vars)
	vars["prompt"] = prompt
	out := make([]string, len(cmd))
	for i, a := range cmd {
		out[i] = expand(a, vars)
	}
	who := label
	if who == "" {
		who = "default"
	}
	return out, who, nil
}

func (r *queueRunner) deferRetry(iss queuedIssue) {
	r.mu.Lock()
	r.retryAt[iss.key()] = r.env.now().Add(r.cfg.Wait)
	r.mu.Unlock()
}

func (r *queueRunner) markDone(iss queuedIssue) {
	r.mu.Lock()
	r.done[iss.key()] = true
	r.mu.Unlock()
}

func (r *queueRunner) pause(ctx context.Context) {
	t := time.NewTimer(r.cfg.Wait)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-r.wake:
	case <-t.C:
	}
}

func (r *queueRunner) poke() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

func (r *queueRunner) logf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, _ = fmt.Fprintf(r.env.out, format+"\n", args...)
}
