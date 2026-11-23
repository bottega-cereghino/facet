package main

import (
	"context"
)

// dryQueue prints the runs the next starts would take and returns. It does
// not spawn and does not start an agent. The gate still runs, because a
// gate that fails is a start that would not happen. A gate that skips a
// run (gateSkipExit) is printed and the next run is looked at, as in the
// loop; any other refusal ends the listing, since the loop would wait. Issues already printed
// are treated as running, so a later line is the run a free slot would take
// while the earlier ones are still going. A slot the higher projects
// cannot fill goes to the next project down, as in the loop. A run whose
// repository spawn cannot route is printed as a start that would not
// happen, and the next run is looked at: the loop defers such an issue
// after its spawn fails, and moves on.
//
// With a fix block, the fix runs come first, as in the loop, and count
// toward max with the new starts. A dry run adds no label and writes no
// state.
func dryQueue(ctx context.Context, cfg queueConfig, env queueEnv) error {
	r, err := newQueueRunner(cfg, env, true)
	if err != nil {
		return err
	}
	held, err := holdFile(cfg.Hold)
	if err != nil {
		return err
	}
	if held != "" {
		r.logf("dry-run: hold file %s exists; would start nothing", held)
		return nil
	}
	started, stop := r.dryFixes(ctx)
	if stop {
		return nil
	}
	for started < cfg.Max {
		if ctx.Err() != nil {
			return nil
		}
		run, err := r.candidate()
		if err != nil {
			return err
		}
		if len(run) == 0 {
			if started == 0 {
				r.logf("dry-run: no open issue labelled %s", cfg.Label)
			}
			return nil
		}
		if r.env.route != nil {
			if err := r.env.route(run[0].Repo); err != nil {
				r.logf("dry-run: would not start %s#%d: spawn: %v", run[0].Repo, run[0].Issue.Number, err)
				r.deferAll(run, r.deferRetry)
				continue
			}
		}
		effort, err := r.cfg.effortFor(run[0].Issue.LabelNames())
		if err != nil {
			r.logf("dry-run: would not start %s#%d: %v", run[0].Repo, run[0].Issue.Number, err)
			return nil
		}
		if err := r.gate(ctx, run, effort); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if gateSkipsRepo(err) {
				r.logf("dry-run: would not start %s#%d: gate skips the repository: %s", run[0].Repo, run[0].Issue.Number, err.Error())
				r.deferRepo(run[0].Repo)
				r.deferAll(run, r.deferRetry)
				continue
			}
			if gateSkips(err) {
				r.logf("dry-run: would not start %s#%d: gate skips it: %s", run[0].Repo, run[0].Issue.Number, err.Error())
				r.deferAll(run, r.deferRetry)
				continue
			}
			r.logf("dry-run: would not start %s#%d: gate: %s", run[0].Repo, run[0].Issue.Number, err.Error())
			return nil
		}
		who, _, err := r.cfg.commandFor(run[0].Issue.LabelNames())
		if err != nil {
			r.logf("dry-run: would not start %s#%d: %v", run[0].Repo, run[0].Issue.Number, err)
			return nil
		}
		if who == "" {
			who = "default"
		}
		r.logf("dry-run: would start %s#%d agent %s effort %s bundle %d", run[0].Repo, run[0].Issue.Number, who, effort, len(run))
		for _, iss := range run {
			r.logf("dry-run: %s#%d: %s", iss.Repo, iss.Issue.Number, iss.Issue.Title)
		}
		r.mu.Lock()
		for _, iss := range run {
			r.inflight[iss.key()] = true
		}
		r.mu.Unlock()
		started++
	}
	run, err := r.candidate()
	if err != nil {
		return err
	}
	if len(run) > 0 {
		r.logf("dry-run: reached max %d; %s#%d waits for a free slot", cfg.Max, run[0].Repo, run[0].Issue.Number)
	}
	return nil
}

// dryFixes prints the fix runs the next starts would take, up to max, and
// the pull requests it would update or merge. stop means a gate refused
// for the whole queue, so nothing more would start.
func (r *queueRunner) dryFixes(ctx context.Context) (int, bool) {
	started := 0
	for _, job := range r.scanQueuePRs(ctx) {
		if started >= r.cfg.Max || ctx.Err() != nil {
			return started, ctx.Err() != nil
		}
		k := job.iss.key()
		if r.repoDeferred(job.iss.Repo) {
			continue
		}
		if err := r.fixGate(ctx, job); err != nil {
			if ctx.Err() != nil {
				return started, true
			}
			switch {
			case gateSkipsRepo(err):
				r.logf("dry-run: would not fix %s PR #%d: gate skips the repository: %s", k, job.pr.Number, err.Error())
				r.deferRepo(job.iss.Repo)
			case gateSkips(err):
				r.logf("dry-run: would not fix %s PR #%d: gate skips it: %s", k, job.pr.Number, err.Error())
			default:
				r.logf("dry-run: would not fix %s PR #%d: gate: %s", k, job.pr.Number, err.Error())
				return started, true
			}
			continue
		}
		r.logf("dry-run: would fix %s PR #%d: %s, try %d of %d", k, job.pr.Number, job.reason, job.try, r.cfg.Fix.Tries)
		started++
	}
	return started, false
}

// repoDeferred reports a repository the gate skipped for wait.
func (r *queueRunner) repoDeferred(repo string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	until, ok := r.repoWait[repo]
	return ok && r.env.now().Before(until)
}
