package main

import (
	"context"
	"time"
)

// dryQueue prints the runs the next starts would take and returns. It does
// not spawn and does not start an agent. The gate still runs, because a
// gate that fails is a start that would not happen. Issues already printed
// are treated as running, so a later line is the run a free slot would take
// while the earlier ones are still going. A lower project is not listed
// while a higher one still has an open queued issue.
func dryQueue(ctx context.Context, cfg queueConfig, env queueEnv) error {
	env.fill()
	r := &queueRunner{
		cfg:      cfg,
		env:      env,
		inflight: map[string]bool{},
		done:     map[string]bool{},
		retryAt:  map[string]time.Time{},
	}
	held, err := holdFile(cfg.Hold)
	if err != nil {
		return err
	}
	if held {
		r.logf("dry-run: hold file %s exists; would start nothing", cfg.Hold)
		return nil
	}
	started := 0
	for started < cfg.Max {
		if ctx.Err() != nil {
			return nil
		}
		run, waiting, err := r.candidate()
		if err != nil {
			return err
		}
		if len(run) == 0 {
			if waiting != "" {
				r.logf("dry-run: %s still has an open issue labelled %s; not starting a lower project", waiting, cfg.Label)
			} else if started == 0 {
				r.logf("dry-run: no open issue labelled %s", cfg.Label)
			}
			return nil
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
	run, _, err := r.candidate()
	if err != nil {
		return err
	}
	if len(run) > 0 {
		r.logf("dry-run: reached max %d; %s#%d waits for a free slot", cfg.Max, run[0].Repo, run[0].Issue.Number)
	}
	return nil
}
