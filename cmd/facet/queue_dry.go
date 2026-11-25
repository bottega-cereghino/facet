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
	// A disk refusal is printed here, once, and the listing below is what
	// those starts would have been. Fixes and landings are still listed:
	// they add no workspace.
	disk := r.noteDisk()
	started, stop := r.dryFixes(ctx, disk)
	if stop {
		return nil
	}
	r.refreshRooms(ctx)
	startWord := "would start"
	if disk.block {
		startWord = "would have started"
	}
	for started < cfg.Max {
		if ctx.Err() != nil {
			return nil
		}
		run, err := r.candidate(ctx)
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
				r.deferGateSkipped(run, err)
				continue
			}
			r.logf("dry-run: would not start %s#%d: gate: %s", run[0].Repo, run[0].Issue.Number, err.Error())
			return nil
		}
		a, from, err := r.cfg.resolveAgent(run[0].Issue.LabelNames(), r.rooms)
		if err != nil {
			r.logf("dry-run: would not start %s#%d: %v", run[0].Repo, run[0].Issue.Number, err)
			return nil
		}
		who := a.Label
		if who == "" {
			who = "default"
		}
		if from != "" {
			r.logf("dry-run: %s#%d lane %s has no room; fallback_from %s", run[0].Repo, run[0].Issue.Number, from, from)
		}
		r.logf("dry-run: %s %s#%d agent %s effort %s bundle %d", startWord, run[0].Repo, run[0].Issue.Number, who, effort, len(run))
		for _, iss := range run {
			r.logf("dry-run: %s#%d: %s", iss.Repo, iss.Issue.Number, iss.Issue.Title)
		}
		r.mu.Lock()
		r.repoRunning[run[0].Repo]++
		for _, iss := range run {
			r.inflight[iss.key()] = true
		}
		r.mu.Unlock()
		// Count the run in the cap window, as a real start would. A dry run
		// still writes nothing: saveState leaves the file alone.
		r.recordStarted(run, "")
		started++
	}
	run, err := r.candidate(ctx)
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
func (r *queueRunner) dryFixes(ctx context.Context, disk diskView) (int, bool) {
	started := 0
	for _, job := range r.scanQueuePRs(ctx) {
		if started >= r.cfg.Max || ctx.Err() != nil {
			return started, ctx.Err() != nil
		}
		k := job.iss.key()
		if r.repoDeferred(job.iss.Repo) {
			continue
		}
		if disk.block && !disk.have[k] {
			r.logf("dry-run: would not fix %s PR #%d: it would spawn a workspace", k, job.pr.Number)
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
