package workspace

import (
	"crypto/sha1" // git names objects by SHA-1: this computes that name, not a security check
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/bottega-cereghino/facet/internal/gitx"
)

// honestStatus goes before `status` in every status reap runs, so no setting
// answers for the disk in git's place:
//   - core.fsmonitor=false: a hook would say which files changed, and one that
//     misses an event hides an edit; core.untrackedCache=false, likewise for
//     new files.
//   - core.checkStat=default and core.trustctime=true: weaker settings let an
//     edit that keeps a file's size and mtime pass as unchanged.
//   - --no-optional-locks: status otherwise writes back the index it
//     refreshed, under the settings above; reap reads, it never writes.
//
// What git status cannot be told to see -- an index entry's own mark and a
// clean filter -- hiddenEdits checks.
var honestStatus = []string{
	"--no-optional-locks",
	"-c", "core.fsmonitor=false",
	"-c", "core.untrackedCache=false",
	"-c", "core.checkStat=default",
	"-c", "core.trustctime=true",
}

// hiddenEdit is a tracked file that differs from its index entry while git
// status calls it unchanged. why says what hides it and how to show it.
type hiddenEdit struct{ path, why string }

// indexEntry is one stage-0 line of `git ls-files -s -v`.
type indexEntry struct {
	tag  byte
	oid  string
	path string
}

// hiddenEdits finds the tracked files under paths (all, when empty), run in
// dir, whose content differs from the index although git status says it does
// not. Two things make status look away, and no option turns either off:
//
//   - a mark on the index entry, assume-unchanged or skip-worktree (the usual
//     way to keep a local config edit out of status): status skips the file;
//   - a clean filter (nbstripout's shape): status compares what the filter
//     makes of the file, so content the filter drops reads as committed.
//
// Each such file is hashed as it is on disk and compared with its blob. A
// blob that is the canonical Git LFS pointer to the file's content matches
// too: the committed copy names that content, and the LFS store holds it.
// Any other filter that changes what it stores (git-crypt) holds the file,
// since the blob alone cannot prove the file holds nothing more. A marked
// file absent from the disk (a sparse checkout) loses nothing.
func hiddenEdits(git gitx.Runner, dir string, paths []string) ([]hiddenEdit, error) {
	entries, err := indexEntries(git, dir, paths)
	if err != nil {
		return nil, err
	}
	filters, err := cleanFilters(git, dir, entries)
	if err != nil {
		return nil, err
	}
	var out []hiddenEdit
	for _, e := range entries {
		why := markedWhy(e)
		if why == "" && filters[e.path] != "" {
			why = fmt.Sprintf("git status reads it through the clean filter %q, which hides the difference; copy the file out, or commit it with the filter off", filters[e.path])
		}
		if why == "" {
			continue
		}
		differs, err := differsFromBlob(filepath.Join(dir, filepath.FromSlash(e.path)), e.oid)
		if err != nil {
			return out, err
		}
		if differs {
			out = append(out, hiddenEdit{path: e.path, why: why})
		}
	}
	return out, nil
}

// indexEntries lists the stage-0 entries under paths. Gitlinks are left out:
// an initialised submodule is judged as a clone of its own. A conflicted
// entry is too, since git status shows it whatever its marks.
func indexEntries(git gitx.Runner, dir string, paths []string) ([]indexEntry, error) {
	out, err := git.Run(dir, nil, append([]string{"ls-files", "-z", "-s", "-v", "--"}, paths...)...)
	if err != nil {
		return nil, err
	}
	var entries []indexEntry
	for _, rec := range strings.Split(out, "\x00") {
		if rec == "" {
			continue
		}
		// "<tag> <mode> <oid> <stage>\t<path>"
		meta, path, ok := strings.Cut(rec, "\t")
		f := strings.Fields(meta)
		if !ok || len(f) != 4 || len(f[0]) != 1 {
			return nil, fmt.Errorf("unexpected line from git ls-files: %q", rec)
		}
		if f[3] != "0" || f[1] == "160000" {
			continue
		}
		entries = append(entries, indexEntry{tag: f[0][0], oid: f[2], path: path})
	}
	return entries, nil
}

// markedWhy explains the mark on e's index entry, or returns "" for none.
// `ls-files -v` prints skip-worktree as S and assume-unchanged in lowercase.
func markedWhy(e indexEntry) string {
	switch {
	case e.tag == 'S' || e.tag == 's':
		return fmt.Sprintf("it is marked skip-worktree, so git status skips it; `git update-index --no-skip-worktree -- %s` shows it again", e.path)
	case unicode.IsLower(rune(e.tag)):
		return fmt.Sprintf("it is marked assume-unchanged, so git status skips it; `git update-index --no-assume-unchanged -- %s` shows it again", e.path)
	}
	return ""
}

// checkAttrBatch bounds how many paths go to one `git check-attr`, well under
// the argument-length limit.
const checkAttrBatch = 1000

// cleanFilters maps each entry's path to the filter driver git status would
// clean it with. Only a driver with a clean or process command configured
// filters anything, so with none (most repositories) no attribute is read.
func cleanFilters(git gitx.Runner, dir string, entries []indexEntry) (map[string]string, error) {
	drivers, err := cleanDrivers(git, dir)
	if err != nil || len(drivers) == 0 {
		return nil, err
	}
	filters := map[string]string{}
	for start := 0; start < len(entries); start += checkAttrBatch {
		args := []string{"check-attr", "-z", "filter", "--"}
		for _, e := range entries[start:min(start+checkAttrBatch, len(entries))] {
			args = append(args, e.path)
		}
		out, err := git.Run(dir, nil, args...)
		if err != nil {
			return nil, err
		}
		// "<path>\0filter\0<value>\0", one triple per path.
		f := strings.Split(strings.TrimSuffix(out, "\x00"), "\x00")
		if len(f)%3 != 0 {
			return nil, fmt.Errorf("unexpected output from git check-attr: %q", out)
		}
		for i := 0; i < len(f); i += 3 {
			if drivers[f[i+2]] {
				filters[f[i]] = f[i+2]
			}
		}
	}
	return filters, nil
}

// cleanDrivers names the filter drivers with a clean or process command, in
// any config git reads.
func cleanDrivers(git gitx.Runner, dir string) (map[string]bool, error) {
	out, err := git.Run(dir, nil, "config", "-z", "--get-regexp", `^filter\..+\.(clean|process)$`)
	if err != nil {
		// Exit 1 is "no such key", not a failure.
		var gitErr *gitx.Error
		if errors.As(err, &gitErr) && gitErr.ExitCode == 1 {
			return nil, nil
		}
		return nil, err
	}
	drivers := map[string]bool{}
	for _, rec := range strings.Split(out, "\x00") {
		key, _, _ := strings.Cut(rec, "\n")
		name := strings.TrimPrefix(key, "filter.")
		if i := strings.LastIndexByte(name, '.'); i > 0 && name != key {
			drivers[name[:i]] = true
		}
	}
	return drivers, nil
}

// differsFromBlob reports whether the file at path holds anything blob oid
// does not: its bytes are neither the blob nor the object of the Git LFS
// pointer the blob is. A file that is gone holds nothing; anything that is
// not a file or a symlink is taken to differ.
func differsFromBlob(path, oid string) (bool, error) {
	fi, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	switch {
	case fi.Mode()&fs.ModeSymlink != 0:
		// git stores a symlink as a blob holding its target.
		target, err := os.Readlink(path)
		if err != nil {
			return false, err
		}
		return differsFromBlobContent(strings.NewReader(target), int64(len(target)), oid)
	case fi.Mode().IsRegular():
		f, err := os.Open(path)
		if err != nil {
			return false, err
		}
		defer func() { _ = f.Close() }()
		return differsFromBlobContent(f, fi.Size(), oid)
	}
	return true, nil
}

// differsFromBlobContent compares size bytes read from r with blob oid, as
// differsFromBlob says. The object format is read off the oid's length.
func differsFromBlobContent(r io.Reader, size int64, oid string) (bool, error) {
	newHash, err := objectHash(oid)
	if err != nil {
		return false, err
	}
	blob := newHash()
	_, _ = fmt.Fprintf(blob, "blob %d\x00", size) // a hash never fails a write
	lfs := sha256.New()
	n, err := io.Copy(io.MultiWriter(blob, lfs), r)
	if err != nil {
		return false, err
	}
	if n != size {
		// It changed while it was read: it may hold anything.
		return true, nil
	}
	if hex.EncodeToString(blob.Sum(nil)) == oid {
		return false, nil
	}
	pointer := fmt.Sprintf("version https://git-lfs.github.com/spec/v1\noid sha256:%x\nsize %d\n", lfs.Sum(nil), size)
	p := newHash()
	_, _ = fmt.Fprintf(p, "blob %d\x00%s", len(pointer), pointer)
	return hex.EncodeToString(p.Sum(nil)) != oid, nil
}

// objectHash returns the hash a repository names its objects with, told by
// the length of one of its object names.
func objectHash(oid string) (func() hash.Hash, error) {
	switch len(oid) {
	case sha1.Size * 2:
		return sha1.New, nil
	case sha256.Size * 2:
		return sha256.New, nil
	}
	return nil, fmt.Errorf("unknown object name %q", oid)
}
