package workspace

import (
	"fmt"
	"strings"

	"github.com/bottega-cereghino/facet/internal/gitx"
)

// notLocal are the ref namespaces reap does not count as local work. Every
// other ref is local, whatever its namespace, so a commit only it reaches
// holds: the branches, refs/stash, the per-worktree refs, refs/replace, and
// any namespace git or a tool adds later (#364). A list of what to count
// would miss the next one, as it missed refs/rewritten (#361).
//
// refs/remotes is what other repositories have, and only a counted remote's
// part of it proves anything pushed (countedRemotes). Tags and notes are
// listed under what reap does not protect in the README. refs/stash is local,
// but the stash is judged on its own, entry by entry (Stashed), and the ref
// names only the newest entry; counting it too would report the stash's
// commits as unpushed ones. A name ending in a slash is a namespace, any
// other is one ref.
var notLocal = []string{"refs/remotes/", "refs/tags/", "refs/notes/", "refs/stash"}

// perWorktreeRefs are the namespaces git keeps per worktree, in its own admin
// directory: refs/worktree, refs/bisect, and refs/rewritten, where an
// interrupted `rebase --rebase-merges` keeps its labels. A commit only one of
// them reaches goes with the worktree.
var perWorktreeRefs = []string{"refs/worktree", "refs/bisect", "refs/rewritten"}

// localRefs is the set of refs whose commits a repository would lose.
type localRefs struct {
	// countArgs name the refs as rev-list arguments, for the fast count.
	countArgs []string
	// names are the same refs one by one, as full names, with "HEAD" when
	// HEAD's branch is not one of them, for the per-ref landing walk.
	names []string
	// odd are refs that point at a tree or a blob: they hold no commit to
	// count, and reap cannot tell whether a remote has what they keep, so
	// each one holds, as the fix to print.
	odd []string
}

// walkedRefs lists the local refs of the repository at dir. own is false for
// a linked worktree: its branches and other shared refs are the checkout's
// and outlive it, so only HEAD and the per-worktree refs go with it.
//
// The count and the walk must name the same refs, or a commit the count saw
// would be judged by nothing; both come from notLocal and perWorktreeRefs.
// --single-worktree keeps --all off the other worktrees' HEADs: each linked
// worktree of a clone holds it on its own (linkedWorktrees).
func walkedRefs(git gitx.Runner, dir string, own bool) (localRefs, error) {
	var r localRefs
	var patterns []string
	if own {
		r.countArgs = []string{"HEAD", "--single-worktree"}
		for _, ns := range notLocal {
			if strings.HasSuffix(ns, "/") {
				ns += "*"
			}
			r.countArgs = append(r.countArgs, "--exclude="+ns)
		}
		r.countArgs = append(r.countArgs, "--all")
		patterns = []string{"refs"}
	} else {
		r.countArgs = []string{"HEAD"}
		for _, ns := range perWorktreeRefs {
			r.countArgs = append(r.countArgs, "--glob="+ns+"/*")
		}
		patterns = perWorktreeRefs
	}

	out, err := git.Run(dir, nil, append([]string{"for-each-ref", "--format=%(objecttype) %(*objecttype) %(refname)"}, patterns...)...)
	if err != nil {
		return localRefs{}, err
	}
	head, _, err := currentRef(git, dir)
	if err != nil {
		return localRefs{}, err
	}
	listed := false
	for _, line := range strings.Split(out, "\n") {
		kind, rest, _ := strings.Cut(line, " ")
		peeled, name, _ := strings.Cut(rest, " ")
		if name == "" || !isLocal(name) {
			continue
		}
		if kind != "commit" && peeled != "commit" {
			if kind == "tag" {
				kind = "tag of a " + peeled
			}
			r.odd = append(r.odd, fmt.Sprintf("%s points at a %s, not a commit, so reap cannot tell whether a remote has it; if it is not wanted, run `git update-ref -d %s`, then reap again", name, kind, name))
			continue
		}
		r.names = append(r.names, name)
		listed = listed || name == head
	}
	// HEAD is walked as itself when it is detached, or when it names a ref
	// not listed: in a linked worktree its branch is the checkout's, which
	// only HEAD's own commits are counted through.
	if !listed {
		r.names = append(r.names, "HEAD")
	}
	return r, nil
}

// isLocal reports whether the full ref name is local work (notLocal).
func isLocal(name string) bool {
	for _, ns := range notLocal {
		if name == ns || (strings.HasSuffix(ns, "/") && strings.HasPrefix(name, ns)) {
			return false
		}
	}
	return true
}
