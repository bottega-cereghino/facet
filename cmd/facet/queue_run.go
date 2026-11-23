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
	"syscall"
	"time"

	"github.com/bottega-cereghino/facet/internal/ghx"
)

// queueEnv is the seams the loop needs. list and spawn are the only fakes a
// test has to supply: the agent and the gate are commands, run by runArgv.
type queueEnv struct {
	list func(repo, label string) ([]ghx.Issue, error)
	// prs lists a repository's open pull requests, with their checks. Nil
	// skips the open pull request check and finds nothing to fix.
	prs func(repo string) ([]ghx.OpenPR, error)
	// addLabel adds a label to an issue. The fix stage uses it to block an
	// issue that used up its tries.
	addLabel func(repo string, number int, label string) error
	// updateBranch, mergeMethod, mergePR, behindBase and prFiles land a
	// green queue pull request (fix.land) and serialize on shared files.
	// Nil skips that step.
	updateBranch func(repo string, number int) error
	mergeMethod  func(repo string) (string, error)
	mergePR      func(repo string, number int, method, head string) error
	behindBase   func(repo, base, head string) (bool, error)
	prFiles      func(repo string, number int) ([]string, error)
	spawn        func(repo string, number int) (string, error)
	// route runs spawn's routing checks for a repository, without the
	// network. Only --dry-run calls it; nil skips it.
	route func(repo string) error
	run   func(ctx context.Context, dir string, argv []string) error
	// lanes runs the lanes command and returns its standard output. Nil
	// uses runLanes. A test sets it so the suite does not start a program.
	lanes func(ctx context.Context, argv []string) ([]byte, error)
	now   func() time.Time
	out   io.Writer
}

func realQueueEnv() queueEnv {
	return queueEnv{
		list: func(repo, label string) ([]ghx.Issue, error) {
			return gh.OpenIssues(repo, label)
		},
		prs:          ghx.CLI{}.OpenPRs,
		addLabel:     ghx.CLI{}.AddIssueLabel,
		updateBranch: ghx.CLI{}.UpdatePRBranch,
		mergeMethod:  ghx.CLI{}.MergeMethod,
		mergePR:      ghx.CLI{}.MergePR,
		behindBase:   ghx.CLI{}.HeadBehindBase,
		prFiles:      ghx.CLI{}.PRFiles,
		spawn:        prepareWorkspace,
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

// gateSkipExit is the gate's exit status for "not this run": a refusal about
// one repository or issue, such as a red pull request there. The queue
// defers that run for wait and moves on to the next. Any other non-zero
// status is about the whole queue, such as a spend cap, and the queue waits.
const gateSkipExit = 3

// gateSkipRepoExit is the gate's exit status for "not this repository": the
// queue defers every queued issue of the run's repository for wait, so the
// gate runs once per repository, not once per issue.
const gateSkipRepoExit = 4

// gateSkips reports that the gate exited with gateSkipExit.
func gateSkips(err error) bool {
	return gateExit(err) == gateSkipExit
}

// gateSkipsRepo reports that the gate exited with gateSkipRepoExit.
func gateSkipsRepo(err error) bool {
	return gateExit(err) == gateSkipRepoExit
}

func gateExit(err error) int {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return 0
}

// queueStopGrace is how long a command has to exit after the queue stops
// it, before it is killed.
const queueStopGrace = 10 * time.Second

type queueRunner struct {
	cfg         queueConfig
	env         queueEnv
	mu          sync.Mutex
	inflight    map[string]bool
	running     int
	repoRunning map[string]int
	done        map[string]bool
	retryAt     map[string]time.Time
	repoWait    map[string]time.Time
	wake        chan struct{}
	wg          sync.WaitGroup
	// round caches each repository's open pull requests for one tick, so
	// a round makes one listing call per repository.
	round map[string]prListing
	// The rest are the fix stage's; state is nil when the config has no
	// fix block. prCache keeps a listing for prListTTL. landed is the
	// listing each pull request was last updated or merged from, so one
	// listing lands it at most once. labelRetry holds a block label that
	// could not be added until wait has passed.
	state        *queueState
	fixing       map[string]bool
	fixRetry     map[string]time.Time
	labelRetry   map[string]time.Time
	prCache      map[string]prListing
	landed       map[string]time.Time
	waited       map[string]string
	mergeMethods map[string]string
	// dry is set by --dry-run: no label is added and no state is written.
	dry bool
	// capSeen and capPass are this round's passes results, one per
	// repository. cappedLog is the cap line already printed for an issue,
	// so a skip is logged once until the window changes.
	capSeen   map[string]bool
	capPass   map[string]bool
	cappedLog map[string]string
	// lastHeld is the hold file path last recorded in the decision log, so
	// held/released is emitted once per change rather than once per poll.
	lastHeld   string
	decisionMu sync.Mutex
	// rooms is this tick's lanes answer. Nil means a lane label keeps its
	// agent. The map is not mutated after refreshRooms returns.
	rooms map[string]string
	// fallbackMu keeps two runs from appending the lanes log at once.
	fallbackMu sync.Mutex
}

// newQueueRunner builds a runner and reads the state file when the config
// has a fix block or a cap. Both record the issues the queue started.
func newQueueRunner(cfg queueConfig, env queueEnv, dry bool) (*queueRunner, error) {
	env.fill()
	r := &queueRunner{
		cfg:          cfg,
		env:          env,
		inflight:     map[string]bool{},
		repoRunning:  map[string]int{},
		done:         map[string]bool{},
		retryAt:      map[string]time.Time{},
		repoWait:     map[string]time.Time{},
		wake:         make(chan struct{}, 1),
		round:        map[string]prListing{},
		fixing:       map[string]bool{},
		fixRetry:     map[string]time.Time{},
		labelRetry:   map[string]time.Time{},
		prCache:      map[string]prListing{},
		landed:       map[string]time.Time{},
		waited:       map[string]string{},
		mergeMethods: map[string]string{},
		dry:          dry,
	}
	if !dry && cfg.Decisions != "" {
		r.lastHeld = lastHoldPath(cfg.Decisions)
	}
	if cfg.State != "" && (cfg.Fix != nil || cfg.Cap != nil) {
		st, err := loadQueueState(cfg.State)
		if err != nil {
			return nil, err
		}
		r.state = st
	}
	return r, nil
}

func runQueue(ctx context.Context, cfg queueConfig, env queueEnv) error {
	r, err := newQueueRunner(cfg, env, false)
	if err != nil {
		return err
	}
	if cfg.Priorities != "" {
		r.logf("queue: up to %d agent(s), at most %d per repository, label %s, projects from %s", cfg.Max, cfg.PerRepo, cfg.Label, cfg.Priorities)
	} else {
		r.logf("queue: up to %d agent(s), at most %d per repository, label %s, order p1 then p2 then p3 then oldest", cfg.Max, cfg.PerRepo, cfg.Label)
	}
	err = r.loop(ctx)
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
	r.mu.Lock()
	r.round = map[string]prListing{}
	r.mu.Unlock()
	r.resetCapRound()
	// The fix stage looks even when every slot is taken: landing a green
	// pull request takes no slot. Red first: the queue's own broken pull
	// request before new work.
	jobs := r.scanQueuePRs(ctx)
	if r.full() {
		r.pause(ctx)
		return false, nil
	}
	if len(jobs) > 0 {
		return r.tryFix(ctx, jobs[0])
	}
	r.refreshRooms(ctx)
	if ctx.Err() != nil {
		r.logf("queue: stopping")
		return true, nil
	}
	run, err := r.candidate(ctx)
	if err != nil {
		if ctx.Err() != nil {
			r.logf("queue: stopping")
			return true, nil
		}
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
		r.recordDecision(run[0].Repo, strconv.Itoa(run[0].Issue.Number), r.laneFor(run[0]), "skip", firstErrorLine(err.Error()))
		r.deferAll(run, r.deferRetry)
		r.pause(ctx)
		return false, nil
	}
	if err := r.gate(ctx, run, effort); err != nil {
		if ctx.Err() != nil {
			r.logf("queue: stopping")
			return true, nil
		}
		if gateSkipsRepo(err) {
			r.logf("queue: %s#%d gate skips the repository: %s; trying the next run, %s again after %s", run[0].Repo, run[0].Issue.Number, err.Error(), run[0].Repo, r.cfg.Wait)
			r.deferRepo(run[0].Repo)
			r.deferAll(run, r.deferRetry)
			return false, nil
		}
		if gateSkips(err) {
			r.logf("queue: %s#%d gate skips it: %s; trying the next run, this one again after %s", run[0].Repo, run[0].Issue.Number, err.Error(), r.cfg.Wait)
			r.deferAll(run, r.deferRetry)
			return false, nil
		}
		r.logf("queue: %s#%d gate: %s; waiting %s", run[0].Repo, run[0].Issue.Number, err.Error(), r.cfg.Wait)
		r.pause(ctx)
		return false, nil
	}
	if stop, err := r.finishIfStopped(ctx); stop || err != nil {
		return true, err
	}
	r.start(ctx, run, r.rooms)
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
	r.recordHold(held)
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
func (r *queueRunner) candidate(ctx context.Context) ([]queuedIssue, error) {
	issues, err := r.collect()
	if err != nil {
		return nil, err
	}
	projects, err := r.projectOrder()
	if err != nil {
		return nil, err
	}
	ranked := orderQueued(issues, projects)
	r.recordBlockedSkips(ranked)
	skip := map[string]bool{}
	for {
		if err := r.skipCapped(ctx, ranked, skip); err != nil {
			return nil, err
		}
		r.mu.Lock()
		now := r.env.now()
		rooms := r.rooms
		run := bundleFor(r.cfg, ranked, func(iss queuedIssue) bool {
			return skip[iss.key()] || r.blocked(iss, now)
		}, rooms)
		r.mu.Unlock()
		if len(run) == 0 {
			return nil, nil
		}
		hit, err := r.withOpenPR(run)
		if err != nil {
			return nil, err
		}
		if len(hit) == 0 {
			return run, nil
		}
		for _, iss := range hit {
			skip[iss.key()] = true
			pr, _ := r.openPRFor(iss)
			r.logf("queue: %s#%d has an open pull request from %s; not starting it again", iss.Repo, iss.Issue.Number, pr.HeadRefName)
			why := "open pull request from " + pr.HeadRefName
			if pr.Number != 0 {
				why = fmt.Sprintf("open pull request #%d from %s", pr.Number, pr.HeadRefName)
			}
			r.recordDecision(iss.Repo, strconv.Itoa(iss.Issue.Number), r.laneFor(iss), "skip", why)
		}
	}
}

func (r *queueRunner) recordBlockedSkips(ranked []queuedIssue) {
	if r.dry || r.cfg.Decisions == "" {
		return
	}
	type skipEntry struct {
		iss queuedIssue
		why string
	}
	r.mu.Lock()
	var skipped []skipEntry
	for _, iss := range ranked {
		k := iss.key()
		if r.inflight[k] {
			continue
		}
		if r.done[k] {
			skipped = append(skipped, skipEntry{iss: iss, why: "already started in this process"})
			continue
		}
		if r.cfg.PerRepo > 0 && r.repoRunning[iss.Repo] >= r.cfg.PerRepo {
			skipped = append(skipped, skipEntry{
				iss: iss,
				why: fmt.Sprintf("per-repo cap (%d/%d running)", r.repoRunning[iss.Repo], r.cfg.PerRepo),
			})
		}
	}
	r.mu.Unlock()
	for _, s := range skipped {
		r.recordDecision(s.iss.Repo, strconv.Itoa(s.iss.Issue.Number), r.laneFor(s.iss), "skip", s.why)
	}
}

// withOpenPR returns the issues of run whose branch has an open pull
// request. The listing is the round's, shared with the fix stage. A
// listing that fails fails the pick: starting while it is unknown could run
// an issue twice.
func (r *queueRunner) withOpenPR(run []queuedIssue) ([]queuedIssue, error) {
	if r.env.prs == nil {
		return nil, nil
	}
	var hit []queuedIssue
	for _, iss := range run {
		_, ok, err := r.openPRForErr(iss)
		if err != nil {
			return nil, fmt.Errorf("list open pull requests in %s: %w\nfix: the queue will not start an issue while it cannot tell whether a pull request is open for it", iss.Repo, err)
		}
		if ok {
			hit = append(hit, iss)
		}
	}
	return hit, nil
}

func (r *queueRunner) openPRFor(iss queuedIssue) (ghx.OpenPR, bool) {
	pr, ok, _ := r.openPRForErr(iss)
	return pr, ok
}

// openPRForErr is the open pull request from iss's branch, as spawn names
// it (feature/<number>-<slug>).
func (r *queueRunner) openPRForErr(iss queuedIssue) (ghx.OpenPR, bool, error) {
	prs, err := r.listPRs(iss.Repo)
	if err != nil {
		return ghx.OpenPR{}, false, err
	}
	pr, ok := branchPR(iss, prs)
	return pr, ok, nil
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
	if r.repoRunning[iss.Repo] >= r.cfg.PerRepo {
		return true
	}
	if until, ok := r.repoWait[iss.Repo]; ok && now.Before(until) {
		return true
	}
	until, ok := r.retryAt[k]
	return ok && now.Before(until)
}

func (r *queueRunner) deferRepo(repo string) {
	r.mu.Lock()
	r.repoWait[repo] = r.env.now().Add(r.cfg.Wait)
	r.mu.Unlock()
}

func (r *queueRunner) gate(ctx context.Context, run []queuedIssue, effort string) error {
	if len(r.cfg.Gate) == 0 {
		return nil
	}
	return r.env.run(ctx, "", gateArgv(r.cfg, run, effort))
}

func (r *queueRunner) start(ctx context.Context, run []queuedIssue, rooms map[string]string) {
	r.mu.Lock()
	r.running++
	r.repoRunning[run[0].Repo]++
	for _, iss := range run {
		r.inflight[iss.key()] = true
	}
	r.mu.Unlock()
	r.recordStarted(run)
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		defer r.poke()
		defer func() {
			r.mu.Lock()
			r.running--
			r.repoRunning[run[0].Repo]--
			for _, iss := range run {
				delete(r.inflight, iss.key())
			}
			r.mu.Unlock()
		}()
		r.runOne(ctx, run, rooms)
	}()
}

func (r *queueRunner) runOne(ctx context.Context, run []queuedIssue, rooms map[string]string) {
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
		if held != "" {
			r.recordHold(held)
		}
		return
	}
	argv, who, from, cmdSummary, err := r.argvFor(run, ws, effort, rooms)
	if err != nil {
		r.logf("queue: %s#%d: %v", iss.Repo, iss.Issue.Number, err)
		r.deferAll(run, r.deferRetry)
		return
	}
	if from != "" {
		r.logf("queue: %s#%d lane %s has no room; fallback_from %s", iss.Repo, iss.Issue.Number, from, from)
		r.noteFallback(iss, from)
	}
	r.logf("queue: %s#%d agent %s effort %s bundle %d in %s", iss.Repo, iss.Issue.Number, who, effort, len(run), ws)
	r.recordDecision(iss.Repo, strconv.Itoa(iss.Issue.Number), who, "start", cmdSummary)
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
