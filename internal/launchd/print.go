package launchd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// printJob runs `launchctl print` for the loaded job label, in the gui/<uid>
// domain and then in user/<uid>, and returns the output of the first that
// answers. The label is passed as `launchctl list` printed it. It fails only
// when both domains fail.
func (j *Jobs) printJob(label string) ([]byte, error) {
	uid := os.Getuid()
	var errs []string
	for _, domain := range []string{"gui", "user"} {
		target := fmt.Sprintf("%s/%d/%s", domain, uid, label)
		raw, err := j.Run("launchctl", "print", target)
		if err == nil {
			return raw, nil
		}
		errs = append(errs, fmt.Sprintf("%s: %v", target, err))
	}
	return nil, fmt.Errorf("launchctl print failed: %s", strings.Join(errs, "; "))
}

// plistPath is the file `launchctl print` says the job was loaded from: its
// "path = ..." line. "" when it names none (a job submitted without a plist).
func plistPath(raw []byte) string {
	for _, line := range strings.Split(string(raw), "\n") {
		if p, ok := strings.CutPrefix(line, "\tpath = "); ok && strings.HasSuffix(p, ".plist") {
			return strings.TrimSpace(p)
		}
	}
	return ""
}

// printKeys maps the top-level `launchctl print` fields that name one path to
// the plist key they come from, so a job read from print is described as one
// read from its plist is.
var printKeys = map[string]string{
	"program":           "Program",
	"working directory": "WorkingDirectory",
	"root directory":    "RootDirectory",
	"stdin path":        "StandardInPath",
	"stdout path":       "StandardOutPath",
	"stderr path":       "StandardErrorPath",
}

// printBlocks maps the top-level `launchctl print` blocks whose lines name
// paths to the plist key they come from.
var printBlocks = map[string]string{
	"arguments":      "ProgramArguments",
	"environment":    "EnvironmentVariables",
	"event triggers": "WatchPaths",
}

// parsePrint reads a loaded job from the output of `launchctl print`, for a
// job whose plist file cannot be read: its program, arguments, working
// directory, standard paths, environment and watched paths. ok is false when
// the output names neither a program nor arguments, since then it does not
// describe what the job runs.
func parsePrint(label, from string, raw []byte) (job, bool) {
	jb := job{label: label, plist: from}
	runs := false
	block := ""
	for _, line := range strings.Split(string(raw), "\n") {
		if block != "" {
			if line == "\t}" {
				block = ""
				continue
			}
			for _, v := range blockValues(line) {
				jb.paths = appendPaths(jb.paths, printBlocks[block], v)
				if block == "arguments" {
					runs = true
				}
			}
			continue
		}
		body, ok := strings.CutPrefix(line, "\t")
		if !ok || strings.HasPrefix(body, "\t") {
			continue
		}
		name, value, ok := strings.Cut(body, " = ")
		if !ok {
			continue
		}
		if _, isBlock := printBlocks[name]; isBlock && value == "{" {
			block = name
			continue
		}
		if key, known := printKeys[name]; known {
			jb.paths = appendPaths(jb.paths, key, value)
			if name == "program" {
				runs = true
			}
		}
	}
	return jb, runs
}

// blockValues is the value a line inside a print block names: an argument as
// it is, `KEY => value` in the environment, and `N = "path"` in a watched
// path list.
func blockValues(line string) []string {
	s := strings.TrimSpace(line)
	if s == "" || strings.HasSuffix(s, "{") || strings.HasSuffix(s, "[") || s == "}" || s == "]" {
		return nil
	}
	if _, v, ok := strings.Cut(s, " => "); ok {
		return []string{v}
	}
	if _, v, ok := strings.Cut(s, " = "); ok {
		if q := strings.Trim(v, `"`); q != v {
			return []string{q}
		}
		return nil
	}
	return []string{s}
}

// appendPaths adds the paths value names: the whole value when it is an
// absolute path (print does not quote, so a path may hold spaces), and every
// absolute path inside it, as a plist string is read.
func appendPaths(paths []keyPath, key, value string) []keyPath {
	value = strings.TrimSpace(value)
	if filepath.IsAbs(value) {
		paths = append(paths, keyPath{key, value})
	}
	for _, c := range pathCandidates(value) {
		if c != value {
			paths = append(paths, keyPath{key, c})
		}
	}
	return paths
}
