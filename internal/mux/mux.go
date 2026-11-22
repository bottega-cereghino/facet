// Package mux answers one question for `facet reap`: is anything still running
// in a workspace that is about to be deleted?
//
// facet used to open workspaces in tmux itself. It no longer does: spawn prints
// where to work, and opening a multiplexer is the operator's business (pass
// `facet spawn --exec` a tmux command to have one opened). What remains is the
// read side. A pane rooted inside the workspace, in any session, or a process
// with anything inside it open holds the reap. A session named after the
// workspace is the workspace's only while one of its panes is rooted there;
// `reap --force` then ends it before the directory goes. One of that name whose
// panes all run elsewhere is someone else's, and is left alone.
package mux

import (
	"os"
	"os/exec"
	"strings"
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

// Live reports whether the named session has a pane rooted in dir, other than
// the one facet runs from. A session of that name whose panes all run
// elsewhere is not the workspace's: the name alone once held, and with
// --force killed, the user's own session for another project (#330).
func (Tmux) Live(session, dir string) bool {
	return len(tmuxPanesUnder(dir, "", session)) > 0
}

// Kill ends the named session when it is the workspace's (Live), and never the
// session facet itself runs in, which would end facet before its delete.
func (t Tmux) Kill(session, dir string) error {
	if !t.Live(session, dir) || ownSession() == session {
		return nil
	}
	// A session that ended meanwhile makes tmux exit non-zero, which is not an
	// error worth surfacing.
	_ = tmuxRun("kill-session", "-t", "="+session)
	return nil
}

// ownSession is the tmux session facet runs in, or "" outside tmux.
func ownSession() string {
	pane := os.Getenv("TMUX_PANE")
	if os.Getenv("TMUX") == "" || pane == "" {
		return ""
	}
	out, err := tmuxOutput("display-message", "-p", "-t", pane, "#{session_name}")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// Pick returns tmux when it is installed, or nil.
func Pick() *Tmux {
	if (Tmux{}).Available() {
		return &Tmux{}
	}
	return nil
}
