package workspace

import (
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/bottega-cereghino/facet/internal/gitx"
)

// regenNames are the directories Prune deletes, each with what rebuilds it and
// the tracked files it is rebuilt from. Deleting one costs a reinstall, never
// work, but only when its rebuild exists: a hand-built .venv in a clone with no
// pyproject.toml would be gone for good. So a directory is named only when the
// clone tracks one of its files beside it, in the same directory of the clone,
// since that is where each of these tools writes it: an install fills the
// node_modules or .venv next to the manifest it reads, and a cache lands in
// the project directory the tool ran in. A rebuild file further up is not
// enough: a .venv in api/ beside nothing is not what the root's pyproject.toml
// installs. Each `from` entry is a path.Match pattern on a base name.
//
// Build output (build/, dist/, out/) is left off on purpose: the same names
// hold hand-made renders and release artefacts, and nothing in the tree says
// which is which. .terraform is left off too: it keeps the selected
// `terraform workspace` and the backend settings given at init, and after a
// fresh init a plan would quietly target "default".
var regenNames = map[string]regenDir{
	// Every package manager (npm, pnpm, yarn, bun) installs from package.json.
	// A lockfile pins the versions but always sits beside it, and installs
	// nothing alone. In a monorepo, each package's node_modules sits beside
	// that package's package.json, and the root install fills it.
	"node_modules": {"the package manager's install", []string{"package.json"}, true},
	// uv sync, pip install . and poetry install read pyproject.toml, older
	// projects setup.py or setup.cfg, pipenv a Pipfile, and pip install -r a
	// requirements file (requirements.txt, requirements-dev.txt). uv.lock and
	// poetry.lock are left out: they always sit beside a pyproject.toml, and
	// neither installs anything without it.
	".venv": {"uv sync, or a new venv and pip install", pythonProject, true},
	// The three caches hold nothing but what their tool computes again from the
	// code. They are named beside a Python project's files or the tool's own
	// configuration, which is where each tool writes its cache: mypy in the
	// directory it ran from, pytest in its rootdir, ruff in its project root. A
	// pyproject.toml or setup.cfg holding only the tool's settings counts here,
	// since that is the file the tool reads.
	".mypy_cache":   {"mypy, on its next run", append([]string{"mypy.ini", ".mypy.ini"}, pythonProject...), false},
	".pytest_cache": {"pytest, on its next run", append([]string{"pytest.ini", "conftest.py"}, pythonProject...), false},
	".ruff_cache":   {"ruff, on its next run", append([]string{"ruff.toml", ".ruff.toml"}, pythonProject...), false},
	// next build, turbo and parcel run as a package's scripts, from the
	// directory of its package.json; turbo.json, next.config.* and .parcelrc,
	// when there is one, sit beside it. What they write is computed from the
	// code, never installed into by hand.
	".next":         {"next build", []string{"package.json"}, false},
	".turbo":        {"turbo, on its next run", []string{"package.json"}, false},
	".parcel-cache": {"parcel, on its next run", []string{"package.json"}, false},
}

// pythonProject is the files that make a directory a Python project's root.
var pythonProject = []string{"pyproject.toml", "setup.py", "setup.cfg", "Pipfile", "requirements*.txt"}

// regenDir is what rebuilds one name in regenNames, and from which files.
type regenDir struct {
	rebuild string
	from    []string // base-name patterns; any one tracked beside it will do
	// installs marks a directory of installed packages. Packages go into one
	// by hand too (pip install, npm install --no-save), and recorded nowhere
	// they are lost with it. A file in from can also sit in a repository
	// without declaring a package: a pyproject.toml holding only ruff's
	// settings, a package.json only marking ES modules. So beside one of these
	// a file counts only when declaresPackage says it declares one.
	installs bool
}

// besideFiles is what a declaresPackage check may look at next to the file it
// reads: the names tracked in the same directory, and their text.
type besideFiles struct {
	names []string
	text  func(name string) (string, bool)
}

func (b besideFiles) has(name string) bool { return slices.Contains(b.names, name) }

func (b besideFiles) read(name string) (string, bool) {
	if !b.has(name) {
		return "", false
	}
	return b.text(name)
}

// trackedBeside returns a check of whether the clone's index holds a file in
// dir ("" for the clone's root) that rebuilds r: one matching a pattern in
// r.from, which for a directory of installed packages must also declare one.
// The index is listed once, on the first check, so a clone with nothing to
// prune costs no listing.
func trackedBeside(git gitx.Runner, clone string) func(dir string, r regenDir) (bool, error) {
	var byDir map[string][]string
	return func(dir string, r regenDir) (bool, error) {
		if byDir == nil {
			out, err := git.Run(clone, nil, "ls-files", "-z", "--cached")
			if err != nil {
				return false, err
			}
			byDir = map[string][]string{}
			for _, f := range strings.Split(out, "\x00") {
				if f != "" {
					byDir[pathDir(f)] = append(byDir[pathDir(f)], path.Base(f))
				}
			}
		}
		beside := besideFiles{names: byDir[dir], text: func(name string) (string, bool) {
			return readTracked(git, clone, path.Join(dir, name))
		}}
		for _, base := range beside.names {
			for _, p := range r.from {
				if ok, _ := path.Match(p, base); !ok {
					continue
				}
				declares, check := declaresPackage[p]
				if !r.installs || !check {
					return true, nil
				}
				if text, ok := beside.read(base); ok && declares(text, beside) {
					return true, nil
				}
			}
		}
		return false, nil
	}
}

// keptUnbuilt is the kept: line for a directory nothing tracked beside it
// rebuilds.
func keptUnbuilt(clone, rel string, r regenDir) string {
	if r.installs {
		return fmt.Sprintf("%s/%s has no %s tracked beside it that declares a package to install, so nothing rebuilds it",
			clone, rel, orList(r.from))
	}
	return fmt.Sprintf("%s/%s has no %s tracked beside it, so nothing rebuilds it", clone, rel, orList(r.from))
}
