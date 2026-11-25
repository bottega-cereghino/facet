package ghx

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRequiredChecksReadsRulesAndTreatsMissingProtectionAsAbsent(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" >> " + filepath.Join(dir, "args") + "\n" +
		"case \" $* \" in\n" +
		"*rules/branches*)\n" +
		"printf '%s\\n' '[{\"type\":\"required_status_checks\",\"parameters\":{\"required_status_checks\":[{\"context\":\"ci\"}]}}]'\n" +
		"exit 0\n" +
		";;\n" +
		"esac\n" +
		"echo 'gh: Branch not protected (HTTP 404)' >&2\n" +
		"exit 1\n"
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	names, known, err := (CLI{}).RequiredChecks("acme/widgets", "main")
	if err != nil || !known || len(names) != 1 || names[0] != "ci" {
		t.Fatalf("checks = %q known %v err %v", names, known, err)
	}
	args, err := os.ReadFile(filepath.Join(dir, "args"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(args)
	if !strings.Contains(text, "repos/acme/widgets/rules/branches/main") || !strings.Contains(text, "repos/acme/widgets/branches/main/protection/required_status_checks") {
		t.Fatalf("args =\n%s", text)
	}
}

// The answers a failed rules read can give, as gh prints them on stderr, and
// whether each means the repository has no rules there.
var failedRuleReads = []struct {
	name   string
	branch string
	stderr string
	absent bool
}{
	{"plan without rulesets", "main", "gh: Upgrade to GitHub Pro or make this repository public to enable this feature. (HTTP 403)", true},
	{"not found", "main", "gh: Not Found (HTTP 404)", true},
	{"not found without a message", "main", "gh: HTTP 404", true},
	{"not found with a scope hint", "main", "gh: Not Found (HTTP 404)\ngh: This API operation needs the \"repo\" scope.", true},
	{"missing permission", "main", "gh: Resource not accessible by personal access token (HTTP 403)", false},
	{"rate limit", "main", "gh: API rate limit exceeded for user ID 1. (HTTP 403)", false},
	{"plan words with another status", "main", "gh: Upgrade to GitHub Pro or make this repository public to enable this feature. (HTTP 500)", false},
	{"another upgrade message", "main", "gh: Upgrade your token to enable this feature. (HTTP 403)", false},
	{"404 in the branch name", "fix-404", "gh: API rate limit exceeded for user ID 1. (HTTP 403)", false},
	{"404 in a network error", "main", "error connecting to 10.0.4.4:404", false},
}

// failingRules puts a gh on PATH that answers repo view with every merge
// method allowed and fails every api call with stderr.
func failingRules(t *testing.T, stderr string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "stderr"), []byte(stderr+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = repo ]; then\n" +
		"echo '{\"squashMergeAllowed\":true,\"mergeCommitAllowed\":true,\"rebaseMergeAllowed\":true}'\n" +
		"exit 0\n" +
		"fi\n" +
		"/bin/cat '" + dir + "/stderr' >&2\n" +
		"exit 1\n"
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
}

func TestRequiredChecksOnAFailedRead(t *testing.T) {
	for _, tc := range failedRuleReads {
		t.Run(tc.name, func(t *testing.T) {
			failingRules(t, tc.stderr)
			names, known, err := (CLI{}).RequiredChecks("acme/widgets", tc.branch)
			if tc.absent {
				if err != nil || !known || len(names) != 0 {
					t.Fatalf("checks = %q known %v err %v; want none, known", names, known, err)
				}
				return
			}
			if err == nil || known || !strings.Contains(err.Error(), tc.stderr) {
				t.Fatalf("checks = %q known %v err %v; want gh's error", names, known, err)
			}
		})
	}
}

func TestMergeMethodOnAFailedRulesRead(t *testing.T) {
	for _, tc := range failedRuleReads {
		t.Run(tc.name, func(t *testing.T) {
			failingRules(t, tc.stderr)
			got, err := (CLI{}).MergeMethod("acme/widgets", tc.branch)
			if tc.absent {
				if err != nil || got != "squash" {
					t.Fatalf("MergeMethod = %q, %v; want squash from the settings", got, err)
				}
				return
			}
			if err == nil || got != "" || !strings.Contains(err.Error(), tc.stderr) {
				t.Fatalf("MergeMethod = %q, %v; want gh's error", got, err)
			}
		})
	}
}

func TestCombineRequired(t *testing.T) {
	names, known, err := combineRequired([]string{"ci"}, false, nil, nil, true, nil)
	if err != nil || !known || len(names) != 1 || names[0] != "ci" {
		t.Fatalf("rules only = %q %v %v", names, known, err)
	}
	names, known, err = combineRequired(nil, true, nil, []string{"lint"}, false, nil)
	if err != nil || !known || len(names) != 1 || names[0] != "lint" {
		t.Fatalf("protection only = %q %v %v", names, known, err)
	}
	names, known, err = combineRequired(nil, true, nil, nil, true, nil)
	if err != nil || !known || len(names) != 0 {
		t.Fatalf("neither = %q %v %v", names, known, err)
	}
	_, known, err = combineRequired(nil, false, errString("HTTP 403"), []string{"ci"}, false, nil)
	if err == nil || known {
		t.Fatalf("unreadable = known %v err %v", known, err)
	}
}

type errString string

func (e errString) Error() string { return string(e) }
