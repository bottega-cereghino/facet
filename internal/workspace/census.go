package workspace

import (
	"bytes"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"

	"github.com/bottega-cereghino/facet/internal/gitx"
	"github.com/bottega-cereghino/facet/internal/manifest"
)

// The census (#392) is reap's last check, and the one that does not depend on
// knowing where work hides. Every other check looks for a kind of work (a
// commit no remote has, a stash, an edit git status shows, an ignored file)
// and each grill round found one more place it was not looking. The census
// asks the question the other way round: it hashes every file under the real
// path of what reap deletes, without following a link, and lets one go only
// when it can account for it. A file is accounted for when
//
//   - its content is a blob in a commit a counted remote has (countedRemotes:
//     the remotes the unpushed count trusts), in the repository whose work
//     tree holds it; a file outside every work tree inside the directory
//     counts against the repository enclosing it;
//   - it lies in a directory something rebuilds (rebuilt), named from the
//     top of the work tree under the deleted root that holds it, never
//     across a repository's boundary;
//   - it is one of facet's own files at a workspace's top level, the CLAUDE.md
//     spawn wrote, or a declared link or clone that is a symlink (top);
//   - or it is in the git directory of a repository another check judges
//     (judged): its refs, stash and config are what those checks read.
//
// Everything else holds: a file in no pushed commit, one that cannot be read,
// a socket or a fifo, the files of a repository nothing judges and its git
// directory. So does a file whose pushed blob is only the Git LFS pointer to
// its content: a pushed pointer does not show that any LFS store has the
// object, and facet reads no store (#397). The census only adds holds; every
// other check still runs.
//
// A file is compared as it is on disk, never through a filter: an edit that a
// clean filter or the ident attribute would hide from git still differs here.
// A tracked file with no filter attribute that is exactly what checkout writes
// from a pushed blob (line endings, working-tree-encoding) is accounted for
// (checkedOut).

// maxUnaccounted bounds how many unaccounted paths are named one by one.
const maxUnaccounted = maxLoose

// censusRepo is one repository the census checks files against.
type censusRepo struct {
	top    string // its work tree's real path
	judged bool   // another check judges it, so its git directory is skipped
	newH   func() hash.Hash
	pushed map[string]bool   // blob names in commits a counted remote has; nil until read
	err    error             // why pushed could not be read
	index  map[string]string // stage-0 blob of each tracked path; nil until read
}

type censusWalk struct {
	git    gitx.Runner
	root   string          // the real path reap deletes
	gone   string          // the directory reap deletes, as countedRemotes takes it
	judged map[string]bool // the real work-tree paths of the judged repositories
	top    map[string]bool // names at root accounted for without being read
	repos  map[string]*censusRepo
	held   []string
	count  int
	tmp    string // where checkedOut has git write files; "" until made
}

// census returns the files under dir that nothing accounts for, each relative
// to dir and saying why, and how many there are in all; the list is capped at
// maxUnaccounted. judged lists the work trees of the repositories other checks
// judge; top names the entries at dir's top level accounted for unread.
// enclosing is the repository files outside every work tree count against,
// or "" when there is none.
func census(git gitx.Runner, dir, gone, enclosing string, judged []string, top map[string]bool) (held []string, count int) {
	root := realPath(dir)
	w := &censusWalk{git: git, root: root, gone: gone, judged: map[string]bool{}, top: top, repos: map[string]*censusRepo{}}
	for _, j := range judged {
		w.judged[realPath(j)] = true
	}
	var repo *censusRepo
	if enclosing != "" {
		repo = w.repo(realPath(enclosing))
	}
	defer func() {
		if w.tmp != "" {
			_ = os.RemoveAll(w.tmp)
		}
	}()
	w.walk(root, repo, "")
	return w.held, w.count
}

func (w *censusWalk) hold(p, format string, a ...any) {
	w.count++
	if len(w.held) >= maxUnaccounted {
		return
	}
	rel, err := filepath.Rel(w.root, p)
	if err != nil {
		rel = p
	}
	w.held = append(w.held, filepath.ToSlash(rel)+" "+fmt.Sprintf(format, a...))
}

// walk accounts for everything in the directory abs, whose files count
// against repo (nil: no repository holds them). base is the top of the work
// tree under the deleted root that abs lies in, which what something rebuilds
// is named relative to, or "" when there is none: outside such a work tree,
// in the enclosing repository's files or below a .git git does not read,
// nothing counts as rebuilt (#400).
func (w *censusWalk) walk(abs string, repo *censusRepo, base string) {
	entries, err := os.ReadDir(abs)
	if err != nil {
		w.hold(abs, "could not be read (%v)", err)
		return
	}
	for _, e := range entries {
		if e.Name() == ".git" {
			// A work tree's top: its files count against its own repository.
			// A .git git does not read leaves them to the enclosing one, and
			// nothing below it is rebuilt.
			base = ""
			if r := w.workTree(abs); r != nil {
				repo, base = r, abs
			}
			break
		}
	}
	for _, e := range entries {
		p := filepath.Join(abs, e.Name())
		if abs == w.root && w.top[e.Name()] {
			continue
		}
		if repo != nil && repo.judged && abs == repo.top && e.Name() == ".git" {
			continue
		}
		if w.rebuilt(base, p, e) {
			continue
		}
		switch t := e.Type(); {
		case t.IsDir():
			w.walk(p, repo, base)
		case t&fs.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				w.hold(p, "could not be read (%v)", err)
				continue
			}
			if why := w.check(repo, strings.NewReader(target), int64(len(target))); why != "" {
				w.hold(p, "%s", why)
			}
		case t.IsRegular():
			w.file(p, repo)
		default:
			w.hold(p, "is a %s, which no commit holds", kindOf(t))
		}
	}
}

// rebuiltEntry reports whether the entry at rel lies in what something
// rebuilds, as rebuilt says, except that Finder's .DS_Store counts only as
// the regular file Finder writes: a directory of that name holds what anyone
// put there (#401).
func rebuiltEntry(rel string, regular bool) bool {
	if rebuiltPath(rel) {
		return true
	}
	steps := strings.Split(rel, "/")
	for i, step := range steps {
		if step == ".DS_Store" {
			if i == len(steps)-1 && regular {
				return true
			}
			continue
		}
		if rebuilt(step) {
			return true
		}
	}
	return false
}

// rebuilt reports whether the entry e at p is accounted for as what
// something rebuilds: Finder's .DS_Store file anywhere, and otherwise a path
// rebuiltEntry names, relative to base, the work tree p lies in. A directory
// holding a .git is a repository, never a build output, and is walked.
func (w *censusWalk) rebuilt(base, p string, e fs.DirEntry) bool {
	if e.Name() == ".DS_Store" && e.Type().IsRegular() {
		return true
	}
	if base == "" {
		return false
	}
	rel, err := filepath.Rel(base, p)
	if err != nil || !rebuiltEntry(filepath.ToSlash(rel), e.Type().IsRegular()) {
		return false
	}
	if e.IsDir() {
		if _, err := os.Lstat(filepath.Join(p, ".git")); err == nil {
			return false
		}
	}
	return true
}

// checkedOut reports whether the file at p is exactly what checkout writes
// for its path from a pushed blob: the index names the blob, a counted remote
// has it, and git, asked to write the entry elsewhere, writes the same bytes.
// That accounts for line endings converted on checkout (text eol=crlf,
// core.autocrlf) and working-tree-encoding, which git status calls clean
// (#402). A path with a filter attribute is never written: a filter driver
// is a command, and the census runs none.
func (w *censusWalk) checkedOut(p string, repo *censusRepo) bool {
	rel, err := filepath.Rel(repo.top, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	rel = filepath.ToSlash(rel)
	if repo.index == nil {
		entries, err := indexEntries(w.git, repo.top, nil)
		if err != nil {
			return false
		}
		repo.index = make(map[string]string, len(entries))
		for _, e := range entries {
			repo.index[e.path] = e.oid
		}
	}
	if oid, ok := repo.index[rel]; !ok || !repo.pushed[oid] {
		return false
	}
	out, err := w.git.Run(repo.top, nil, "check-attr", "-z", "filter", "--", rel)
	if f := strings.Split(out, "\x00"); err != nil || len(f) < 3 || (f[2] != "unspecified" && f[2] != "unset") {
		return false
	}
	if w.tmp == "" {
		if w.tmp, err = os.MkdirTemp("", "facet-census-"); err != nil {
			w.tmp = ""
			return false
		}
	}
	dir, err := os.MkdirTemp(w.tmp, "")
	if err != nil {
		return false
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if _, err := w.git.Run(repo.top, nil, "checkout-index", "--prefix="+dir+string(filepath.Separator), "--", rel); err != nil {
		return false
	}
	return sameBytes(p, filepath.Join(dir, filepath.FromSlash(rel)))
}

// sameBytes reports whether the regular files a and b hold the same bytes,
// neither read through a link.
func sameBytes(a, b string) bool {
	fa, err := os.OpenFile(a, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return false
	}
	defer func() { _ = fa.Close() }()
	fb, err := os.OpenFile(b, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return false
	}
	defer func() { _ = fb.Close() }()
	ia, errA := fa.Stat()
	ib, errB := fb.Stat()
	if errA != nil || errB != nil || !ia.Mode().IsRegular() || !ib.Mode().IsRegular() || ia.Size() != ib.Size() {
		return false
	}
	ha, hb := sha256.New(), sha256.New()
	if _, err := io.Copy(ha, fa); err != nil {
		return false
	}
	if _, err := io.Copy(hb, fb); err != nil {
		return false
	}
	return bytes.Equal(ha.Sum(nil), hb.Sum(nil))
}

// workTree returns the repository whose work tree's top is abs, or nil when
// git does not read abs as one: its .git is then a file like any other, and
// holds.
func (w *censusWalk) workTree(abs string) *censusRepo {
	top, err := w.git.Run(abs, nil, "rev-parse", "--show-toplevel")
	if err != nil || realPath(strings.TrimSpace(top)) != abs {
		return nil
	}
	return w.repo(abs)
}

// repo returns the census's record of the repository whose work tree is top.
func (w *censusWalk) repo(top string) *censusRepo {
	if r, ok := w.repos[top]; ok {
		return r
	}
	r := &censusRepo{top: top, judged: w.judged[top]}
	w.repos[top] = r
	return r
}

// file accounts for the regular file at p. It is opened without following a
// link, so a file swapped for one while the census runs is not read through
// it.
func (w *censusWalk) file(p string, repo *censusRepo) {
	f, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		w.hold(p, "could not be read (%v)", err)
		return
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		w.hold(p, "could not be read as a file")
		return
	}
	why := w.check(repo, f, fi.Size())
	if why == whyUnpushed && w.checkedOut(p, repo) {
		return
	}
	if why != "" {
		w.hold(p, "%s", why)
	}
}

// whyUnpushed is check's reason for content no pushed blob accounts for.
const whyUnpushed = "is in no commit a remote has"

// check returns why content, size bytes read from r, holds, or "" when it is
// a pushed blob of repo, or it is a pushed blob as checkout writes it with the
// ident attribute (identExpanded). Content whose Git LFS pointer is a pushed
// blob holds with its own reason, which names the push that sends the object.
func (w *censusWalk) check(repo *censusRepo, r io.Reader, size int64) string {
	if repo == nil {
		return "is in no repository"
	}
	if err := w.readPushed(repo); err != nil {
		return fmt.Sprintf("could not be checked against what its repository has pushed (%v)", err)
	}
	blob := repo.newH()
	_, _ = fmt.Fprintf(blob, "blob %d\x00", size) // a hash never fails a write
	lfs := sha256.New()
	sinks := []io.Writer{blob, lfs}
	var small *bytes.Buffer
	if size <= maxIdentBytes {
		small = &bytes.Buffer{}
		sinks = append(sinks, small)
	}
	n, err := io.Copy(io.MultiWriter(sinks...), r)
	if err != nil {
		return fmt.Sprintf("could not be read (%v)", err)
	}
	if n != size {
		return "changed while it was read"
	}
	if repo.pushed[hex.EncodeToString(blob.Sum(nil))] {
		return ""
	}
	pointer := fmt.Sprintf("version https://git-lfs.github.com/spec/v1\noid sha256:%x\nsize %d\n", lfs.Sum(nil), size)
	ptr := repo.newH()
	_, _ = fmt.Fprintf(ptr, "blob %d\x00%s", len(pointer), pointer)
	if small != nil && identExpanded(repo, small.Bytes()) {
		return ""
	}
	if repo.pushed[hex.EncodeToString(ptr.Sum(nil))] {
		return "is Git LFS content whose pointer is pushed, but facet cannot see that any LFS store has it " +
			"(run git lfs push --all <remote> in its clone, then reap the workspace by name with --force)"
	}
	return whyUnpushed
}

// maxIdentBytes bounds the files identExpanded reads: an ident keyword is for
// source files, and a larger file is only hashed.
const maxIdentBytes = 1 << 20

// identKeyword is the ident attribute's keyword as checkout expands it.
var identKeyword = regexp.MustCompile(`\$Id: ([0-9a-f]+) \$`)

// identExpanded reports whether content is exactly what checkout writes for
// a pushed blob of repo with the ident attribute: the blob with each "$Id$"
// expanded to "$Id: <the blob's name> $". Collapsing the keywords gives the
// blob; every keyword must then name that very blob, so the file can be
// written again from it byte for byte. Anything else between "$Id:" and "$",
// such as a note written into the keyword, leaves the file unaccounted for
// (#385).
func identExpanded(repo *censusRepo, content []byte) bool {
	found := identKeyword.FindAllSubmatch(content, -1)
	if len(found) == 0 {
		return false
	}
	collapsed := identKeyword.ReplaceAll(content, []byte("$$Id$$"))
	h := repo.newH()
	_, _ = fmt.Fprintf(h, "blob %d\x00", len(collapsed))
	_, _ = h.Write(collapsed)
	oid := hex.EncodeToString(h.Sum(nil))
	if !repo.pushed[oid] {
		return false
	}
	for _, m := range found {
		if string(m[1]) != oid {
			return false
		}
	}
	return true
}

// readPushed lists, once per repository, the blobs in the commits its counted
// remotes have. With no remote counted, nothing is pushed.
func (w *censusWalk) readPushed(r *censusRepo) error {
	if r.pushed != nil || r.err != nil {
		return r.err
	}
	r.err = func() error {
		format, err := w.git.Run(r.top, nil, "rev-parse", "--show-object-format")
		if err != nil {
			return err
		}
		switch strings.TrimSpace(format) {
		case "sha1":
			r.newH = sha1.New
		case "sha256":
			r.newH = sha256.New
		default:
			return fmt.Errorf("unknown object format %q", strings.TrimSpace(format))
		}
		pushed, err := countedRemotes(w.git, r.top, w.gone)
		if err != nil {
			return err
		}
		r.pushed = map[string]bool{}
		if len(pushed.globs) == 0 {
			return nil
		}
		// A glob that matches no ref names nothing; with every glob empty,
		// rev-list has nothing to walk and prints nothing.
		args := append([]string{"rev-list", "--objects", "--no-object-names", "--filter=object:type=blob"}, pushed.globs...)
		out, err := w.git.Run(r.top, nil, args...)
		if err != nil {
			r.pushed = nil
			return err
		}
		for _, oid := range strings.Fields(out) {
			r.pushed[oid] = true
		}
		return nil
	}()
	return r.err
}

// kindOf names a file type that is neither a directory, a symlink nor a
// regular file.
func kindOf(t fs.FileMode) string {
	switch {
	case t&fs.ModeNamedPipe != 0:
		return "fifo"
	case t&fs.ModeSocket != 0:
		return "socket"
	case t&fs.ModeDevice != 0:
		return "device"
	}
	return "special file"
}

// unaccountedBlocker is the blocker for the files the census holds.
func (s *IssueState) unaccountedBlocker() string {
	list := s.Unaccounted
	if more := s.UnaccountedCount - len(list); more > 0 {
		list = append(list[:len(list):len(list)], fmt.Sprintf("... and %d more", more))
	}
	return fmt.Sprintf("%d file(s) are in no commit a remote has and in no directory something rebuilds -- this is their only copy: %s; push or copy out what you need, then reap again",
		s.UnaccountedCount, strings.Join(list, "; "))
}

// cloneDirs is the work tree of each judged clone, base joined with its Dir.
func cloneDirs(base string, clones []CloneState) []string {
	out := make([]string, 0, len(clones))
	for _, c := range clones {
		out = append(out, filepath.Join(base, c.Dir))
	}
	return out
}

// topAccounted names the entries at a workspace's top level the census lets
// go unread: facet's own files, the CLAUDE.md spawn wrote, and the declared
// links and clones that are symlinks, which reap unlinks and never follows.
func topAccounted(ws string, m *manifest.Manifest) map[string]bool {
	top := map[string]bool{}
	for n := range ownFiles {
		if fi, err := os.Lstat(filepath.Join(ws, n)); err == nil && fi.Mode().IsRegular() {
			top[n] = true
		}
	}
	if spawnsClaudeMD(ws, m) {
		top[claudeMD] = true
	}
	for _, names := range [][]string{sortedKeys(m.Links), sortedKeys(m.Clones)} {
		for _, n := range names {
			if fi, err := os.Lstat(filepath.Join(ws, n)); err == nil && fi.Mode()&fs.ModeSymlink != 0 {
				top[n] = true
			}
		}
	}
	return top
}
