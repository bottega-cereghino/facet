package workspace

import (
	"strings"

	"github.com/bottega-cereghino/facet/internal/gitx"
)

// fetchRemotes fetches every remote of the repository at dir with --prune, each
// bounded by fetchTimeout where the Runner supports it, and returns the ones
// that could not be fetched. The unpushed count subtracts every remote's refs,
// so each must be fresh: a branch pushed to a fork and deleted there since
// would otherwise still read as pushed. A remote list that cannot be read is
// an error.
func fetchRemotes(git gitx.Runner, dir string) (stale []string, err error) {
	out, err := git.Run(dir, nil, "remote")
	if err != nil {
		return nil, err
	}
	for _, name := range strings.Fields(out) {
		args := []string{"fetch", "--prune", "--", name}
		if tr, ok := git.(timeoutRunner); ok {
			_, err = tr.RunTimeout(dir, nil, fetchTimeout, args...)
		} else {
			_, err = git.Run(dir, nil, args...)
		}
		if err != nil {
			stale = append(stale, name)
		}
	}
	return stale, nil
}
