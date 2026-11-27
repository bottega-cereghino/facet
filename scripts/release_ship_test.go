package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// These tests run `scripts/release.sh ship` against a real git repository with
// a bare repository as origin, and fakes for the tools that would touch the
// network: gh, npm, release-please, go and the swap command. Nothing real is
// tagged, installed or swapped; the install root is a temporary directory.

func writeExe(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/usr/bin/env bash\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func runIn(t *testing.T, dir string, env []string, name string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

type shipFixture struct {
	work, root, bin string
	env             []string
}

func newShipFixture(t *testing.T, tags []string, manifestVersion string) shipFixture {
	t.Helper()
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "bin")
	origin := filepath.Join(tmp, "origin.git")
	work := filepath.Join(tmp, "work")
	root := filepath.Join(tmp, "install")
	for _, d := range []string{bin, work} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	base := []string{"PATH=" + bin + ":" + os.Getenv("PATH"), "HOME=" + tmp, "TMPDIR=" + tmp,
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com"}

	script, err := os.ReadFile("release.sh")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(work, "scripts", "release"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeExe(t, filepath.Join(work, "scripts", "release.sh"), strings.TrimPrefix(string(script), "#!/usr/bin/env bash\n"))
	for _, f := range []string{"package.json", "package-lock.json"} {
		if err := os.WriteFile(filepath.Join(work, "scripts", "release", f), []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	manifest := `{".": "` + manifestVersion + `"}`
	changelog := "# Changelog\n\n## [" + manifestVersion + "](https://example.com) (2026-11-27)\n\n* a change\n\n## [0.0.1](https://example.com)\n\n* old\n"
	for f, c := range map[string]string{".release-please-manifest.json": manifest, "CHANGELOG.md": changelog} {
		if err := os.WriteFile(filepath.Join(work, f), []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	steps := [][]string{
		{"init", "-q", "-b", "main", work}, {"-C", work, "add", "."}, {"-C", work, "commit", "-q", "-m", "init"},
		{"init", "-q", "--bare", "-b", "main", origin},
		{"-C", work, "remote", "add", "origin", origin},
	}
	for _, tag := range tags {
		steps = append(steps, []string{"-C", work, "tag", tag})
	}
	steps = append(steps, []string{"-C", work, "push", "-q", "origin", "main", "--tags"})
	for _, s := range steps {
		if out, err := runIn(t, tmp, base, "git", s...); err != nil {
			t.Fatalf("git %v: %v\n%s", s, err, out)
		}
	}

	writeExe(t, filepath.Join(bin, "gh"), `echo "gh $*" >>"$FAKE_LOG"
[ "${1:-}" = auth ] && [ "${2:-}" = token ] && echo faketoken
exit 0
`)
	writeExe(t, filepath.Join(bin, "npm"), `prefix=
while [ $# -gt 0 ]; do [ "$1" = --prefix ] && prefix=$2; shift; done
mkdir -p "$prefix/node_modules/.bin"
printf '#!/usr/bin/env bash\necho release-please "$@" >>"$FAKE_LOG"\n' >"$prefix/node_modules/.bin/release-please"
chmod +x "$prefix/node_modules/.bin/release-please"
`)
	// The fake go installs a fake facet that reports the version it was
	// installed at, like the real binary does.
	writeExe(t, filepath.Join(bin, "go"), `echo "go $* GOBIN=$GOBIN" >>"$FAKE_LOG"
[ -z "${FAKE_GO_FAIL:-}" ] || exit 1
ver=${2##*@}
mkdir -p "$GOBIN"
printf '#!/usr/bin/env bash\necho "facet %s"\n' "${FAKE_BINARY_VERSION:-$ver}" >"$GOBIN/facet"
chmod +x "$GOBIN/facet"
`)
	writeExe(t, filepath.Join(bin, "swap"), `echo "swap $*" >>"$FAKE_LOG"
exit "${FAKE_SWAP_EXIT:-0}"
`)
	env := append(base, "FAKE_LOG="+filepath.Join(tmp, "log"), "FACET_INSTALL_ROOT="+root)
	return shipFixture{work: work, root: root, bin: bin, env: env}
}

func (f shipFixture) log(t *testing.T) string {
	t.Helper()
	b, _ := os.ReadFile(f.envValue("FAKE_LOG"))
	return string(b)
}

func (f shipFixture) envValue(k string) string {
	for _, e := range f.env {
		if v, ok := strings.CutPrefix(e, k+"="); ok {
			return v
		}
	}
	return ""
}

func (f shipFixture) ship(t *testing.T, extra ...string) (string, error) {
	t.Helper()
	return runIn(t, f.work, append(append([]string{}, f.env...), extra...), "bash", "scripts/release.sh", "ship")
}

func TestShipInstallsSwapsAndPrintsTheWayBack(t *testing.T) {
	f := newShipFixture(t, []string{"v0.19.3", "v0.19.4"}, "0.19.4")
	out, err := f.ship(t, "FACET_SWAP_CMD="+filepath.Join(f.bin, "swap"))
	if err != nil {
		t.Fatalf("ship failed: %v\n%s", err, out)
	}
	for _, want := range []string{"swapped to v0.19.4", "facet v0.19.4", "way back: GOBIN=" + f.root + "/v0.19.3 go install", "@v0.19.3"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	log := f.log(t)
	for _, want := range []string{"release-please github-release", "go install github.com/bottega-cereghino/facet/cmd/facet@v0.19.4 GOBIN=" + f.root + "/v0.19.4", "swap v0.19.4 v0.19.3"} {
		if !strings.Contains(log, want) {
			t.Errorf("log lacks %q:\n%s", want, log)
		}
	}
	for _, p := range []string{"v0.19.4/facet", "current/facet"} {
		if _, err := os.Stat(filepath.Join(f.root, p)); err != nil {
			t.Errorf("%s was not installed: %v", p, err)
		}
	}
}

func TestShipWithoutSwapCommandSaysTheServiceWasNotSwapped(t *testing.T) {
	f := newShipFixture(t, []string{"v0.19.3", "v0.19.4"}, "0.19.4")
	out, err := f.ship(t)
	if err != nil {
		t.Fatalf("ship failed: %v\n%s", err, out)
	}
	for _, want := range []string{"the service was not swapped", "v0.19.4 v0.19.3", "way back:"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(f.log(t), "swap ") {
		t.Errorf("a swap ran with no command set:\n%s", f.log(t))
	}
}

func TestShipStopsWhenTheSwapFails(t *testing.T) {
	f := newShipFixture(t, []string{"v0.19.3", "v0.19.4"}, "0.19.4")
	out, err := f.ship(t, "FACET_SWAP_CMD="+filepath.Join(f.bin, "swap"), "FAKE_SWAP_EXIT=3")
	if err == nil {
		t.Fatalf("ship succeeded with a failing swap:\n%s", out)
	}
	if !strings.Contains(out, "the swap command failed for v0.19.4. way back:") {
		t.Errorf("no way back printed:\n%s", out)
	}
}

func TestShipStopsWhenTheInstallFails(t *testing.T) {
	f := newShipFixture(t, []string{"v0.19.3", "v0.19.4"}, "0.19.4")
	out, err := f.ship(t, "FACET_SWAP_CMD="+filepath.Join(f.bin, "swap"), "FAKE_GO_FAIL=1")
	if err == nil {
		t.Fatalf("ship succeeded with a failing install:\n%s", out)
	}
	if strings.Contains(f.log(t), "swap ") {
		t.Errorf("the swap ran after a failed install:\n%s", f.log(t))
	}
	if _, err := os.Stat(filepath.Join(f.root, "current", "facet")); err == nil {
		t.Error("current was written after a failed install")
	}
}

func TestShipFailsWhenTheRunningVersionIsNotTheNewOne(t *testing.T) {
	f := newShipFixture(t, []string{"v0.19.3", "v0.19.4"}, "0.19.4")
	out, err := f.ship(t, "FACET_SWAP_CMD="+filepath.Join(f.bin, "swap"), "FAKE_BINARY_VERSION=v0.19.3")
	if err == nil {
		t.Fatalf("ship succeeded with the wrong version running:\n%s", out)
	}
	if !strings.Contains(out, `says "facet v0.19.3", not "facet v0.19.4"`) {
		t.Errorf("the mismatch is not named:\n%s", out)
	}
}

func TestShipRefusesWithoutAPreviousTag(t *testing.T) {
	f := newShipFixture(t, []string{"v0.19.4"}, "0.19.4")
	out, err := f.ship(t, "FACET_SWAP_CMD="+filepath.Join(f.bin, "swap"))
	if err == nil || !strings.Contains(out, "there is no way back") {
		t.Fatalf("want a refusal naming the missing way back, got %v:\n%s", err, out)
	}
	if strings.Contains(f.log(t), "go install") {
		t.Errorf("an install ran with no way back:\n%s", f.log(t))
	}
}
