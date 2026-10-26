// Package scope reads and writes .scope: the issues a workspace covers, one
// owner/repo#n per line, each newline-terminated. `facet spawn` writes it with
// the issue the workspace was created for first; `facet scope` reads and amends
// it. A workspace with no .scope has no recorded scope, which is a normal state.
package scope

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ScopeFile is the file name, relative to a workspace root.
const ScopeFile = ".scope"

// Ref is one issue a workspace covers, in the owner/repo#n form the file uses.
type Ref struct {
	Repo   string // owner/name
	Number int
}

// String renders the canonical form, owner/repo#n, which round-trips through
// ParseRef.
func (r Ref) String() string {
	return fmt.Sprintf("%s#%d", r.Repo, r.Number)
}

// refFix is the fix line every ParseRef refusal ends with.
const refFix = "fix: write it as owner/repo#123"

// ParseRef reads one scope entry. It is strict about the form because the file
// is a wire contract with a separate reader: an entry that is nearly right is
// worse than one that is refused, since the reader will simply not match it and
// the workspace will look out of scope for work it was created to do.
func ParseRef(s string) (Ref, error) {
	t := strings.TrimSpace(s)

	repo, num, ok := strings.Cut(t, "#")
	if !ok {
		return Ref{}, fmt.Errorf("scope entry %q has no '#'\n%s", s, refFix)
	}
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" {
		return Ref{}, fmt.Errorf("scope entry %q does not name owner/repo before the '#'\n%s", s, refFix)
	}
	if strings.ContainsAny(repo, " \t") {
		return Ref{}, fmt.Errorf("scope entry %q has whitespace inside owner/repo\n%s", s, refFix)
	}
	n, err := strconv.Atoi(num)
	if err != nil {
		return Ref{}, fmt.Errorf("scope entry %q: %q is not an issue number\n%s", s, num, refFix)
	}
	if n < 1 {
		return Ref{}, fmt.Errorf("scope entry %q: issue numbers start at 1\n%s", s, refFix)
	}
	return Ref{Repo: repo, Number: n}, nil
}

// ParseRefs reads several entries, reporting the first that is wrong. Duplicates
// are dropped, keeping the first occurrence: the order is meaningful (the issue
// a workspace was created for comes first) and repeating an entry is a slip
// rather than a request for two lines.
func ParseRefs(in []string) ([]Ref, error) {
	var out []Ref
	for _, s := range in {
		r, err := ParseRef(s)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return Dedupe(out), nil
}

// Dedupe keeps the first occurrence of each reference. Order is meaningful --
// the issue a workspace was created for leads -- so this cannot sort.
func Dedupe(in []Ref) []Ref {
	var out []Ref
	seen := map[string]bool{}
	for _, r := range in {
		if seen[r.String()] {
			continue
		}
		seen[r.String()] = true
		out = append(out, r)
	}
	return out
}

// Join renders references for a person to read on one line.
func Join(refs []Ref) string {
	s := make([]string, len(refs))
	for i, r := range refs {
		s[i] = r.String()
	}
	return strings.Join(s, ", ")
}

// Write records the issues a workspace covers. An empty scope writes no file
// at all rather than an empty one: absent means "no recorded scope".
func Write(workspaceDir string, scope []Ref) error {
	if len(scope) == 0 {
		return nil
	}
	return writeAndVerify(filepath.Join(workspaceDir, ScopeFile), marshalScope(scope))
}

// marshalScope renders the file: one reference per line, every line terminated.
// A terminated last line is what is on disk and what a line-at-a-time reader
// expects; it also makes appending a matter of writing more bytes.
func marshalScope(scope []Ref) []byte {
	var b bytes.Buffer
	for _, r := range scope {
		fmt.Fprintf(&b, "%s\n", r)
	}
	return b.Bytes()
}

// writeAndVerify writes the file and then reads it back, failing when the bytes
// on disk are not the bytes that were asked for.
//
// This is not defensive padding. A write that reports success and does not land
// is the failure this codebase has hit most often, in enough different tools
// that treating a nil error as evidence is no longer reasonable: a full disk, a
// path that resolved somewhere unexpected, another process writing the same
// file. The check costs one read of a file measured in bytes.
func writeAndVerify(path string, want []byte) error {
	if err := os.WriteFile(path, want, 0o666); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return verify(path, want)
}

// verify is the half of writeAndVerify that can be shown failing without having
// to arrange a filesystem that lies, which is why it is its own function.
func verify(path string, want []byte) error {
	got, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read back %s: %w", path, err)
	}
	if !bytes.Equal(got, want) {
		return fmt.Errorf("%s read back as %q after writing %q; the write reported success and did not land\n"+
			"fix: check the filesystem this workspace is on, then create it again", path, got, want)
	}
	return nil
}

// ReadScope returns the issues a workspace covers. No file means no scope
// recorded, which is not an error -- see Write.
func ReadScope(workspaceDir string) ([]Ref, error) {
	b, err := os.ReadFile(filepath.Join(workspaceDir, ScopeFile))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Ref
	for i, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		r, err := ParseRef(line)
		if err != nil {
			return nil, fmt.Errorf("%s line %d: %w", filepath.Join(workspaceDir, ScopeFile), i+1, err)
		}
		out = append(out, r)
	}
	return out, nil
}

// AppendScope adds issues to a workspace's scope, and reports which were new. It
// exists because a workspace is sometimes handed a second issue after it
// was created.
//
// Adding a reference already present is a no-op rather than a second line, so
// running it twice is safe.
func AppendScope(workspaceDir string, add []Ref) ([]Ref, error) {
	have, err := ReadScope(workspaceDir)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, r := range have {
		seen[r.String()] = true
	}
	var added []Ref
	for _, r := range add {
		if seen[r.String()] {
			continue
		}
		seen[r.String()] = true
		have = append(have, r)
		added = append(added, r)
	}
	if len(added) == 0 {
		return nil, nil
	}
	if err := writeAndVerify(filepath.Join(workspaceDir, ScopeFile), marshalScope(have)); err != nil {
		return nil, err
	}
	return added, nil
}

// writeScope replaces .scope's contents wholesale, deleting the file rather
// than writing an empty one -- the same rule Write follows, for the same
// reason: absent means "nothing to check here", a real state a workspace can
// be in, and an empty file would be a hole with a name on it that ReadScope
// cannot tell apart from "the writer meant to record something and did not".
func writeScope(workspaceDir string, scope []Ref) error {
	path := filepath.Join(workspaceDir, ScopeFile)
	if len(scope) == 0 {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove %s: %w", path, err)
		}
		return nil
	}
	return writeAndVerify(path, marshalScope(scope))
}

// RemoveScope drops entries from a workspace's scope and reports what is left.
// Removing an entry not present is a no-op for that entry, matching AppendScope's
// idempotence in the other direction -- running it twice, or against a scope
// that never had the entry, is safe.
//
// This is the verb `add` had no opposite for (facet#112): a boundary that can
// only widen is a ratchet, and correcting a wrong entry required editing
// .scope by hand -- exactly what these tools exist to make unnecessary.
func RemoveScope(workspaceDir string, remove []Ref) (removed, remaining []Ref, err error) {
	have, err := ReadScope(workspaceDir)
	if err != nil {
		return nil, nil, err
	}
	drop := map[string]bool{}
	for _, r := range remove {
		drop[r.String()] = true
	}
	remaining = have[:0:0] // never alias `have`'s backing array with the shrunk result
	for _, r := range have {
		if drop[r.String()] {
			removed = append(removed, r)
			continue
		}
		remaining = append(remaining, r)
	}
	if len(removed) == 0 {
		return nil, have, nil
	}
	if err := writeScope(workspaceDir, remaining); err != nil {
		return nil, nil, err
	}
	return removed, remaining, nil
}

// SetScope replaces a workspace's scope wholesale and reports what it
// replaced, the way tree wire reports the parent an edge moved a child away
// from -- so the previous state is not simply gone the moment the new one is
// written.
//
// refs is deduplicated the same way AppendScope's input is; SetScope always
// writes (even to the same value) so it is not idempotent in the sense
// AppendScope is -- it is an assignment, not a merge, and the previous value
// is returned specifically so a caller can tell whether anything changed.
func SetScope(workspaceDir string, refs []Ref) (previous []Ref, err error) {
	previous, err = ReadScope(workspaceDir)
	if err != nil {
		return nil, err
	}
	if err := writeScope(workspaceDir, Dedupe(refs)); err != nil {
		return nil, err
	}
	return previous, nil
}
