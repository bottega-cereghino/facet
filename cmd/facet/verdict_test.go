package main

import (
	"testing"

	"github.com/bottega-cereghino/facet/internal/workspace"
)

func TestReapVerdictSaysWhatItDidNotCheck(t *testing.T) {
	clean := &workspace.IssueState{}
	stale := &workspace.IssueState{Clone: []workspace.CloneState{{StaleRemotes: []string{"origin"}}}}
	for _, c := range []struct {
		st      *workspace.IssueState
		offline bool
		want    string
	}{
		{clean, false, "reapable"},
		{clean, true, "reapable offline (PR unchecked)"},
		{stale, false, "held (1)"},
		{stale, true, "held (1)"},
	} {
		if got := reapVerdict(c.st, c.offline); got != c.want {
			t.Errorf("reapVerdict(offline=%v) = %q, want %q", c.offline, got, c.want)
		}
	}
}
