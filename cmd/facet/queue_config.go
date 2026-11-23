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
	defaultQueueMax     = 6
	defaultQueuePerRepo = 2
	defaultQueueWait    = 30 * time.Second
	defaultQueueLabel   = "next"
	defaultQueueEffort  = "medium"
	// maxQueueBundle is the most issues one agent run takes. One is the
	// default, and a larger batch hides which issue failed.
	maxQueueBundle = 5
)

// defaultQueuePrompt is what an agent is told when the config leaves prompt
// empty. The issue body is not in it: spawn writes that into the workspace,
// quoted, and a prompt is an argument vector. {issues} is one line per
// issue in the run, so a bundle still names every issue.
const defaultQueuePrompt = "Do each issue below, in order. The workspace is {workspace}. " +
	"{issues} " +
	"Do the work each issue describes. Merge when the checks are green. " +
	"Comment the result on the issue. Remove the {label} label when the issue is done. Then exit."

// queueConfig is a validated queue file. Durations and defaults are resolved.
type queueConfig struct {
	Max          int
	PerRepo      int
	Hold         []string
	Gate         []string
	Wait         time.Duration
	Label        string
	Priorities   string
	Repos        []string
	Prompt       string
	Agents       []queueAgent
	Effort       string
	effortLabels map[string]string
	// State is the state file's path, set when Fix is.
	State string
	// Fix is nil when the config has no fix block.
	Fix *queueFix
	// Lanes is nil when the config has no lanes block. A lane: label is
	// then always its own agent, as before.
	Lanes *queueLanes
	// Serialize is nil when the config has no serialize block. Landing then
	// does not serialize on shared files.
	Serialize *queueSerialize
}

// queueAgent is one command, chosen when the issue carries Label.
// An empty Label is the default, used when no other entry matches.
// Bundle is how many issues one run of Command takes from one repository.
type queueAgent struct {
	Label   string
	Command []string
	Bundle  int
}

// queueAgentFile is the JSON shape of one agent, before defaults.
type queueAgentFile struct {
	Label   string   `json:"label"`
	Command []string `json:"command"`
	Bundle  *int     `json:"bundle"`
}

// queueEffortFile is the JSON shape of the effort map, before checks.
// Default is the effort when the issue carries none of Labels.
type queueEffortFile struct {
	Default string            `json:"default"`
	Labels  map[string]string `json:"labels"`
}

// queueHolds is the hold field: one path, or a list of paths. A list lets a
// stop for every tool and a stop for the queue alone both apply.
type queueHolds []string

func (h *queueHolds) UnmarshalJSON(b []byte) error {
	var one string
	if err := json.Unmarshal(b, &one); err == nil {
		*h = queueHolds{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return fmt.Errorf("hold is a path or a list of paths: %w", err)
	}
	*h = many
	return nil
}

// paths drops blank entries, so "" and [] both mean no hold file.
func (h queueHolds) paths() []string {
	var out []string
	for _, p := range h {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// queueFile is the JSON shape, before defaults and checks.
type queueFile struct {
	Max        *int                `json:"max"`
	PerRepo    *int                `json:"per_repo"`
	Hold       queueHolds          `json:"hold"`
	Gate       []string            `json:"gate"`
	Wait       string              `json:"wait"`
	Label      string              `json:"label"`
	Priorities string              `json:"priorities"`
	Repos      []string            `json:"repos"`
	Prompt     string              `json:"prompt"`
	Agents     []queueAgentFile    `json:"agents"`
	Effort     queueEffortFile     `json:"effort"`
	State      string              `json:"state"`
	Fix        *queueFixFile       `json:"fix"`
	Lanes      *queueLanesFile     `json:"lanes"`
	Serialize  *queueSerializeFile `json:"serialize"`
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
		return queueConfig{}, fmt.Errorf("queue config %s: %w\nfix: the fields are max, per_repo, hold, gate, wait, label, priorities, repos, prompt, agents, effort, state, fix, lanes, serialize", path, err)
	}
	if dec.More() {
		return queueConfig{}, fmt.Errorf("queue config %s: extra data after the JSON value\nfix: the file must be one JSON object", path)
	}
	return raw.normalize(path)
}

func (f queueFile) normalize(configPath string) (queueConfig, error) {
	cfg := queueConfig{
		Max:     defaultQueueMax,
		PerRepo: defaultQueuePerRepo,
		Hold:    f.Hold.paths(),
		Gate:    f.Gate,
		Wait:    defaultQueueWait,
		Label:   defaultQueueLabel,
		Prompt:  defaultQueuePrompt,
	}
	if f.Max != nil {
		cfg.Max = *f.Max
	}
	if f.PerRepo != nil {
		cfg.PerRepo = *f.PerRepo
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
	for _, raw := range f.Agents {
		cfg.Agents = append(cfg.Agents, raw.agent())
	}
	effort, labels, err := normalizeEffort(cfg.Label, f.Effort)
	if err != nil {
		return queueConfig{}, err
	}
	cfg.Effort = effort
	cfg.effortLabels = labels
	cfg.Fix, cfg.State, err = normalizeFix(configPath, f.State, f.Fix)
	if err != nil {
		return queueConfig{}, err
	}
	cfg.Lanes, err = normalizeLanes(configPath, f.Lanes)
	if err != nil {
		return queueConfig{}, err
	}
	cfg.Serialize, err = normalizeSerialize(f.Serialize)
	if err != nil {
		return queueConfig{}, err
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
	if c.PerRepo < 1 {
		return fmt.Errorf("queue config: per_repo must be at least 1\nfix: set per_repo to how many agents one repository may run at once")
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
	if err := validateQueueAgents(c.Label, c.Agents); err != nil {
		return err
	}
	return validateBundlePrompt(c)
}

func (f queueAgentFile) agent() queueAgent {
	n := 1
	if f.Bundle != nil {
		n = *f.Bundle
	}
	return queueAgent{
		Label:   strings.TrimSpace(f.Label),
		Command: f.Command,
		Bundle:  n,
	}
}

// normalizeEffort resolves the default and the label map. Label keys are
// matched later without regard to case, so two keys that differ only by
// case are the same label and are refused here.
func normalizeEffort(queueLabel string, raw queueEffortFile) (string, map[string]string, error) {
	effort := defaultQueueEffort
	if raw.Default != "" {
		effort = strings.TrimSpace(raw.Default)
		if effort == "" {
			return "", nil, fmt.Errorf("queue config: effort default is empty\nfix: set default to a word such as medium, or omit it")
		}
	}
	labels := make(map[string]string, len(raw.Labels))
	for name, value := range raw.Labels {
		n := strings.TrimSpace(name)
		v := strings.TrimSpace(value)
		if n == "" || v == "" {
			return "", nil, fmt.Errorf("queue config: an effort entry is empty\nfix: each labels entry is a label and an effort, such as \"fix\": \"low\"")
		}
		if strings.EqualFold(n, queueLabel) {
			return "", nil, fmt.Errorf("queue config: effort label %q is the queue label\nfix: the queue label is on every queued issue, so it cannot choose an effort; use a label such as fix or feat", n)
		}
		key := strings.ToLower(n)
		if _, ok := labels[key]; ok {
			return "", nil, fmt.Errorf("queue config: two effort labels are %q\nfix: keep one entry per label", n)
		}
		labels[key] = v
	}
	return effort, labels, nil
}

// effortFor is the effort the issue's labels ask for. Labels that are not
// in the map are ignored. Two that name different efforts are a refusal:
// guessing would hide one of them. None matching uses the default.
func (c queueConfig) effortFor(labels []string) (string, error) {
	var (
		effort string
		from   string
	)
	for _, label := range labels {
		key := strings.ToLower(strings.TrimSpace(label))
		if key == "" {
			continue
		}
		got, ok := c.effortLabels[key]
		if !ok {
			continue
		}
		if effort == "" {
			effort = got
			from = strings.TrimSpace(label)
			continue
		}
		if !strings.EqualFold(effort, got) {
			return "", fmt.Errorf("labels %s and %s ask for effort %s and %s\nfix: map those labels to one effort, or take one of them off the issue", from, strings.TrimSpace(label), effort, got)
		}
	}
	if effort == "" {
		if c.Effort == "" {
			return defaultQueueEffort, nil
		}
		return c.Effort, nil
	}
	return effort, nil
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
		if a.Bundle < 1 || a.Bundle > maxQueueBundle {
			return fmt.Errorf("queue config: the agent labelled %q has bundle %d\nfix: set bundle from 1 to %d, or omit it for 1", displayLabel(label), a.Bundle, maxQueueBundle)
		}
	}
	if defaults != 1 {
		return fmt.Errorf("queue config: %d agents have an empty label, want 1\nfix: one entry with an empty label is the command used when no other label matches", defaults)
	}
	return nil
}

// validateBundlePrompt refuses a batch whose prompt cannot name every
// issue. A run of one still has {number} and {title}.
func validateBundlePrompt(c queueConfig) error {
	if strings.Contains(c.Prompt, "{issues}") {
		return nil
	}
	for _, a := range c.Agents {
		if a.Bundle > 1 {
			return fmt.Errorf("queue config: the agent labelled %q bundles %d issues and the prompt has no {issues}\nfix: put {issues} in the prompt, where the list of issues in the run goes", displayLabel(a.Label), a.Bundle)
		}
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

// commandFor returns the label and argv of the agent agentFor chose.
func (c queueConfig) commandFor(labels []string) (string, []string, error) {
	a, err := c.agentFor(labels)
	if err != nil {
		return "", nil, err
	}
	return a.Label, a.Command, nil
}

// agentFor returns the first agent whose label the issue carries, or the
// default entry when none do. Matching is case-insensitive. The default is
// not a match-all: it is only the fallback, wherever it sits in the list.
func (c queueConfig) agentFor(labels []string) (queueAgent, error) {
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
			return *a, nil
		}
	}
	if fallback == nil {
		return queueAgent{}, fmt.Errorf("no default agent\nfix: add an agents entry with an empty label")
	}
	return *fallback, nil
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
