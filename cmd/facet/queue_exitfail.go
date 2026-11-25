package main

import (
	"context"
	"strconv"
)

// maxAgentFailures is how many non-zero exits of an issue's agent this
// process accepts before it labels the issue and notifies.
const maxAgentFailures = 3

// failRun handles an agent that ran and exited non-zero. That is a failed
// try, not a finished issue: each issue waits and is started again. The
// third failed try in this process labels it with fix.blockLabel and runs
// fix.notify, as a refused fix does. Nothing is written to state.Done: only
// a zero exit is a done issue.
func (r *queueRunner) failRun(ctx context.Context, run []queuedIssue, reason string) {
	for _, iss := range run {
		k := iss.key()
		r.mu.Lock()
		r.failTries[k]++
		tries := r.failTries[k]
		r.mu.Unlock()
		if tries < maxAgentFailures {
			r.deferRetry(iss)
			continue
		}
		r.blockFailed(ctx, iss, tries, reason)
	}
}

func (r *queueRunner) blockFailed(ctx context.Context, iss queuedIssue, tries int, reason string) {
	fix := r.cfg.Fix
	if fix == nil || r.env.addLabel == nil {
		// Nothing can label it. Park it in this process only, as before.
		r.logf("queue: %s#%d: agent failed %d times and no block label can be added; not started again in this process", iss.Repo, iss.Issue.Number, tries)
		r.mu.Lock()
		r.done[iss.key()] = true
		r.mu.Unlock()
		return
	}
	// The label keeps the issue out of the list. If it is removed later, the
	// issue is tried again after wait.
	r.deferRetry(iss)
	if err := r.env.addLabel(iss.Repo, iss.Issue.Number, fix.BlockLabel); err != nil {
		r.logf("queue: %s#%d: add label %s: %v\nfix: create the label %s in %s, or set fix.blockLabel to a label that exists; trying again after %s",
			iss.Repo, iss.Issue.Number, fix.BlockLabel, err, fix.BlockLabel, iss.Repo, r.cfg.Wait)
		return
	}
	r.logf("queue: %s#%d agent failed %d times (%s); labelled %s", iss.Repo, iss.Issue.Number, tries, reason, fix.BlockLabel)
	r.mu.Lock()
	r.failTries[iss.key()] = 0
	r.mu.Unlock()
	if len(fix.Notify) == 0 {
		return
	}
	vars := map[string]string{"repo": iss.Repo, "number": strconv.Itoa(iss.Issue.Number), "pr": "", "reason": reason}
	if err := r.runCmd(ctx, "", expandArgv(fix.Notify, vars), nil); err != nil {
		r.logf("queue: %s#%d notify: %v", iss.Repo, iss.Issue.Number, err)
	}
}
