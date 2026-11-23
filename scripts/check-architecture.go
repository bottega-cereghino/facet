// check-architecture verifies Go's import graph, the routing core boundary,
// and the source-file count ratchets recorded at the repository root.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

const (
	violationsFile = "known-violations.json"
	layoutFile     = "layout-baseline.json"
)

type packageInfo struct {
	ImportPath string
	Imports    []string
}

type boundary struct {
	Core                 string   `json:"core"`
	ForbiddenCoreImports []string `json:"forbiddenCoreImports"`
	KnownViolations      []string `json:"knownViolations"`
}

func main() {
	if err := check(); err != nil {
		fmt.Fprintln(os.Stderr, "architecture:", err)
		os.Exit(1)
	}
}

func check() error {
	pkgs, err := packages()
	if err != nil {
		return err
	}
	if err := checkBoundary(pkgs); err != nil {
		return err
	}
	if err := checkLayout(); err != nil {
		return err
	}
	fmt.Println("architecture: no import cycles; known boundary violations and layout counts match")
	return nil
}

func packages() ([]packageInfo, error) {
	cmd := exec.Command("go", "list", "-json", "./...")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("go list ./...: %w: %s", err, strings.TrimSpace(stderr.String()))
	}

	decoder := json.NewDecoder(&stdout)
	var out []packageInfo
	for {
		var pkg packageInfo
		if err := decoder.Decode(&pkg); err == io.EOF {
			break
		} else if err != nil {
			return nil, fmt.Errorf("decode go list output: %w", err)
		}
		out = append(out, pkg)
	}
	return out, nil
}

func checkBoundary(pkgs []packageInfo) error {
	data, err := os.ReadFile(violationsFile)
	if err != nil {
		return fmt.Errorf("read %s: %w", violationsFile, err)
	}
	var rule boundary
	if err := json.Unmarshal(data, &rule); err != nil {
		return fmt.Errorf("parse %s: %w", violationsFile, err)
	}
	forbidden := make(map[string]bool, len(rule.ForbiddenCoreImports))
	for _, imp := range rule.ForbiddenCoreImports {
		forbidden[imp] = true
	}

	var core *packageInfo
	for i := range pkgs {
		if pkgs[i].ImportPath == rule.Core {
			core = &pkgs[i]
			break
		}
	}
	if core == nil {
		return fmt.Errorf("core package %q was not found", rule.Core)
	}

	actual := make([]string, 0)
	for _, imp := range core.Imports {
		if forbidden[imp] {
			actual = append(actual, rule.Core+" -> "+imp)
		}
	}
	sort.Strings(actual)
	known := append([]string(nil), rule.KnownViolations...)
	sort.Strings(known)
	if hasDuplicates(known) {
		return fmt.Errorf("%s lists a known violation more than once", violationsFile)
	}
	for _, violation := range known {
		parts := strings.SplitN(violation, " -> ", 2)
		if len(parts) != 2 || parts[0] != rule.Core || !forbidden[parts[1]] {
			return fmt.Errorf("invalid known violation %q in %s", violation, violationsFile)
		}
	}
	if !equalStrings(actual, known) {
		return fmt.Errorf("%s known violations do not match current imports; found %v, recorded %v", violationsFile, actual, known)
	}
	return nil
}

func checkLayout() error {
	data, err := os.ReadFile(layoutFile)
	if err != nil {
		return fmt.Errorf("read %s: %w", layoutFile, err)
	}
	var baseline map[string]int
	if err := json.Unmarshal(data, &baseline); err != nil {
		return fmt.Errorf("parse %s: %w", layoutFile, err)
	}
	if len(baseline) == 0 {
		return fmt.Errorf("%s must list at least one directory", layoutFile)
	}
	dirs := make([]string, 0, len(baseline))
	for dir := range baseline {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)
	for _, dir := range dirs {
		want := baseline[dir]
		entries, err := os.ReadDir(dir)
		if err != nil {
			return fmt.Errorf("read layout directory %q: %w", dir, err)
		}
		count := 0
		for _, entry := range entries {
			if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".go") && !strings.HasSuffix(entry.Name(), "_test.go") {
				count++
			}
		}
		if count != want {
			return fmt.Errorf("%s: %s has %d production Go files, baseline is %d; update the baseline when the count decreases", layoutFile, filepath.Clean(dir), count, want)
		}
		if count <= 20 {
			return fmt.Errorf("%s: %s has %d production Go files; only directories over 20 belong in the baseline", layoutFile, filepath.Clean(dir), count)
		}
	}
	return nil
}

func hasDuplicates(values []string) bool {
	for i := 1; i < len(values); i++ {
		if values[i] == values[i-1] {
			return true
		}
	}
	return false
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
