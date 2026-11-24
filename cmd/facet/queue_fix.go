package main

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/bottega-cereghino/facet/internal/ghx"
)

// fixJob is one fix run about to start.
type fixJob struct {
	iss    queuedIssue
	pr     ghx.OpenPR
	reason string
	try    int
}

// branchPR is the open pull request from iss's spawn branch, from anyone.
// New work uses it: an issue with such a pull request is not started again,
// whoever opened it.
func branchPR(iss queuedIssue, prs []ghx.OpenPR) (ghx.OpenPR, bool) {
	prefix := issueBranchName(iss.Issue.Number, "")
	for _, pr := range prs {
		if strings.HasPrefix(pr.HeadRefName, prefix) {
			return pr, true
		}
	}
	return ghx.OpenPR{}, false
}

// prClockSkew is how much earlier than the start a queue pull request may
// say it was opened, for a clock that is behind GitHub's.
const prClockSkew = time.Minute

// queuePR is the queue's own pull request for iss: one its own run
// opened, never a person's. It comes from this repository (not a fork), on
// the spawn branch, opened after the start by the account the queue runs
// as, and its description carries the start's trailer line (ownsPR). The
// first such pull request's number is recorded, and from then on only that
// pull request is the queue's, and only while it still passes ownsPR. An
// error means the account could not be read: the caller must not take it
// as "no pull request" and forget the start.
func (r *queueRunner) queuePR(iss queuedIssue, prs []ghx.OpenPR) (ghx.OpenPR, bool, error) {
	k := iss.key()
	r.mu.Lock()
	s := r.state.Started[k]
	r.mu.Unlock()
	if s.PR != 0 {
		for _, pr := range prs {
			if pr.Number != s.PR {
				continue
			}
			ok, err := r.ownsPR(s, pr)
			if err != nil || !ok {
				return ghx.OpenPR{}, false, err
			}
			return pr, true, nil
		}
		return ghx.OpenPR{}, false, nil
	}
	prefix := issueBranchName(iss.Issue.Number, "")
	for _, pr := range prs {
		if !strings.HasPrefix(pr.HeadRefName, prefix) || openedBefore(pr, s.At) {
			continue
		}
		ok, err := r.ownsPR(s, pr)
		if err != nil {
			return ghx.OpenPR{}, false, err
		}
		if !ok {
			continue
		}
		if !r.dry {
			r.mu.Lock()
			s.PR = pr.Number
			r.state.Started[k] = s
			r.mu.Unlock()
			r.saveState()
		}
		return pr, true, nil
	}
	return ghx.OpenPR{}, false, nil
}

func openedBefore(pr ghx.OpenPR, start time.Time) bool {
	created, err := time.Parse(time.RFC3339, pr.CreatedAt)
	return err == nil && created.Before(start.Add(-prClockSkew))
}

// startedQueued is the state's started issues in the config's repositories,
// as queued issues, so they sort like new work. A repository taken out of
// the config is left alone.
func (r *queueRunner) startedQueued() []queuedIssue {
	repos := map[string]bool{}
	for _, repo := range r.cfg.Repos {
		repos[repo] = true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]queuedIssue, 0, len(r.state.Started))
	for _, s := range r.state.Started {
		if !repos[s.Repo] {
			continue
		}
		iss := ghx.Issue{Number: s.Number, Title: s.Title, URL: s.URL, CreatedAt: s.CreatedAt, State: "OPEN"}
		for _, l := range s.Labels {
			iss.Labels = append(iss.Labels, ghx.Label{Name: l})
		}
		out = append(out, queuedIssue{Repo: s.Repo, Issue: iss})
	}
	return out
}

// scanQueuePRs looks at every queue pull request once, in the order new
// issues are taken, and returns those that need a fix run. On the way it
// lands a green one (fix.land), clears the record of a green one, forgets
// one that is merged or closed, and blocks an issue that used up its tries.
// A repository is listed only when one of its started issues can act.
func (r *queueRunner) scanQueuePRs(ctx context.Context) []*fixJob {
	if r.cfg.Fix == nil || r.state == nil || r.env.prs == nil {
		return nil
	}
	projects, err := r.projectOrder()
	if err != nil {
		r.logf("queue: %v", err)
		return nil
	}
	issues := orderQueued(r.startedQueued(), projects)
	need := r.listingNeeds(issues)
	failed := map[string]bool{}
	mergedRepo := map[string]bool{}
	var jobs []*fixJob
	for _, iss := range issues {
		if act, _ := r.canAct(iss); !act {
			r.forgetIfGone(iss)
			continue
		}
		got := r.fixListing(iss.Repo, need[iss.Repo])
		if got.err != nil {
			if !failed[iss.Repo] {
				failed[iss.Repo] = true
				r.logf("queue: list open pull requests in %s: %v; starting no fix there this round", iss.Repo, got.err)
			}
			continue
		}
		pr, ok, err := r.queuePR(iss, got.prs)
		if err != nil {
			r.noteOnce(iss.key()+" owner", err.Error(), func() {
				r.logf("queue: %s: %v; touching nothing of it this round", iss.key(), err)
			})
			continue
		}
		if !ok {
			r.forgetStarted(iss)
			continue
		}
		if job := r.judgeQueuePR(ctx, iss, pr, got, mergedRepo); job != nil {
			jobs = append(jobs, job)
		}
	}
	return jobs
}

// listingNeeds is, per repository, the time its listing must be no older
// than: the latest settling of the issues there that can act.
func (r *queueRunner) listingNeeds(issues []queuedIssue) map[string]time.Time {
	need := map[string]time.Time{}
	for _, iss := range issues {
		if act, after := r.canAct(iss); act && after.After(need[iss.Repo]) {
			need[iss.Repo] = after
		}
	}
	return need
}

// canAct reports whether the fix stage could do anything for iss now, and
// the time a listing must be no older than to judge it: the end of its
// last fix run's settling. An issue that is running, waiting, settling or
// blocked cannot act, so its repository is not listed for it.
func (r *queueRunner) canAct(iss queuedIssue) (bool, time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	k := iss.key()
	now := r.env.now()
	if r.inflight[k] || r.fixing[k] {
		return false, time.Time{}
	}
	if _, alive := r.activeAgentAliveLocked(k); alive {
		return false, time.Time{}
	}
	for _, until := range []time.Time{r.repoWait[iss.Repo], r.fixRetry[k], r.labelRetry[k]} {
		if now.Before(until) {
			return false, time.Time{}
		}
	}
	rec := r.state.Fixes[k]
	if rec == nil {
		return true, time.Time{}
	}
	settled := rec.Last.Add(r.cfg.Wait)
	if rec.Blocked || now.Before(settled) {
		return false, time.Time{}
	}
	return true, settled
}

// forgetIfGone forgets a blocked issue whose pull request is gone from a
// listing already in hand. A blocked issue never makes the queue list.
func (r *queueRunner) forgetIfGone(iss queuedIssue) {
	r.mu.Lock()
	rec := r.state.Fixes[iss.key()]
	r.mu.Unlock()
	if rec == nil || !rec.Blocked {
		return
	}
	got, ok := r.heldListing(iss.Repo)
	if !ok {
		return
	}
	if _, open, err := r.queuePR(iss, got.prs); err == nil && !open {
		r.forgetStarted(iss)
	}
}

// judgeQueuePR returns a fix job for pr, or nil when it is pending, green,
// blocked, or just used its last try. got.at is when pr was listed: a
// listing from before a fix run settled says nothing about that run.
func (r *queueRunner) judgeQueuePR(ctx context.Context, iss queuedIssue, pr ghx.OpenPR, got prListing, mergedRepo map[string]bool) *fixJob {
	verdict, reason := judgePR(pr)
	k := iss.key()
	r.mu.Lock()
	rec := r.state.Fixes[k]
	var runs int
	var settling, blocked bool
	if rec != nil {
		runs, blocked = rec.Runs, rec.Blocked
		// A fix run that just pushed may not have its checks listed yet.
		settling = got.at.Before(rec.Last.Add(r.cfg.Wait))
	}
	r.mu.Unlock()
	// Blocked stays blocked: the issue is a person's now.
	if blocked || verdict == prPending || settling {
		return nil
	}
	if verdict == prGreen {
		if why := r.landBlockReason(iss.Repo, pr); why != "" {
			// A cancelled, skipped or missing required check is not a pass,
			// and it is not a red result for a fix run either: the pull
			// request stays as it is until the check really passed.
			r.noteOnce(k+" land "+pr.HeadRefOid, why, func() {
				r.logf("queue: %s PR #%d: not landing: %s", k, pr.Number, why)
			})
			return nil
		}
		if !r.mayRetry(r.forbidHolds, k, pr.HeadRefOid) {
			// The diff of this head could not be read; it is read again
			// after its wait, not every tick.
			return nil
		}
		pattern, file, bad, err := r.suppression(iss, pr)
		if err == nil {
			r.clearFailure(r.forbidHolds, k)
		}
		switch {
		case err != nil:
			// A diff that cannot be read is not a pass: landing it could
			// merge a suppression the check did not see.
			wait := r.noteFailure(r.forbidHolds, k, pr.HeadRefOid)
			r.logf("queue: %s PR #%d: %v; not landing, reading it again after %s", k, pr.Number, err, wait)
			return nil
		case bad:
			reason = fmt.Sprintf("the fix suppresses instead of fixing: %s in %s", pattern, file)
			r.noteOnce(k+" suppress "+pr.HeadRefOid, reason, func() {
				r.logf("queue: %s PR #%d: %s; not landing", k, pr.Number, reason)
			})
		default:
			// Keep the fix record across a green: tries are bounded per pull
			// request, not per red streak. mergePR and forgetStarted drop it
			// once the pull request is merged or closed.
			if !r.cfg.Fix.Land || !r.land(ctx, iss, pr, got, mergedRepo) {
				return nil
			}
			reason = "conflicts with " + baseName(pr)
		}
	}
	// A fix run and a block label act on the pull request and the issue:
	// read the issue again first.
	if r.touchStop(iss) != "" {
		return nil
	}
	if runs >= r.cfg.Fix.Tries {
		r.block(ctx, iss, pr, reason, runs)
		return nil
	}
	return &fixJob{iss: iss, pr: pr, reason: reason, try: runs + 1}
}

func baseName(pr ghx.OpenPR) string {
	if pr.BaseRefName == "" {
		return "its base"
	}
	return pr.BaseRefName
}

// forgetStarted drops an issue whose pull request is no longer open and
// that is not running: merged or closed. Its fix record goes with it. A
// dry run forgets nothing.
func (r *queueRunner) forgetStarted(iss queuedIssue) {
	if r.dry {
		return
	}
	k := iss.key()
	r.mu.Lock()
	s, ok := r.state.Started[k]
	// An issue started a moment ago may not have opened its pull request.
	// One whose pull request was seen has opened it.
	recent := ok && s.PR == 0 && r.env.now().Before(s.At.Add(startedKeep))
	_, fixed := r.state.Fixes[k]
	if !ok || (recent && !fixed) {
		r.mu.Unlock()
		return
	}
	delete(r.state.Fixes, k)
	if s.PR != 0 {
		delete(r.state.Merges, landKey(iss.Repo, s.PR))
	}
	if !recent {
		// The pull request is gone. The cap still counts the run.
		if r.cfg.Cap != nil {
			r.state.noteStart(s)
		}
		delete(r.state.Started, k)
		delete(r.state.Done, k)
		delete(r.done, k)
	}
	r.mu.Unlock()
	r.saveState()
}

// startedKeep is how long a started issue with no open pull request stays
// in the state. Its agent may still be about to open one.
const startedKeep = 24 * time.Hour

// block labels the issue and stops fixing it. A label that cannot be added
// is tried again after wait, not every tick, so the issue is not left
// looking free and a missing label does not cost a call per tick.
func (r *queueRunner) block(ctx context.Context, iss queuedIssue, pr ghx.OpenPR, reason string, runs int) {
	fix := r.cfg.Fix
	k := iss.key()
	if r.dry {
		r.logf("dry-run: would label %s#%d %s: PR #%d is %s after %d fix run(s)", iss.Repo, iss.Issue.Number, fix.BlockLabel, pr.Number, reason, runs)
		return
	}
	if r.env.addLabel == nil {
		r.logf("queue: %s#%d: cannot add the %s label: no label seam", iss.Repo, iss.Issue.Number, fix.BlockLabel)
		return
	}
	if err := r.env.addLabel(iss.Repo, iss.Issue.Number, fix.BlockLabel); err != nil {
		r.mu.Lock()
		r.labelRetry[k] = r.env.now().Add(r.cfg.Wait)
		r.mu.Unlock()
		r.logf("queue: %s#%d: add label %s: %v\nfix: create the label %s in %s, or set fix.blockLabel to a label that exists; trying again after %s",
			iss.Repo, iss.Issue.Number, fix.BlockLabel, err, fix.BlockLabel, iss.Repo, r.cfg.Wait)
		return
	}
	r.mu.Lock()
	rec := r.state.Fixes[k]
	if rec == nil {
		rec = &fixRecord{PR: pr.Number, Runs: runs}
		r.state.Fixes[k] = rec
	}
	rec.Blocked = true
	r.mu.Unlock()
	r.saveState()
	r.logf("queue: %s#%d PR #%d is still %s after %d fix run(s); labelled %s, no more fixes", iss.Repo, iss.Issue.Number, pr.Number, reason, runs, fix.BlockLabel)
	if len(fix.Notify) == 0 {
		return
	}
	vars := map[string]string{"repo": iss.Repo, "number": strconv.Itoa(iss.Issue.Number), "pr": strconv.Itoa(pr.Number), "reason": reason}
	if err := r.runCmd(ctx, "", expandArgv(fix.Notify, vars), nil); err != nil {
		r.logf("queue: %s#%d notify: %v", iss.Repo, iss.Issue.Number, err)
	}
}

func expandArgv(argv []string, vars map[string]string) []string {
	out := make([]string, len(argv))
	for i, a := range argv {
		out[i] = expand(a, vars)
	}
	return out
}

// fixVars is a start's placeholders plus the fix's own. {prompt} is the fix
// prompt, expanded first and then dropped in as one value.
func (r *queueRunner) fixVars(job *fixJob, ws string) map[string]string {
	effort, err := r.cfg.effortFor(job.iss.Issue.LabelNames())
	if err != nil {
		effort = r.cfg.Effort
	}
	vars := queueVars(r.cfg, []queuedIssue{job.iss}, ws, effort)
	vars["pr"] = strconv.Itoa(job.pr.Number)
	vars["branch"] = job.pr.HeadRefName
	vars["base"] = baseName(job.pr)
	vars["reason"] = job.reason
	vars["try"] = strconv.Itoa(job.try)
	vars["prompt"] = expand(r.cfg.Fix.Prompt, vars)
	return vars
}

// fixGate runs fix.gate, or the main gate when fix.gate is omitted.
func (r *queueRunner) fixGate(ctx context.Context, job *fixJob) error {
	gate := r.cfg.Fix.Gate
	if len(gate) == 0 {
		gate = r.cfg.Gate
	}
	if len(gate) == 0 {
		return nil
	}
	vars := r.fixVars(job, "")
	delete(vars, "prompt")
	stop := r.pulseHeartbeat(ctx)
	defer stop()
	return r.runCmd(ctx, "", expandArgv(gate, vars), nil)
}

// tryFix gates and starts one fix run. It answers like tick.
func (r *queueRunner) tryFix(ctx context.Context, job *fixJob) (bool, error) {
	k := job.iss.key()
	if err := r.fixGate(ctx, job); err != nil {
		if ctx.Err() != nil {
			r.logf("queue: stopping")
			return true, nil
		}
		switch {
		case gateSkipsRepo(err):
			r.logf("queue: fix %s PR #%d: gate skips the repository: %s; %s again after %s", k, job.pr.Number, err.Error(), job.iss.Repo, r.cfg.Wait)
			r.deferRepo(job.iss.Repo)
		case gateSkips(err):
			r.logf("queue: fix %s PR #%d: gate skips it: %s; again after %s", k, job.pr.Number, err.Error(), r.cfg.Wait)
			r.mu.Lock()
			r.fixRetry[k] = r.env.now().Add(r.cfg.Wait)
			r.mu.Unlock()
		default:
			r.logf("queue: fix %s PR #%d: gate: %s; waiting %s", k, job.pr.Number, err.Error(), r.cfg.Wait)
			r.pause(ctx)
		}
		return false, nil
	}
	if stop, err := r.finishIfStopped(ctx); stop || err != nil {
		return true, err
	}
	r.startFix(ctx, job)
	return false, nil
}

// startFix counts the run before it starts, so a crash mid-run still counts
// as a try. A run that never started is taken back.
func (r *queueRunner) startFix(ctx context.Context, job *fixJob) {
	k := job.iss.key()
	r.mu.Lock()
	r.running++
	r.fixing[k] = true
	now := r.env.now()
	r.active[k] = heartbeatRun{Issue: k, Agent: "fix", Since: now.UTC()}
	rec := r.state.Fixes[k]
	if rec == nil || rec.PR != job.pr.Number {
		rec = &fixRecord{PR: job.pr.Number}
		r.state.Fixes[k] = rec
	}
	rec.Runs++
	rec.Last = now
	// The first try's head stays. A later try is judged on the diff from
	// that head, so a suppression the first try pushed is still visible.
	if rec.Head == "" {
		rec.Head = job.pr.HeadRefOid
	}
	r.mu.Unlock()
	r.writeHeartbeat()
	r.saveState()
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		defer r.poke()
		defer func() {
			r.mu.Lock()
			r.running--
			delete(r.fixing, k)
			delete(r.active, k)
			r.mu.Unlock()
			r.writeHeartbeat()
		}()
		started := r.runFix(ctx, job)
		r.mu.Lock()
		if rec := r.state.Fixes[k]; rec != nil {
			if !started {
				rec.Runs--
				r.fixRetry[k] = r.env.now().Add(r.cfg.Wait)
			}
			rec.Last = r.env.now()
		}
		r.mu.Unlock()
		r.saveState()
	}()
}

// runFix runs the fix command in the issue's workspace and reports whether
// it ran at all.
func (r *queueRunner) runFix(ctx context.Context, job *fixJob) bool {
	k := job.iss.key()
	ws, err := r.env.spawn(job.iss.Repo, job.iss.Issue.Number)
	if err != nil {
		r.logf("queue: fix %s: spawn: %v", k, err)
		return false
	}
	if ctx.Err() != nil {
		return false
	}
	vars := r.fixVars(job, ws)
	argv := expandArgv(r.cfg.Fix.Command, vars)
	r.logf("queue: fix %s PR #%d: %s, try %d of %d, in %s", k, job.pr.Number, job.reason, job.try, r.cfg.Fix.Tries, ws)
	why := fmt.Sprintf("PR #%d (%s, try %d): %s", job.pr.Number, job.reason, job.try, commandSummary(r.cfg.Fix.Command, argv))
	r.recordDecision(job.iss.Repo, strconv.Itoa(job.iss.Issue.Number), "fix", "fix_start", why)
	defer r.clearActive([]queuedIssue{job.iss})
	err = r.runCmd(ctx, ws, argv, func(pid int) {
		r.recordActive([]queuedIssue{job.iss}, pid)
	})
	var exitErr *exec.ExitError
	switch {
	case err != nil && ctx.Err() != nil:
		// Stopped with the queue: the run did not get to finish, so it
		// is not a try.
		r.logf("queue: fix %s PR #%d stopped with the queue; not counted as a try", k, job.pr.Number)
		return false
	case err == nil:
		r.logf("queue: fix %s PR #%d exited", k, job.pr.Number)
	case errors.As(err, &exitErr):
		r.logf("queue: fix %s PR #%d exited: %v", k, job.pr.Number, err)
	default:
		r.logf("queue: fix %s PR #%d did not start: %v", k, job.pr.Number, err)
		return false
	}
	return true
}

// recordStarted remembers the issues of a run, so their pull requests are
// the queue's own after a restart, and so the cap window counts them.
// marker is the run's trailer token, empty when the fix stage is off.
func (r *queueRunner) recordStarted(run []queuedIssue, marker string) {
	if r.state == nil {
		return
	}
	r.mu.Lock()
	for _, iss := range run {
		rec := startedIssue{
			Repo: iss.Repo, Number: iss.Issue.Number, At: r.env.now(),
			Title: iss.Issue.Title, URL: iss.Issue.URL, CreatedAt: iss.Issue.CreatedAt,
			Labels: iss.Issue.LabelNames(), Marker: marker,
		}
		r.state.Started[iss.key()] = rec
		if r.cfg.Cap != nil {
			r.state.noteStart(rec)
		}
	}
	r.mu.Unlock()
	r.saveState()
}

// saveState writes the state file. A dry run writes nothing.
func (r *queueRunner) saveState() {
	if r.dry || r.cfg.State == "" || r.state == nil {
		return
	}
	r.mu.Lock()
	err := r.state.save(r.cfg.State)
	r.mu.Unlock()
	if err != nil {
		r.setLastError(err.Error())
		r.logf("queue: %v", err)
	}
}
