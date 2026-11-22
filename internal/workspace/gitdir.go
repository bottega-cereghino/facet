package workspace

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/bottega-cereghino/facet/internal/gitx"
)

// maxAlternateDepth is how deep git follows alternates of alternates.
const maxAlternateDepth = 5

// gitDirHolds returns the reasons the repository git runs at in dir cannot be
// judged by its refs, from git's own layout (gitrepository-layout), each with
// the fix (#364):
//
//   - info/grafts, which rewrites history locally as replace refs do, and
//     which GIT_NO_REPLACE_OBJECTS does not turn off: a graft hanging a local
//     commit on a remote's branch makes it read as pushed;
//   - objects/info/alternates leading into gone, the directory reap deletes:
//     the repository borrows objects that go with the workspace. For a
//     worktree clone this is the checkout's repository, outside the
//     workspace, which would be left broken.
func gitDirHolds(git gitx.Runner, dir, gone string) ([]string, error) {
	common, err := commonDir(git, dir)
	if err != nil {
		return nil, err
	}
	var holds []string
	grafts := filepath.Join(common, "info", "grafts")
	if _, err := os.Lstat(grafts); err == nil {
		holds = append(holds, fmt.Sprintf("its repository has %s, which rewrites history locally, so reap cannot tell what a remote has; "+
			"run `git replace --convert-graft-file` in it, then reap again", grafts))
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	inside, err := alternateInside(filepath.Join(common, "objects"), gone)
	if err != nil {
		return holds, fmt.Errorf("read the alternates of %s: %w", common, err)
	}
	if inside != "" {
		holds = append(holds, fmt.Sprintf("its repository %s borrows objects from %s, which goes with the workspace; "+
			"run `git repack -a -d` in it and delete its objects/info/alternates, then reap again", common, inside))
	}
	return holds, nil
}

// commonDir is the real path of the git directory that holds the objects and
// refs of the repository git runs at in dir: a worktree's commondir, followed.
func commonDir(git gitx.Runner, dir string) (string, error) {
	out, err := git.Run(dir, nil, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", err
	}
	return realPath(strings.TrimSpace(out)), nil
}

// alternateInside follows the alternates of the object directory objects as
// git does, maxAlternateDepth deep, and returns the first one that lies
// inside gone, or "". An alternates file that cannot be read is an error.
func alternateInside(objects, gone string) (string, error) {
	root := realPath(gone)
	seen := map[string]bool{}
	var walk func(objects string, depth int) (string, error)
	walk = func(objects string, depth int) (string, error) {
		if depth > maxAlternateDepth || seen[objects] {
			return "", nil
		}
		seen[objects] = true
		alts, err := readAlternates(objects)
		if err != nil {
			return "", err
		}
		for _, alt := range alts {
			resolved := resolvePath(alt)
			if isUnder(root, resolved) {
				return alt, nil
			}
			if found, err := walk(resolved, depth+1); err != nil || found != "" {
				return found, err
			}
		}
		return "", nil
	}
	return walk(realPath(objects), 0)
}

// readAlternates lists the object directories objects/info/alternates names,
// as absolute paths: one per line, relative to objects when not absolute,
// C-quoted when it starts with a double quote, a line starting with # a
// comment. A missing file names none.
func readAlternates(objects string) ([]string, error) {
	f, err := os.Open(filepath.Join(objects, "info", "alternates"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, `"`) {
			unq, err := strconv.Unquote(line)
			if err != nil {
				return nil, fmt.Errorf("cannot read alternate %s: %w", line, err)
			}
			line = unq
		}
		if !filepath.IsAbs(line) {
			line = filepath.Join(objects, line)
		}
		out = append(out, filepath.Clean(line))
	}
	return out, sc.Err()
}
