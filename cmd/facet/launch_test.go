package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RiccardoCereghino/facet/internal/claudex"
	"github.com/RiccardoCereghino/facet/internal/routing"
	"github.com/spf13/pflag"
)

// resolveFlags parses argv the way cobra would, so the Changed() bookkeeping
// resolve depends on is real.
func resolveFlags(t *testing.T, argv ...string) (launch, error) {
	t.Helper()
	var l launchFlags
	f := pflag.NewFlagSet("x", pflag.ContinueOnError)
	f.SetOutput(io.Discard)
	l.register(f)
	if err := f.Parse(argv); err != nil {
		t.Fatalf("parsing %v: %v", argv, err)
	}
	return l.resolve(f)
}

func TestLaunchFlagsResolve(t *testing.T) {
	yes, no := true, false
	tests := []struct {
		name   string
		argv   []string
		claude bool
		remote *bool
		exec   string
	}{
		{"no flags: run nothing", nil, false, nil, ""},
		{"--claude: claude, routing decides Remote Control", []string{"--claude"}, true, nil, ""},
		{"--claude --remote=false", []string{"--claude", "--remote=false"}, true, &no, ""},
		{"--claude --remote", []string{"--claude", "--remote"}, true, &yes, ""},
		{"--exec", []string{"--exec", "code ."}, false, nil, "code ."},

		// The deprecated spellings.
		{"--rc means --claude --remote", []string{"--rc"}, true, &yes, ""},
		{"--no-rc beats --rc, as it always did", []string{"--rc", "--no-rc"}, false, nil, ""},
		{"--claude=false beats --rc", []string{"--rc", "--claude=false"}, false, nil, ""},
		{"--attach opens nothing now", []string{"--attach", "--mux", "tmux", "--session"}, false, nil, ""},
		{"--no-attach is accepted", []string{"--no-attach"}, false, nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveFlags(t, tt.argv...)
			if err != nil {
				t.Fatalf("resolve(%v): %v", tt.argv, err)
			}
			if got.Claude != tt.claude || got.Exec != tt.exec {
				t.Errorf("resolve(%v) = %+v, want Claude:%v Exec:%q", tt.argv, got, tt.claude, tt.exec)
			}
			if (got.Remote == nil) != (tt.remote == nil) || (got.Remote != nil && *got.Remote != *tt.remote) {
				t.Errorf("resolve(%v).Remote = %v, want %v", tt.argv, got.Remote, tt.remote)
			}
		})
	}
}

func TestLaunchFlagsRefuseClaudeAndExecTogether(t *testing.T) {
	if _, err := resolveFlags(t, "--claude", "--exec", "code ."); err == nil {
		t.Fatal("--claude and --exec together were accepted")
	}
}

// routing's spawn.rc used to decide WHETHER a bare spawn launched claude. Now
// it only decides whether a requested claude gets Remote Control.
func TestClaudeOptionsTakeRemoteControlFromRouting(t *testing.T) {
	on := &routing.Routing{Spawn: &routing.Spawn{RC: true, SessionNamePrefix: "mini"}}
	no := false
	tests := []struct {
		name  string
		l     launch
		route *routing.Routing
		want  string
	}{
		{"routing on", launch{Claude: true}, on, "CLAUDE_REMOTE_CONTROL_SESSION_NAME_PREFIX=mini claude --remote-control=iss-facet-27"},
		{"routing on, --remote=false", launch{Claude: true, Remote: &no}, on, "CLAUDE_REMOTE_CONTROL_SESSION_NAME_PREFIX=mini claude"},
		{"no routing", launch{Claude: true}, nil, "claude"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := claudex.ShellCommand(claudeOptions(tt.l, "iss-facet-27", tt.route))
			if got != tt.want {
				t.Errorf("claude invocation = %q, want %q", got, tt.want)
			}
		})
	}
}

// A bare spawn runs nothing, whatever routing says.
func TestRunLaunchRunsNothingByDefault(t *testing.T) {
	on := &routing.Routing{Spawn: &routing.Spawn{RC: true}}
	var out strings.Builder
	if err := runLaunch(&out, launch{}, t.TempDir(), t.TempDir(), "ws", on); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Errorf("a bare spawn launched something:\n%s", out.String())
	}
}

// --exec runs in the home clone, with the workspace named in the environment.
func TestRunLaunchExecRunsInTheHomeClone(t *testing.T) {
	ws := t.TempDir()
	home := filepath.Join(ws, "repo")
	if err := os.Mkdir(home, 0o777); err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(t.TempDir(), "record")
	cmd := `printf '%s\n%s\n%s\n' "$(pwd -P)" "$PWD" "$FACET_WORKSPACE" > ` + record
	if err := runLaunch(io.Discard, launch{Exec: cmd}, ws, home, "ws", nil); err != nil {
		t.Fatalf("runLaunch: %v", err)
	}
	b, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	resolved, _ := filepath.EvalSymlinks(home)
	want := resolved + "\n" + home + "\n" + ws + "\n"
	if string(b) != want {
		t.Errorf("--exec saw\n%s\nwant\n%s", b, want)
	}
}

func TestRunLaunchExecReportsAFailure(t *testing.T) {
	if err := runLaunch(io.Discard, launch{Exec: "exit 3"}, t.TempDir(), t.TempDir(), "ws", nil); err == nil {
		t.Fatal("a failing --exec reported success")
	}
}
