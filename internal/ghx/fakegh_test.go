package ghx

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// fakeGh puts a gh on PATH that records its arguments and answers with the
// given stdout, stderr and exit code. It is the seam every CLI method already
// has: they all run "gh" by name. PATH holds nothing else, so no test using
// it can reach the real gh or GitHub. The returned func reads the arguments
// of the last call.
func fakeGh(t *testing.T, stdout, stderr string, exit int) func() []string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake gh is a shell script; Windows is not supported")
	}
	dir := t.TempDir()
	for name, contents := range map[string]string{"stdout": stdout, "stderr": stderr} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	q := "'" + dir + "'"
	script := "#!/bin/sh\n" +
		"for a in \"$@\"; do printf '%s\\n' \"$a\"; done > " + q + "/args\n" +
		"/bin/cat " + q + "/stdout\n" +
		"/bin/cat " + q + "/stderr >&2\n" +
		"exit " + strconv.Itoa(exit) + "\n"
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	return func() []string {
		t.Helper()
		raw, err := os.ReadFile(filepath.Join(dir, "args"))
		if err != nil {
			t.Fatalf("the fake gh was never run: %v", err)
		}
		return strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	}
}
