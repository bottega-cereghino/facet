package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bottega-cereghino/facet/internal/ghx"
)

// queueEnv is the seams the loop needs. list and spawn are the only fakes a
// test has to supply: the agent and the gate are commands, run by runArgv.
type queueEnv struct {
	list func(repo, label string) ([]ghx.Issue, error)
	// prBranches lists the head branches of a repository's open pull
	// requests. Nil skips the check.
	prBranches func(repo string) ([]string, error)
	spawn      func(repo string, number int) (string, error)
	// route runs spawn's routing checks for a repository, without the
	// network. Only --dry-run calls it; nil skips it.
	route func(repo string) error
	run   func(ctx context.Context, dir string, argv []string) error
	now   func() time.Time
	out   io.Writer
}

func realQueueEnv() queueEnv {
	return queueEnv{
		list: func(repo, label string) ([]ghx.Issue, error) {
			return gh.OpenIssues(repo, label)
		},
		prBranches: ghx.CLI{}.OpenPRBranches,
		spawn:      prepareWorkspace,
		route: func(repo string) error {
			_, _, err := spawnRoute(repo)
			return err
		},
		run: runArgv,
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
//
// The command runs in a process group of its own. An agent, or something it
// starts, that signals its own group would otherwise stop the queue too:
// under launchd the queue then logged "stopping" after each agent and was
// restarted (#420). Stopping the queue still stops the command: the cancel
// sends SIGTERM to the whole group, and SIGKILL follows if it has not exited
// within queueStopGrace.
func runArgv(ctx context.Context, dir string, argv []string) error {
	if len(argv) == 0 || argv[0] == "" {
		return fmt.Errorf("empty command\nfix: set the command in the queue config")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	}
	cmd.WaitDelay = queueStopGrace
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// queueStopGrace is how long a command has to exit after the queue stops
// it, before it is killed.
const queueStopGrace = 10 * time.Second

type queueRunner struct {
	cfg      queueConfig
	env      queueEnv
	mu       sync.Mutex
	inflight map[string]bool
	running  int
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
	if cfg.Priorities != "" {
		r.logf("queue: up to %d agent(s), label %s, projects from %s", cfg.Max, cfg.Label, cfg.Priorities)
	} else {
		r.logf("queue: up to %d agent(s), label %s, order p1 then p2 then p3 then oldest", cfg.Max, cfg.Label)
	}
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
	run, err := r.candidate()
	if err != nil {
		r.logf("queue: %s", err.Error())
		r.pause(ctx)
		return false, nil
	}
	if len(run) == 0 {
		r.logf("queue: no open issue labelled %s to start; waiting %s", r.cfg.Label, r.cfg.Wait)
		r.pause(ctx)
		return false, nil
	}
	// Effort is resolved before the gate. A disagreement is not an issue to
	// start, and the gate would otherwise judge a run that never begins.
	effort, err := r.cfg.effortFor(run[0].Issue.LabelNames())
	if err != nil {
		r.logf("queue: %s#%d: %v", run[0].Repo, run[0].Issue.Number, err)
		r.deferAll(run, r.deferRetry)
		r.pause(ctx)
		return false, nil
	}
	if err := r.gate(ctx, run, effort); err != nil {
		if ctx.Err() != nil {
			r.logf("queue: stopping")
			return true, nil
		}
		r.logf("queue: %s#%d gate: %s; waiting %s", run[0].Repo, run[0].Issue.Number, err.Error(), r.cfg.Wait)
		r.pause(ctx)
		return false, nil
	}
	if stop, err := r.finishIfStopped(ctx); stop || err != nil {
		return true, err
	}
	r.start(ctx, run)
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
	if held != "" {
		r.logf("queue: hold file %s exists; starting nothing", held)
		return true, nil
	}
	return false, nil
}

// holdFile returns the first of paths that exists, or "" when none does.
func holdFile(paths []string) (string, error) {
	for _, path := range paths {
		_, err := os.Stat(path)
		if err == nil {
			return path, nil
		}
		if !os.IsNotExist(err) {
			return "", fmt.Errorf("hold file %s: %w\nfix: the queue stops when it cannot tell whether the hold file exists", path, err)
		}
	}
	return "", nil
}

// full reports that max agents are already running. inflight counts issues,
// and one run can hold several, so the cap is the number of runs.
func (r *queueRunner) full() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.running >= r.cfg.Max
}

// candidate returns the issues one run takes, or none when no queued issue
// is free to start. The priority file is read here, so an edit applies to
// the next pick.
//
// An issue with an open pull request from its branch is not started: its
// agent opened the pull request and exited, and the label stays until the
// merge. This process may not remember handing it out, after a restart
// (#421). Such an issue counts as not free, like one already running.
func (r *queueRunner) candidate() ([]queuedIssue, error) {
	issues, err := r.collect()
	if err != nil {
		return nil, err
	}
	projects, err := r.projectOrder()
	if err != nil {
		return nil, err
	}
	ranked := orderQueued(issues, projects)
	prs := map[string][]string{}
	skip := map[string]bool{}
	for {
		r.mu.Lock()
		now := r.env.now()
		run := bundleFor(r.cfg, ranked, func(iss queuedIssue) bool {
			return skip[iss.key()] || r.blocked(iss, now)
		})
		r.mu.Unlock()
		if len(run) == 0 {
			return nil, nil
		}
		hit, err := r.withOpenPR(run, prs)
		if err != nil {
			return nil, err
		}
		if len(hit) == 0 {
			return run, nil
		}
		for _, iss := range hit {
			skip[iss.key()] = true
			r.logf("queue: %s#%d has an open pull request from %s; not starting it again", iss.Repo, iss.Issue.Number, hitBranch(iss, prs[iss.Repo]))
		}
	}
}

// withOpenPR returns the issues of run whose branch has an open pull
// request. prs caches each repository's open branches for this pick. A
// listing that fails fails the pick: starting while it is unknown could run
// an issue twice.
func (r *queueRunner) withOpenPR(run []queuedIssue, prs map[string][]string) ([]queuedIssue, error) {
	if r.env.prBranches == nil {
		return nil, nil
	}
	var hit []queuedIssue
	for _, iss := range run {
		branches, ok := prs[iss.Repo]
		if !ok {
			got, err := r.env.prBranches(iss.Repo)
			if err != nil {
				return nil, fmt.Errorf("list open pull requests in %s: %w\nfix: the queue will not start an issue while it cannot tell whether a pull request is open for it", iss.Repo, err)
			}
			prs[iss.Repo] = got
			branches = got
		}
		if hitBranch(iss, branches) != "" {
			hit = append(hit, iss)
		}
	}
	return hit, nil
}

// hitBranch is the first of branches that is iss's branch, as spawn names
// it (feature/<number>-<slug>), or "".
func hitBranch(iss queuedIssue, branches []string) string {
	prefix := issueBranchName(iss.Issue.Number, "")
	for _, b := range branches {
		if strings.HasPrefix(b, prefix) {
			return b
		}
	}
	return ""
}

func (r *queueRunner) projectOrder() ([]string, error) {
	if r.cfg.Priorities == "" {
		return nil, nil
	}
	return readPriorities(r.cfg.Priorities)
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

func (r *queueRunner) gate(ctx context.Context, run []queuedIssue, effort string) error {
	if len(r.cfg.Gate) == 0 {
		return nil
	}
	return r.env.run(ctx, "", gateArgv(r.cfg, run, effort))
}

func (r *queueRunner) start(ctx context.Context, run []queuedIssue) {
	r.mu.Lock()
	r.running++
	for _, iss := range run {
		r.inflight[iss.key()] = true
	}
	r.mu.Unlock()
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		defer r.poke()
		defer func() {
			r.mu.Lock()
			r.running--
			for _, iss := range run {
				delete(r.inflight, iss.key())
			}
			r.mu.Unlock()
		}()
		r.runOne(ctx, run)
	}()
}

func (r *queueRunner) runOne(ctx context.Context, run []queuedIssue) {
	if ctx.Err() != nil || len(run) == 0 {
		return
	}
	iss := run[0]
	// Resolve the effort before spawning. A disagreement between labels is
	// not an issue to start, and a workspace would only sit there.
	effort, err := r.cfg.effortFor(iss.Issue.LabelNames())
	if err != nil {
		r.logf("queue: %s#%d: %v", iss.Repo, iss.Issue.Number, err)
		r.deferAll(run, r.deferRetry)
		return
	}
	ws, err := r.env.spawn(iss.Repo, iss.Issue.Number)
	if err != nil {
		r.logf("queue: spawn %s#%d: %v", iss.Repo, iss.Issue.Number, err)
		r.deferAll(run, r.deferRetry)
		return
	}
	if ctx.Err() != nil {
		return
	}
	held, err := holdFile(r.cfg.Hold)
	if err != nil || held != "" {
		return
	}
	argv, who, err := r.argvFor(run, ws, effort)
	if err != nil {
		r.logf("queue: %s#%d: %v", iss.Repo, iss.Issue.Number, err)
		r.deferAll(run, r.deferRetry)
		return
	}
	r.logf("queue: %s#%d agent %s effort %s bundle %d in %s", iss.Repo, iss.Issue.Number, who, effort, len(run), ws)
	err = r.env.run(ctx, ws, argv)
	if err == nil {
		r.logf("queue: %s#%d agent exited, bundle %d", iss.Repo, iss.Issue.Number, len(run))
		r.deferAll(run, r.markDone)
		return
	}
	// An ExitError means the process ran and finished, whatever its status.
	// This process does not run that issue again. Anything else means the
	// command never started (the binary is missing, or it cannot be executed),
	// which is not the agent finishing the issue.
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		r.logf("queue: %s#%d agent exited: %v, bundle %d", iss.Repo, iss.Issue.Number, err, len(run))
		r.deferAll(run, r.markDone)
		return
	}
	r.logf("queue: %s#%d did not start: %v", iss.Repo, iss.Issue.Number, err)
	if ctx.Err() != nil {
		return
	}
	r.deferAll(run, r.deferRetry)
}

func (r *queueRunner) deferAll(run []queuedIssue, fn func(queuedIssue)) {
	for _, iss := range run {
		fn(iss)
	}
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
