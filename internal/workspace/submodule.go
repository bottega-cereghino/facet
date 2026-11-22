package workspace

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/bottega-cereghino/facet/internal/gitx"
)

// submoduleStates judges every repository the clone at p holds besides its
// own, recursively, each as a clone of its own: their commits, branches and
// stash go with the clone, while the clone sees at most the commit it records.
// Each is named dir/<path>. Two places hold them:
//
//   - its work tree, where git does not look inside a directory holding a
//     .git: a checked-out submodule, a repository added with `git add` and no
//     .gitmodules (a gitlink, git's "embedded repository"), or one sitting in
//     a directory the clone tracks files in;
//   - its git directory's modules, where a submodule's repository stays when
//     its work tree goes: switched to a commit without it, or deinitialised.
//
// Every other repository in the work tree holds, whatever it is
// (unjudgedRepos). A listing that fails is recorded on a state of its own, so
// it blocks.
func submoduleStates(git gitx.Runner, dir, p, gone string) []CloneState {
	n := &innerRepos{git: git, gone: gone, seen: map[string]bool{}}
	if gd, err := absGitDir(git, p); err == nil {
		n.seen[gd] = true
	}
	n.workTree(dir, p)
	return n.states
}

// innerRepos collects the states of the repositories one clone holds. seen holds
// the real git directories already judged, so a checked-out submodule is not
// judged again from the modules directory.
type innerRepos struct {
	git    gitx.Runner
	gone   string // the directory reap deletes
	seen   map[string]bool
	states []CloneState
}

func (n *innerRepos) fail(name, format string, a ...any) {
	n.states = append(n.states, CloneState{Dir: name, Unverifiable: []string{fmt.Sprintf(format, a...)}})
}

// workTree judges the repositories inside the work tree p of the repository
// named name, then those in its modules directory.
func (n *innerRepos) workTree(name, p string) {
	rels, err := gitEntryDirs(n.git, p)
	if err != nil {
		n.fail(name, "could not list the repositories inside it: %v", err)
	}
	for _, rel := range rels {
		sub := filepath.Join(p, filepath.FromSlash(rel))
		if _, err := os.Lstat(filepath.Join(sub, ".git")); err != nil {
			continue // not checked out: its repository, if any, is in modules
		}
		subName := filepath.Join(name, filepath.FromSlash(rel))
		gd, err := absGitDir(n.git, sub)
		if err != nil {
			n.fail(subName, "holds a .git that git cannot read as a repository of its own (%v); push what it holds, or move it out of the clone", err)
			continue
		}
		if n.seen[gd] {
			continue
		}
		n.seen[gd] = true
		n.states = append(n.states, inspectClone(n.git, subName, sub, n.gone, "", nil))
		n.workTree(subName, sub)
	}

	out, err := n.git.Run(p, nil, "rev-parse", "--path-format=absolute", "--git-path", "modules")
	if err != nil {
		n.fail(name, "could not find its submodules' repositories: %v", err)
		return
	}
	n.modules(name, p, strings.TrimSpace(out))
}

// modules judges each repository under the modules directory mods that no
// work tree was found for, and the modules each of those holds. clone is the
// work tree the repositories' locations are named from.
func (n *innerRepos) modules(name, clone, mods string) {
	repos, err := gitDirsUnder(mods)
	if err != nil {
		n.fail(name, "could not read its submodules' repositories in %s: %v", mods, err)
	}
	for _, g := range repos {
		resolved := realPath(g)
		if n.seen[resolved] {
			continue
		}
		n.seen[resolved] = true
		rel, _ := filepath.Rel(mods, g)
		where := g
		if r, err := filepath.Rel(clone, g); err == nil && !strings.HasPrefix(r, "..") {
			where = filepath.Join(name, r)
		}
		label := fmt.Sprintf("%s (not checked out; its repository is %s)", filepath.Join(name, rel), where)
		n.states = append(n.states, inspectGitDir(n.git, label, g, n.gone))
		n.modules(filepath.Join(name, rel), clone, filepath.Join(g, "modules"))
	}
}

// gitEntryDirs lists, relative to the work tree p and slash-separated, every
// gitlink in its index and every directory holding a file it tracks: the
// places a repository can sit inside a clone with neither git status nor the
// ignored-directory walk seeing it. Sorted, so a repository comes before the
// directories inside it.
func gitEntryDirs(git gitx.Runner, p string) ([]string, error) {
	idx, err := readIndex(git, p)
	if err != nil {
		return nil, err
	}
	dirs := map[string]bool{}
	for d := range idx.dirs {
		dirs[d] = true
	}
	for g := range idx.gitlinks {
		dirs[g] = true
	}
	return sortedKeys(dirs), nil
}

// gitDirsUnder finds the git directories under mods, a modules directory: one
// per submodule name, which may hold slashes. A missing directory holds none.
// Nothing inside a git directory is walked; its own modules are the caller's.
func gitDirsUnder(mods string) ([]string, error) {
	if _, err := os.Lstat(mods); os.IsNotExist(err) {
		return nil, nil
	}
	var out []string
	err := filepath.WalkDir(mods, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() || p == mods {
			return nil
		}
		if isGitDir(p) {
			out = append(out, p)
			return filepath.SkipDir
		}
		return nil
	})
	return out, err
}

// isGitDir reports whether dir looks like a git directory: a HEAD file and an
// objects directory.
func isGitDir(dir string) bool {
	head, err := os.Stat(filepath.Join(dir, "HEAD"))
	if err != nil || !head.Mode().IsRegular() {
		return false
	}
	objects, err := os.Stat(filepath.Join(dir, "objects"))
	return err == nil && objects.IsDir()
}

// absGitDir is the real git directory of the repository whose work tree is p.
// A p that git reads as part of a repository above it is an error.
func absGitDir(git gitx.Runner, p string) (string, error) {
	out, err := git.Run(p, nil, "rev-parse", "--show-toplevel", "--absolute-git-dir")
	if err != nil {
		return "", err
	}
	top, gd, _ := strings.Cut(strings.TrimSpace(out), "\n")
	if realPath(top) != realPath(p) {
		return "", fmt.Errorf("git reads it as part of %s", top)
	}
	return realPath(gd), nil
}

// inspectGitDir judges a repository that has no work tree, at its git
// directory g: a submodule switched away from or deinitialised. It has no
// files to be dirty; its commits, linked worktrees and stash are judged as a
// clone's, against its remotes fetched first.
func inspectGitDir(git gitx.Runner, name, g, gone string) CloneState {
	c := CloneState{Dir: name}
	if stale, err := fetchRemotes(git, g); err != nil {
		c.Unverifiable = append(c.Unverifiable, fmt.Sprintf("could not list its remotes: %v", err))
	} else {
		c.StaleRemotes = stale
	}
	if out, err := git.Run(g, nil, "worktree", "list", "--porcelain"); err != nil {
		c.Unverifiable = append(c.Unverifiable, fmt.Sprintf("git worktree list failed: %v", err))
	} else {
		c.Worktrees = linkedWorktrees(out)
	}

	pushed, err := countedRemotes(git, g, gone)
	if err != nil {
		c.Unverifiable = append(c.Unverifiable, fmt.Sprintf("could not tell which remotes hold its commits: %v", err))
	}
	if holds, err := gitDirHolds(git, g, gone); err != nil {
		c.Unverifiable = append(c.Unverifiable, fmt.Sprintf("could not read its git directory: %v", err))
	} else {
		c.Unverifiable = append(c.Unverifiable, holds...)
	}
	refs, err := walkedRefs(git, g, true)
	if err != nil {
		c.Unverifiable = append(c.Unverifiable, fmt.Sprintf("could not list its refs: %v", err))
	}
	c.Unverifiable = append(c.Unverifiable, refs.odd...)
	countArgs := append(append([]string{"rev-list", "--count"}, refs.countArgs...), pushed.not()...)
	if out, err := git.Run(g, nil, countArgs...); err != nil {
		c.Unverifiable = append(c.Unverifiable, fmt.Sprintf("git rev-list failed: %v", err))
	} else if count, err := strconv.Atoi(strings.TrimSpace(out)); err != nil {
		c.Unverifiable = append(c.Unverifiable, fmt.Sprintf("could not read the unpushed-commit count: %v", err))
	} else if count > 0 {
		if unpushed, landed, defUnresolved, err := unpushedLanding(git, g, "", nil, refs.names, pushed); err != nil {
			c.Unverifiable = append(c.Unverifiable, fmt.Sprintf("could not verify unpushed commits: %v", err))
		} else {
			c.Unpushed = unpushed
			c.SquashLanded = landed
			c.DefaultBranchUnresolved = defUnresolved
		}
	}

	if count, err := gitDirStash(git, g); err != nil {
		c.Unverifiable = append(c.Unverifiable, fmt.Sprintf("could not count its stash: %v", err))
	} else {
		c.Stashed = count
	}
	return c
}

// gitDirStash counts the stash entries of the repository at g, a work tree or
// a git directory (git stash list needs a work tree). The stash is the reflog
// of refs/stash, but the ref outlives its reflog: `git reflog expire --all`
// empties the reflog and leaves refs/stash naming the newest entry, which
// `git stash list` then never prints (#378). So refs/stash is at least one
// entry.
func gitDirStash(git gitx.Runner, g string) (int, error) {
	out, err := git.Run(g, nil, "for-each-ref", "refs/stash")
	if err != nil || strings.TrimSpace(out) == "" {
		return 0, err
	}
	out, err = git.Run(g, nil, "rev-list", "--walk-reflogs", "--count", "refs/stash", "--")
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(out))
	return max(n, 1), err
}
