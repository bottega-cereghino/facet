package main

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/bottega-cereghino/facet/internal/ghx"
)

// prVerdict is what the queue makes of one pull request this round.
type prVerdict int

const (
	prGreen prVerdict = iota
	// prPending is not judged: a check is still running, or GitHub has not
	// worked out whether it merges.
	prPending
	prNeedsFix
)

// redConclusions are the check results that make a pull request red.
var redConclusions = []string{"FAILURE", "TIMED_OUT", "ACTION_REQUIRED", "STARTUP_FAILURE", "ERROR"}

// judgePR reads a pull request as `gh pr checks` would: the latest run of
// each check by name. Pending wins over red, so a check that is running
// again is not counted as a failure. A conflict is reported before a red
// check, since a fix has to merge the base first.
func judgePR(pr ghx.OpenPR) (prVerdict, string) {
	if pr.Mergeable == "" || strings.EqualFold(pr.Mergeable, "UNKNOWN") {
		return prPending, ""
	}
	latest := latestChecks(pr.Checks)
	for _, c := range latest {
		if checkPending(c) {
			return prPending, ""
		}
	}
	if strings.EqualFold(pr.Mergeable, "CONFLICTING") {
		base := pr.BaseRefName
		if base == "" {
			base = "its base"
		}
		return prNeedsFix, "conflicts with " + base
	}
	for _, c := range latest {
		if res := checkResult(c); isRed(res) {
			return prNeedsFix, fmt.Sprintf("red: %s is %s", checkName(c), strings.ToUpper(res))
		}
	}
	return prGreen, ""
}

func isStatus(c ghx.CheckRun) bool {
	return c.Typename == "StatusContext" || (c.Typename == "" && c.Context != "")
}

func checkName(c ghx.CheckRun) string {
	if isStatus(c) {
		return c.Context
	}
	return c.Name
}

func checkPending(c ghx.CheckRun) bool {
	if isStatus(c) {
		return strings.EqualFold(c.State, "PENDING") || strings.EqualFold(c.State, "EXPECTED")
	}
	return !strings.EqualFold(c.Status, "COMPLETED")
}

func checkResult(c ghx.CheckRun) string {
	if isStatus(c) {
		return c.State
	}
	return c.Conclusion
}

func isRed(result string) bool {
	for _, r := range redConclusions {
		if strings.EqualFold(result, r) {
			return true
		}
	}
	return false
}

// latestChecks keeps the most recently started run of each check name, in
// name order. Of two that started together, the later one listed wins.
func latestChecks(all []ghx.CheckRun) []ghx.CheckRun {
	byName := map[string]ghx.CheckRun{}
	for _, c := range all {
		name := checkName(c)
		if prev, ok := byName[name]; ok && compareCreated(c.StartedAt, prev.StartedAt) < 0 {
			continue
		}
		byName[name] = c
	}
	names := make([]string, 0, len(byName))
	for n := range byName {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]ghx.CheckRun, len(names))
	for i, n := range names {
		out[i] = byName[n]
	}
	return out
}

// prListing is one repository's open pull requests for this round.
type prListing struct {
	prs []ghx.OpenPR
	err error
}

// listPRs lists repo's open pull requests once per round. Both the fix stage
// and the open pull request check read the same listing.
func (r *queueRunner) listPRs(repo string) ([]ghx.OpenPR, error) {
	if got, ok := r.round[repo]; ok {
		return got.prs, got.err
	}
	prs, err := r.env.prs(repo)
	r.round[repo] = prListing{prs: prs, err: err}
	return prs, err
}

// fixJob is one fix run about to start.
type fixJob struct {
	iss    queuedIssue
	pr     ghx.OpenPR
	reason string
	try    int
}

// queuePR is the open pull request from iss's spawn branch, if any.
func queuePR(iss queuedIssue, prs []ghx.OpenPR) (ghx.OpenPR, bool) {
	prefix := issueBranchName(iss.Issue.Number, "")
	for _, pr := range prs {
		if strings.HasPrefix(pr.HeadRefName, prefix) {
			return pr, true
		}
	}
	return ghx.OpenPR{}, false
}

// startedQueued is the state's started issues as queued issues, so they sort
// like new work.
func (r *queueRunner) startedQueued() []queuedIssue {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]queuedIssue, 0, len(r.state.Started))
	for _, s := range r.state.Started {
		iss := ghx.Issue{Number: s.Number, Title: s.Title, URL: s.URL, CreatedAt: s.CreatedAt, State: "OPEN"}
		for _, l := range s.Labels {
			iss.Labels = append(iss.Labels, ghx.Label{Name: l})
		}
		out = append(out, queuedIssue{Repo: s.Repo, Issue: iss})
	}
	return out
}

// nextFix returns the first queue pull request that needs a fix run, in the
// order new issues are taken, or nil. On the way it clears the record of a
// pull request that is green or gone, and blocks an issue that used up its
// tries. Only issues this queue started are looked at, so a person's own
// pull request is never touched, whatever its branch is called.
func (r *queueRunner) nextFix(ctx context.Context) *fixJob {
	if r.cfg.Fix == nil || r.state == nil || r.env.prs == nil {
		return nil
	}
	projects, err := r.projectOrder()
	if err != nil {
		r.logf("queue: %v", err)
		return nil
	}
	failed := map[string]bool{}
	for _, iss := range orderQueued(r.startedQueued(), projects) {
		if r.fixBusy(iss) {
			continue
		}
		prs, err := r.listPRs(iss.Repo)
		if err != nil {
			if !failed[iss.Repo] {
				failed[iss.Repo] = true
				r.logf("queue: list open pull requests in %s: %v; starting no fix there this round", iss.Repo, err)
			}
			continue
		}
		pr, ok := queuePR(iss, prs)
		if !ok {
			r.forgetStarted(iss)
			continue
		}
		if job := r.judgeQueuePR(ctx, iss, pr); job != nil {
			return job
		}
	}
	return nil
}

// fixBusy reports an issue whose agent or fix run is still going, or that
// waits after a gate refused its fix.
func (r *queueRunner) fixBusy(iss queuedIssue) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	k := iss.key()
	if r.inflight[k] || r.fixing[k] {
		return true
	}
	now := r.env.now()
	if until, ok := r.repoWait[iss.Repo]; ok && now.Before(until) {
		return true
	}
	until, ok := r.fixRetry[k]
	return ok && now.Before(until)
}

// judgeQueuePR returns a fix job for pr, or nil when it is pending, green,
// blocked, or just used its last try.
func (r *queueRunner) judgeQueuePR(ctx context.Context, iss queuedIssue, pr ghx.OpenPR) *fixJob {
	verdict, reason := judgePR(pr)
	k := iss.key()
	r.mu.Lock()
	rec := r.state.Fixes[k]
	var runs int
	var settling, blocked bool
	if rec != nil {
		runs, blocked = rec.Runs, rec.Blocked
		// A fix run that just pushed may not have its checks listed yet.
		settling = r.env.now().Before(rec.Last.Add(r.cfg.Wait))
	}
	r.mu.Unlock()
	switch {
	case verdict == prPending || settling:
		return nil
	case verdict == prGreen:
		// No checks at all cannot tell a green PR from one whose checks
		// have not been listed yet, so that keeps the record.
		if rec != nil && len(pr.Checks) > 0 {
			r.clearFix(k, fmt.Sprintf("queue: %s PR #%d is green; clearing its fix record", k, pr.Number))
		}
		return nil
	case blocked:
		return nil
	case runs >= r.cfg.Fix.Tries:
		r.block(ctx, iss, pr, reason, runs)
		return nil
	}
	return &fixJob{iss: iss, pr: pr, reason: reason, try: runs + 1}
}

// forgetStarted drops an issue whose pull request is no longer open and
// that is not running: merged or closed. Its fix record goes with it.
func (r *queueRunner) forgetStarted(iss queuedIssue) {
	k := iss.key()
	r.mu.Lock()
	s, ok := r.state.Started[k]
	// An issue started a moment ago may not have opened its pull request.
	recent := ok && r.env.now().Before(s.At.Add(startedKeep))
	_, fixed := r.state.Fixes[k]
	if !ok || (recent && !fixed) {
		r.mu.Unlock()
		return
	}
	delete(r.state.Fixes, k)
	if !recent {
		delete(r.state.Started, k)
	}
	r.mu.Unlock()
	r.saveState()
}

// startedKeep is how long a started issue with no open pull request stays
// in the state. Its agent may still be about to open one.
const startedKeep = 24 * time.Hour

func (r *queueRunner) clearFix(k, msg string) {
	r.mu.Lock()
	delete(r.state.Fixes, k)
	r.mu.Unlock()
	r.logf("%s", msg)
	r.saveState()
}

// block labels the issue and stops fixing it. A label that cannot be added
// is tried again next round, so the issue is not left looking free.
func (r *queueRunner) block(ctx context.Context, iss queuedIssue, pr ghx.OpenPR, reason string, runs int) {
	fix := r.cfg.Fix
	if r.dry {
		r.logf("dry-run: would label %s#%d %s: PR #%d is %s after %d fix run(s)", iss.Repo, iss.Issue.Number, fix.BlockLabel, pr.Number, reason, runs)
		return
	}
	if r.env.addLabel == nil {
		r.logf("queue: %s#%d: cannot add the %s label: no label seam", iss.Repo, iss.Issue.Number, fix.BlockLabel)
		return
	}
	if err := r.env.addLabel(iss.Repo, iss.Issue.Number, fix.BlockLabel); err != nil {
		r.logf("queue: %s#%d: add label %s: %v; trying again next round", iss.Repo, iss.Issue.Number, fix.BlockLabel, err)
		return
	}
	r.mu.Lock()
	if rec := r.state.Fixes[iss.key()]; rec != nil {
		rec.Blocked = true
	}
	r.mu.Unlock()
	r.saveState()
	r.logf("queue: %s#%d PR #%d is still %s after %d fix run(s); labelled %s, no more fixes", iss.Repo, iss.Issue.Number, pr.Number, reason, runs, fix.BlockLabel)
	if len(fix.Notify) == 0 {
		return
	}
	vars := map[string]string{"repo": iss.Repo, "number": strconv.Itoa(iss.Issue.Number), "pr": strconv.Itoa(pr.Number), "reason": reason}
	if err := r.env.run(ctx, "", expandArgv(fix.Notify, vars)); err != nil {
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
	return r.env.run(ctx, "", expandArgv(gate, vars))
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
	rec := r.state.Fixes[k]
	if rec == nil || rec.PR != job.pr.Number {
		rec = &fixRecord{PR: job.pr.Number}
		r.state.Fixes[k] = rec
	}
	rec.Runs++
	rec.Last = r.env.now()
	r.mu.Unlock()
	r.saveState()
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		defer r.poke()
		defer func() {
			r.mu.Lock()
			r.running--
			delete(r.fixing, k)
			r.mu.Unlock()
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
	r.logf("queue: fix %s PR #%d: %s, try %d of %d, in %s", k, job.pr.Number, job.reason, job.try, r.cfg.Fix.Tries, ws)
	err = r.env.run(ctx, ws, expandArgv(r.cfg.Fix.Command, vars))
	var exitErr *exec.ExitError
	switch {
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
// the queue's own after a restart.
func (r *queueRunner) recordStarted(run []queuedIssue) {
	if r.cfg.Fix == nil || r.state == nil {
		return
	}
	r.mu.Lock()
	for _, iss := range run {
		r.state.Started[iss.key()] = startedIssue{
			Repo: iss.Repo, Number: iss.Issue.Number, At: r.env.now(),
			Title: iss.Issue.Title, URL: iss.Issue.URL, CreatedAt: iss.Issue.CreatedAt,
			Labels: iss.Issue.LabelNames(),
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
		r.logf("queue: %v", err)
	}
}
