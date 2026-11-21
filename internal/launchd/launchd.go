// Package launchd finds the loaded launchd jobs that run from inside a
// directory, so reap can hold a workspace a background job still needs.
//
// A running job is already caught by lsof (package mux): its working directory
// or executable is open. A loaded job that is not running at that moment, such
// as a scheduled job between runs, holds nothing open, and deleting the
// directory it runs from breaks it at its next run (facet#234).
//
// Only the jobs `launchctl list` shows are considered: the user's own domain.
// A plist that is not loaded starts nothing, so it holds nothing. Like the
// probes in package mux, a missing tool is not a failure: off macOS there is
// nothing to report.
package launchd

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// Runner runs a command and returns its standard output. It is the seam the
// tests fake; Default runs the real launchctl and plutil.
type Runner func(name string, args ...string) ([]byte, error)

// Jobs reads the loaded jobs once, on first use, and answers for any number of
// directories from that one read. A facet command is short-lived, so a job
// loaded after the read is not seen until the next command.
type Jobs struct {
	// Dirs are the folders whose *.plist files define the user's jobs.
	Dirs []string
	// Run runs launchctl and plutil.
	Run Runner

	once sync.Once
	jobs []job
}

// job is one loaded job and the paths its plist runs it from.
type job struct {
	label, plist string
	paths        []keyPath
}

// keyPath is a path a plist names, and the key it is under.
type keyPath struct{ key, path string }

// Default reads the user's LaunchAgents folders with the real tools.
// /Library/LaunchDaemons is left out: those jobs live in the system domain,
// which `launchctl list` does not show to a user.
func Default() *Jobs {
	dirs := []string{"/Library/LaunchAgents"}
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append([]string{filepath.Join(home, "Library", "LaunchAgents")}, dirs...)
	}
	return &Jobs{Dirs: dirs, Run: run}
}

func run(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).Output()
}

// Under describes each loaded job whose program, arguments or working
// directory is dir or inside it, naming the job, the key and the plist.
func (j *Jobs) Under(dir string) ([]string, error) {
	j.once.Do(j.load)
	root := canonical(dir)
	var out []string
	for _, jb := range j.jobs {
		for _, kp := range jb.paths {
			if within(root, canonical(kp.path)) {
				out = append(out, fmt.Sprintf("launchd job %s runs from it (%s %s, loaded from %s)", jb.label, kp.key, kp.path, jb.plist))
				break
			}
		}
	}
	return out, nil
}

func (j *Jobs) load() {
	loaded := j.loadedLabels()
	if len(loaded) == 0 {
		return
	}
	for _, d := range j.Dirs {
		files, _ := filepath.Glob(filepath.Join(d, "*.plist"))
		sort.Strings(files)
		for _, f := range files {
			raw, err := j.Run("plutil", "-convert", "xml1", "-o", "-", f)
			if err != nil {
				continue
			}
			jb, err := parsePlist(raw)
			if err != nil || !loaded[jb.label] {
				continue
			}
			jb.plist = f
			j.jobs = append(j.jobs, jb)
		}
	}
}

// loadedLabels reads `launchctl list`: a header line, then PID, status and
// label separated by tabs.
func (j *Jobs) loadedLabels() map[string]bool {
	raw, err := j.Run("launchctl", "list")
	if err != nil {
		return nil
	}
	labels := map[string]bool{}
	for _, line := range strings.Split(string(raw), "\n") {
		f := strings.Split(line, "\t")
		if len(f) != 3 || f[2] == "Label" {
			continue
		}
		labels[strings.TrimSpace(f[2])] = true
	}
	return labels
}

// parsePlist reads the keys of an XML plist's top-level dict that say where a
// job runs from: Label, Program, ProgramArguments and WorkingDirectory.
// Arguments that are not absolute paths are dropped.
func parsePlist(raw []byte) (job, error) {
	var jb job
	dec := xml.NewDecoder(bytes.NewReader(raw))
	dec.Strict = false
	depth, key, inArray := 0, "", false
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return job{}, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "dict":
				depth++
			case "array":
				inArray = depth == 1
			case "key":
				if depth == 1 && !inArray {
					var s string
					if err := dec.DecodeElement(&s, &t); err != nil {
						return job{}, err
					}
					key = s
				}
			case "string":
				var s string
				if err := dec.DecodeElement(&s, &t); err != nil {
					return job{}, err
				}
				if depth != 1 {
					continue
				}
				switch {
				case key == "Label" && !inArray:
					jb.label = s
				case (key == "Program" || key == "WorkingDirectory") && !inArray,
					key == "ProgramArguments" && inArray:
					if filepath.IsAbs(s) {
						jb.paths = append(jb.paths, keyPath{key, s})
					}
				}
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "dict":
				depth--
			case "array":
				if depth == 1 {
					inArray = false
				}
			}
		}
	}
	if jb.label == "" {
		return job{}, errors.New("plist has no Label")
	}
	return jb, nil
}

// canonical resolves symlinks, so /var and /private/var, or a symlinked
// ~/Workspaces, compare equal. A path that does not exist yet (a program not
// built yet) is resolved through its deepest existing parent.
func canonical(p string) string {
	p = filepath.Clean(p)
	rest := ""
	for {
		if r, err := filepath.EvalSymlinks(p); err == nil {
			return filepath.Join(r, rest)
		}
		parent := filepath.Dir(p)
		if parent == p {
			return filepath.Join(p, rest)
		}
		rest = filepath.Join(filepath.Base(p), rest)
		p = parent
	}
}

// within reports whether p is root or inside it.
func within(root, p string) bool {
	return p == root || strings.HasPrefix(p, root+string(filepath.Separator))
}
