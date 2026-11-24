package workspace

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/bottega-cereghino/facet/internal/gitx"
	"github.com/bottega-cereghino/facet/internal/manifest"
)

// ownFiles are facet's own files at a workspace root. Deleting the workspace
// is deleting them, so they are never loose. The seat files are listed so a
// workspace that still carries them from an older setup is not held by them.
//
// .DS_Store is here too: Finder writes it, it holds nothing of the user's, and
// it would otherwise hold every long-lived workspace a Mac has opened.
var ownFiles = map[string]bool{
	manifest.FileName: true, ".scope": true,
	".seat": true, ".seat-issue": true, ".catalog.json": true,
	".DS_Store": true,
	// A sync that crashed leaves its lock behind; it holds nothing of the user's.
	syncLockName: true,
}

// claudeMD is the file `facet spawn` writes at an issue workspace's root. It is
// facet's own only while it is what spawn wrote: nothing rewrites it, since
// spawn refuses a workspace that exists, so an edited one may hold an agent's
// notes and nothing else has them.
const claudeMD = "CLAUDE.md"

// WriteIssueClaudeMD writes an issue workspace's CLAUDE.md and records its hash
// in the manifest, so reap can tell spawn's file from one edited since.
func WriteIssueClaudeMD(ws string, m *manifest.Manifest, md []byte) error {
	if m.Issue == nil {
		return fmt.Errorf("%s is not an issue workspace", ws)
	}
	if err := os.WriteFile(filepath.Join(ws, claudeMD), md, 0o666); err != nil {
		return err
	}
	m.Issue.ClaudeMD = contentHash(md)
	return m.Write(ws)
}

// spawnsClaudeMD reports whether the CLAUDE.md in ws is still the one spawn
// wrote. A manifest with no hash (spawned before it was recorded) or a file
// that cannot be read is not: either may hide notes, so the file is judged.
func spawnsClaudeMD(ws string, m *manifest.Manifest) bool {
	if m.Issue == nil || m.Issue.ClaudeMD == "" {
		return false
	}
	b, err := os.ReadFile(filepath.Join(ws, claudeMD))
	return err == nil && contentHash(b) == m.Issue.ClaudeMD
}

func contentHash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// maxLoose bounds how many loose paths are named one by one.
const maxLoose = 10

// looseFiles finds what a workspace holds outside its clones and links that
// exists nowhere else, and the enclosing repository it checked against. Each
// result is phrased as a blocker.
//
// It holds on anything it cannot account for (#364). An entry at the top
// level is accounted for when it is a declared clone, a declared link that is
// a symlink, one of facet's own files, the CLAUDE.md spawn wrote, or a
// directory whose every file the enclosing repository has committed at that
// exact path with the same content. Committed means in the enclosing
// repository's HEAD: an ignore rule, the index and what git status reports do
// not count, and a name is matched byte for byte, never across case or
// Unicode forms. A directory holding a repository marker of any kind, at any
// depth, holds. Outside an enclosing repository every loose entry is the only
// copy. Only a repository above the workspace counts as enclosing: one at its
// root goes with it (ownRepository).
func looseFiles(git gitx.Runner, ws string, m *manifest.Manifest) (enclosing string, loose []string) {
	entries, err := os.ReadDir(ws)
	if err != nil {
		return "", []string{fmt.Sprintf("could not list %s: %v -- cannot confirm it is safe to delete", ws, err)}
	}
	var names []string
	for _, e := range entries {
		n := e.Name()
		if _, ok := m.Clones[n]; ok {
			continue
		}
		// A link is skipped only as a symlink the manifest declares: a real
		// directory at a link's name is no clone either, so nothing else would
		// judge it, and an undeclared symlink is judged as any loose entry.
		if _, ok := m.Links[n]; ok && e.Type()&os.ModeSymlink != 0 {
			continue
		}
		// An own name is facet's only as a regular file: a directory or a
		// repository at one is judged like any loose entry (#401).
		if (ownFiles[n] && e.Type().IsRegular()) || (n == claudeMD && spawnsClaudeMD(ws, m)) {
			continue
		}
		names = append(names, n)
	}
	enclosing = enclosingRepo(git, ws)
	if held := ownRepository(ws, enclosing); held != "" {
		return "", []string{held}
	}
	if len(names) == 0 {
		return enclosing, nil
	}
	if enclosing == "" {
		for _, n := range names {
			if _, held := holdsRepository(filepath.Join(ws, n)); held || isCheckout(filepath.Join(ws, n)) {
				loose = append(loose, repositoryOfItsOwn(n))
				continue
			}
			loose = append(loose, fmt.Sprintf("%s is in no repository -- this workspace is its only copy", n))
		}
		return "", capList(loose, "files not committed anywhere")
	}
	prefix, err := git.Run(ws, nil, "rev-parse", "--show-prefix")
	if err != nil {
		return enclosing, []string{fmt.Sprintf("git rev-parse in %s failed: %v -- cannot confirm the files beside the clones are safe to delete", enclosing, err)}
	}
	prefix = strings.TrimSpace(prefix)
	committed, err := committedFiles(git, ws)
	if err != nil {
		return enclosing, []string{fmt.Sprintf("could not list what %s has committed: %v -- cannot confirm the files beside the clones are safe to delete", enclosing, err)}
	}
	for _, n := range names {
		loose = append(loose, unaccounted(ws, n, prefix, enclosing, committed)...)
	}
	return enclosing, capList(loose, "files not committed anywhere")
}

// repositoryOfItsOwn is the blocker for a repository at rel in a workspace,
// beside the clones.
func repositoryOfItsOwn(rel string) string {
	return fmt.Sprintf("%s is a repository of its own -- its commits, branches and stashes are nowhere else; push what it holds and move it out of the workspace (or `facet add clone` it), then reap again", rel)
}

// treeEntry is one blob of a tree: its mode and object name.
type treeEntry struct{ mode, oid string }

// committedFiles lists the blobs the enclosing repository's HEAD holds under
// ws, by their path relative to ws, exactly as the tree spells it. An
// enclosing repository with no commit yet has committed nothing.
func committedFiles(git gitx.Runner, ws string) (map[string]treeEntry, error) {
	if _, err := git.Run(ws, nil, "rev-parse", "--verify", "--quiet", "HEAD^{commit}"); err != nil {
		var gitErr *gitx.Error
		if errors.As(err, &gitErr) && gitErr.ExitCode == 1 {
			return map[string]treeEntry{}, nil
		}
		return nil, err
	}
	// Run in ws, ls-tree lists only what is under it, by paths relative to it.
	out, err := git.Run(ws, nil, "ls-tree", "-r", "-z", "HEAD")
	if err != nil {
		return nil, err
	}
	files := map[string]treeEntry{}
	for _, rec := range strings.Split(out, "\x00") {
		meta, path, ok := strings.Cut(rec, "\t")
		if !ok {
			continue
		}
		f := strings.Fields(meta) // mode, type, object name
		if len(f) != 3 || f[1] != "blob" {
			continue
		}
		files[path] = treeEntry{mode: f[0], oid: f[2]}
	}
	return files, nil
}

// unaccounted walks the loose entry name in ws and returns a blocker for each
// thing in it the enclosing repository has not committed at that path with
// the same content (looseFiles). prefix is ws relative to the enclosing
// repository's top level, which the blockers name paths from. The walk never
// follows a symlink.
func unaccounted(ws, name, prefix, enclosing string, committed map[string]treeEntry) []string {
	var held []string
	_ = filepath.WalkDir(filepath.Join(ws, name), func(p string, d fs.DirEntry, err error) error {
		rel, relErr := filepath.Rel(ws, p)
		if relErr != nil {
			rel = p
		}
		rel = filepath.ToSlash(rel)
		if err != nil {
			held = append(held, fmt.Sprintf("could not read %s%s: %v -- cannot confirm it is safe to delete", prefix, rel, err))
			return nil
		}
		if d.IsDir() {
			if isCheckout(p) {
				held = append(held, repositoryOfItsOwn(rel))
				return filepath.SkipDir
			}
			return nil
		}
		if why := differsFromCommitted(p, d, committed[rel]); why != "" {
			held = append(held, fmt.Sprintf("%s%s %s %s -- this workspace is its only copy", prefix, rel, why, enclosing))
		}
		return nil
	})
	return held
}

// differsFromCommitted says how the file at p, the entry d, is not the blob e
// committed at its path, or "" when it is: the same kind (a symlink, or a
// regular file) and the same bytes.
func differsFromCommitted(p string, d fs.DirEntry, e treeEntry) string {
	if e.oid == "" {
		return "is not committed in"
	}
	symlink := d.Type()&fs.ModeSymlink != 0
	switch {
	case symlink && e.mode == "120000":
	case d.Type().IsRegular() && (e.mode == "100644" || e.mode == "100755"):
	default:
		return "is not what is committed at its path in"
	}
	differs, err := differsFromBlob(p, e.oid)
	if err != nil {
		return fmt.Sprintf("could not be compared (%v) with what is committed in", err)
	}
	if differs {
		return "differs from what is committed in"
	}
	return ""
}

// enclosingRepo returns the top level of the repository ws sits inside, or ""
// when there is none.
func enclosingRepo(git gitx.Runner, ws string) string {
	out, err := git.Run(ws, nil, "rev-parse", "--show-toplevel")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// ownRepository returns a blocker when the workspace is itself a repository: a
// .git of any kind at its root, or git placing the top level there. Its
// commits, branches, stashes and config live in what reap deletes, so a file
// committed there has no other copy, and nothing judges the rest; it holds
// whatever it holds. A .git git cannot read holds too, since git looks past it
// to the repository above, whose status never reports a .git. top is what
// enclosingRepo found.
func ownRepository(ws, top string) string {
	_, err := os.Lstat(filepath.Join(ws, ".git"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Sprintf("could not look for a .git in %s: %v -- cannot confirm it is safe to delete", ws, err)
	}
	if err != nil && (top == "" || realPath(top) != realPath(ws)) {
		return ""
	}
	return fmt.Sprintf("%s is itself a git repository -- its .git, with every commit, branch and stash in it, would go with the workspace; "+
		"push what you need, move .git out of the workspace, then reap again", ws)
}

// capList keeps the first maxLoose entries of list and counts the rest as
// more of what.
func capList(list []string, what string) []string {
	if len(list) <= maxLoose {
		return list
	}
	return append(list[:maxLoose:maxLoose], fmt.Sprintf("... and %d more %s", len(list)-maxLoose, what))
}
