package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/RiccardoCereghino/facet/internal/knowledge"
	"github.com/RiccardoCereghino/facet/internal/manifest"
)

// A clone that failed leaves no repository behind, and Sync only warns about
// it. missingClones is what stops spawn announcing that workspace as ready
// (facet#170): it names every declared clone that is not a repository on disk,
// and nothing else.
func TestMissingClonesNamesWhatSyncCouldNotClone(t *testing.T) {
	ws := t.TempDir()
	m := &manifest.Manifest{Name: "w", Clones: map[string]string{
		"home":   "git@example.com:o/home.git",
		"absent": "git@example.com:o/absent.git",
		"empty":  "git@example.com:o/empty.git",
	}}
	if err := m.Write(ws); err != nil {
		t.Fatal(err)
	}
	if _, err := git.Run(ws, nil, "init", "-q", filepath.Join(ws, "home")); err != nil {
		t.Fatal(err)
	}
	// A directory left behind that is not a repository counts as missing too.
	if err := os.MkdirAll(filepath.Join(ws, "empty"), 0o777); err != nil {
		t.Fatal(err)
	}

	got, err := missingClones(ws)
	if err != nil {
		t.Fatalf("missingClones: %v", err)
	}
	if want := []string{"absent", "empty"}; !reflect.DeepEqual(got, want) {
		t.Errorf("missingClones = %v, want %v", got, want)
	}
}

// A fragment with no review date is stale and says so, instead of printing
// "reviewed " followed by nothing.
func TestStaleNoteNamesWhyAFragmentIsStale(t *testing.T) {
	now := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
	for reviewed, want := range map[string]string{
		"2026-10-01": "",
		"2026-01-01": "  (STALE -- reviewed 2026-01-01)",
		"":           "  (STALE -- never reviewed: no last_reviewed date)",
		"last week":  "  (STALE -- last_reviewed last week is not a YYYY-MM-DD date)",
	} {
		f := knowledge.Fragment{Meta: knowledge.Meta{LastReviewed: reviewed}}
		if got := staleNote(f, now); got != want {
			t.Errorf("staleNote(%q) = %q, want %q", reviewed, got, want)
		}
	}
}
