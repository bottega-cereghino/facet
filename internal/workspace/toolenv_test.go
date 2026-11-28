package workspace

import (
	"path/filepath"
	"testing"

	"github.com/bottega-cereghino/facet/internal/gitx"
)

// Tool environments and test run leftovers go with a clone (#591): reap lets
// go of an ignored path named by its place in the clone, and holds its near
// misses.
var toolEnvIgnore = []string{".tools/", "venv/", ".venv-types/", ".import_linter_cache/", "test-results/"}

func toolEnvClone(t *testing.T) (ws, clone string) {
	t.Helper()
	ws, clone = longLivedWorkspace(t, t.TempDir())
	for _, pattern := range toolEnvIgnore {
		excludeIn(t, clone, pattern)
	}
	return ws, clone
}

func putFile(t *testing.T, clone, rel string) {
	t.Helper()
	mkdirs(t, filepath.Dir(filepath.Join(clone, filepath.FromSlash(rel))))
	writeTestFile(t, filepath.Join(clone, filepath.FromSlash(rel)), "left over\n")
}

func TestReapReleasesToolEnvironmentsAndTestRunLeftovers(t *testing.T) {
	t.Parallel()
	for _, rel := range []string{
		".tools/venv/bin/python",
		".tools/pip-cache/http/a",
		".venv-types/lib/x.pyi",
		".import_linter_cache/data.json",
		"test-results/.last-run.json",
		"test-results/.playwright-artifacts-0/trace.zip",
		"test-results/.playwright-artifacts-12/a/b.png",
	} {
		t.Run(rel, func(t *testing.T) {
			t.Parallel()
			ws, clone := toolEnvClone(t)
			putFile(t, clone, rel)
			st, err := Inspect(ws, gitx.Git{}, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			if b := st.Blockers(); len(b) != 0 {
				t.Fatalf("%s goes with the clone: %v", rel, b)
			}
		})
	}
}

func TestReapHoldsTheNearMissesOfToolEnvironments(t *testing.T) {
	t.Parallel()
	for _, rel := range []string{
		"venv/bin/python",
		".tools/notes",
		".tools/venv-notes/a",
		".tools/pip-cache-x/a",
		"sub/.tools/venv/bin/python",
		"test-results/records/x",
		"test-results/.last-run.json.bak",
		"test-results/.playwright-artifacts-x/a",
		"test-results/.playwright-artifacts-/a",
		"test-results/.playwright-artifacts-1x/a",
		".venv-types-old/a",
		".import_linter_cache2/a",
	} {
		t.Run(rel, func(t *testing.T) {
			t.Parallel()
			ws, clone := toolEnvClone(t)
			excludeIn(t, clone, "sub/")
			excludeIn(t, clone, ".venv-types-old/")
			excludeIn(t, clone, ".import_linter_cache2/")
			putFile(t, clone, rel)
			st, err := Inspect(ws, gitx.Git{}, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			requireHeld(t, st, rel)
		})
	}
}

// A bare directory of a name holds nothing to release only when it is empty
// or released; a file named like one of the directories is not one.
func TestReapHoldsWorkBesideToolEnvironments(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, clone string)
	}{
		{"uncommitted change", func(t *testing.T, clone string) {
			writeTestFile(t, filepath.Join(clone, "README"), "edited\n")
		}},
		{"untracked file", func(t *testing.T, clone string) {
			writeTestFile(t, filepath.Join(clone, "notes.txt"), "mine\n")
		}},
		{"unpushed commit", func(t *testing.T, clone string) {
			commitFile(t, clone, "new.txt", "x\n", "local only")
		}},
		{"stash", func(t *testing.T, clone string) {
			writeTestFile(t, filepath.Join(clone, "README"), "stashed\n")
			mustGit(t, clone, "-c", "user.name=t", "-c", "user.email=t@example.com", "stash", "push", "-q")
		}},
		{"repository inside .tools/venv", func(t *testing.T, clone string) {
			inner := filepath.Join(clone, ".tools", "venv", "lib", "editable")
			mkdirs(t, inner)
			gitOut(t, inner, "init", "-q", "-b", "main")
			commitFile(t, inner, "work.txt", "only here\n", "inner work")
		}},
		{"repository inside .tools/pip-cache", func(t *testing.T, clone string) {
			inner := filepath.Join(clone, ".tools", "pip-cache", "src")
			mkdirs(t, inner)
			gitOut(t, inner, "init", "-q", "-b", "main")
			commitFile(t, inner, "work.txt", "only here\n", "inner work")
		}},
		{"repository inside .venv-types", func(t *testing.T, clone string) {
			inner := filepath.Join(clone, ".venv-types", "src")
			mkdirs(t, inner)
			gitOut(t, inner, "init", "-q", "-b", "main")
			commitFile(t, inner, "work.txt", "only here\n", "inner work")
		}},
		{"repository inside .import_linter_cache", func(t *testing.T, clone string) {
			inner := filepath.Join(clone, ".import_linter_cache", "src")
			mkdirs(t, inner)
			gitOut(t, inner, "init", "-q", "-b", "main")
			commitFile(t, inner, "work.txt", "only here\n", "inner work")
		}},
		{"repository inside a playwright artifacts directory", func(t *testing.T, clone string) {
			inner := filepath.Join(clone, "test-results", ".playwright-artifacts-1", "src")
			mkdirs(t, inner)
			gitOut(t, inner, "init", "-q", "-b", "main")
			commitFile(t, inner, "work.txt", "only here\n", "inner work")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ws, clone := toolEnvClone(t)
			putFile(t, clone, ".tools/venv/bin/python")
			tc.setup(t, clone)
			st, err := Inspect(ws, gitx.Git{}, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			requireHeld(t, st, tc.name)
		})
	}
}
