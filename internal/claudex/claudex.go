// Package claudex launches the Claude Code CLI, the same way ghx/gitx wrap gh
// and git: facet shells out rather than embedding anything.
//
// It backs `facet spawn --claude`, which runs claude in the home clone once the
// workspace is ready, optionally with Remote Control so the session is
// reachable over Anthropic's relay. Remote Control needs the CLI signed in via
// claude.ai OAuth (Pro/Max/Team, not an API key) and the workspace trusted
// once; a launch failure is surfaced but never fatal to a spawn.
//
// ONE place decides what the invocation looks like: Args. Launch runs it;
// ShellCommand renders the same invocation as the command line spawn prints.
package claudex

import (
	"os"
	"os/exec"
	"strings"
)

// Exe is the CLI facet starts. Not configurable: `facet spawn --exec` runs any
// other command instead.
const Exe = "claude"

// Options describes one claude invocation.
type Options struct {
	// RemoteControl enables Remote Control for the session.
	RemoteControl bool
	// SessionName names the Remote Control session. Ignored when RemoteControl
	// is false; empty means let the CLI name the session itself.
	SessionName string
	// SessionNamePrefix, when set, is exported as
	// CLAUDE_REMOTE_CONTROL_SESSION_NAME_PREFIX so sessions are named per host.
	SessionNamePrefix string
}

// Args returns the arguments that follow Exe.
//
// The session name is attached with `--remote-control=<name>`, never as the
// positional `--rc <name>`. The value is optional, so a positional name is
// ambiguous the moment anything else follows it on the command line: an initial
// prompt is read as the session name, or the name as the prompt, depending on
// which way the CLI resolves it. The `=` form cannot be misread, and costs
// nothing when there is no prompt.
func Args(o Options) []string {
	if !o.RemoteControl {
		return nil
	}
	if o.SessionName == "" {
		return []string{"--remote-control"}
	}
	return []string{"--remote-control=" + o.SessionName}
}

// ShellCommand renders the invocation as a single POSIX shell command line,
// including the session-name-prefix assignment when there is one. It is what
// spawn prints before it launches; nothing here blocks or touches stdio.
func ShellCommand(o Options) string {
	var b strings.Builder
	if o.SessionNamePrefix != "" {
		b.WriteString("CLAUDE_REMOTE_CONTROL_SESSION_NAME_PREFIX=")
		b.WriteString(ShellQuote(o.SessionNamePrefix))
		b.WriteString(" ")
	}
	b.WriteString(Exe)
	for _, a := range Args(o) {
		b.WriteString(" ")
		b.WriteString(ShellQuote(a))
	}
	return b.String()
}

// ShellQuote makes s a single POSIX shell word. Anything outside a conservative
// safe set is single-quoted, and an embedded single quote is closed, escaped and
// reopened -- the only way to quote one inside single quotes.
//
// A workspace name reaches the printed command line through here, and a
// workspace name comes from an issue title.
func ShellQuote(s string) string {
	if s == "" {
		return "''"
	}
	if !strings.ContainsFunc(s, needsQuoting) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func needsQuoting(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return false
	}
	return !strings.ContainsRune("-_./=:@,+", r)
}

// Launch runs claude interactively in dir. The call blocks for the life of the
// session, wired to the current stdio, and returns whatever the CLI exits with.
func Launch(dir string, o Options) error {
	cmd := exec.Command(Exe, Args(o)...)
	cmd.Dir = dir
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = os.Environ()
	if o.SessionNamePrefix != "" {
		cmd.Env = append(cmd.Env, "CLAUDE_REMOTE_CONTROL_SESSION_NAME_PREFIX="+o.SessionNamePrefix)
	}
	return cmd.Run()
}
