package workspace

import (
	"fmt"
	"os"
	"strings"

	"github.com/bottega-cereghino/facet/internal/gitx"
)

// gitOps are the files git keeps in a repository's (or a worktree's) git
// directory while an operation is stopped partway, and what each means. A
// rebase stopped at a break, an edit or a failing exec leaves a clean tree, an
// empty stash list and HEAD on pushed commits, while its autostash commit, the
// edits it moved away, is named by a file in rebase-merge and by no ref
// (#379). A merge's autostash and a cherry-pick's resolution are the same.
var gitOps = []struct{ path, what string }{
	{"rebase-merge", "a rebase"},
	{"rebase-apply", "a rebase or git am"},
	{"MERGE_HEAD", "a merge"},
	{"MERGE_AUTOSTASH", "a merge's autostash"},
	{"CHERRY_PICK_HEAD", "a cherry-pick"},
	{"REVERT_HEAD", "a revert"},
	{"sequencer", "a cherry-pick or revert sequence"},
}

// opsInProgress names the git operations stopped partway in the repository
// whose work tree is p.
func opsInProgress(git gitx.Runner, p string) ([]string, error) {
	args := []string{"rev-parse", "--path-format=absolute"}
	for _, op := range gitOps {
		args = append(args, "--git-path", op.path)
	}
	out, err := git.Run(p, nil, args...)
	if err != nil {
		return nil, err
	}
	paths := strings.Split(strings.TrimSpace(out), "\n")
	if len(paths) != len(gitOps) {
		return nil, fmt.Errorf("git rev-parse --git-path: unexpected output %q", out)
	}
	var ops []string
	for i, path := range paths {
		if _, err := os.Lstat(path); err == nil {
			ops = append(ops, gitOps[i].what)
		} else if !os.IsNotExist(err) {
			return nil, err
		}
	}
	return ops, nil
}
