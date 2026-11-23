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
	for _, want := range []string{"com.example.env", "com.example.sibling", "com.example.unloaded"} {
		if want == "com.example.env" {
			if !strings.Contains(joined, want+" runs from it (EnvironmentVariables ") {
				t.Errorf("missing path-valued plist string %s:\n%s", want, joined)
			}
			continue
		}
		if strings.Contains(joined, want) {
			t.Errorf("%s must not hold the workspace:\n%s", want, joined)
		}
	}
	if len(got) != 4 {
		t.Errorf("got %d holders, want 4:\n%s", len(got), joined)
	}
}

func TestUnderHoldsJobsThatNameTheDirectoryInAnyStringValue(t *testing.T) {
	agents, root := t.TempDir(), t.TempDir()
	ws := filepath.Join(root, "svc")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ label, body, key string }{
		{"shell", "<key>ProgramArguments</key><array><string>/bin/sh</string><string>-c</string><string>cd " + ws + " &amp;&amp; ./run</string></array>", "ProgramArguments"},
		{"config", "<key>ProgramArguments</key><array><string>/bin/app</string><string>--config=" + filepath.Join(ws, "app.yaml") + "</string></array>", "ProgramArguments"},
		{"stdout", "<key>StandardOutPath</key><string>" + filepath.Join(ws, "out.log") + "</string>", "StandardOutPath"},
		{"stderr", "<key>StandardErrorPath</key><string>" + filepath.Join(ws, "err.log") + "</string>", "StandardErrorPath"},
		{"watch", "<key>WatchPaths</key><array><string>" + filepath.Join(ws, "watch") + "</string></array>", "WatchPaths"},
		{"queue", "<key>QueueDirectories</key><array><string>" + filepath.Join(ws, "queue") + "</string></array>", "QueueDirectories"},
	} {
		plist(t, agents, "com.example."+tc.label, tc.body)
	}
	got, err := (&Jobs{Dirs: []string{agents}, Run: fakeTools(
		"com.example.shell", "com.example.config", "com.example.stdout", "com.example.stderr", "com.example.watch", "com.example.queue",
	)}).Under(ws)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(got, "\n")
	for _, tc := range []struct{ label, key string }{
		{"shell", "ProgramArguments"}, {"config", "ProgramArguments"}, {"stdout", "StandardOutPath"},
		{"stderr", "StandardErrorPath"}, {"watch", "WatchPaths"}, {"queue", "QueueDirectories"},
	} {
		if !strings.Contains(joined, "com.example."+tc.label+" runs from it ("+tc.key+" ") {
			t.Errorf("missing %s from %s in:\n%s", tc.key, tc.label, joined)
		}
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

// A program inside the directory that is a symlink to an interpreter
// elsewhere still runs from the directory. Resolving that last element
// would place it outside, and reap would delete what the job runs.
func TestGrillUnderHoldsAJobRunThroughASymlinkInsideTheDirectory(t *testing.T) {
	agents, root := t.TempDir(), t.TempDir()
	ws := filepath.Join(root, "svc")
	bin := filepath.Join(ws, ".venv", "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(root, "python-real")
	if err := os.WriteFile(outside, []byte("interpreter\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	python := filepath.Join(bin, "python")
	if err := os.Symlink(outside, python); err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(python)
	if err != nil {
		t.Fatal(err)
	}
	if resolved == python || strings.HasPrefix(resolved, ws+string(filepath.Separator)) {
		t.Fatal("python did not point outside the directory, so this would pass without the fix")
	}
	via, err := filepath.EvalSymlinks(ws)
	if err != nil {
		t.Fatal(err)
	}
	plist(t, agents, "com.example.venv", "\t<key>Program</key>\n\t<string>"+python+"</string>")
	got, err := (&Jobs{Dirs: []string{agents}, Run: fakeTools("com.example.venv")}).Under(via)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !strings.Contains(got[0], "com.example.venv runs from it (Program "+python) {
		t.Fatalf("Under = %v; want the job whose program is the symlink %s", got, python)
	}
}

// foldsCase follows the volume: a case-insensitive disk folds, a
// case-sensitive one does not. Getting the capability bits wrong would hold
// one directory for a job that runs from another, or miss the job entirely.
func TestFoldsCaseFollowsTheVolume(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "Aa"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := os.Stat(filepath.Join(dir, "aa"))
	want := err == nil
	if got := foldsCase(dir); got != want {
		t.Fatalf("foldsCase = %v, volume is case-insensitive = %v", got, want)
	}
}

// On a case-insensitive disk the directory and the path a plist names can
// differ only in case, and a string compare misses the job.
func TestGrillUnderMatchesACaseVariantOfTheDirectory(t *testing.T) {
	agents, root := t.TempDir(), t.TempDir()
	ws := filepath.Join(root, "svc")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	variant := filepath.Join(root, "SVC")
	if _, err := os.Stat(variant); err != nil {
		t.Skip("needs a case-insensitive disk (the macOS job runs it)")
	}
	plist(t, agents, "com.example.case", "\t<key>WorkingDirectory</key>\n\t<string>"+ws+"</string>")
	plist(t, agents, "com.example.sibling", "\t<key>WorkingDirectory</key>\n\t<string>"+filepath.Join(root, "SVC-old")+"</string>")
	j := &Jobs{Dirs: []string{agents}, Run: fakeTools("com.example.case", "com.example.sibling")}
	got, err := j.Under(variant)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !strings.Contains(got[0], "com.example.case") {
		t.Fatalf("Under(%s) = %v; want the job whose plist names %s", variant, got, ws)
	}
	// The other way round: the plist names the variant, the directory is asked as stored.
	agents2 := t.TempDir()
	plist(t, agents2, "com.example.case", "\t<key>WorkingDirectory</key>\n\t<string>"+variant+"</string>")
	got, err = (&Jobs{Dirs: []string{agents2}, Run: fakeTools("com.example.case")}).Under(ws)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !strings.Contains(got[0], "com.example.case") {
		t.Fatalf("Under(%s) = %v; want the job whose plist names %s", ws, got, variant)
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

func TestUnderPrefersThePlistLaunchctlReportsOverAStaleCopy(t *testing.T) {
	agents, source, root := t.TempDir(), t.TempDir(), t.TempDir()
	ws := filepath.Join(root, "svc")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	label := "com.example.same"
	plist(t, agents, label, "<key>Program</key><string>/usr/bin/false</string>")
	loadedFrom := plist(t, source, label, "<key>Program</key><string>"+filepath.Join(ws, "run")+"</string>")
	run := func(name string, args ...string) ([]byte, error) {
		switch {
		case name == "launchctl" && args[0] == "list":
			return []byte("PID\tStatus\tLabel\n-\t0\t" + label + "\n"), nil
		case name == "launchctl" && args[0] == "print":
			return []byte("gui/501/" + label + " = {\n\tpath = " + loadedFrom + "\n}\n"), nil
		case name == "plutil":
			return os.ReadFile(args[len(args)-1])
		}
		return nil, errors.New("unexpected tool " + name)
	}
	got, err := (&Jobs{Dirs: []string{agents}, Run: run}).Under(ws)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !strings.Contains(got[0], loadedFrom) || strings.Contains(strings.Join(got, "\n"), agents) {
		t.Fatalf("Under = %v; want only the job from %s", got, loadedFrom)
	}
}
