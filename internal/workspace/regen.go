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
	// Kept names the directories of a listed name left alone, and why: they
	// hold a repository (`pip install -e git+...`, or hg+, svn+ or bzr+,
	// checks one out inside a .venv, and a fork is sometimes cloned into
	// node_modules to debug it), or nothing the clone tracks beside them
	// rebuilds them.
	Kept []string
	Held []string // why Prune leaves it alone; empty means it may prune
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
// nested repository or worktree, has a file that rebuilds it tracked beside
// it, and frees at least minRegenBytes. It holds the workspace when a clone
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
		found, kept, last, err := findRegen(git, dir, p)
		if err != nil {
			st.Held = append(st.Held, fmt.Sprintf("%s: cannot list its ignored or tracked files (%v) -- fix the clone, then run prune again", dir, err))
			continue
		}
		st.Found = append(st.Found, found...)
		st.Kept = append(st.Kept, kept...)
		used, err := lastGitUse(git, p)
		if err != nil {
			st.Held = append(st.Held, fmt.Sprintf("%s: cannot tell when it was last used (%v) -- prune holds what it cannot date", dir, err))
			continue
		}
		for _, t := range []time.Time{last, used} {
			if t.After(st.LastUsed) {
				st.LastUsed = t
			}
		}
	}
	if idle > 0 && now.Sub(st.LastUsed) < idle {
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
	if live.Live(filepath.Base(ws), ws) {
		out = append(out, fmt.Sprintf("tmux session %q runs in it -- end it first", filepath.Base(ws)))
	}
	if lr, ok := live.(LiveRootChecker); ok {
		roots, err := lr.LiveRoots(ws)
		if err != nil {
			out = append(out, fmt.Sprintf("could not list what runs from it (%v) -- prune holds what it cannot check", err))
		}
		for _, r := range roots {
			out = append(out, fmt.Sprintf("%s -- stop it first", r))
		}
	}
	if jc, ok := live.(JobChecker); ok {
		jobs, err := jc.JobsUnder(ws)
		if err != nil {
			out = append(out, fmt.Sprintf("could not read the loaded launchd jobs (%v) -- prune holds what it cannot check; run `launchctl list` to see why", err))
		}
		for _, j := range jobs {
			out = append(out, fmt.Sprintf("%s -- unload the job first", j))
		}
	}
	return out
}

// lastGitUse is the newest of the clone's index, HEAD and reflog, read from its
// own git directory, which for a worktree is not <clone>/.git. FETCH_HEAD is
// left out: `facet ls --size` fetches every clone, and looking is not using. A
// clone none of them can be read for is an error: prune holds what it cannot
// date.
func lastGitUse(git gitx.Runner, clone string) (time.Time, error) {
	out, err := git.Run(clone, nil, "rev-parse", "--path-format=absolute", "--git-dir")
	if err != nil {
		return time.Time{}, err
	}
	gitDir := strings.TrimSpace(out)
	var last time.Time
	for _, f := range []string{"index", "HEAD", filepath.Join("logs", "HEAD")} {
		last = newer(last, filepath.Join(gitDir, f))
	}
	if last.IsZero() {
		return last, fmt.Errorf("no index, HEAD or reflog in %s", gitDir)
	}
	return last, nil
}

// findRegen walks one clone for regenerable directories. It returns them, the
// ones kept because they hold a repository or nothing tracked rebuilds them,
// and the newest use it saw: the newest entry in each listed directory, since
// an install or an edit writes deep inside it, not at its top.
func findRegen(git gitx.Runner, name, clone string) (found []Regen, kept []string, last time.Time, err error) {
	ignored, err := ignoredDirs(git, clone)
	if err != nil {
		return nil, nil, time.Time{}, err
	}
	tracked := trackedBeside(git, clone)
	err = filepath.WalkDir(clone, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() || path == clone {
			return nil
		}
		if repoMarkers[d.Name()] || isCheckout(path) {
			return filepath.SkipDir // a nested repository or worktree: another checkout's
		}
		regen, ok := regenNames[d.Name()]
		if !ok {
			return nil
		}
		rel, err := filepath.Rel(clone, path)
		if err != nil || !underIgnored(ignored, filepath.ToSlash(rel)) {
			return filepath.SkipDir
		}
		rel = filepath.ToSlash(rel)
		last = newer(last, path)
		if repo, held := holdsRepository(path); held {
			kept = append(kept, fmt.Sprintf("%s/%s holds a repository at %s", name, rel, repo))
			return filepath.SkipDir
		}
		n, used := treeStats(path)
		if used.After(last) {
			last = used
		}
		if n < minRegenBytes {
			return filepath.SkipDir
		}
		rebuilt, err := tracked(pathDir(rel), regen)
		if err != nil {
			return err
		}
		if !rebuilt {
			kept = append(kept, keptUnbuilt(name, rel, regen))
			return filepath.SkipDir
		}
		found = append(found, Regen{Clone: name, Rel: rel, Path: path, Rebuild: regen.rebuild, Bytes: n})
		return filepath.SkipDir
	})
	return found, kept, last, err
}

// holdsRepository reports whether a repository of any kind (see
// marksRepository) sits anywhere under dir, and where. A tree that cannot be
// walked in full counts as holding one.
func holdsRepository(dir string) (string, bool) {
	var repo string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path != dir && marksRepository(filepath.Dir(path), d) {
			repo = path
			return fs.SkipAll
		}
		return nil
	})
	if err != nil {
		return dir + " (could not walk it: " + err.Error() + ")", true
	}
	return repo, repo != ""
}

// repoMarkers are the names, of a directory or a file, that mark a repository
// or a checkout of one. pip checks git, Mercurial, Subversion and Bazaar
// repositories out into a .venv; the rest are there because a name costs
// nothing to keep, and a repository missed is commits lost.
var repoMarkers = map[string]bool{
	".git":      true, // git; a file in a worktree or a submodule
	".hg":       true, // Mercurial
	".sl":       true, // Sapling
	".jj":       true, // Jujutsu, which needs no .git beside it
	".svn":      true, // Subversion
	".bzr":      true, // Bazaar and Breezy
	"_darcs":    true, // Darcs
	".pijul":    true, // Pijul
	"CVS":       true, // a CVS checkout
	"RCS":       true, // RCS keeps each file's history beside it
	"SCCS":      true, // and so does SCCS
	".fslckout": true, // a Fossil checkout
	"_FOSSIL_":  true, // a Fossil checkout, by its older name
}

// marksRepository reports whether the entry d, in the directory parent, marks
// a repository: a name in repoMarkers, a Fossil repository (one file, named
// *.fossil by convention), or the HEAD of a bare git repository, which has no
// .git, beside its objects and refs directories.
func marksRepository(parent string, d fs.DirEntry) bool {
	name := d.Name()
	switch {
	case repoMarkers[name], strings.HasSuffix(name, ".fossil"):
		return true
	case name == "HEAD" && !d.IsDir():
		return isDir(filepath.Join(parent, "objects")) && isDir(filepath.Join(parent, "refs"))
	}
	return false
}

func isDir(path string) bool {
	fi, err := os.Lstat(path)
	return err == nil && fi.IsDir()
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

// isCheckout reports whether dir is a repository or a checkout of one: an
// entry in it marks one. A directory that cannot be read is not called one
// here; the walk, or holdsRepository for a listed name, answers for it.
func isCheckout(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if marksRepository(dir, e) {
			return true
		}
	}
	return false
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
		if err := removeInClone(filepath.Join(s.Dir, r.Clone), r.Rel); err != nil {
			errs = append(errs, fmt.Errorf("%s/%s: %w", r.Clone, r.Rel, err))
		}
	}
	return errors.Join(errs...)
}

// removeInClone deletes rel inside clone, after checking that no step of the
// path from the clone down is a symlink: a parent swapped for a link after the
// inspection would otherwise point the delete outside the clone. The check and
// the delete are not one step, so a swap in the instant between them is not
// caught; os.Root would close that gap, but its Windows flaw (GO-2026-6604) is
// fixed only in a Go newer than this module's. A path that is no longer a
// directory is left alone, since it is not what was listed.
func removeInClone(clone, rel string) error {
	p := clone
	for _, seg := range strings.Split(rel, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("refusing the path %q", rel)
		}
		p = filepath.Join(p, seg)
		fi, err := os.Lstat(p)
		if err != nil {
			return nil
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s is now a symlink; left alone", p)
		}
		if !fi.IsDir() {
			return nil
		}
	}
	return os.RemoveAll(p)
}
