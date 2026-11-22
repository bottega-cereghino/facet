package workspace

import (
	"reflect"
	"testing"
)

// HoldKinds names each kind of hold once, in a fixed order, from the state
// Blockers reads; nothing held means no kinds.
func TestHoldKinds(t *testing.T) {
	if k := (&IssueState{}).HoldKinds(); len(k) != 0 {
		t.Errorf("an empty state has kinds %v", k)
	}
	st := &IssueState{
		Clone: []CloneState{
			{Dir: "a", Dirty: true, IgnoredFiles: []string{".env"}},
			{Dir: "b", Unpushed: 2, Hidden: []string{"x differs"}},
		},
		LiveRoots: []string{"process sleep (pid 1)"},
	}
	want := []string{"unpushed commits", "ignored files nothing rebuilds", "uncommitted changes", "something running"}
	if got := st.HoldKinds(); !reflect.DeepEqual(got, want) {
		t.Errorf("HoldKinds() = %v, want %v", got, want)
	}
	if got := st.IgnoredPaths(); !reflect.DeepEqual(got, []string{"a/.env"}) {
		t.Errorf("IgnoredPaths() = %v", got)
	}
	if len(st.Blockers()) == 0 {
		t.Error("a state with kinds has no blockers")
	}
	stopped := &IssueState{Clone: []CloneState{{Dir: "a", InProgress: []string{"a rebase"}}}}
	if got := stopped.HoldKinds(); !reflect.DeepEqual(got, []string{"uncommitted changes"}) {
		t.Errorf("a rebase stopped partway is kind %v, want uncommitted changes", got)
	}
}
