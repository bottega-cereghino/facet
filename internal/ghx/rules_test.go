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
		"echo 'HTTP 404: Not Found' >&2\n" +
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
