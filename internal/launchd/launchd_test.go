package launchd

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// plist writes an XML job definition into dir.
func plist(t *testing.T, dir, label, body string) string {
	t.Helper()
	p := filepath.Join(dir, label+".plist")
	doc := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>` + label + `</string>
` + body + `
</dict>
</plist>
`
	if err := os.WriteFile(p, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// fakeTools answers `launchctl list` with the loaded labels and plutil by
// returning the file as it is, since the fixtures are already XML.
func fakeTools(loaded ...string) Runner {
	return func(name string, args ...string) ([]byte, error) {
		switch name {
		case "launchctl":
			var b strings.Builder
			b.WriteString("PID\tStatus\tLabel\n")
			for i, l := range loaded {
				fmt.Fprintf(&b, "-\t0\t%s\n", l)
				if i == 0 {
					fmt.Fprintf(&b, "123\t0\tcom.example.running-elsewhere\n")
				}
			}
			return []byte(b.String()), nil
		case "plutil":
			return os.ReadFile(args[len(args)-1])
		}
		return nil, errors.New("unexpected tool " + name)
	}
}

func TestUnderHoldsALoadedJobThatRunsFromTheDirectory(t *testing.T) {
	agents, root := t.TempDir(), t.TempDir()
	ws := filepath.Join(root, "svc")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	plist(t, agents, "com.example.wd", "\t<key>WorkingDirectory</key>\n\t<string>"+ws+"</string>")
	plist(t, agents, "com.example.prog", "\t<key>Program</key>\n\t<string>"+filepath.Join(ws, "bin", "svc")+"</string>")
	plist(t, agents, "com.example.args", "\t<key>ProgramArguments</key>\n\t<array>\n\t\t<string>/bin/sh</string>\n\t\t<string>"+filepath.Join(ws, "run.sh")+"</string>\n\t</array>")
	plist(t, agents, "com.example.env", "\t<key>EnvironmentVariables</key>\n\t<dict>\n\t\t<key>DATA</key>\n\t\t<string>"+ws+"</string>\n\t</dict>\n\t<key>Program</key>\n\t<string>/usr/bin/true</string>")
	plist(t, agents, "com.example.sibling", "\t<key>WorkingDirectory</key>\n\t<string>"+ws+"-old</string>")
	plist(t, agents, "com.example.unloaded", "\t<key>WorkingDirectory</key>\n\t<string>"+ws+"</string>")

	j := &Jobs{Dirs: []string{agents, filepath.Join(agents, "missing")}, Run: fakeTools(
		"com.example.wd", "com.example.prog", "com.example.args", "com.example.env", "com.example.sibling")}
	got, err := j.Under(ws)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(got, "\n")
	for _, want := range []string{"com.example.wd runs from it (WorkingDirectory ", "com.example.prog runs from it (Program ", "com.example.args runs from it (ProgramArguments "} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in:\n%s", want, joined)
		}
	}
	for _, not := range []string{"com.example.env", "com.example.sibling", "com.example.unloaded"} {
		if strings.Contains(joined, not) {
			t.Errorf("%s must not hold the workspace:\n%s", not, joined)
		}
	}
	if len(got) != 3 {
		t.Errorf("got %d holders, want 3:\n%s", len(got), joined)
	}
}

// Off macOS there is no launchctl: nothing to report, not a failure.
func TestUnderWithoutTheToolsReportsNothing(t *testing.T) {
	agents := t.TempDir()
	plist(t, agents, "com.example.wd", "\t<key>WorkingDirectory</key>\n\t<string>/x</string>")
	missing := func(name string, _ ...string) ([]byte, error) {
		return nil, &exec.Error{Name: name, Err: exec.ErrNotFound}
	}
	got, err := (&Jobs{Dirs: []string{agents}, Run: missing}).Under("/x")
	if err != nil || len(got) != 0 {
		t.Fatalf("Under = %v, %v; want nothing", got, err)
	}
}

// A launchctl that is there but fails cannot say which jobs are loaded: that is
// an error, never an empty list, or reap would delete what a loaded job runs
// from (facet#273).
func TestUnderReportsAListThatFails(t *testing.T) {
	agents := t.TempDir()
	plist(t, agents, "com.example.wd", "\t<key>WorkingDirectory</key>\n\t<string>/x</string>")
	failing := func(name string, args ...string) ([]byte, error) {
		if name == "launchctl" {
			return nil, errors.New("exit status 5")
		}
		return os.ReadFile(args[len(args)-1])
	}
	j := &Jobs{Dirs: []string{agents}, Run: failing}
	for range 2 {
		got, err := j.Under("/x")
		if err == nil || !strings.Contains(err.Error(), "launchctl list") || !strings.Contains(err.Error(), "exit status 5") {
			t.Fatalf("Under = %v, %v; want an error naming launchctl list and why", got, err)
		}
	}
}

func TestParsePlistNeedsALabel(t *testing.T) {
	if _, err := parsePlist([]byte(`<plist><dict><key>Program</key><string>/x</string></dict></plist>`)); err == nil {
		t.Fatal("a plist without a Label parsed")
	}
}

// A job loaded from a plist outside the LaunchAgents folders is still found:
// launchctl print names the file it came from. Apple's and apps' own jobs are
// not asked about.
func TestUnderFindsAJobLoadedFromAPlistElsewhere(t *testing.T) {
	agents, elsewhereDir, root := t.TempDir(), t.TempDir(), t.TempDir()
	ws := filepath.Join(root, "svc")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	p := plist(t, elsewhereDir, "com.example.away", "\t<key>Program</key>\n\t<string>"+filepath.Join(ws, "run.sh")+"</string>")
	var asked []string
	run := func(name string, args ...string) ([]byte, error) {
		switch {
		case name == "launchctl" && args[0] == "list":
			return []byte("PID\tStatus\tLabel\n-\t0\tcom.example.away\n-\t0\tcom.apple.Finder\n-\t0\tapplication.com.example.App.123\n"), nil
		case name == "launchctl" && args[0] == "print":
			asked = append(asked, args[1])
			if strings.HasSuffix(args[1], "/com.example.away") {
				return []byte("gui/501/com.example.away = {\n\tactive count = 0\n\tpath = " + p + "\n\tstate = waiting\n}\n"), nil
			}
			return nil, errors.New("not found")
		case name == "plutil":
			return os.ReadFile(args[len(args)-1])
		}
		return nil, errors.New("unexpected tool " + name)
	}
	got, err := (&Jobs{Dirs: []string{agents}, Run: run}).Under(ws)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !strings.Contains(got[0], "com.example.away") || !strings.Contains(got[0], p) {
		t.Fatalf("Under = %v; want the job loaded from %s", got, p)
	}
	if len(asked) != 1 {
		t.Errorf("asked launchctl print about %v; want only com.example.away", asked)
	}
}
