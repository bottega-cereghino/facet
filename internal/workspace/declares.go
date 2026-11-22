package workspace

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/bottega-cereghino/facet/internal/gitx"
)

// declaresPackage tells, for each from pattern of an installs directory that
// can name a file declaring nothing to install, whether one does. A pattern
// missing here counts by its name: a setup.py and a Pipfile exist only to
// declare a package. Each check reads as little as it must, and what it does
// not understand declares nothing: a directory wrongly kept costs a few
// megabytes, one wrongly deleted is gone.
var declaresPackage = map[string]func(text string, beside besideFiles) bool{
	// npm, pnpm, yarn and bun install what package.json lists under a
	// dependency field, and the packages of its workspaces; pnpm reads its
	// workspaces from pnpm-workspace.yaml instead.
	"package.json": func(text string, beside besideFiles) bool {
		return beside.has("pnpm-workspace.yaml") || packageJSONInstalls(text)
	},
	// uv sync and pip install . need a [project] table, poetry install a
	// [tool.poetry] one. A uv.lock beside it means uv resolved the directory as
	// a project or as a workspace root, which has no [project].
	"pyproject.toml": func(text string, beside besideFiles) bool {
		return beside.has("uv.lock") || tomlHasTable(text, "project", "tool.poetry")
	},
	// setuptools reads a package from setup.cfg's [metadata] and [options], but
	// pip builds only a directory with a setup.py, or a pyproject.toml that
	// leaves the build to setuptools.
	"setup.cfg": func(text string, beside besideFiles) bool {
		if !setupCfgDeclares(text) {
			return false
		}
		if beside.has("setup.py") {
			return true
		}
		py, ok := beside.read("pyproject.toml")
		return ok && buildsWithSetuptools(py)
	},
	"requirements*.txt": requirementsList,
}

// maxRebuildFile bounds what readTracked reads. No manifest comes near it.
const maxRebuildFile = 1 << 20

// readTracked returns the text of the file rel in the clone when the copy in
// the index and the regular file on disk say the same: the one a fresh clone
// rebuilds from and the one an install reads now. A file changed since it was
// staged, a symlink, or one that cannot be read is not returned, and so
// declares nothing. Line endings are left out of the comparison, so a
// checkout that converts them still reads.
func readTracked(git gitx.Runner, clone, rel string) (string, bool) {
	p := filepath.Join(clone, filepath.FromSlash(rel))
	if fi, err := os.Lstat(p); err != nil || !fi.Mode().IsRegular() {
		return "", false
	}
	f, err := os.Open(p)
	if err != nil {
		return "", false
	}
	defer func() { _ = f.Close() }()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return "", false
	}
	disk, err := io.ReadAll(io.LimitReader(f, maxRebuildFile+1))
	if err != nil || len(disk) > maxRebuildFile {
		return "", false
	}
	staged, err := git.Run(clone, nil, "cat-file", "blob", ":"+rel)
	if err != nil {
		return "", false
	}
	text := normalizeEOL(staged)
	if text != normalizeEOL(string(disk)) {
		return "", false
	}
	return text, true
}

func normalizeEOL(s string) string {
	return strings.TrimRight(strings.ReplaceAll(s, "\r\n", "\n"), " \t\r\n")
}

// packageJSONInstalls reports whether a package.json lists a dependency or a
// workspace. One that only sets "type", scripts or a tool's settings installs
// nothing.
func packageJSONInstalls(text string) bool {
	var pkg map[string]json.RawMessage
	if json.Unmarshal([]byte(text), &pkg) != nil {
		return false
	}
	for _, field := range []string{"dependencies", "devDependencies", "optionalDependencies", "peerDependencies"} {
		var deps map[string]json.RawMessage
		if json.Unmarshal(pkg[field], &deps) == nil && len(deps) > 0 {
			return true
		}
	}
	var list []string // "workspaces": ["packages/*"]
	if json.Unmarshal(pkg["workspaces"], &list) == nil && len(list) > 0 {
		return true
	}
	var obj struct{ Packages []string } // "workspaces": {"packages": [...]}, as yarn takes it
	return json.Unmarshal(pkg["workspaces"], &obj) == nil && len(obj.Packages) > 0
}

// setupCfgDeclares reports whether a setup.cfg holds a section setuptools
// builds a package from. A section header starts its line: an indented one
// continues the value above it.
func setupCfgDeclares(text string) bool {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimRight(line, " \t\r")
		if line == "[metadata]" || line == "[options]" ||
			(strings.HasPrefix(line, "[options.") && strings.HasSuffix(line, "]")) {
			return true
		}
	}
	return false
}

// buildsWithSetuptools reports whether pip builds the directory of this
// pyproject.toml with setuptools, the one backend that reads setup.cfg: it
// names setuptools as its build backend, or names none, and pip then falls
// back to setuptools.
func buildsWithSetuptools(pyproject string) bool {
	backend := ""
	tomlLines(pyproject, func(table, line string) {
		key, value, ok := strings.Cut(line, "=")
		if ok && table == "build-system" && strings.TrimSpace(key) == "build-backend" {
			value, _, _ = strings.Cut(value, "#")
			backend = strings.Trim(strings.TrimSpace(value), `"'`)
		}
	})
	if backend == "" {
		return !strings.Contains(pyproject, "build-backend")
	}
	return backend == "setuptools.build_meta" || backend == "setuptools.build_meta:__legacy__"
}

// requirementsList reports whether a requirements file names a requirement.
// Comments, blank lines and options (-r, -c, --index-url) are not counted: an
// option alone installs nothing from this file.
func requirementsList(text string, _ besideFiles) bool {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") && !strings.HasPrefix(line, "-") {
			return true
		}
	}
	return false
}

// tomlHasTable reports whether a TOML document opens one of the tables, or a
// table below one, with a header line.
func tomlHasTable(doc string, tables ...string) bool {
	found := false
	tomlLines(doc, func(table, _ string) {
		for _, t := range tables {
			if table == t || strings.HasPrefix(table, t+".") {
				found = true
			}
		}
	})
	return found
}

// tomlLines calls f with each line of a TOML document that starts outside a
// multi-line string, and the table the line is in: the name in the last header
// ([name] or [[name]]) at or above it, its whitespace removed. A header quoted
// inside a multi-line string is not read as one. This is no TOML parser: a
// table opened another way (a dotted key, an inline table, a quoted name in
// its header) is not seen, which only ever keeps a directory.
func tomlLines(doc string, f func(table, line string)) {
	table, open := "", ""
	for _, line := range strings.Split(doc, "\n") {
		if open == "" {
			if name, ok := tomlHeader(strings.TrimSpace(line)); ok {
				table = name
			}
			f(table, line)
		}
		open = tomlOpenString(line, open)
	}
}

// tomlHeader returns the table name a trimmed line opens, if it is a header.
func tomlHeader(line string) (string, bool) {
	var inner, rest string
	var ok bool
	if s, found := strings.CutPrefix(line, "[["); found {
		inner, rest, ok = strings.Cut(s, "]]")
	} else if s, found := strings.CutPrefix(line, "["); found {
		inner, rest, ok = strings.Cut(s, "]")
	}
	rest = strings.TrimSpace(rest)
	if !ok || (rest != "" && !strings.HasPrefix(rest, "#")) {
		return "", false
	}
	return strings.Join(strings.Fields(inner), ""), true
}

// tomlOpenString returns the delimiter of the multi-line string still open at
// the end of line ("" for none), given the one open at its start. Comments and
// single-line strings are skipped, so a delimiter inside either opens nothing.
func tomlOpenString(line, open string) string {
	for i := 0; i < len(line); {
		if open != "" {
			j := strings.Index(line[i:], open)
			if j < 0 {
				return open
			}
			end := i + j
			if open == `"""` && escaped(line, end) {
				i = end + 1
				continue
			}
			// A string may end in one or two of its own quotes before its
			// delimiter, so the whole run of quotes closes it.
			i = end + 3
			i += len(line[i:]) - len(strings.TrimLeft(line[i:], open[:1]))
			open = ""
			continue
		}
		switch rest := line[i:]; {
		case rest[0] == '#':
			return ""
		case strings.HasPrefix(rest, `"""`), strings.HasPrefix(rest, "'''"):
			open, i = rest[:3], i+3
		case rest[0] == '"' || rest[0] == '\'':
			i = skipString(line, i)
		default:
			i++
		}
	}
	return open
}

// escaped reports whether the byte at i follows an odd run of backslashes.
func escaped(s string, i int) bool {
	n := 0
	for j := i - 1; j >= 0 && s[j] == '\\'; j-- {
		n++
	}
	return n%2 == 1
}

// skipString returns the index just past the single-line string that opens at
// i, or the end of s when it does not close there.
func skipString(s string, i int) int {
	q := s[i]
	for j := i + 1; j < len(s); j++ {
		switch {
		case q == '"' && s[j] == '\\':
			j++
		case s[j] == q:
			return j + 1
		}
	}
	return len(s)
}

// orList joins names as "a, b or c".
func orList(names []string) string {
	if len(names) < 2 {
		return strings.Join(names, "")
	}
	return strings.Join(names[:len(names)-1], ", ") + " or " + names[len(names)-1]
}
