package main

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	defaultFixTries      = 2
	maxFixTries          = 5
	defaultFixBlockLabel = "blocked"
)

// defaultFixPrompt is what a fix run is told when fix.prompt is empty.
// It opens with the rule that a fix takes the long road or stops, and it
// forbids clearing a ratchet by suppressing the finding.
const defaultFixPrompt = "Never take a shortcut. Take the long road, or stop, say so in the report, and label the issue blocked. " +
	"Pull request #{pr} on {branch} in {repo} is {reason}. This is try {try}. " +
	"The workspace is {workspace}. Make CI green on the same branch. If it conflicts, merge origin/{base} into it. " +
	"Never weaken a test without the repository's own rule for that. " +
	"Never clear a ratchet, baseline or lint finding by suppressing it " +
	"(a non-null assertion, an as cast, any, @ts-ignore, # type: ignore, eslint-disable, nolint, or a higher baseline or limit). " +
	"If that is the only way, stop and say so. Push, then exit."

// defaultForbid is the pattern list used when fix.forbid is omitted. Each
// entry is a regular expression matched against lines a fix run added.
// An empty fix.forbid turns the check off; it does not mean this list.
// The first defaultCastPatterns entries are casts: they apply only to
// TypeScript files, outside strings, comments and import or export lines.
// The rest are comment directives and apply to any code file. A fix.forbid
// that is set is used as written, with none of this scoping.
var defaultForbid = []string{
	`(?:[[:alpha:]_][[:alnum:]_]*|\]|\))!(?:[^\w=]|$)`,
	` as `,
	`\bas any\b`,
	`@ts-ignore`,
	`@ts-expect-error`,
	`# type: ignore`,
	`eslint-disable`,
	`\bnolint\b`,
	`\bnoqa\b`,
}

// defaultCastPatterns is how many leading defaultForbid entries are casts.
const defaultCastPatterns = 3

// queueFix is the validated fix block: how the queue repairs its own red or
// conflicting pull requests before it starts new work.
type queueFix struct {
	Command    []string
	Prompt     string
	Gate       []string
	Tries      int
	BlockLabel string
	Notify     []string
	// Forbid is matched against lines a fix run added. Nil when the check
	// is off (fix.forbid is an empty list). Omitted fix.forbid compiles
	// defaultForbid.
	Forbid []*regexp.Regexp
	// Land updates a green queue pull request that is behind its base, and
	// merges one that is up to date, without a model run.
	Land bool
	// forbidOff is set when fix.forbid is an empty list, which turns the
	// suppression check off. Omitted fix.forbid leaves it false.
	forbidOff bool
	// forbidCustom is set when fix.forbid lists patterns: they match every
	// added line of every file, as written. False means defaultForbid, with
	// its scoping.
	forbidCustom bool
}

// queueFixFile is the JSON shape of fix, before defaults.
type queueFixFile struct {
	Command    []string `json:"command"`
	Prompt     string   `json:"prompt"`
	Gate       []string `json:"gate"`
	Tries      *int     `json:"tries"`
	BlockLabel string   `json:"blockLabel"`
	Notify     []string `json:"notify"`
	Land       *bool    `json:"land"`
	// Forbid nil means the default patterns. A pointer distinguishes an
	// omitted field from an empty list, which turns the check off.
	Forbid *[]string `json:"forbid"`
}

// resolveQueueState makes a relative state path sit beside the config file.
// An empty path stays empty; the caller says which block required it.
func resolveQueueState(configPath, state string) string {
	state = strings.TrimSpace(state)
	if state == "" {
		return ""
	}
	if !filepath.IsAbs(state) {
		return filepath.Join(filepath.Dir(configPath), state)
	}
	return state
}

// normalizeFix resolves the fix block and the state path. Without fix the
// queue behaves as before and needs no state file.
func normalizeFix(configPath, state string, raw *queueFixFile) (*queueFix, string, error) {
	if raw == nil {
		return nil, "", nil
	}
	state = resolveQueueState(configPath, state)
	if state == "" {
		return nil, "", fmt.Errorf("queue config: fix is set and state is not\nfix: set state to a file path, where the queue records the issues it started and the fix runs it made")
	}
	fix := &queueFix{
		Command:    raw.Command,
		Prompt:     defaultFixPrompt,
		Gate:       raw.Gate,
		Tries:      defaultFixTries,
		BlockLabel: defaultFixBlockLabel,
		Notify:     raw.Notify,
		Land:       raw.Land == nil || *raw.Land,
	}
	if strings.TrimSpace(raw.Prompt) != "" {
		fix.Prompt = raw.Prompt
	}
	if raw.Tries != nil {
		fix.Tries = *raw.Tries
	}
	if l := strings.TrimSpace(raw.BlockLabel); l != "" {
		fix.BlockLabel = l
	}
	patterns := defaultForbid
	switch {
	case raw.Forbid == nil:
	case len(*raw.Forbid) == 0:
		fix.forbidOff = true
		patterns = nil
	default:
		patterns = *raw.Forbid
		fix.forbidCustom = true
	}
	if !fix.forbidOff {
		compiled, err := compileForbid(patterns)
		if err != nil {
			return nil, "", err
		}
		fix.Forbid = compiled
	}
	if err := fix.validate(); err != nil {
		return nil, "", err
	}
	return fix, state, nil
}

// compileForbid compiles the suppression patterns. An empty entry is a
// refusal: it would match every line and block every fix.
func compileForbid(patterns []string) ([]*regexp.Regexp, error) {
	out := make([]*regexp.Regexp, 0, len(patterns))
	for _, p := range patterns {
		if strings.TrimSpace(p) == "" {
			return nil, fmt.Errorf("queue config: fix.forbid has an empty pattern\nfix: give a regular expression, or an empty list to turn the check off")
		}
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("queue config: fix.forbid %q: %w\nfix: give a valid regular expression, or an empty list to turn the check off", p, err)
		}
		out = append(out, re)
	}
	return out, nil
}

// defaultForbidPatterns is defaultForbid compiled. The patterns are fixed
// and valid; a failure here is a programming error.
var defaultForbidPatterns = mustCompileForbid(defaultForbid)

func mustCompileForbid(patterns []string) []*regexp.Regexp {
	compiled, err := compileForbid(patterns)
	if err != nil {
		panic(err)
	}
	return compiled
}

// checksForbid reports whether a fix run's diff is checked for suppressions.
func (f *queueFix) checksForbid() bool {
	return f != nil && !f.forbidOff
}

// forbidPatterns is the list the check uses. Nil Forbid means the defaults:
// a queueFix built in a test does not go through the config loader.
func (f *queueFix) forbidPatterns() []*regexp.Regexp {
	if f == nil || f.forbidOff {
		return nil
	}
	if len(f.Forbid) == 0 {
		return defaultForbidPatterns
	}
	return f.Forbid
}

func (f *queueFix) validate() error {
	if len(f.Command) == 0 || strings.TrimSpace(f.Command[0]) == "" {
		return fmt.Errorf("queue config: fix has an empty command\nfix: set fix.command to an argument list, with {prompt} where the task text goes")
	}
	if !commandHasPrompt(f.Command) {
		return fmt.Errorf("queue config: fix.command has no {prompt}\nfix: put {prompt} in the argument that receives the task text")
	}
	if f.Tries < 1 || f.Tries > maxFixTries {
		return fmt.Errorf("queue config: fix.tries is %d\nfix: set tries from 1 to %d, or omit it for %d", f.Tries, maxFixTries, defaultFixTries)
	}
	if strings.Contains(f.BlockLabel, ",") {
		return fmt.Errorf("queue config: fix.blockLabel %q contains a comma\nfix: name one label", f.BlockLabel)
	}
	if len(f.Gate) > 0 && strings.TrimSpace(f.Gate[0]) == "" {
		return fmt.Errorf("queue config: fix.gate has an empty command\nfix: set fix.gate to an argument list, or omit it to run the main gate")
	}
	if len(f.Notify) > 0 && strings.TrimSpace(f.Notify[0]) == "" {
		return fmt.Errorf("queue config: fix.notify has an empty command\nfix: set fix.notify to an argument list, or omit it")
	}
	return nil
}
