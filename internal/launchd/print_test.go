package launchd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// printOutput is `launchctl print` for a job loaded from plist p, in the shape
// the real tool prints, with the working directory wd.
func printOutput(label, p, wd string) string {
	return fmt.Sprintf(`gui/501/%[1]s = {
	active count = 1
	path = %[2]s
	type = LaunchAgent
	state = running

	program = /usr/bin/env
	arguments = {
		/usr/bin/env
		node
		server.js
	}

	working directory = %[3]s

	inherited environment = {
		SSH_AUTH_SOCK => /private/tmp/listeners
	}

	domain = gui/501 [100014]
	runs = 3
}
`, label, p, wd)
}

// gonePlist is a runner for one loaded label whose plist file p is gone: plutil
// fails on it, and launchctl print answers with answers[domain] in the domains it is
// given. Every print target asked is recorded in asked.
func gonePlist(label, p string, answers map[string]string, asked *[]string) Runner {
	return func(name string, args ...string) ([]byte, error) {
		switch {
		case name == "launchctl" && args[0] == "list":
			return []byte("PID\tStatus\tLabel\n123\t0\t" + label + "\n"), nil
		case name == "launchctl" && args[0] == "print":
			*asked = append(*asked, args[1])
			domain, _, _ := strings.Cut(args[1], "/")
			if out, ok := answers[domain]; ok {
				return []byte(out), nil
			}
			return nil, errors.New("exit status 113")
		case name == "plutil" && args[len(args)-1] == p:
			if _, err := os.Stat(p); err != nil {
				return nil, fmt.Errorf("plutil: %w", err)
			}
			return os.ReadFile(p)
		}
		return nil, errors.New("unexpected tool " + name)
	}
}

func TestUnderReadsALoadedJobFromLaunchctlPrintWhenItsPlistIsGone(t *testing.T) {
	agents, root := t.TempDir(), t.TempDir()
	ws := filepath.Join(root, "svc")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	label := "com.example.app.server"
	p := filepath.Join(agents, label+".plist") // never written: the file is gone
	var asked []string
	run := gonePlist(label, p, map[string]string{"gui": printOutput(label, p, filepath.Join(ws, "app"))}, &asked)
	got, err := (&Jobs{Dirs: []string{agents}, Run: run}).Under(ws)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !strings.Contains(got[0], label) || !strings.Contains(got[0], "WorkingDirectory") || !strings.Contains(got[0], "launchctl print") {
		t.Fatalf("Under = %v; want %s held through launchctl print", got, label)
	}
	want := fmt.Sprintf("gui/%d/%s", os.Getuid(), label)
	if len(asked) != 1 || asked[0] != want {
		t.Errorf("asked launchctl print about %v; want only the full label %s", asked, want)
	}
}

func TestUnderDoesNotHoldWhenTheGoneJobRunsElsewhere(t *testing.T) {
	agents, root := t.TempDir(), t.TempDir()
	ws := filepath.Join(root, "svc")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	label := "com.example.app.server"
	p := filepath.Join(agents, label+".plist")
	var asked []string
	run := gonePlist(label, p, map[string]string{"gui": printOutput(label, p, filepath.Join(root, "other"))}, &asked)
	got, err := (&Jobs{Dirs: []string{agents}, Run: run}).Under(ws)
	if err != nil || len(got) != 0 {
		t.Fatalf("Under = %v, %v; want no holders and no error", got, err)
	}
}

func TestUnderTriesTheUserDomainAfterGui(t *testing.T) {
	agents, root := t.TempDir(), t.TempDir()
	ws := filepath.Join(root, "svc")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	label := "com.example.app.server"
	p := filepath.Join(agents, label+".plist")
	var asked []string
	run := gonePlist(label, p, map[string]string{"user": printOutput(label, p, ws)}, &asked)
	got, err := (&Jobs{Dirs: []string{agents}, Run: run}).Under(ws)
	if err != nil || len(got) != 1 {
		t.Fatalf("Under = %v, %v; want the job read from the user domain", got, err)
	}
	uid := os.Getuid()
	want := []string{fmt.Sprintf("gui/%d/%s", uid, label), fmt.Sprintf("user/%d/%s", uid, label)}
	if strings.Join(asked, " ") != strings.Join(want, " ") {
		t.Errorf("asked launchctl print about %v; want %v", asked, want)
	}
}

func TestUnderHoldsWhenBothThePlistAndLaunchctlPrintFail(t *testing.T) {
	agents := t.TempDir()
	label := "com.example.app.server"
	p := filepath.Join(agents, label+".plist")
	var asked []string
	run := gonePlist(label, p, nil, &asked)
	got, err := (&Jobs{Dirs: []string{agents}, Run: run}).Under("/workspace")
	if err == nil || len(got) != 0 || !strings.Contains(err.Error(), label) || !strings.Contains(err.Error(), "unload it or fix its plist") {
		t.Fatalf("Under = %v, %v; want a labeled error with a fix", got, err)
	}
	if len(asked) != 2 {
		t.Errorf("asked launchctl print about %v; want the gui and then the user domain", asked)
	}
}

func TestParsePrintReadsEveryPathField(t *testing.T) {
	raw := `gui/501/com.example.watch = {
	path = /gone/com.example.watch.plist
	program = /Users/me/Application Support/tool/run
	arguments = {
		/Users/me/Application Support/tool/run
		--config=/etc/tool.conf
	}

	working directory = /srv/wd
	stdout path = /var/log/out.log
	stderr path = /var/log/err.log

	inherited environment = {
		SSH_AUTH_SOCK => /inherited/sock
	}

	environment = {
		DATA => /srv/data
	}

	event triggers = {
		com.apple.launchd.WatchPaths => {
			keepalive = 0
			stream = com.apple.fsevents.matching
			descriptor = {
				"WatchPaths" => [
					0 = "/srv/watched dir"
				]
			}
		}
	}
}
`
	jb, ok := parsePrint("com.example.watch", "from", []byte(raw))
	if !ok {
		t.Fatal("parsePrint found no program")
	}
	got := map[string]bool{}
	for _, kp := range jb.paths {
		got[kp.key+" "+kp.path] = true
	}
	for _, want := range []string{
		"Program /Users/me/Application Support/tool/run",
		"ProgramArguments /Users/me/Application Support/tool/run",
		"ProgramArguments /etc/tool.conf",
		"WorkingDirectory /srv/wd",
		"StandardOutPath /var/log/out.log",
		"StandardErrorPath /var/log/err.log",
		"EnvironmentVariables /srv/data",
		"WatchPaths /srv/watched dir",
	} {
		if !got[want] {
			t.Errorf("paths lack %q; got %v", want, jb.paths)
		}
	}
	for k := range got {
		if strings.Contains(k, "/inherited/sock") || strings.Contains(k, "/gone/") {
			t.Errorf("paths hold %q, which the job does not name", k)
		}
	}
}
