package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bottega-cereghino/facet/internal/ghx"
)

func TestForbidHitOnAddedLines(t *testing.T) {
	patterns, err := compileForbid(defaultForbid)
	if err != nil {
		t.Fatal(err)
	}
	foo := []ghx.DiffFile{{Name: "a.ts", Patch: "@@ -1 +1 @@\n-foo\n+foo!\n"}}
	cast := []ghx.DiffFile{{Name: "a.ts", Patch: "@@ -1 +1 @@\n-const x = foo\n+const x = foo as unknown as T\n"}}
	kept := []ghx.DiffFile{{Name: "a.ts", Patch: "@@ -1 +1 @@\n-foo!\n+foo\n"}}
	neq := []ghx.DiffFile{{Name: "a.ts", Patch: "@@ -1 +1 @@\n-if foo != 1 {\n+if foo !== 1 {\n"}}
	call := []ghx.DiffFile{{Name: "a.ts", Patch: "@@ -1 +1 @@\n-bar\n+arr[i]!\n+\n+(expr)!\n"}}
	baseUp := []ghx.DiffFile{{Name: "config/size-limit.json", Patch: "@@ -1 +1 @@\n-{\"max\": 3}\n+{\"max\": 4}\n"}}
	baseDown := []ghx.DiffFile{{Name: "config/size-limit.json", Patch: "@@ -1 +1 @@\n-{\"max\": 4}\n+{\"max\": 3}\n"}}
	otherUp := []ghx.DiffFile{{Name: "unlimited.go", Patch: "@@ -1 +1 @@\n-n := 3\n+n := 4\n"}}
	one := func(name, line string) []ghx.DiffFile {
		return []ghx.DiffFile{{Name: name, Patch: "@@ -1 +1 @@\n-x\n+" + line + "\n"}}
	}
	multi := []ghx.DiffFile{{Name: "a.ts", Patch: "@@ -1 +1 @@\n-x\n+import {\n+  strings as stringsFor,\n+} from './s'\n+const y = z as Q\n"}}
	// body opens a brace on an export line that is not an import or export list.
	body := func(opener string) []ghx.DiffFile {
		return []ghx.DiffFile{{Name: "a.ts", Patch: "@@ -1 +1 @@\n-x\n+" + opener + "\n+  const y = z as Q\n+}\n"}}
	}
	for _, tc := range []struct {
		name    string
		files   []ghx.DiffFile
		pattern string
		file    string
	}{
		{"postfix bang", foo, defaultForbid[0], "a.ts"},
		{"as cast", cast, " as ", "a.ts"},
		{"a removed bang is not added", kept, "", ""},
		{"inequality is not a non-null assertion", neq, "", ""},
		{"bang after a bracket or a paren", call, defaultForbid[0], "a.ts"},
		{"import alias", one("a.ts", "import { strings as stringsFor } from './s'"), "", ""},
		{"multi-line import alias, then a cast", multi, " as ", "a.ts"},
		{"cast in an exported function", body("export function f(z: unknown) {"), " as ", "a.ts"},
		{"cast in an exported class", body("export class A {"), " as ", "a.ts"},
		{"cast in an exported object", body("export const o = {"), " as ", "a.ts"},
		{"cast in an exported type", body("export type T = {"), " as ", "a.ts"},
		{"multi-line type re-export", one("a.ts", "export type {"), "", ""},
		{"re-export alias", one("a.ts", "export { a as b } from './a'"), "", ""},
		{"python import as", one("a.py", "import numpy as np"), "", ""},
		{"python with as", one("a.py", "with open(p) as f:"), "", ""},
		{"such as in markdown", one("README.md", "Files such as this one"), "", ""},
		{"as in a comment", one("a.ts", "// a cast such as x as T is banned"), "", ""},
		{"as in a string", one("a.ts", "const m = 'cast as T'"), "", ""},
		{"cast in a tsx file", one("a.tsx", "const v = x as Bar"), " as ", "a.tsx"},
		{"as any", one("a.ts", "const v = x as any"), " as ", "a.ts"},
		{"nolint in go", one("a.go", "// nolint"), `\bnolint\b`, "a.go"},
		{"ts-ignore in ts", one("a.ts", "// @ts-ignore"), "@ts-ignore", "a.ts"},
		{"bang in go is not a cast", one("a.go", "x := f()!"), "", ""},
		{"nolint in markdown is prose", one("a.md", "use nolint here"), "", ""},
		{"a baseline number raised", baseUp, "higher number", "config/size-limit.json"},
		{"a baseline number lowered", baseDown, "", ""},
		{"a higher number in another file", otherUp, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pattern, file, bad := defaultForbidHit(tc.files, patterns)
			if tc.pattern == "" {
				if bad {
					t.Fatalf("forbidHit = %q in %s", pattern, file)
				}
				return
			}
			if !bad || pattern != tc.pattern || file != tc.file {
				t.Fatalf("forbidHit = %q in %s, bad %v", pattern, file, bad)
			}
		})
	}
}

// A configured fix.forbid is used as written, on every file.
func TestForbidHitAsWritten(t *testing.T) {
	patterns, err := compileForbid([]string{" as "})
	if err != nil {
		t.Fatal(err)
	}
	files := []ghx.DiffFile{{Name: "README.md", Patch: "@@ -1 +1 @@\n-x\n+such as this\n"}}
	if p, f, bad := forbidHit(files, patterns); !bad || p != " as " || f != "README.md" {
		t.Fatalf("forbidHit = %q in %s, bad %v", p, f, bad)
	}
}

func TestQueueRefusesAFixThatSuppresses(t *testing.T) {
	foo := "@@ -1 +1 @@\n-foo\n+foo!\n"
	cast := "@@ -1 +1 @@\n-const x = foo\n+const x = foo as unknown as T\n"
	raised := "@@ -1 +1 @@\n-{\"max\": 3}\n+{\"max\": 4}\n"
	clean := "@@ -1 +1 @@\n-old\n+new\n"
	for _, tc := range []struct {
		name    string
		patch   string
		file    string
		off     bool
		block   bool
		lands   bool
		wantLog string
	}{
		{"foo!", foo, "a.ts", false, true, false, "the fix suppresses instead of fixing:"},
		{"as unknown as T", cast, "a.ts", false, true, false, "the fix suppresses instead of fixing:"},
		{"baseline number raised", raised, "eslint-baseline.json", false, true, false, "higher number"},
		{"clean fix lands", clean, "a.ts", false, false, true, ""},
		{"forbid off accepts a bang", foo, "a.ts", true, false, true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log := filepath.Join(t.TempDir(), "log")
			notified := filepath.Join(t.TempDir(), "notify")
			cfg := fixQueue(t, log)
			cfg.Fix.Notify = []string{appendScript(t), notified, "{repo}#{number} PR {pr}: {reason}"}
			if tc.off {
				cfg.Fix.forbidOff = true
				cfg.Fix.Forbid = nil
			}
			seedState(t, cfg.State, []int{1}, nil)
			labels := &labelCalls{}
			land := &landCalls{}
			prs := func(string) ([]ghx.OpenPR, error) {
				n := countLines(log)
				pr := redPR(11, "feature/1-bell")
				pr.HeadRefOid = fmt.Sprintf("h%d", n)
				if n > 0 {
					pr = landPR(11, "feature/1-bell", "CLEAN")
					pr.HeadRefOid = fmt.Sprintf("h%d", n)
				}
				return []ghx.OpenPR{pr}, nil
			}
			compare := func(string, string, string) ([]ghx.DiffFile, error) {
				return []ghx.DiffFile{{Name: tc.file, Patch: tc.patch}}, nil
			}
			s := startQueue(t, cfg, land.env(queueEnv{
				list:         noIssues,
				prs:          prs,
				addLabel:     labels.add,
				compareFiles: compare,
			}))
			if tc.block {
				waitFor(t, "the notify", func() bool { return len(readLines(t, notified)) >= 1 })
			} else {
				waitFor(t, "the merge", func() bool { return len(land.list()) >= 1 })
			}
			s.stop(t)
			if tc.lands != (len(land.list()) > 0) {
				t.Fatalf("landed %q, want lands %v", land.list(), tc.lands)
			}
			if tc.block {
				got := readLines(t, log)
				if len(got) < 2 || !strings.Contains(got[0], "try 1:") || !strings.Contains(got[1], "try 2:") {
					t.Fatalf("runs = %q, want two tries", got)
				}
				if labels.list()[0] != "acme/widgets#1 blocked" {
					t.Fatalf("labels = %q", labels.list())
				}
				note := readLines(t, notified)
				if len(note) != 1 || !strings.Contains(note[0], tc.wantLog) || !strings.Contains(note[0], tc.file) {
					t.Fatalf("notify = %q", note)
				}
				st, err := loadQueueState(cfg.State)
				if err != nil {
					t.Fatal(err)
				}
				rec := st.Fixes["acme/widgets#1"]
				if rec == nil || !rec.Blocked || rec.Runs != 2 {
					t.Fatalf("record = %+v", rec)
				}
			}
			if tc.wantLog != "" && !tc.lands {
				if !strings.Contains(s.buf.String(), tc.wantLog) || !strings.Contains(s.buf.String(), "not landing") {
					t.Fatalf("log =\n%s", s.buf.String())
				}
			}
		})
	}
}

func countLines(path string) int {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	s := strings.TrimSuffix(string(b), "\n")
	if s == "" {
		return 0
	}
	return strings.Count(s, "\n") + 1
}

// A fix diff that cannot be read is not landed, and is read again only
// after a wait that doubles, not on every tick.
func TestQueueBacksOffAnUnreadableFixDiff(t *testing.T) {
	cfg := fixQueue(t, filepath.Join(t.TempDir(), "log"))
	seedState(t, cfg.State, []int{1}, nil)
	st, err := loadQueueState(cfg.State)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	st.Fixes["acme/widgets#1"] = &fixRecord{PR: 11, Runs: 1, Last: start.Add(-time.Hour), Head: "h0"}
	if err := st.save(cfg.State); err != nil {
		t.Fatal(err)
	}
	reads := &countCalls{}
	ticks := &countCalls{}
	land := &landCalls{}
	var now atomic.Value
	now.Store(start)
	s := startQueue(t, cfg, land.env(queueEnv{
		list: func(repo, _ string) ([]ghx.Issue, error) { ticks.add(repo); return nil, nil },
		prs:  staticPRs(landPR(11, "feature/1-bell", "CLEAN")),
		compareFiles: func(repo, _, _ string) ([]ghx.DiffFile, error) {
			reads.add(repo)
			return nil, errors.New("no patch for a.ts")
		},
		now: func() time.Time { return now.Load().(time.Time) },
	}))
	waitFor(t, "ten rounds", func() bool { return ticks.get("acme/widgets") >= 10 })
	if got := reads.get("acme/widgets"); got != 1 {
		t.Fatalf("diff read %d times in ten rounds, want 1", got)
	}
	now.Store(start.Add(cfg.Wait))
	waitFor(t, "the second read", func() bool { return reads.get("acme/widgets") >= 2 })
	before := ticks.get("acme/widgets")
	waitFor(t, "ten more rounds", func() bool { return ticks.get("acme/widgets") >= before+10 })
	s.stop(t)
	if got := reads.get("acme/widgets"); got != 2 {
		t.Fatalf("diff read %d times, want 2", got)
	}
	if len(land.list()) != 0 {
		t.Fatalf("landed = %q", land.list())
	}
	out := s.buf.String()
	if strings.Count(out, "no patch for a.ts; not landing, reading it again after") != 2 ||
		!strings.Contains(out, "reading it again after "+(2*cfg.Wait).String()) {
		t.Fatalf("log =\n%s", out)
	}
}

func TestForbidCustomFlag(t *testing.T) {
	base := `"repos": ["acme/widgets"], "agents": [{"command": ["agent", "{prompt}"]}]`
	for forbid, want := range map[string]bool{``: false, `"forbid": [" as "], `: true} {
		cfg, err := loadTempConfig(t, `{"state": "s", "fix": {`+forbid+`"command": ["fixer", "{prompt}"]}, `+base+`}`)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Fix.forbidCustom != want {
			t.Fatalf("forbid %q: forbidCustom = %v, want %v", forbid, cfg.Fix.forbidCustom, want)
		}
	}
}

// A line a merge brought in from the base branch is in the run's compare but
// not in the pull request's diff, so it is not judged. A line the run wrote,
// in a merge commit's conflict resolution too, is in both and is judged.
func TestOnlyInBothJudgesTheRunsOwnLines(t *testing.T) {
	patterns, err := compileForbid(defaultForbid)
	if err != nil {
		t.Fatal(err)
	}
	own := "@@ -1 +1,2 @@\n x\n+// nolint\n"
	for _, tc := range []struct {
		name string
		pr   []ghx.DiffFile
		bad  bool
	}{
		{"a line merged in from the base", []ghx.DiffFile{{Name: "a.go", Patch: "@@ -1 +1 @@\n-x\n+y\n"}}, false},
		{"a merged file the pull request does not touch", nil, false},
		{"a line the run wrote", []ghx.DiffFile{{Name: "a.go", Patch: own}}, true},
		{"the same line in another file", []ghx.DiffFile{{Name: "b.go", Patch: own}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files := onlyInBoth([]ghx.DiffFile{{Name: "a.go", Patch: own}}, tc.pr)
			if _, _, bad := defaultForbidHit(files, patterns); bad != tc.bad {
				t.Fatalf("bad = %v, want %v", bad, tc.bad)
			}
		})
	}
}
