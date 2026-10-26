package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"

	"github.com/RiccardoCereghino/facet/internal/claudex"
	"github.com/RiccardoCereghino/facet/internal/routing"
	"github.com/spf13/pflag"
)

// launch is what `facet spawn` runs once the workspace is ready. The zero value
// runs nothing: spawn prints where to work and returns, and the operator opens
// the workspace however they like. Both options run in this terminal, in the
// home clone, and block until they exit.
type launch struct {
	// Claude runs the claude CLI.
	Claude bool
	// Remote overrides whether that claude gets Remote Control. Nil leaves it
	// to routing's spawn.rc.
	Remote *bool
	// Exec is a shell command to run instead.
	Exec string
}

// launchFlags is the flag surface behind launch.
type launchFlags struct {
	claude, remote, rc, noRC bool
	exec                     string
}

func (l *launchFlags) register(f *pflag.FlagSet) {
	f.BoolVar(&l.claude, "claude", false, "once the workspace is ready, run claude in the home clone, in this terminal")
	f.BoolVar(&l.remote, "remote", false, "with --claude, give claude Remote Control (default: routing's spawn.rc)")
	f.StringVar(&l.exec, "exec", "", "once the workspace is ready, run this shell command in the home clone, in this terminal; $FACET_WORKSPACE is the workspace")

	f.BoolVar(&l.rc, "rc", false, "deprecated alias for --claude --remote")
	f.BoolVar(&l.noRC, "no-rc", false, "deprecated: launching nothing is the default")
	_ = f.MarkDeprecated("rc", "use --claude --remote")
	_ = f.MarkDeprecated("no-rc", "launching nothing is now the default")

	// facet no longer opens a multiplexer. The flags that drove it stay
	// accepted, and do nothing, so an old command line still spawns.
	f.Bool("attach", false, "")
	f.Bool("no-attach", false, "")
	f.Bool("session", false, "")
	f.String("mux", "", "")
	const noMux = "facet no longer opens a multiplexer; use --exec to run one, for example --exec 'tmux new-window -c \"$PWD\"'"
	for _, name := range []string{"attach", "session", "mux"} {
		_ = f.MarkDeprecated(name, noMux)
	}
	_ = f.MarkDeprecated("no-attach", "opening nothing is the default")
}

// resolve folds the deprecated aliases in and refuses a contradiction. It runs
// before spawn touches anything, so a refusal leaves nothing behind.
func (l launchFlags) resolve(f *pflag.FlagSet) (launch, error) {
	c := launch{Claude: l.claude, Exec: l.exec}
	if !f.Changed("claude") && l.rc && !l.noRC {
		c.Claude = true
	}
	if f.Changed("remote") {
		r := l.remote
		c.Remote = &r
	} else if c.Claude && l.rc {
		r := true
		c.Remote = &r
	}
	if c.Claude && c.Exec != "" {
		return launch{}, fmt.Errorf("--claude and --exec both say what to run; pass one")
	}
	return c, nil
}

// claudeOptions is the claude invocation for a workspace: Remote Control named
// after it when asked for, or when routing turns it on.
func claudeOptions(l launch, name string, route *routing.Routing) claudex.Options {
	remote := route.SpawnRC()
	if l.Remote != nil {
		remote = *l.Remote
	}
	return claudex.Options{
		RemoteControl:     remote,
		SessionName:       name,
		SessionNamePrefix: route.SpawnSessionPrefix(),
	}
}

// runLaunch runs what l asks for in workDir. It reports what it starts on w.
// The caller treats an error as a warning: by now the workspace is complete.
func runLaunch(w io.Writer, l launch, ws, workDir, name string, route *routing.Routing) error {
	switch {
	case l.Exec != "":
		_, _ = fmt.Fprintf(w, "\nrunning %s in %s\n", l.Exec, workDir)
		cmd := exec.Command("/bin/sh", "-c", l.Exec)
		cmd.Dir = workDir
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		// PWD is set because the child inherits ours otherwise, and a shell
		// that trusts it would report the directory spawn was run from.
		cmd.Env = append(os.Environ(), "FACET_WORKSPACE="+ws, "PWD="+workDir)
		return cmd.Run()
	case l.Claude:
		opts := claudeOptions(l, name, route)
		_, _ = fmt.Fprintf(w, "\nlaunching %s in %s\n", claudex.ShellCommand(opts), workDir)
		return claudex.Launch(workDir, opts)
	}
	return nil
}
