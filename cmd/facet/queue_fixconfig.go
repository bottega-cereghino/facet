package main

import (
	"fmt"
	"path/filepath"
	"strings"
)

const (
	defaultFixTries      = 2
	maxFixTries          = 5
	defaultFixBlockLabel = "blocked"
)

// defaultFixPrompt is what a fix run is told when fix.prompt is empty.
const defaultFixPrompt = "Pull request #{pr} on {branch} in {repo} is {reason}. This is try {try}. " +
	"The workspace is {workspace}. Make CI green on the same branch. If it conflicts, merge origin/{base} into it. " +
	"Never weaken a test without the repository's own rule for that. Push, then exit."

// queueFix is the validated fix block: how the queue repairs its own red or
// conflicting pull requests before it starts new work.
type queueFix struct {
	Command    []string
	Prompt     string
	Gate       []string
	Tries      int
	BlockLabel string
	Notify     []string
	// Land updates a green queue pull request that is behind its base, and
	// merges one that is up to date, without a model run.
	Land bool
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
}

// normalizeFix resolves the fix block and the state path. Without fix the
// queue behaves as before and needs no state file.
func normalizeFix(configPath, state string, raw *queueFixFile) (*queueFix, string, error) {
	state = strings.TrimSpace(state)
	if raw == nil {
		return nil, "", nil
	}
	if state == "" {
		return nil, "", fmt.Errorf("queue config: fix is set and state is not\nfix: set state to a file path, where the queue records the issues it started and the fix runs it made")
	}
	if !filepath.IsAbs(state) {
		state = filepath.Join(filepath.Dir(configPath), state)
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
	if err := fix.validate(); err != nil {
		return nil, "", err
	}
	return fix, state, nil
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
