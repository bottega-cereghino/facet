package scope

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The bytes below are the format, copied from workspaces that already carry
// these files and read back with `od -c`. They are asserted as literals rather
// than rebuilt from marshalScope, because a test that renders the file the same
// way the code does agrees with the code by construction and would not notice a
// format change -- and a separate program reads these files.
const wantScope = "owner/repo#12\nacme/tools#7\n"

func TestWriteProducesTheOnDiskFormat(t *testing.T) {
	ws := t.TempDir()
	scope := []Ref{{Repo: "owner/repo", Number: 12}, {Repo: "acme/tools", Number: 7}}
	if err := Write(ws, scope); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(ws, ScopeFile))
	if err != nil {
		t.Fatalf("read %s: %v", ScopeFile, err)
	}
	if string(got) != wantScope {
		t.Errorf("%s = %q, want %q", ScopeFile, got, wantScope)
	}
}

// A workspace may cover no single issue. That has to be the absence of a file rather than an empty one: a reader
// treats "no scope recorded" as nothing to check, and an empty file is a scope
// that permits nothing, which would need an exemption to work around.
func TestWriteWithoutScopeLeavesNoScopeFile(t *testing.T) {
	ws := t.TempDir()
	if err := Write(ws, nil); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := os.Stat(filepath.Join(ws, ScopeFile)); !os.IsNotExist(err) {
		t.Errorf("stat %s: err = %v, want IsNotExist", ScopeFile, err)
	}
	got, err := ReadScope(ws)
	if err != nil || got != nil {
		t.Errorf("ReadScope of a workspace with no scope = %v, %v; want nil, nil", got, err)
	}
}

func TestParseRef(t *testing.T) {
	tests := []struct {
		in      string
		want    Ref
		wantErr bool
	}{
		{"owner/repo#1", Ref{Repo: "owner/repo", Number: 1}, false},
		{"acme/some-tool#4321", Ref{Repo: "acme/some-tool", Number: 4321}, false},
		{"  owner/repo#2  ", Ref{Repo: "owner/repo", Number: 2}, false},

		{"owner/repo", Ref{}, true},   // no number at all
		{"#7", Ref{}, true},           // no repo
		{"repo#7", Ref{}, true},       // unqualified: a reader cannot resolve it
		{"owner/#7", Ref{}, true},     // half a repo
		{"/repo#7", Ref{}, true},      //
		{"owner/repo#0", Ref{}, true}, // issue numbers start at 1
		{"owner/repo#-3", Ref{}, true},
		{"owner/repo#seven", Ref{}, true},
		{"owner/repo#", Ref{}, true},
		{"own er/repo#7", Ref{}, true},
		{"", Ref{}, true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := ParseRef(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseRef(%q) = %v, %v; wantErr %v", tt.in, got, err, tt.wantErr)
			}
			if err == nil && got != tt.want {
				t.Errorf("ParseRef(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestParseRefsDropsDuplicatesAndKeepsOrder(t *testing.T) {
	got, err := ParseRefs([]string{"owner/repo#12", "acme/tools#7", "owner/repo#12"})
	if err != nil {
		t.Fatalf("ParseRefs: %v", err)
	}
	want := []Ref{{Repo: "owner/repo", Number: 12}, {Repo: "acme/tools", Number: 7}}
	if len(got) != len(want) {
		t.Fatalf("ParseRefs = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("ParseRefs[%d] = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestAppendScopeIsAdditiveAndIdempotent(t *testing.T) {
	ws := t.TempDir()
	if err := Write(ws, []Ref{{Repo: "owner/repo", Number: 12}}); err != nil {
		t.Fatalf("Write: %v", err)
	}

	added, err := AppendScope(ws, []Ref{{Repo: "acme/tools", Number: 7}})
	if err != nil {
		t.Fatalf("AppendScope: %v", err)
	}
	if len(added) != 1 || added[0].String() != "acme/tools#7" {
		t.Errorf("AppendScope reported %v as new, want [acme/tools#7]", added)
	}

	// Again, with one already present and one new: only the new one is added,
	// and the file gains exactly one line.
	added, err = AppendScope(ws, []Ref{{Repo: "acme/tools", Number: 7}, {Repo: "owner/repo", Number: 99}})
	if err != nil {
		t.Fatalf("AppendScope: %v", err)
	}
	if len(added) != 1 || added[0].String() != "owner/repo#99" {
		t.Errorf("AppendScope reported %v as new, want [owner/repo#99]", added)
	}

	got, err := os.ReadFile(filepath.Join(ws, ScopeFile))
	if err != nil {
		t.Fatal(err)
	}
	want := "owner/repo#12\nacme/tools#7\nowner/repo#99\n"
	if string(got) != want {
		t.Errorf("%s = %q, want %q", ScopeFile, got, want)
	}
}

// AppendScope is the one entry point that can meet a scope file somebody edited
// by hand. It must refuse rather than silently drop the line it cannot read.
func TestAppendScopeRefusesAnUnreadableFile(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, ScopeFile), []byte("owner/repo#12\nnot a reference\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	if _, err := AppendScope(ws, []Ref{{Repo: "acme/tools", Number: 7}}); err == nil {
		t.Fatal("AppendScope accepted a scope file with an unparseable line")
	} else if !strings.Contains(err.Error(), "line 2") {
		t.Errorf("error %q does not say which line is wrong", err)
	}
}

// RemoveScope is add's opposite (facet#112): a boundary that could only
// widen was a ratchet, and a wrong entry was permanent. This is the basic
// shape -- drop one, keep the rest, report which was actually removed.
func TestRemoveScopeDropsOnlyWhatWasAsked(t *testing.T) {
	ws := t.TempDir()
	if err := Write(ws, []Ref{
		{Repo: "owner/repo", Number: 12},
		{Repo: "acme/tools", Number: 7},
	}); err != nil {
		t.Fatalf("Write: %v", err)
	}

	removed, remaining, err := RemoveScope(ws, []Ref{{Repo: "acme/tools", Number: 7}})
	if err != nil {
		t.Fatalf("RemoveScope: %v", err)
	}
	if len(removed) != 1 || removed[0].String() != "acme/tools#7" {
		t.Errorf("removed = %v, want [acme/tools#7]", removed)
	}
	if len(remaining) != 1 || remaining[0].String() != "owner/repo#12" {
		t.Errorf("remaining = %v, want [owner/repo#12]", remaining)
	}

	got, err := ReadScope(ws)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].String() != "owner/repo#12" {
		t.Errorf("scope on disk = %v, want [owner/repo#12]", got)
	}
}

// Removing something never present is a no-op, matching AppendScope's
// idempotence in the other direction.
func TestRemoveScopeOfAnAbsentEntryIsANoOp(t *testing.T) {
	ws := t.TempDir()
	if err := Write(ws, []Ref{{Repo: "owner/repo", Number: 12}}); err != nil {
		t.Fatalf("Write: %v", err)
	}

	removed, remaining, err := RemoveScope(ws, []Ref{{Repo: "acme/tools", Number: 7}})
	if err != nil {
		t.Fatalf("RemoveScope: %v", err)
	}
	if len(removed) != 0 {
		t.Errorf("removed = %v, want none", removed)
	}
	if len(remaining) != 1 || remaining[0].String() != "owner/repo#12" {
		t.Errorf("remaining = %v, want [owner/repo#12] unchanged", remaining)
	}
}

// Removing the last entry deletes .scope rather than leaving an empty file --
// the same "absent means nothing recorded" rule Write follows.
func TestRemoveScopeToEmptyDeletesTheFile(t *testing.T) {
	ws := t.TempDir()
	if err := Write(ws, []Ref{{Repo: "owner/repo", Number: 12}}); err != nil {
		t.Fatalf("Write: %v", err)
	}

	if _, _, err := RemoveScope(ws, []Ref{{Repo: "owner/repo", Number: 12}}); err != nil {
		t.Fatalf("RemoveScope: %v", err)
	}
	if _, err := os.Stat(filepath.Join(ws, ScopeFile)); !os.IsNotExist(err) {
		t.Errorf("stat %s = %v, want IsNotExist", ScopeFile, err)
	}
	got, err := ReadScope(ws)
	if err != nil {
		t.Fatalf("ReadScope after delete: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("ReadScope = %v, want none", got)
	}
}

// SetScope always writes, even when the new value equals the old -- it is an
// assignment, not a merge -- and reports the previous value the way tree
// wire reports the parent an edge moved a child away from.
func TestSetScopeReplacesAndReportsThePrevious(t *testing.T) {
	ws := t.TempDir()
	if err := Write(ws, []Ref{
		{Repo: "owner/repo", Number: 12},
		{Repo: "acme/tools", Number: 7},
	}); err != nil {
		t.Fatalf("Write: %v", err)
	}

	previous, err := SetScope(ws, []Ref{{Repo: "acme/tools", Number: 99}})
	if err != nil {
		t.Fatalf("SetScope: %v", err)
	}
	if len(previous) != 2 {
		t.Errorf("previous = %v, want the 2 original entries", previous)
	}

	got, err := ReadScope(ws)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].String() != "acme/tools#99" {
		t.Errorf("scope on disk = %v, want [acme/tools#99]", got)
	}
}

// SetScope with no refs clears the scope entirely, deleting the file --
// the documented way to empty a workspace's scope rather than hand-editing.
func TestSetScopeToNothingDeletesTheFile(t *testing.T) {
	ws := t.TempDir()
	if err := Write(ws, []Ref{{Repo: "owner/repo", Number: 12}}); err != nil {
		t.Fatalf("Write: %v", err)
	}

	if _, err := SetScope(ws, nil); err != nil {
		t.Fatalf("SetScope: %v", err)
	}
	if _, err := os.Stat(filepath.Join(ws, ScopeFile)); !os.IsNotExist(err) {
		t.Errorf("stat %s = %v, want IsNotExist", ScopeFile, err)
	}
}

func TestReadScopeIgnoresBlankLines(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, ScopeFile), []byte("owner/repo#12\n\n  \nacme/tools#7\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	got, err := ReadScope(ws)
	if err != nil {
		t.Fatalf("ReadScope: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("ReadScope = %v, want 2 entries", got)
	}
}

// The read-back exists because a write that reports success and does not land is
// this codebase's most repeated failure. A read-back nobody has seen catch
// anything is decoration, so this hands it a file whose contents are not what
// was asked for and requires it to say so.
func TestVerifyCatchesAWriteThatDidNotLand(t *testing.T) {
	p := filepath.Join(t.TempDir(), ScopeFile)
	if err := os.WriteFile(p, []byte("w-someone-else\n"), 0o666); err != nil {
		t.Fatal(err)
	}

	err := verify(p, []byte("w-example-12\n"))
	if err == nil {
		t.Fatal("verify passed on a file that does not hold what was written")
	}
	// It has to name both, or the reader cannot tell which end is wrong.
	for _, want := range []string{"w-someone-else", "w-example-12"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}

	if err := verify(p, []byte("w-someone-else\n")); err != nil {
		t.Errorf("verify failed on a file that does hold what was written: %v", err)
	}
}

func TestVerifyReportsAMissingFile(t *testing.T) {
	if err := verify(filepath.Join(t.TempDir(), "absent"), []byte("x\n")); err == nil {
		t.Fatal("verify passed on a file that does not exist")
	}
}
