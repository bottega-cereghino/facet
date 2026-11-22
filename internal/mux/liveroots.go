package mux

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// LiveRoots reports tmux panes, in any session, and processes -- via lsof --
// whose current working directory is rooted inside dir. Live only checks the
// one session named after the workspace; a pane moved elsewhere (tmux's
// link-window does exactly this) would otherwise still hold a process open on
// a directory reap is about to delete, unnoticed.
//
// facet itself is left out, with the lsof it runs and the shell that ran it:
// `facet reap` run from inside the workspace, its documented form, would
// otherwise always be held by its own shell, itself and its probe. Anything
// else in that shell's session still counts.
//
// tmux missing, or no server running, means no panes. lsof ships with macOS,
// so lsof missing or failing is an error, and the caller holds what it cannot
// check: read as "nothing runs", it would let reap and prune delete a
// directory a process runs from. The panes found are returned with it.
func (Tmux) LiveRoots(dir string) ([]string, error) {
	live := tmuxPanesUnder(dir, "")
	procs, err := lsofProcessesUnder(dir)
	return append(live, procs...), err
}

// ProcessesUnder is the lsof half of LiveRoots, for a machine with no
// multiplexer: a process rooted in dir holds it whether or not tmux exists.
func ProcessesUnder(dir string) ([]string, error) { return lsofProcessesUnder(dir) }

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
		if pid == strconv.Itoa(os.Getppid()) {
			continue // the shell facet was run from, in this pane
		}
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
// What runs inside such a VM is not visible from here either way. A git object
// file with another hard link is left out too (withoutSharedObjects). lsof
// sees only processes this user may inspect.
//
// lsof exits 1 with nothing on stderr when nothing matches, and at times
// when something does; its stdout is read either way. Any other exit, a
// warning on stderr, or lsof missing is an error: a directory it cannot stat
// reads exactly like one nothing runs from.
func lsofProcessesUnder(dir string) ([]string, error) {
	if _, err := exec.LookPath("lsof"); err != nil {
		return nil, fmt.Errorf("lsof is not on PATH (it ships with macOS in /usr/sbin): %w", err)
	}
	var stderr strings.Builder
	cmd := exec.Command("lsof", "-FpRcfn", "+D", canonicalPath(dir))
	cmd.Stderr = &stderr
	raw, err := cmd.Output()
	var probeErr error
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 1 || stderr.Len() != 0 {
			probeErr = fmt.Errorf("lsof failed: %w: %s", err, strings.TrimSpace(stderr.String()))
		}
	}
	// What lsof did report is named even when it failed.
	var live []string
	for _, p := range withoutSelf(parseLsof(string(raw)), os.Getpid(), os.Getppid()) {
		p.files = withoutSharedObjects(p.files)
		if p.cwd == "" && (len(p.files) == 0 || isSystemService(p.pid)) {
			continue
		}
		live = append(live, p.describe())
	}
	return live, probeErr
}

// lsofProc is one process in lsof's output and what it holds under the
// directory asked about.
type lsofProc struct {
	pid, ppid, cmd, cwd string
	files               []string
}

// withoutSelf drops facet's own process, the lsof it started and lsof's own
// helper (found through their parent pids), and the shell that ran it
// (parent). Any other child of facet still counts.
func withoutSelf(procs []lsofProc, self, parent int) []lsofProc {
	mine := map[string]bool{strconv.Itoa(self): true}
	for grew := true; grew; {
		grew = false
		for _, p := range procs {
			if !mine[p.pid] && mine[p.ppid] && p.cmd == "lsof" {
				mine[p.pid], grew = true, true
			}
		}
	}
	out := make([]lsofProc, 0, len(procs))
	for _, p := range procs {
		if mine[p.pid] || p.pid == strconv.Itoa(parent) {
			continue
		}
		out = append(out, p)
	}
	return out
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
		case 'R':
			if len(procs) > 0 {
				procs[len(procs)-1].ppid = line[1:]
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

// withoutSharedObjects drops the git object files that have another hard link.
// lsof +D matches by device and inode, and a clone's objects are hardlinks
// shared with the mirror and sibling clones, so a process in another
// workspace holding its own pack is reported under this one's path (#331).
// Deleting one name of such a file breaks no process that has it open, so it
// holds nothing here, whoever has it open.
func withoutSharedObjects(files []string) []string {
	out := files[:0]
	for _, f := range files {
		if !sharedObject(f) {
			out = append(out, f)
		}
	}
	return out
}

// sharedObject reports whether path is a regular file under a git directory's
// objects with more than one hard link.
func sharedObject(path string) bool {
	slash := filepath.ToSlash(path)
	i := strings.Index(slash, "/.git/")
	if i < 0 || !strings.Contains(slash[i:], "/objects/") {
		return false
	}
	fi, err := os.Stat(path)
	if err != nil || !fi.Mode().IsRegular() {
		return false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && uint64(st.Nlink) > 1
}

// isSystemService reports whether the process runs an executable the
// operating system ships (systemExecutable), read from the kernel, never from
// the argv[0] a process may set to anything. A process that cannot be looked
// up is not one: when in doubt, it holds the workspace.
var isSystemService = func(pid string) bool {
	return systemExecutable(execPath(pid))
}

// systemExecutable reports whether path is a service macOS ships under
// /System. The apps under /System/Applications are the user's (TextEdit,
// Preview), and /System/Volumes/Data is the user's own disk.
func systemExecutable(path string) bool {
	return strings.HasPrefix(path, "/System/") &&
		!strings.HasPrefix(path, "/System/Applications/") &&
		!strings.HasPrefix(path, "/System/Volumes/")
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
