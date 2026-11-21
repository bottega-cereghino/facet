package workspace

import (
	"path/filepath"
	"strings"
	"sync"

	"github.com/bottega-cereghino/facet/internal/gitx"
)

// fetchRemotes fetches every remote of the repository at dir with --prune, each
// bounded by fetchTimeout where the Runner supports it, and returns the ones
// that could not be fetched. The unpushed count subtracts every remote's refs,
// so each must be fresh: a branch pushed to a fork and deleted there since
// would otherwise still read as pushed. A remote list that cannot be read is
// an error.
func fetchRemotes(git gitx.Runner, dir string) (stale []string, err error) {
	if sf, ok := git.(*SharedFetch); ok {
		return sf.fetch(dir)
	}
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

// SharedFetch is a Runner that fetches each repository once, however many
// clones of it are inspected, and at the same moment from however many
// goroutines. Worktrees of one checkout share its refs, so fetches run in
// several of them at once collide ("reference already exists") and leave the
// count judged stale; and one fetch is all the refs need. Use one SharedFetch
// for one pass over many workspaces, not for longer: a later pass must fetch
// again.
type SharedFetch struct {
	gitx.Runner
	mu   sync.Mutex
	done map[string]*sharedFetch
}

type sharedFetch struct {
	once  sync.Once
	stale []string
	err   error
}

// NewSharedFetch wraps git so each repository is fetched once.
func NewSharedFetch(git gitx.Runner) *SharedFetch {
	return &SharedFetch{Runner: git, done: map[string]*sharedFetch{}}
}

// fetch fetches the repository dir belongs to, keyed by its common git
// directory, or returns what that fetch found. A directory whose common git
// directory cannot be read is fetched on its own.
func (s *SharedFetch) fetch(dir string) ([]string, error) {
	out, err := s.Run(dir, nil, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return fetchRemotes(s.Runner, dir)
	}
	key := filepath.Clean(strings.TrimSpace(out))
	s.mu.Lock()
	f, ok := s.done[key]
	if !ok {
		f = &sharedFetch{}
		s.done[key] = f
	}
	s.mu.Unlock()
	f.once.Do(func() { f.stale, f.err = fetchRemotes(s.Runner, dir) })
	return f.stale, f.err
}
