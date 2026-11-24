package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/bottega-cereghino/facet/internal/config"
)

// diskGB is one gigabyte as df -g counts it on macOS: 1024^3 bytes.
// minFreeGB in the config is this unit. The log says GB, which is the name
// the config and df -g use for it.
const diskGB int64 = 1024 * 1024 * 1024

// defaultDiskMinFreeGB is the free-space floor when disk is set and
// minFreeGB is omitted. Below it, no new run starts.
const defaultDiskMinFreeGB = 20

// queueDiskFile is the JSON shape of disk, before defaults.
type queueDiskFile struct {
	Path          string `json:"path"`
	MinFreeGB     *int   `json:"minFreeGB"`
	MaxWorkspaces *int   `json:"maxWorkspaces"`
}

// queueDisk is a validated disk block. MaxWorkspaces nil means no count limit.
type queueDisk struct {
	Path          string
	MinFreeGB     int
	MaxWorkspaces *int
}

// diskView is one tick's disk check. have is the issue keys (repo#n) that
// already have a workspace, so a fix for one of them adds nothing.
type diskView struct {
	block bool
	line  string
	have  map[string]bool
}

// normalizeDisk resolves disk. A nil raw means the queue does not look at
// the disk. A relative path is beside the config file, as the other paths are.
func normalizeDisk(configPath string, raw *queueDiskFile) (*queueDisk, error) {
	if raw == nil {
		return nil, nil
	}
	d := &queueDisk{MinFreeGB: defaultDiskMinFreeGB}
	if raw.MinFreeGB != nil {
		if *raw.MinFreeGB < 0 {
			return nil, fmt.Errorf("queue config: disk.minFreeGB is %d\nfix: set minFreeGB to 0 or more, in gigabytes of free space", *raw.MinFreeGB)
		}
		d.MinFreeGB = *raw.MinFreeGB
	}
	if raw.MaxWorkspaces != nil {
		if *raw.MaxWorkspaces < 1 {
			return nil, fmt.Errorf("queue config: disk.maxWorkspaces is %d\nfix: set maxWorkspaces to at least 1, or omit it for no count limit", *raw.MaxWorkspaces)
		}
		n := *raw.MaxWorkspaces
		d.MaxWorkspaces = &n
	}
	p := strings.TrimSpace(raw.Path)
	if p != "" && !filepath.IsAbs(p) {
		p = filepath.Join(filepath.Dir(configPath), p)
	}
	d.Path = p
	return d, nil
}

// noteDisk runs the disk check once and logs a refusal or a clearance only
// when that state changes. The first look at a clear disk logs nothing:
// clear is where the queue already was. A configured decision log gets the
// same line, as "disk" or "disk_clear".
func (r *queueRunner) noteDisk() diskView {
	if r.cfg.Disk == nil {
		return diskView{}
	}
	view := r.measureDisk()
	r.mu.Lock()
	seen, held := r.diskSeen, r.diskHeld
	r.diskSeen = true
	r.diskHeld = view.block
	r.mu.Unlock()
	switch {
	case !seen && !view.block:
		return view
	case seen && held == view.block:
		return view
	case view.block:
		r.sayDisk(view.line, "disk")
	default:
		r.sayDisk("disk: clear", "disk_clear")
	}
	return view
}

func (r *queueRunner) sayDisk(line, decision string) {
	r.logf("%s", line)
	r.recordDecision("", "", "", decision, line)
}

// measureDisk is the free-space floor and the workspace count. A probe that
// errors refuses new work: not knowing and having room must not look the same.
func (r *queueRunner) measureDisk() diskView {
	volume, root := diskFolders(r.cfg.Disk.Path)
	if volume == "" || root == "" {
		return diskView{block: true, line: "disk: no folder to watch: starting no new work"}
	}
	n, have, err := issueWorkspaces(root)
	if err != nil {
		return diskView{block: true, line: "disk: " + firstErrorLine(err.Error()) + ": starting no new work", have: have}
	}
	var parts []string
	if msg, freeErr := r.freePart(volume, r.cfg.Disk.MinFreeGB); freeErr != nil || msg != "" {
		parts = append(parts, msg)
	}
	if lim := r.cfg.Disk.MaxWorkspaces; lim != nil && n >= *lim {
		parts = append(parts, workspaceLimitPhrase(n, *lim))
	}
	if len(parts) == 0 {
		return diskView{have: have}
	}
	return diskView{
		block: true,
		line:  "disk: " + strings.Join(parts, ", and ") + ": starting no new work",
		have:  have,
	}
}

// diskFolders is the volume to measure and the directory to count. path is
// whose volume is watched. The issue workspaces live in the workspaces root
// even when path points at another folder on that volume. Either one, when
// the other was left empty, stands in for both.
func diskFolders(path string) (volume, root string) {
	volume = path
	root = roots.Workspaces
	if volume == "" {
		volume = root
	}
	if root == "" {
		root = volume
	}
	return volume, root
}

// freePart is the free-space phrase when the volume is under the floor, or
// the error phrase when the volume cannot be read. Empty and a nil error
// means there is room.
func (r *queueRunner) freePart(volume string, minGB int) (string, error) {
	fn := r.env.diskFree
	if fn == nil {
		fn = diskFreeBytes
	}
	free, err := fn(volume)
	if err != nil {
		return "free space of " + volume + ": " + firstErrorLine(err.Error()), err
	}
	if free < int64(minGB)*diskGB {
		return fmt.Sprintf("%d GB free, below %d", free/diskGB, minGB), nil
	}
	return "", nil
}

func workspaceLimitPhrase(n, limit int) string {
	word := "workspaces"
	if n == 1 {
		word = "workspace"
	}
	return fmt.Sprintf("%d issue %s, limit %d", n, word, limit)
}

// issueWorkspaces counts iss-* directories directly under root whose
// manifest has an issue, and the issue key of each. A symlink is not a
// directory here, so it cannot point the count outside root. A manifest
// that cannot be read refuses the count: skipping it could hide a workspace.
func issueWorkspaces(root string) (int, map[string]bool, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, map[string]bool{}, nil
		}
		return 0, nil, fmt.Errorf("read workspaces %s: %w\nfix: FACET_WORKSPACES must be a directory, or set disk.path", root, err)
	}
	have := map[string]bool{}
	n := 0
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), config.IssuePrefix) {
			continue
		}
		m, err := readIssueManifest(filepath.Join(root, e.Name()))
		if err != nil {
			return n, have, err
		}
		if m == nil || m.Issue == nil {
			continue
		}
		n++
		have[m.Issue.Repo+"#"+strconv.Itoa(m.Issue.Number)] = true
	}
	return n, have, nil
}

// nextFix is the first fix job that may start. While the disk check is
// refusing, a job whose issue has no workspace would spawn one, so it waits.
func (r *queueRunner) nextFix(jobs []*fixJob, disk diskView) *fixJob {
	for _, job := range jobs {
		if disk.block && !disk.have[job.iss.key()] {
			line := fmt.Sprintf("disk: fix %s would spawn a workspace; leaving it", job.iss.key())
			r.noteOnce("disk-fix:"+job.iss.key(), line, func() { r.logf("%s", line) })
			continue
		}
		return job
	}
	return nil
}
