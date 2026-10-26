package mux

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

// LiveRoots reports tmux panes, in any session, and processes -- via lsof --
// whose current working directory is rooted inside dir. Live only checks the
// one session named after the workspace; a pane moved elsewhere (tmux's
// link-window does exactly this) would otherwise still hold a process open on
// a directory reap is about to delete, unnoticed.
//
// Both probes are a convenience layered on top of the git-based checks in
// package workspace: a tool that is missing, or a server that is not running,
// is not a failure -- it means there is nothing to report, the same way
// Available and Kill degrade elsewhere in this package.
func (Tmux) LiveRoots(dir string) ([]string, error) {
	var live []string
	live = append(live, tmuxPanesUnder(dir, "")...)
	live = append(live, lsofProcessesUnder(dir)...)
	return live, nil
}

// tmuxPanesUnder lists, as human-readable descriptions naming the target and
// pid, every pane across every session whose pane_current_path is dir or a
// directory beneath it. socket selects an alternate server via `tmux -L
// <socket>`, so tests can target an isolated server; production callers pass
// "" for the default one.
func tmuxPanesUnder(dir, socket string) []string {
	if !(Tmux{}).Available() {
		return nil
	}
	dir = canonicalPath(dir)
	argv := []string{"list-panes", "-a", "-F",
		"#{session_name}:#{window_index}.#{pane_index}\t#{pane_pid}\t#{pane_current_path}"}
	if socket != "" {
		argv = append([]string{"-L", socket}, argv...)
	}
	raw, err := tmuxOutput(argv...)
	if err != nil && len(raw) == 0 {
		// No server running exits non-zero with nothing on stdout -- that is
		// "no panes", not a failure worth surfacing.
		return nil
	}
	var live []string
	for _, line := range strings.Split(strings.TrimRight(raw, "\n"), "\n") {
		if line == "" {
			continue
		}
		f := strings.SplitN(line, "\t", 3)
		if len(f) != 3 {
			continue
		}
		target, pid, path := f[0], f[1], f[2]
		if underRoot(dir, path) {
			live = append(live, "tmux pane "+target+" (pid "+pid+") at "+path)
		}
	}
	return live
}

// lsofProcessesUnder lists, as human-readable descriptions naming the command
// and pid, every process holding something inside dir open: its working
// directory, its executable, or any other file. It is the fallback for a live
// process that tmux does not know about at all -- one started outside any
// pane -- and the only probe available on a host with no tmux.
//
// It used to match the working directory alone, which missed a watcher
// started elsewhere that keeps a file in the workspace open; a refusal then
// named some of the holders and read as if it named all of them (facet#171).
//
// A macOS system service with files open but its working directory elsewhere
// is left out: Spotlight indexing the tree, or the virtualization service
// sharing it with a VM, would otherwise hold every workspace on the machine.
// What runs inside such a VM is not visible from here either way. lsof sees
// only processes this user may inspect.
func lsofProcessesUnder(dir string) []string {
	if _, err := exec.LookPath("lsof"); err != nil {
		return nil
	}
	raw, err := exec.Command("lsof", "-Fpcfn", "+D", canonicalPath(dir)).Output()
	if err != nil && len(raw) == 0 {
		// lsof exits non-zero with nothing on stdout when nothing matches --
		// that is "no processes", not a failure worth surfacing.
		return nil
	}
	var live []string
	for _, p := range parseLsof(string(raw)) {
		if p.cwd == "" && isSystemService(p.pid) {
			continue
		}
		live = append(live, p.describe())
	}
	return live
}

// lsofProc is one process in lsof's output and what it holds under the
// directory asked about.
type lsofProc struct {
	pid, cmd, cwd string
	files         []string
}

// parseLsof reads lsof's -Fpcfn output into its processes, in the order lsof
// listed them, keeping only those that hold something.
func parseLsof(raw string) []lsofProc {
	var procs []lsofProc
	fd := ""
	for _, line := range strings.Split(raw, "\n") {
		if line == "" {
			continue
		}
		switch line[0] {
		case 'p':
			procs = append(procs, lsofProc{pid: line[1:]})
			fd = ""
		case 'c':
			if len(procs) > 0 {
				procs[len(procs)-1].cmd = line[1:]
			}
		case 'f':
			fd = line[1:]
		case 'n':
			switch {
			case len(procs) == 0:
			case fd == "cwd":
				procs[len(procs)-1].cwd = line[1:]
			default:
				procs[len(procs)-1].files = append(procs[len(procs)-1].files, line[1:])
			}
		}
	}
	out := procs[:0]
	for _, p := range procs {
		if p.cwd != "" || len(p.files) > 0 {
			out = append(out, p)
		}
	}
	return out
}

// describe names the process and where it holds the directory: its working
// directory when that is inside, otherwise the first file it has open and how
// many more.
func (p lsofProc) describe() string {
	desc := "process " + p.cmd + " (pid " + p.pid + ")"
	switch {
	case p.cwd != "":
		return desc + " at " + p.cwd
	case len(p.files) == 1:
		return desc + " has " + p.files[0] + " open"
	default:
		return desc + fmt.Sprintf(" has %s and %d more file(s) open", p.files[0], len(p.files)-1)
	}
}

// isSystemService reports whether the process runs an executable the
// operating system ships, under /System. A process that cannot be looked up
// is not one: when in doubt, it holds the workspace.
var isSystemService = func(pid string) bool {
	out, err := exec.Command("ps", "-o", "comm=", "-p", pid).Output()
	return err == nil && strings.HasPrefix(strings.TrimSpace(string(out)), "/System/")
}

// underRoot reports whether path is root itself or lies beneath it.
func underRoot(root, path string) bool {
	root, path = canonicalPath(root), canonicalPath(path)
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || (!strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel))
}

// canonicalPath resolves symlinks so a directory reached through one (macOS
// routes /tmp and /var through /private) still matches what tmux or lsof
// report, which is already the resolved form. A path that cannot be resolved
// (already gone, or one lsof reported that this process cannot stat) falls
// back to a plain Clean.
func canonicalPath(p string) string {
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved
	}
	return filepath.Clean(p)
}
