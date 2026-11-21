package workspace

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bottega-cereghino/facet/internal/gitx"
	"github.com/bottega-cereghino/facet/internal/manifest"
)

// regenNames are the directories Prune deletes, each with what rebuilds it.
// Each is rebuilt by one command from files the repository tracks (a lockfile,
// a requirements file, terraform's lock file), so deleting one costs a
// reinstall, never work. Build output (build/, dist/, out/) is left off on
// purpose: the same names hold hand-made renders and release artefacts, and
// nothing in the tree says which is which.
var regenNames = map[string]string{
	"node_modules":  "the package manager's install",
	".venv":         "uv sync, or a new venv and pip install",
	".terraform":    "terraform init",
	".mypy_cache":   "mypy, on its next run",
	".pytest_cache": "pytest, on its next run",
	".ruff_cache":   "ruff, on its next run",
	".next":         "next build",
	".turbo":        "turbo, on its next run",
	".parcel-cache": "parcel, on its next run",
}

// minRegenBytes is the least a directory must free to be named. A node_modules
// that pnpm cloned from its store frees next to nothing, and deleting it would
// only cost a reinstall.
const minRegenBytes = 1 << 20

// Regen is one directory Prune would delete.
type Regen struct {
	Clone   string // the clone's directory name in the manifest
	Rel     string // its path inside the clone, slash-separated
	Path    string
	Rebuild string // what brings it back
	Bytes   int64  // what deleting it frees
}

// PruneState is what InspectPrune found in one workspace.
type PruneState struct {
	Dir      string
	LastUsed time.Time // the newest git or install activity in any clone
	Found    []Regen
	Held     []string // why Prune leaves it alone; empty means it may prune
}

// Frees is what deleting everything found would give back.
func (s *PruneState) Frees() int64 {
	var n int64
	for _, r := range s.Found {
		n += r.Bytes
	}
	return n
}

// InspectPrune finds the regenerable directories in ws's clones: a directory
// named in regenNames that git ignores, holds no tracked file, is not inside a
// nested repository or worktree, and frees at least minRegenBytes. It holds the workspace when a clone
// was used within idle, when something runs from it (live, as reap asks), or
// when a clone's ignored files cannot be listed. idle 0 skips the age check.
//
// Only ignored, regenerable directories are ever named, so neither a commit
// nor an uncommitted change can be among them: reap's guarantee that work is
// never lost does not need re-proving here. Links are not followed; their
// targets belong to other checkouts.
func InspectPrune(ws string, git gitx.Runner, live LiveChecker, now time.Time, idle time.Duration) (*PruneState, error) {
	m, err := manifest.Read(ws)
	if err != nil {
		return nil, err
	}
	st := &PruneState{Dir: ws}
	for _, dir := range sortedKeys(m.Clones) {
		p := filepath.Join(ws, dir)
		if !gitx.IsRepo(p) {
			continue
		}
		found, last, err := findRegen(git, dir, p)
		if err != nil {
			st.Held = append(st.Held, fmt.Sprintf("%s: cannot list its ignored files (%v) -- fix the clone, then run prune again", dir, err))
			continue
		}
		st.Found = append(st.Found, found...)
		if last.After(st.LastUsed) {
			st.LastUsed = last
		}
	}
	if idle > 0 && !st.LastUsed.IsZero() && now.Sub(st.LastUsed) < idle {
		st.Held = append(st.Held, fmt.Sprintf("used %s ago, within the idle window of %s -- wait, or pass --idle-days 0",
			roundDays(now.Sub(st.LastUsed)), roundDays(idle)))
	}
	st.Held = append(st.Held, liveHolders(ws, live)...)
	return st, nil
}

// liveHolders is what runs from ws, asked the way reap asks. Deleting a
// node_modules under a running dev server breaks it as surely as deleting the
// workspace would.
func liveHolders(ws string, live LiveChecker) []string {
	var out []string
	if live == nil {
		return nil
	}
	if live.Live(filepath.Base(ws)) {
		out = append(out, "a multiplexer session is attached -- end it first")
	}
	if lr, ok := live.(LiveRootChecker); ok {
		if roots, err := lr.LiveRoots(ws); err == nil {
			for _, r := range roots {
				out = append(out, fmt.Sprintf("%s -- stop it first", r))
			}
		}
	}
	if jc, ok := live.(JobChecker); ok {
		if jobs, err := jc.JobsUnder(ws); err == nil {
			for _, j := range jobs {
				out = append(out, fmt.Sprintf("%s -- unload the job first", j))
			}
		}
	}
	return out
}

// findRegen walks one clone for regenerable directories, and returns the
// newest activity it saw: git's index, HEAD and reflog, and the found
// directories themselves, which an install touches. FETCH_HEAD is left out:
// `facet ls --size` fetches every clone, and looking is not using.
func findRegen(git gitx.Runner, name, clone string) ([]Regen, time.Time, error) {
	ignored, err := ignoredDirs(git, clone)
	if err != nil {
		return nil, time.Time{}, err
	}
	var last time.Time
	for _, f := range []string{"index", "HEAD", filepath.Join("logs", "HEAD")} {
		last = newer(last, filepath.Join(clone, ".git", f))
	}
	var found []Regen
	err = filepath.WalkDir(clone, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() || path == clone {
			return nil
		}
		if d.Name() == ".git" || isCheckout(path) {
			return filepath.SkipDir // a nested repository or worktree: another checkout's
		}
		rebuild, ok := regenNames[d.Name()]
		if !ok {
			return nil
		}
		rel, err := filepath.Rel(clone, path)
		if err != nil || !underIgnored(ignored, filepath.ToSlash(rel)) {
			return filepath.SkipDir
		}
		last = newer(last, path)
		if n := freedBytes(path); n >= minRegenBytes {
			found = append(found, Regen{Clone: name, Rel: filepath.ToSlash(rel), Path: path, Rebuild: rebuild, Bytes: n})
		}
		return filepath.SkipDir
	})
	return found, last, err
}

// ignoredDirs lists the clone's wholly ignored directories. With --directory
// git names such a directory once, with a trailing slash, and lists files one
// by one in a directory holding a tracked file; so a directory is safe to
// delete when it, or a directory above it, is named.
func ignoredDirs(git gitx.Runner, clone string) (map[string]bool, error) {
	out, err := git.Run(clone, nil, "ls-files", "-z", "--others", "--ignored", "--exclude-standard", "--directory")
	if err != nil {
		return nil, err
	}
	ignored := map[string]bool{}
	for _, e := range strings.Split(out, "\x00") {
		if strings.HasSuffix(e, "/") {
			ignored[strings.TrimSuffix(e, "/")] = true
		}
	}
	return ignored, nil
}

func isCheckout(dir string) bool {
	_, err := os.Lstat(filepath.Join(dir, ".git"))
	return err == nil
}

// newer is the later of t and path's modification time.
func newer(t time.Time, path string) time.Time {
	if fi, err := os.Stat(path); err == nil && fi.ModTime().After(t) {
		return fi.ModTime()
	}
	return t
}

// underIgnored reports whether rel or a directory above it is wholly ignored.
func underIgnored(ignored map[string]bool, rel string) bool {
	for p := rel; p != "." && p != ""; p = pathDir(p) {
		if ignored[p] {
			return true
		}
	}
	return false
}

func pathDir(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[:i]
	}
	return ""
}

func roundDays(d time.Duration) string {
	days := int(d.Hours() / 24)
	if days == 0 {
		return "less than a day"
	}
	if days == 1 {
		return "1 day"
	}
	return fmt.Sprintf("%d days", days)
}

// Prune deletes what InspectPrune found, and refuses a held state. A directory
// that has turned into something else since it was found (a symlink, a file)
// is left alone: removing it would follow nothing, but it is no longer what
// was shown.
func Prune(s *PruneState) error {
	if len(s.Held) > 0 {
		return fmt.Errorf("%s is held (%s); prune refuses it", s.Dir, strings.Join(s.Held, "; "))
	}
	var errs []error
	for _, r := range s.Found {
		fi, err := os.Lstat(r.Path)
		if err != nil || !fi.IsDir() {
			continue
		}
		if err := os.RemoveAll(r.Path); err != nil {
			errs = append(errs, fmt.Errorf("%s/%s: %w", r.Clone, r.Rel, err))
		}
	}
	return errors.Join(errs...)
}
