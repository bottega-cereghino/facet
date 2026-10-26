// Package mux answers one question for `facet reap`: is anything still running
// in a workspace that is about to be deleted?
//
// facet used to open workspaces in tmux itself. It no longer does: spawn prints
// where to work, and opening a multiplexer is the operator's business (pass
// `facet spawn --exec` a tmux command to have one opened). What remains is the
// read side. A tmux session named after the workspace, a pane rooted inside
// it, or a process whose working directory is inside it all hold the reap, and
// a lingering session of that name is killed before the directory goes.
package mux

import (
	"os/exec"
)

// Tmux reads the tmux server.
//
// Every session name is targeted with an exact-match prefix (`-t "=<name>"`).
// Without it tmux does prefix-matching, and `iss-repo-6` would match
// `iss-repo-67`.
type Tmux struct{}

// tmuxRun and tmuxOutput are the two ways this package talks to tmux, and the
// only places the executable is named.
//
// They take no socket argument. Tests that need an isolated server pass
// `-L <socket>` as part of argv (see liveroots.go); teaching these helpers
// about sockets would put test-only state in the production path.

// tmuxRun runs a tmux command for its exit status alone.
func tmuxRun(args ...string) error { return exec.Command("tmux", args...).Run() }

// tmuxOutput runs a tmux command and returns its stdout verbatim.
func tmuxOutput(args ...string) (string, error) {
	out, err := exec.Command("tmux", args...).Output()
	return string(out), err
}

// Available reports whether tmux is installed.
func (Tmux) Available() bool { return tmuxRun("-V") == nil }

// Live reports whether the named session exists on the tmux server. tmux has
// no "exited" state: a session is either present or it is not.
func (Tmux) Live(session string) bool {
	return tmuxRun("has-session", "-t", "="+session) == nil
}

// Kill ends the session, and reports success when there was nothing to kill.
func (Tmux) Kill(name string) error {
	// Killing a session that never existed makes tmux exit non-zero, which is
	// not an error worth surfacing.
	_ = tmuxRun("kill-session", "-t", "="+name)
	return nil
}

// Pick returns tmux when it is installed, or nil.
func Pick() *Tmux {
	if (Tmux{}).Available() {
		return &Tmux{}
	}
	return nil
}
