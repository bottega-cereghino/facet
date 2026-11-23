package main

import (
	"fmt"
	"strconv"
	"strings"
)

// aloneLabels are issue labels that are one run, whatever the agent's
// bundle is. A feature or a research issue stays on its own so a failure
// in it is not hidden inside a batch. The match ignores case and is the
// whole label: feature is not feat.
var aloneLabels = []string{"feat", "research"}

func issueStaysAlone(labels []string) bool {
	for _, label := range labels {
		for _, alone := range aloneLabels {
			if strings.EqualFold(strings.TrimSpace(label), alone) {
				return true
			}
		}
	}
	return false
}

// bundleFor chooses the issues one run takes. ranked is in start order,
// highest project first. blocked reports an issue this process will not
// start now.
//
// The first free issue is the run, whatever its project: a project whose
// issues are all running, done or waiting to retry leaves the slot to the
// next project down. Further issues join it only while they are the next
// free issue, in the same repository, for the same agent and the same
// effort, and neither issue is feat or research. A free issue that does not
// fit stops the run, so a later small issue does not jump ahead of it. The
// size is the agent's bundle, or 1 when the first issue stays alone.
func bundleFor(cfg queueConfig, ranked []queuedIssue, blocked func(queuedIssue) bool, rooms map[string]string) []queuedIssue {
	for i, iss := range ranked {
		if !blocked(iss) {
			return fillBundle(cfg, iss, ranked[i+1:], blocked, rooms)
		}
	}
	return nil
}

func fillBundle(cfg queueConfig, primary queuedIssue, rest []queuedIssue, blocked func(queuedIssue) bool, rooms map[string]string) []queuedIssue {
	run := []queuedIssue{primary}
	limit := runLimit(cfg, primary, rooms)
	if limit < 2 {
		return run
	}
	who, effort, ok := bundleKey(cfg, primary, rooms)
	if !ok {
		return run
	}
	for _, iss := range rest {
		if len(run) >= limit {
			break
		}
		if blocked(iss) {
			continue
		}
		if !joinsBundle(cfg, primary, iss, who, effort, rooms) {
			break
		}
		run = append(run, iss)
	}
	return run
}

// runLimit is how many issues may follow the primary, including it.
// A feature or research issue is one. A missing agent is one, and the
// start reports that.
func runLimit(cfg queueConfig, primary queuedIssue, rooms map[string]string) int {
	if issueStaysAlone(primary.Issue.LabelNames()) {
		return 1
	}
	a, _, err := cfg.resolveAgent(primary.Issue.LabelNames(), rooms)
	if err != nil || a.Bundle < 2 {
		return 1
	}
	return a.Bundle
}

// bundleKey is the agent label and effort a companion has to share.
// ok is false when the effort labels disagree or there is no agent:
// that issue is started on its own, and the start reports the refusal.
func bundleKey(cfg queueConfig, iss queuedIssue, rooms map[string]string) (string, string, bool) {
	a, _, err := cfg.resolveAgent(iss.Issue.LabelNames(), rooms)
	if err != nil {
		return "", "", false
	}
	effort, err := cfg.effortFor(iss.Issue.LabelNames())
	if err != nil {
		return "", "", false
	}
	return a.Label, effort, true
}

func joinsBundle(cfg queueConfig, primary, iss queuedIssue, who, effort string, rooms map[string]string) bool {
	if iss.Repo != primary.Repo || issueStaysAlone(iss.Issue.LabelNames()) {
		return false
	}
	gotWho, gotEffort, ok := bundleKey(cfg, iss, rooms)
	return ok && gotWho == who && strings.EqualFold(gotEffort, effort)
}

func (r *queueRunner) argvFor(run []queuedIssue, ws, effort string, rooms map[string]string) ([]string, string, string, error) {
	iss := run[0]
	a, from, err := r.cfg.resolveAgent(iss.Issue.LabelNames(), rooms)
	if err != nil {
		return nil, "", "", err
	}
	vars := queueVars(r.cfg, run, ws, effort)
	// prompt is expanded first, then dropped in as a single value, so a title
	// is not scanned for further placeholders.
	prompt := expand(r.cfg.Prompt, vars)
	vars["prompt"] = prompt
	vars["fallback_from"] = from
	out := make([]string, len(a.Command))
	for i, arg := range a.Command {
		out[i] = expand(arg, vars)
	}
	who := a.Label
	if who == "" {
		who = "default"
	}
	return out, who, from, nil
}

// queueVars is the placeholders one run substitutes. {prompt} is added by
// the caller after the prompt itself has been expanded, so a value is not
// scanned for further placeholders. labels is comma-separated; a label
// does not contain a comma.
func queueVars(cfg queueConfig, run []queuedIssue, ws, effort string) map[string]string {
	iss := run[0]
	return map[string]string{
		"repo":          iss.Repo,
		"number":        strconv.Itoa(iss.Issue.Number),
		"title":         iss.Issue.Title,
		"workspace":     ws,
		"label":         cfg.Label,
		"url":           iss.Issue.URL,
		"effort":        effort,
		"issues":        issuesText(run),
		"bundle":        strconv.Itoa(len(run)),
		"labels":        strings.Join(iss.Issue.LabelNames(), ","),
		"fallback_from": "",
	}
}

// gateArgv expands the gate command for the run about to start. The gate
// runs before spawn, so {workspace} is empty: there is no directory yet.
// {prompt} is not a gate placeholder. The prompt is the agent's text.
func gateArgv(cfg queueConfig, run []queuedIssue, effort string) []string {
	vars := queueVars(cfg, run, "", effort)
	out := make([]string, len(cfg.Gate))
	for i, a := range cfg.Gate {
		out[i] = expand(a, vars)
	}
	return out
}

// issuesText is the {issues} value: one line per issue, repo#n: title.
func issuesText(run []queuedIssue) string {
	lines := make([]string, len(run))
	for i, iss := range run {
		lines[i] = fmt.Sprintf("%s#%d: %s", iss.Repo, iss.Issue.Number, iss.Issue.Title)
	}
	return strings.Join(lines, "\n")
}
