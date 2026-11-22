package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	defaultQueueMax   = 6
	defaultQueueWait  = 30 * time.Second
	defaultQueueLabel = "next"
)

// defaultQueuePrompt is what an agent is told when the config leaves prompt
// empty. The issue body is not in it: spawn writes that into the workspace,
// quoted, and a prompt is an argument vector.
const defaultQueuePrompt = "Do {repo}#{number}: {title}. The workspace is {workspace}. " +
	"Do the work the issue describes. Merge when the checks are green. " +
	"Comment the result on the issue. Remove the {label} label when the issue is done. Then exit."

// queueConfig is a validated queue file. Durations and defaults are resolved.
type queueConfig struct {
	Max        int
	Hold       string
	Gate       []string
	Wait       time.Duration
	Label      string
	Priorities string
	Repos      []string
	Prompt     string
	Agents     []queueAgent
}

// queueAgent is one command, chosen when the issue carries Label.
// An empty Label is the default, used when no other entry matches.
type queueAgent struct {
	Label   string   `json:"label"`
	Command []string `json:"command"`
}

// queueFile is the JSON shape, before defaults and checks.
type queueFile struct {
	Max        *int         `json:"max"`
	Hold       string       `json:"hold"`
	Gate       []string     `json:"gate"`
	Wait       string       `json:"wait"`
	Label      string       `json:"label"`
	Priorities string       `json:"priorities"`
	Repos      []string     `json:"repos"`
	Prompt     string       `json:"prompt"`
	Agents     []queueAgent `json:"agents"`
}

func loadQueueConfig(path string) (queueConfig, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return queueConfig{}, fmt.Errorf("read queue config: %w\nfix: pass --config or set FACET_QUEUE to a JSON file (docs/queue.example.json is an example)", err)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var raw queueFile
	if err := dec.Decode(&raw); err != nil {
		return queueConfig{}, fmt.Errorf("queue config %s: %w\nfix: the fields are max, hold, gate, wait, label, priorities, repos, prompt, agents", path, err)
	}
	if dec.More() {
		return queueConfig{}, fmt.Errorf("queue config %s: extra data after the JSON value\nfix: the file must be one JSON object", path)
	}
	return raw.normalize(path)
}

func (f queueFile) normalize(configPath string) (queueConfig, error) {
	cfg := queueConfig{
		Max:    defaultQueueMax,
		Hold:   strings.TrimSpace(f.Hold),
		Gate:   f.Gate,
		Wait:   defaultQueueWait,
		Label:  defaultQueueLabel,
		Prompt: defaultQueuePrompt,
		Agents: f.Agents,
	}
	if f.Max != nil {
		cfg.Max = *f.Max
	}
	if strings.TrimSpace(f.Label) != "" {
		cfg.Label = strings.TrimSpace(f.Label)
	}
	if strings.TrimSpace(f.Prompt) != "" {
		cfg.Prompt = f.Prompt
	}
	if strings.TrimSpace(f.Wait) != "" {
		d, err := time.ParseDuration(strings.TrimSpace(f.Wait))
		if err != nil {
			return queueConfig{}, fmt.Errorf("queue config: wait %q is not a duration\nfix: use a Go duration such as 30s or 1m", f.Wait)
		}
		cfg.Wait = d
	}
	if p := strings.TrimSpace(f.Priorities); p != "" {
		if !filepath.IsAbs(p) {
			p = filepath.Join(filepath.Dir(configPath), p)
		}
		cfg.Priorities = p
		// Read once here so a missing file fails before the loop. The loop
		// reads it again on each pick, so a later edit is not missed.
		if _, err := readPriorities(p); err != nil {
			return queueConfig{}, err
		}
	}
	for _, repo := range f.Repos {
		cfg.Repos = append(cfg.Repos, strings.TrimSpace(repo))
	}
	for i := range cfg.Agents {
		cfg.Agents[i].Label = strings.TrimSpace(cfg.Agents[i].Label)
	}
	if err := cfg.validate(); err != nil {
		return queueConfig{}, err
	}
	return cfg, nil
}

func (c queueConfig) validate() error {
	if c.Max < 1 {
		return fmt.Errorf("queue config: max must be at least 1\nfix: set max to how many agents may run at once (a hold file is how you pause)")
	}
	if c.Wait <= 0 {
		return fmt.Errorf("queue config: wait must be greater than zero\nfix: use a Go duration such as 30s, or a hold file to stop the queue")
	}
	if strings.Contains(c.Label, ",") {
		return fmt.Errorf("queue config: label %q contains a comma\nfix: name one label; a comma would ask gh for several", c.Label)
	}
	if err := validateQueueRepos(c.Repos); err != nil {
		return err
	}
	return validateQueueAgents(c.Label, c.Agents)
}

func validateQueueRepos(repos []string) error {
	if len(repos) == 0 {
		return fmt.Errorf("queue config: no repos\nfix: set repos to a list of owner/name")
	}
	seen := map[string]bool{}
	for _, repo := range repos {
		owner, name, ok := strings.Cut(repo, "/")
		if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
			return fmt.Errorf("queue config: repo %q is not owner/name\nfix: use the form owner/name, one repository per entry", repo)
		}
		if seen[repo] {
			return fmt.Errorf("queue config: repo %s is listed twice\nfix: list each repository once", repo)
		}
		seen[repo] = true
	}
	return nil
}

func validateQueueAgents(queueLabel string, agents []queueAgent) error {
	if len(agents) == 0 {
		return fmt.Errorf("queue config: no agents\nfix: add an agents entry whose label is empty; that command runs when the issue carries none of the others")
	}
	seen := map[string]bool{}
	defaults := 0
	for _, a := range agents {
		label := strings.TrimSpace(a.Label)
		if label == "" {
			defaults++
		}
		if strings.EqualFold(label, queueLabel) && label != "" {
			return fmt.Errorf("queue config: an agent is labelled %q, which is also the queue label\nfix: the queue label marks work and an agent label chooses the command; use a different agent label", label)
		}
		if label != "" {
			key := strings.ToLower(label)
			if seen[key] {
				return fmt.Errorf("queue config: two agents are labelled %q\nfix: keep one entry per label; the first match is the one that would have run", a.Label)
			}
			seen[key] = true
		}
		if len(a.Command) == 0 || strings.TrimSpace(a.Command[0]) == "" {
			return fmt.Errorf("queue config: the agent labelled %q has an empty command\nfix: set command to an argument list, with {prompt} where the task text goes", displayLabel(label))
		}
		if !commandHasPrompt(a.Command) {
			return fmt.Errorf("queue config: the agent labelled %q has no {prompt}\nfix: put {prompt} in the argument that receives the task text", displayLabel(label))
		}
	}
	if defaults != 1 {
		return fmt.Errorf("queue config: %d agents have an empty label, want 1\nfix: one entry with an empty label is the command used when no other label matches", defaults)
	}
	return nil
}

func displayLabel(label string) string {
	if label == "" {
		return "(default)"
	}
	return label
}

func commandHasPrompt(argv []string) bool {
	for _, a := range argv {
		if strings.Contains(a, "{prompt}") {
			return true
		}
	}
	return false
}

// commandFor returns the first agent whose label the issue carries, or the
// default entry when none do. Matching is case-insensitive. The default is
// not a match-all: it is only the fallback, wherever it sits in the list.
func (c queueConfig) commandFor(labels []string) (string, []string, error) {
	have := map[string]bool{}
	for _, l := range labels {
		have[strings.ToLower(l)] = true
	}
	var fallback *queueAgent
	for i := range c.Agents {
		a := &c.Agents[i]
		if strings.TrimSpace(a.Label) == "" {
			if fallback == nil {
				fallback = a
			}
			continue
		}
		if have[strings.ToLower(a.Label)] {
			return a.Label, a.Command, nil
		}
	}
	if fallback == nil {
		return "", nil, fmt.Errorf("no default agent\nfix: add an agents entry with an empty label")
	}
	return "", fallback.Command, nil
}

// expand replaces {key} placeholders in one pass. A value is not scanned
// again, so an issue title that itself contains {workspace} stays text.
func expand(s string, vars map[string]string) string {
	var b strings.Builder
	for {
		i := strings.IndexByte(s, '{')
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		b.WriteString(s[:i])
		rest := s[i+1:]
		j := strings.IndexByte(rest, '}')
		if j < 0 {
			b.WriteByte('{')
			s = rest
			continue
		}
		if v, ok := vars[rest[:j]]; ok {
			b.WriteString(v)
		} else {
			b.WriteByte('{')
			b.WriteString(rest[:j])
			b.WriteByte('}')
		}
		s = rest[j+1:]
	}
}
