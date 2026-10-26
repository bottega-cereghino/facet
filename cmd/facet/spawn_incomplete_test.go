package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

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
