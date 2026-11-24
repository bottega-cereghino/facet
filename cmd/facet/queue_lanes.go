package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// lanesTimeout is how long the lanes command may take. A slow answer must
// not hold the tick: the queue then keeps each lane label's own agent,
// which is the same choice it makes when the command cannot be read.
const lanesTimeout = 3 * time.Second

// queueLanesFile is the JSON shape of the lanes block, before checks.
type queueLanesFile struct {
	Command []string `json:"command"`
	Log     string   `json:"log"`
}

// queueLanes is a validated lanes block. Command prints which plans have
// room. Log is the ledger a fallback row is appended to, or empty.
type queueLanes struct {
	Command []string
	Log     string
}

// fallbackRow is one line of the lanes log. fallback_from is the lane the
// issue was labelled with; the run itself uses the default agent.
type fallbackRow struct {
	At           string `json:"at"`
	Repo         string `json:"repo"`
	Issue        int    `json:"issue"`
	FallbackFrom string `json:"fallback_from"`
	Agent        string `json:"agent"`
}

func normalizeLanes(configPath string, raw *queueLanesFile) (*queueLanes, error) {
	if raw == nil {
		return nil, nil
	}
	if len(raw.Command) == 0 || strings.TrimSpace(raw.Command[0]) == "" {
		return nil, fmt.Errorf("queue config: lanes.command is empty\nfix: set command to the program that prints which lanes have room")
	}
	cmd := append([]string(nil), raw.Command...)
	cmd[0] = strings.TrimSpace(cmd[0])
	logPath := strings.TrimSpace(raw.Log)
	if logPath != "" && !filepath.IsAbs(logPath) {
		logPath = filepath.Join(filepath.Dir(configPath), logPath)
	}
	return &queueLanes{Command: cmd, Log: logPath}, nil
}

// lanePreference is the lane a label names, when the label is lane:NAME.
// A model: label is not a preference: that issue keeps its agent.
// The match ignores case. The name is folded to lower case.
func lanePreference(label string) string {
	const prefix = "lane:"
	label = strings.TrimSpace(label)
	if len(label) <= len(prefix) || !strings.EqualFold(label[:len(prefix)], prefix) {
		return ""
	}
	name := strings.ToLower(strings.TrimSpace(label[len(prefix):]))
	if name == "" || strings.Contains(name, ":") {
		return ""
	}
	return name
}

// resolveAgent is agentFor, then the default agent when the matched entry
// is a lane preference and rooms says that lane has no room. rooms nil
// means the lanes command is not configured, or it could not be read:
// the labelled agent stays. Any room other than "no" stays too.
// The returned name is the lane that was given up, or "".
func (c queueConfig) resolveAgent(labels []string, rooms map[string]string) (queueAgent, string, error) {
	a, err := c.agentFor(labels)
	if err != nil {
		return queueAgent{}, "", err
	}
	from := lanePreference(a.Label)
	if from == "" || rooms == nil || !strings.EqualFold(rooms[from], "no") {
		return a, "", nil
	}
	fb, err := c.defaultAgent()
	if err != nil {
		return queueAgent{}, "", fmt.Errorf("lane %s has no room and there is no default agent\nfix: add an agents entry with an empty label; that command runs when a lane preference has no room", from)
	}
	return fb, from, nil
}

func (c queueConfig) defaultAgent() (queueAgent, error) {
	for i := range c.Agents {
		if strings.TrimSpace(c.Agents[i].Label) == "" {
			return c.Agents[i], nil
		}
	}
	return queueAgent{}, fmt.Errorf("no default agent\nfix: add an agents entry with an empty label")
}

// refreshRooms runs the lanes command once for this tick. A failure leaves
// rooms nil, so a lane label keeps its agent: a missing answer is not "no".
func (r *queueRunner) refreshRooms(ctx context.Context) {
	r.rooms = nil
	if r.cfg.Lanes == nil || len(r.cfg.Lanes.Command) == 0 {
		return
	}
	fn := r.env.lanes
	if fn == nil {
		fn = runLanes
	}
	out, err := fn(ctx, r.cfg.Lanes.Command)
	if err != nil {
		r.logf("queue: lanes: %s; a lane label keeps its agent", err.Error())
		return
	}
	rooms, err := parseLaneRooms(out)
	if err != nil {
		r.logf("queue: %s; a lane label keeps its agent", err.Error())
		return
	}
	r.rooms = rooms
}

// runLanes runs the lanes command and returns its standard output.
// It does not use a shell. The command has lanesTimeout.
func runLanes(ctx context.Context, argv []string) ([]byte, error) {
	if len(argv) == 0 || argv[0] == "" {
		return nil, fmt.Errorf("lanes command is empty\nfix: set lanes.command in the queue config")
	}
	ctx, cancel := context.WithTimeout(ctx, lanesTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.WaitDelay = time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		if i := strings.IndexByte(msg, '\n'); i >= 0 {
			msg = msg[:i]
		}
		return nil, fmt.Errorf("%s\nfix: the command must print the lanes JSON and exit 0; the queue keeps each lane label's agent until it does", msg)
	}
	return stdout.Bytes(), nil
}

// parseLaneRooms reads {"lanes":[{"lane":"name","room":"yes|no|unknown"}]}.
// key is accepted as well as lane. Names are folded to lower case.
// A later lane field wins over key on the same row.
func parseLaneRooms(b []byte) (map[string]string, error) {
	var raw struct {
		Lanes []struct {
			Lane string `json:"lane"`
			Key  string `json:"key"`
			Room string `json:"room"`
		} `json:"lanes"`
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("lanes output is not JSON\nfix: the command must print {\"lanes\":[{\"lane\":\"name\",\"room\":\"no\"}]}")
	}
	rooms := map[string]string{}
	for _, row := range raw.Lanes {
		room := strings.ToLower(strings.TrimSpace(row.Room))
		if k := strings.ToLower(strings.TrimSpace(row.Key)); k != "" {
			rooms[k] = room
		}
		if k := strings.ToLower(strings.TrimSpace(row.Lane)); k != "" {
			rooms[k] = room
		}
	}
	return rooms, nil
}

// noteFallback appends one ledger row for a run that left its lane.
// A dry run prints the decision and writes nothing. A write that fails
// is logged; the run still starts.
func (r *queueRunner) noteFallback(iss queuedIssue, from string) {
	if from == "" || r.cfg.Lanes == nil || r.cfg.Lanes.Log == "" || r.dry {
		return
	}
	row := fallbackRow{
		At:           r.env.now().UTC().Format(time.RFC3339),
		Repo:         iss.Repo,
		Issue:        iss.Issue.Number,
		FallbackFrom: from,
		Agent:        "default",
	}
	b, err := json.Marshal(row)
	if err != nil {
		r.logf("queue: %s#%d fallback log: %v", iss.Repo, iss.Issue.Number, err)
		return
	}
	r.fallbackMu.Lock()
	defer r.fallbackMu.Unlock()
	f, err := os.OpenFile(r.cfg.Lanes.Log, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		r.logf("queue: %s#%d fallback log: %v\nfix: lanes.log must be a file the queue can append to", iss.Repo, iss.Issue.Number, err)
		return
	}
	_, werr := f.Write(append(b, '\n'))
	cerr := f.Close()
	if werr != nil {
		r.logf("queue: %s#%d fallback log: %v", iss.Repo, iss.Issue.Number, werr)
		return
	}
	if cerr != nil {
		r.logf("queue: %s#%d fallback log: %v", iss.Repo, iss.Issue.Number, cerr)
	}
}
