package workspace

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/bottega-cereghino/facet/internal/gitx"
)

// submoduleStates judges each initialised submodule of the clone at p,
// recursively, as a clone of its own: its commits, branches and stash live in
// the clone's git directory and go with it, while the superproject sees only
// the commit it records. Each is named dir/<path>. A listing that fails is
// recorded on a state of its own, so it blocks.
func submoduleStates(git gitx.Runner, dir, p string) []CloneState {
	if _, err := os.Stat(filepath.Join(p, ".gitmodules")); err != nil {
		return nil
	}
	out, err := git.Run(p, nil, "submodule", "foreach", "--recursive", "--quiet", `printf '%s\0' "$displaypath"`)
	if err != nil {
		return []CloneState{{Dir: dir, Unverifiable: []string{fmt.Sprintf("could not list its submodules: %v", err)}}}
	}
	var states []CloneState
	for _, rel := range strings.Split(out, "\x00") {
		if rel == "" {
			continue
		}
		rel = filepath.FromSlash(rel)
		states = append(states, inspectClone(git, filepath.Join(dir, rel), filepath.Join(p, rel), "", nil))
	}
	return states
}
