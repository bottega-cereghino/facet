package workspace

import (
	"path/filepath"
	"testing"

	"github.com/bottega-cereghino/facet/internal/gitx"
)

// An ignored file nothing rebuilds (a .env, a local config) is the clone's
// only copy, so it holds. Dependency folders and build output something
// rebuilds do not (#364, part 7).
func TestReapHoldsAnIgnoredFileNothingRebuilds(t *testing.T) {
	ws, clone := longLivedWorkspace(t, t.TempDir())
	excludeIn(t, clone, ".env")
	excludeIn(t, clone, "node_modules/")
	excludeIn(t, clone, "dist/")
	excludeIn(t, clone, "__pycache__/")
	for _, rebuildable := range []string{"node_modules/left-pad/index.js", "dist/app.js", "pkg/__pycache__/m.pyc"} {
		mkdirs(t, filepath.Dir(filepath.Join(clone, rebuildable)))
		writeTestFile(t, filepath.Join(clone, rebuildable), "rebuilt\n")
	}
	st, err := Inspect(ws, gitx.Git{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if b := st.Blockers(); len(b) != 0 {
		t.Fatalf("what a tool rebuilds holds: %v", b)
	}

	writeTestFile(t, filepath.Join(clone, ".env"), "TOKEN=only here\n")
	st, err = Inspect(ws, gitx.Git{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !hasBlocker(st, "files git ignores that nothing rebuilds: .env") {
		t.Fatalf("reap would delete the only copy of .env: %v", st.Blockers())
	}
}
