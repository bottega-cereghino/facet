package workspace

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/bottega-cereghino/facet/internal/config"
	"github.com/bottega-cereghino/facet/internal/ghx"
	"github.com/bottega-cereghino/facet/internal/gitx"
	"github.com/bottega-cereghino/facet/internal/manifest"
)

// CloneState is what a clone inside an issue workspace looks like right now.
type CloneState struct {
	Dir    string
	Branch string
	// Dirty means uncommitted changes or untracked files.
	Dirty bool
	// InProgress names the git operations stopped partway (a rebase, a merge),
	// whose state, an autostash among it, is only in the git directory.
	InProgress []string
	// Hidden names the tracked files that differ from what is committed while
	// git status calls them unchanged: an index mark or a clean filter hides
	// them (see hiddenEdits). Each entry says what hides it and how to show
	// it. Non-empty is a blocker.
	Hidden []string
	// Worktrees names the clone's linked worktrees (`git worktree add`). Their
	// uncommitted files and detached commits are invisible to the checks here,
	// which read only the main worktree, and deleting the clone deletes their
	// repository. Non-empty is a blocker.
	Worktrees []string
	// WorktreeOf is the checkout this clone is a linked worktree of, or "" for
	// a clone with a repository of its own. A worktree shares its branches,
	// stashes and other worktrees with that checkout, and they stay there when
	// the worktree is removed, so only what removing it loses is judged: its
	// uncommitted files and the commits only its HEAD reaches.
	WorktreeOf string
	// Unpushed counts commits reachable from a local ref (walkedRefs) that no
	// remote has *and* whose content is not proven to have already landed on
	// the remote's default branch (see proofLanded). This deliberately covers
	// branches with no upstream at all, and commits made on a detached HEAD:
	// those are the ones most easily lost. The landing proof is what keeps a
	// squash-merged, auto-deleted branch from being counted here: squash
	// rewrites the sha, so a sha-only reachability test can never see it.
	Unpushed int
	// SquashLanded counts commits that failed the sha-based check above --
	// Unpushed would otherwise have included them -- but were proven, by
	// proofLanded, to already be on the remote's default branch under a
	// different sha. Purely informational: these are excluded from Unpushed
	// and never block reap, but staying silent about them is what taught
	// operators to reach for --force (facet#66).
	SquashLanded int
	// StaleRemotes names the remotes `git fetch --prune` failed for, or did not
	// finish within fetchTimeout, before Unpushed was computed. The count above
	// is still computed and used against whatever refs are on disk -- refusing
	// reap on every offline run would make it useless without a network -- but
	// Blockers appends a disclaimer to the unpushed-commit line whenever this is
	// set and there are unpushed commits to disclaim.
	StaleRemotes []string
	// DefaultBranchUnresolved is set when origin's default branch could not be
	// determined -- no refs/remotes/origin/HEAD and no origin/main, which is a
	// hand-built remote, an old fetch config, or a repo whose default is not
	// main -- *and* there were unpushed candidates that needed it. Nothing can
	// be proven to have landed on a branch that cannot be named, so proofLanded
	// fails safe and every candidate is counted unpushed. Without this the
	// operator is told about unpushed commits and never about the real cause
	// (facet#80). Like StaleRemotes it is a disclaimer on that line and never a
	// blocker of its own: it is set only when the fail-safe actually decided
	// something, so a clean workspace on such a remote still reaps.
	DefaultBranchUnresolved bool
	// IgnoredFiles names what the clone ignores that nothing rebuilds
	// (unrebuiltIgnored): a .env, a local config. Non-empty is a blocker.
	IgnoredFiles []string
	// Stashed counts `git stash` entries, which no push would ever carry.
	Stashed int
	// InnerRepos names the repositories inside the clone's work tree that no
	// check judges (unjudgedRepos): any VCS, bare or not, ignored or not, but a
	// git repository at a gitlink or in a tracked directory, which is judged as
	// a clone. Their commits are invisible to the checks here, and they go with
	// the clone. Non-empty is a blocker.
	InnerRepos []string
	// StrayGitlinks names the submodule directories, not checked out, that hold
	// files: git status never looks inside them, and the files are in no
	// repository. Non-empty is a blocker.
	StrayGitlinks []string
	// Unverifiable holds the reasons this clone's state could not be confirmed --
	// a git command that failed, or a directory that is not a readable repo. A
	// non-empty slice is itself a blocker: a clone we could not inspect must not
	// be assumed empty, or a git hiccup becomes silent data loss.
	Unverifiable []string
}

// IssueState is the full picture of one workspace: an ephemeral issue
// workspace, or -- with Issue nil -- a long-lived one `facet new` made.
type IssueState struct {
	Dir   string
	Name  string
	Issue *manifest.Issue
	// Worktree marks a plain linked worktree reaped on its own, not a
	// workspace: Dir is the worktree, and Clone holds it as its one entry.
	Worktree bool
	// Enclosing is the top level of a repository the workspace directory sits
	// inside (~/Workspaces is one), or "" when there is none.
	Enclosing string
	// Loose names files in the workspace, outside its clones and links, that
	// exist nowhere else: not committed in Enclosing, or there is no Enclosing
	// at all. A checkout `facet rm` dropped from the manifest, a hand clone or
	// a note at the root are all loose. Non-empty is a blocker.
	Loose []string
	Clone []CloneState
	// Unaccounted names, capped, the files under the workspace's real path
	// the census cannot account for (census, #392): in no pushed commit, in
	// no directory something rebuilds, and none of facet's own. Each says why.
	// UnaccountedCount is how many there are in all. Non-zero is a blocker.
	Unaccounted      []string
	UnaccountedCount int
	// PR is the pull request for the issue branch, or nil.
	PR *ghx.PR
	// PRUnknown is set when a pull-request lookup was attempted but failed, so an
	// open PR cannot be ruled out. Distinct from PR == nil, which means the lookup
	// ran and found none.
	PRUnknown bool
	// SessionLive means the multiplexer session of this name has a pane
	// rooted in the workspace; reap --force ends it.
	SessionLive bool
	// LiveRoots names tmux panes or processes -- in any session, not only the
	// one above -- whose current working directory is rooted here. Non-empty
	// is itself a blocker: deleting a directory out from under a running
	// process is silent corruption, not a merely premature reap.
	LiveRoots []string
	// LiveRootsUnknown is why the panes and processes rooted here could not be
	// listed, or nil. Set, one cannot be ruled out, so it holds the workspace.
	LiveRootsUnknown error
	// Jobs names the loaded background jobs (launchd on macOS) whose program or
	// working directory is here. A job that is not running holds nothing open,
	// so LiveRoots misses it, but deleting the directory breaks its next run.
	Jobs []string
	// JobsUnknown is why the loaded jobs could not be read, or nil. Set, a job
	// that runs from here cannot be ruled out, so it holds the workspace.
	JobsUnknown error
	// SizeBytes is how much disk deleting the workspace frees: objects
	// hardlinked from the mirror stay behind, so they are not counted.
	SizeBytes int64
}

// Blockers lists the reasons this workspace must not be deleted. An empty slice
// means it is safe to reap.
//
// The ordering matters: work that cannot be recovered or even confirmed comes
// first, an open PR is merely premature. Every check fails safe -- a state we
// could not read is reported as a blocker, never assumed clean.
func (s *IssueState) Blockers() []string {
	var out []string
	for _, c := range s.Clone {
		for _, u := range c.Unverifiable {
			out = append(out, fmt.Sprintf("%s: %s -- cannot confirm it is safe to delete", c.Dir, u))
		}
	}
	for _, c := range s.Clone {
		if c.Unpushed > 0 {
			msg := fmt.Sprintf("%s has %d unpushed commit(s) -- this workspace is their only copy", c.Dir, c.Unpushed)
			if len(c.StaleRemotes) > 0 {
				msg += fmt.Sprintf(" (could not fetch %s first -- a branch merged and deleted upstream may be miscounted here)", strings.Join(c.StaleRemotes, ", "))
			}
			if c.DefaultBranchUnresolved {
				msg += " (could not resolve origin's default branch -- work already landed there cannot be told from unpushed work here)"
			}
			out = append(out, msg)
		}
	}
	out = append(out, s.Loose...)
	for _, c := range s.Clone {
		for _, w := range c.Worktrees {
			out = append(out, fmt.Sprintf("%s has a linked worktree at %s -- reap cannot see its work; push it, then `git worktree remove` it", c.Dir, w))
		}
	}
	for _, c := range s.Clone {
		for _, r := range c.InnerRepos {
			out = append(out, fmt.Sprintf("%s has a repository at %s that reap does not judge (another VCS, a bare repository, or one in a directory it ignores) -- reap cannot see its work; push it or move it out of the clone", c.Dir, r))
		}
		for _, g := range c.StrayGitlinks {
			out = append(out, fmt.Sprintf("%s has files in %s, a submodule's directory with no repository checked out -- git status never looks there, and they are in no repository; move them out of the clone, then reap again", c.Dir, g))
		}
	}
	for _, c := range s.Clone {
		if len(c.IgnoredFiles) > 0 {
			out = append(out, fmt.Sprintf("%s has files git ignores that nothing rebuilds: %s -- the clone is their only copy; copy out what you need and delete the rest, then reap again", c.Dir, strings.Join(capList(c.IgnoredFiles, "ignored paths"), ", ")))
		}
	}
	for _, c := range s.Clone {
		if c.Stashed > 0 {
			out = append(out, fmt.Sprintf("%s has %d stash entry(ies) -- no push would carry them", c.Dir, c.Stashed))
		}
	}
	for _, c := range s.Clone {
		if c.Dirty {
			out = append(out, fmt.Sprintf("%s has uncommitted changes", c.Dir))
		}
	}
	for _, c := range s.Clone {
		for _, op := range c.InProgress {
			out = append(out, fmt.Sprintf("%s has %s in progress -- its state, an autostash of uncommitted edits among it, is only in the git directory; finish or abort it, then reap again", c.Dir, op))
		}
	}
	for _, c := range s.Clone {
		for _, h := range c.Hidden {
			out = append(out, fmt.Sprintf("%s: %s", c.Dir, h))
		}
	}
	if s.UnaccountedCount > 0 {
		out = append(out, s.unaccountedBlocker())
	}
	if s.PR != nil && strings.EqualFold(s.PR.State, "OPEN") {
		out = append(out, fmt.Sprintf("pull request #%d is still open", s.PR.Number))
	}
	if s.PRUnknown {
		out = append(out, "could not determine the pull-request state -- an open PR cannot be ruled out")
	}
	if s.SessionLive {
		out = append(out, fmt.Sprintf("tmux session %q runs in it -- end it, or --force ends it", filepath.Base(s.Dir)))
	}
	for _, r := range s.LiveRoots {
		out = append(out, fmt.Sprintf("%s -- kill it before reaping", r))
	}
	if s.LiveRootsUnknown != nil {
		out = append(out, fmt.Sprintf("could not list the panes and processes running from here (%v) -- one cannot be ruled out; check that tmux and lsof run, then reap again", s.LiveRootsUnknown))
	}
	for _, j := range s.Jobs {
		out = append(out, fmt.Sprintf("%s -- unload the job, or move it out, before reaping", j))
	}
	if s.JobsUnknown != nil {
		out = append(out, fmt.Sprintf("could not read the loaded launchd jobs (%v) -- a job that runs from here cannot be ruled out; run `launchctl list` to see why, then reap again", s.JobsUnknown))
	}
	return out
}

// StaleFetch reports whether any clone was judged against refs that could not
// be fetched first. A single reap goes ahead with a note, since refusing every
// offline reap would make it useless without a network; reap --reapable, which
// never guesses, leaves such a workspace alone.
func (s *IssueState) StaleFetch() bool {
	for _, c := range s.Clone {
		if len(c.StaleRemotes) > 0 {
			return true
		}
	}
	return false
}

// Notes lists non-blocking information about a workspace's state -- things
// worth saying so an operator does not read silence as "nothing to report"
// (facet#66). Nothing here belongs in Blockers(): every entry is something
// that does *not* hold the workspace.
func (s *IssueState) Notes() []string {
	var out []string
	for _, c := range s.Clone {
		// With unpushed commits the disclaimer rides on that blocker; with none,
		// "nothing unpushed" was still judged against the last fetch, and a
		// branch deleted upstream since then would read as pushed.
		if len(c.StaleRemotes) > 0 && c.Unpushed == 0 {
			out = append(out, fmt.Sprintf(
				"%s: could not fetch %s, so it was judged against the last fetch -- a branch deleted upstream since then reads as pushed; reap again online to be sure",
				c.Dir, strings.Join(c.StaleRemotes, ", ")))
		}
	}
	for _, c := range s.Clone {
		if c.SquashLanded > 0 {
			out = append(out, fmt.Sprintf(
				"%s: %d local commit(s) already landed upstream under a different sha (squash-merged) -- not held",
				c.Dir, c.SquashLanded))
		}
	}
	return out
}

// LiveChecker reports whether the multiplexer session of this name is the
// workspace's: it has a pane rooted in dir. A session that only shares the
// name is someone else's. It is an interface so the reap logic is testable
// without a multiplexer.
type LiveChecker interface {
	Live(session, dir string) bool
}

// LiveRootChecker reports tmux panes or processes, in any session, whose
// current working directory is rooted inside a workspace -- unlike
// LiveChecker, this is not limited to the one session named after the
// workspace, so a pane moved elsewhere (tmux's link-window does exactly
// this) is still caught. Implemented optionally by whatever LiveChecker is
// passed to InspectIssue; one that does not implement it is simply not
// asked, the same way a missing PaneReader is skipped in package mux.
type LiveRootChecker interface {
	LiveRoots(dir string) ([]string, error)
}

// JobChecker reports the loaded background jobs that run from inside a
// workspace, running or not. Implemented optionally by whatever LiveChecker is
// passed, as LiveRootChecker is.
type JobChecker interface {
	JobsUnder(dir string) ([]string, error)
}

// PRLookup finds the pull request for a branch. Nil means do not look.
type PRLookup interface {
	ViewPR(repo, branch string) (*ghx.PR, error)
}

// CommitPRLookup finds the merged pull request that had a specific commit sha,
// directly -- unlike PRLookup, which resolves by branch name and goes blind
// the moment GitHub deletes the branch on merge. Optional on whatever PRLookup
// is passed to InspectIssue: a lookup that does not implement it is simply not
// asked, and the squash-merge landing proof falls back to the ancestor check
// alone (see proofLanded).
type CommitPRLookup interface {
	MergedPRForSHA(repo, sha string) (*ghx.PRForCommit, error)
}

// timeoutRunner is implemented by gitx.Git for operations that must not hang
// the caller forever -- a fetch against a blackholed connection or captive
// portal never returns on its own. Optional, the same way LiveRootChecker is
// optional on LiveChecker above: a Runner fake that does not implement it
// simply runs the fetch unbounded, since fakes model failure, not a hang.
type timeoutRunner interface {
	RunTimeout(dir string, env []string, timeout time.Duration, args ...string) (string, error)
}

// fetchTimeout bounds the pre-unpushed-check fetch. Long enough for a real,
// slow-but-working origin; short enough that a hung one degrades reap/issues/
// attach to a disclaimer instead of blocking them indefinitely.
const fetchTimeout = 15 * time.Second

// InspectIssue gathers the state of one issue workspace, refusing any other.
// Network and multiplexer lookups are optional: pass nil to skip them.
func InspectIssue(ws string, git gitx.Runner, pr PRLookup, mux LiveChecker) (*IssueState, error) {
	m, err := manifest.Read(ws)
	if err != nil {
		return nil, err
	}
	if !m.IsIssueWorkspace() {
		return nil, fmt.Errorf("%s is not an issue workspace", ws)
	}
	return inspect(ws, m, git, pr, mux), nil
}

// Inspect gathers the state of any workspace, issue or long-lived, for reap.
// A long-lived workspace has no issue branch, so no pull request is looked up
// for it; in exchange its loose files are checked, because unlike an issue
// workspace it can hold files of its own beside its clones.
func Inspect(ws string, git gitx.Runner, pr PRLookup, mux LiveChecker) (*IssueState, error) {
	m, err := manifest.Read(ws)
	if err != nil {
		return nil, err
	}
	return inspect(ws, m, git, pr, mux), nil
}

func inspect(ws string, m *manifest.Manifest, git gitx.Runner, pr PRLookup, mux LiveChecker) *IssueState {
	st := &IssueState{Dir: ws, Name: m.Name, Issue: m.Issue}
	for _, dir := range sortedKeys(m.Clones) {
		p := filepath.Join(ws, dir)
		if !gitx.IsRepo(p) {
			// A dir that never got cloned holds nothing to lose. But a dir that
			// exists and simply is not a valid repo -- an interrupted clone, a
			// damaged .git -- may still carry a working tree full of edits, so it
			// must block rather than vanish from the accounting.
			if _, err := os.Stat(p); err == nil {
				st.Clone = append(st.Clone, CloneState{
					Dir: dir, Unverifiable: []string{"present but not a git repository"},
				})
			}
			continue
		}
		// The squash-merge landing proof asks GitHub about the clone's own
		// repo, read from its URL, falling back to the issue's repo. A clone
		// with neither gets the ancestor check alone, which fails in the safe
		// direction: a squash-merged commit there still counts as unpushed.
		repo, clonePR := githubRepo(m.Clones[dir]), pr
		if repo == "" && m.Issue != nil {
			repo = m.Issue.Repo
		}
		if repo == "" {
			clonePR = nil
		}
		st.Clone = append(st.Clone, inspectClone(git, dir, p, ws, repo, clonePR))
		st.Clone = append(st.Clone, submoduleStates(git, dir, p, ws)...)
	}

	st.Enclosing, st.Loose = looseFiles(git, ws, m)
	st.Unaccounted, st.UnaccountedCount = census(git, ws, ws, st.Enclosing, cloneDirs(ws, st.Clone), topAccounted(ws, m))
	if pr != nil && m.Issue != nil && m.Issue.Branch != "" {
		// A lookup that errors is not "no PR": auth expiry, a network blip, or a
		// rate limit must not silently drop the open-PR guard. Record that the
		// state is unknown so Blockers refuses.
		if found, err := pr.ViewPR(m.Issue.Repo, m.Issue.Branch); err == nil {
			st.PR = found
		} else {
			st.PRUnknown = true
		}
	}
	if mux != nil {
		st.SessionLive = mux.Live(filepath.Base(ws), ws)
	}
	st.checkRunning(ws, mux)
	st.SizeBytes = freedBytes(ws)
	return st
}

// checkRunning records the panes, processes and launchd jobs running from dir,
// asked through mux's optional interfaces.
func (s *IssueState) checkRunning(dir string, mux LiveChecker) {
	if lr, ok := mux.(LiveRootChecker); ok {
		s.LiveRoots, s.LiveRootsUnknown = lr.LiveRoots(dir)
	}
	if jc, ok := mux.(JobChecker); ok {
		s.Jobs, s.JobsUnknown = jc.JobsUnder(dir)
	}
}

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

// linkedWorktrees reads `git worktree list --porcelain`: one "worktree <path>"
// line per worktree, the main one first. It returns the others.
func linkedWorktrees(porcelain string) []string {
	var out []string
	first := true
	for _, line := range strings.Split(porcelain, "\n") {
		path, ok := strings.CutPrefix(line, "worktree ")
		if !ok {
			continue
		}
		if first {
			first = false
			continue
		}
		out = append(out, path)
	}
	return out
}

// nestedWorktrees keeps the worktrees whose directory exists strictly inside
// dir. One whose directory is gone loses nothing when dir is deleted: its
// entry and HEAD live in the checkout's git directory, which stays.
func nestedWorktrees(worktrees []string, dir string) []string {
	root := realPath(dir)
	var out []string
	for _, w := range worktrees {
		resolved := realPath(w)
		if resolved == root || !isUnder(root, resolved) {
			continue
		}
		if fi, err := os.Stat(w); err != nil || !fi.IsDir() {
			continue
		}
		out = append(out, w)
	}
	return out
}

// githubRepo returns owner/name for a GitHub clone URL, in the SSH, scp-like
// or HTTPS form, and "" for anything else.
func githubRepo(url string) string {
	var rest string
	for _, prefix := range []string{"git@github.com:", "ssh://git@github.com/", "https://github.com/"} {
		if r, ok := strings.CutPrefix(url, prefix); ok {
			rest = r
			break
		}
	}
	owner, name, ok := strings.Cut(strings.TrimSuffix(rest, ".git"), "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return ""
	}
	return owner + "/" + name
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

// inspectClone reads one clone's state. Every git probe that fails is recorded
// as an Unverifiable reason rather than dropped: a git error must block the reap,
// not masquerade as a clean, empty tree. repo is the issue's home repo
// ("owner/name"), used for the squash-merge landing proof's PR lookup; pr may
// be nil to skip that lookup (the ancestor check alone still runs).
// inspectClone judges the repository whose work tree is p, named dir. gone is
// the directory reap deletes: a remote whose repository lies inside it is no
// copy at all.
func inspectClone(git gitx.Runner, dir, p, gone, repo string, pr PRLookup) CloneState {
	c := CloneState{Dir: dir}

	// A probe that fails leaves WorktreeOf empty, so the clone is judged as a
	// repository of its own: the stricter reading, which holds on every
	// branch and stash the repository has.
	if of, err := worktreeOf(git, p); err != nil {
		c.Unverifiable = append(c.Unverifiable, fmt.Sprintf("could not tell whether it is a worktree: %v", err))
	} else {
		c.WorktreeOf = of
	}
	own := c.WorktreeOf == ""

	// core.worktree can point git at another directory, and every check of
	// files below would then read that one, never the files deleted here.
	if top, err := git.Run(p, nil, "rev-parse", "--show-toplevel"); err != nil {
		c.Unverifiable = append(c.Unverifiable, fmt.Sprintf("could not find its work tree: %v", err))
	} else if top = strings.TrimSpace(top); realPath(top) != realPath(p) {
		c.Unverifiable = append(c.Unverifiable, fmt.Sprintf("its work tree is %s (core.worktree), so git never looks at the files here; run `git config --unset core.worktree` in it, then reap again", top))
	}

	// The flags are explicit so no config can hide a file from the check:
	// status.showUntrackedFiles=no drops untracked files, and
	// submodule.<name>.ignore or diff.ignoreSubmodules drop a dirty submodule.
	// honestStatus turns off the caches that answer for the disk.
	statusArgs := append(append([]string{}, honestStatus...), "status", "--porcelain", "--untracked-files=all", "--ignore-submodules=none")
	if out, err := git.Run(p, nil, statusArgs...); err != nil {
		c.Unverifiable = append(c.Unverifiable, fmt.Sprintf("git status failed: %v", err))
	} else {
		c.Dirty = strings.TrimSpace(out) != ""
	}
	if ops, err := opsInProgress(git, p); err != nil {
		c.Unverifiable = append(c.Unverifiable, fmt.Sprintf("could not tell whether a rebase or merge is in progress: %v", err))
	} else {
		c.InProgress = ops
	}
	if hidden, err := hiddenEdits(git, p, nil); err != nil {
		c.Unverifiable = append(c.Unverifiable, fmt.Sprintf("could not look for edits git status does not show: %v", err))
	} else {
		for _, h := range hidden {
			c.Hidden = append(c.Hidden, fmt.Sprintf("%s differs from what is committed -- %s", h.path, h.why))
		}
		c.Hidden = capList(c.Hidden, "files git status does not show as changed")
	}

	// A clone with its own repository is held by every linked worktree: each
	// one's HEAD and index live in the clone's git directory. A worktree's
	// siblings, the checkout's own included, belong to the checkout's
	// repository, which removing this one leaves in place; but a worktree that
	// sits inside this one's directory (Claude Code's .claude/worktrees, made
	// from inside the clone) is deleted with it, so it holds too.
	if out, err := git.Run(p, nil, "worktree", "list", "--porcelain"); err != nil {
		c.Unverifiable = append(c.Unverifiable, fmt.Sprintf("git worktree list failed: %v", err))
	} else if own {
		c.Worktrees = linkedWorktrees(out)
	} else {
		c.Worktrees = nestedWorktrees(linkedWorktrees(out), p)
	}

	if repos, stray, err := unjudgedRepos(git, p, c.Worktrees); err != nil {
		c.Unverifiable = append(c.Unverifiable, fmt.Sprintf("could not look for repositories inside it: %v", err))
	} else {
		c.InnerRepos, c.StrayGitlinks = repos, stray
	}
	if files, err := unrebuiltIgnored(git, p, append(append([]string{}, c.Worktrees...), c.InnerRepos...)); err != nil {
		c.Unverifiable = append(c.Unverifiable, fmt.Sprintf("could not list the files it ignores: %v", err))
	} else {
		c.IgnoredFiles = files
	}

	// The branch name is informational only, so a failure here is not a safety
	// signal and is not recorded as a blocker.
	if out, err := git.Run(p, nil, "rev-parse", "--abbrev-ref", "HEAD"); err == nil {
		c.Branch = out
	}

	// Refresh remote-tracking refs before judging unpushed-ness: a clone that
	// hasn't talked to a remote since a PR branch was merged and deleted there
	// would otherwise judge those commits against a stale ref and count them as
	// pushed, or as this workspace's only copy. Every remote is fetched, since
	// every remote's refs count as pushed below. A fetch that fails (no network,
	// an unreachable remote) or hangs is not fatal -- the count below still runs
	// against whatever refs are on disk -- but it is recorded so Blockers and
	// Notes can disclose it.
	if stale, err := fetchRemotes(git, p); err != nil {
		c.Unverifiable = append(c.Unverifiable, fmt.Sprintf("could not list its remotes: %v", err))
	} else {
		c.StaleRemotes = stale
	}
	pushed, err := countedRemotes(git, p, gone)
	if err != nil {
		c.Unverifiable = append(c.Unverifiable, fmt.Sprintf("could not tell which remotes hold its commits: %v", err))
	}
	if holds, err := gitDirHolds(git, p, gone); err != nil {
		c.Unverifiable = append(c.Unverifiable, fmt.Sprintf("could not read its git directory: %v", err))
	} else {
		c.Unverifiable = append(c.Unverifiable, holds...)
	}

	// Commits reachable from a local ref but from no counted remote. Every ref
	// is local except refs/remotes, refs/tags, refs/notes and the stash, which
	// is counted below (walkedRefs): HEAD, so a commit on a detached HEAD
	// counts, the branches, the per-worktree refs, refs/replace, and any
	// namespace git or a tool adds later. This is a cheap fast-path check: the
	// common case is 0, and only then is the more expensive per-ref landing
	// proof run.
	//
	// In a worktree the shared refs are the checkout's, which outlive the
	// worktree, so only HEAD and the per-worktree refs are counted there: they
	// live in the worktree's own admin directory and go with it.
	refs, err := walkedRefs(git, p, own)
	if err != nil {
		c.Unverifiable = append(c.Unverifiable, fmt.Sprintf("could not list its refs: %v", err))
	}
	c.Unverifiable = append(c.Unverifiable, refs.odd...)
	countArgs := append([]string{"rev-list", "--count"}, refs.countArgs...)
	countArgs = append(countArgs, pushed.not()...)
	if out, err := git.Run(p, nil, countArgs...); err != nil {
		c.Unverifiable = append(c.Unverifiable, fmt.Sprintf("git rev-list failed: %v", err))
	} else if _, err := strconv.Atoi(strings.TrimSpace(out)); err != nil {
		c.Unverifiable = append(c.Unverifiable, fmt.Sprintf("could not read the unpushed-commit count: %v", err))
	} else if strings.TrimSpace(out) != "0" {
		// A sha not reachable from any remote is not the same question as
		// "is this content already upstream": squash-merge rewrites the sha,
		// and GitHub's auto-delete-on-merge then removes the only ref that
		// could have proven it by name. Prove landing per ref instead.
		var commitPR CommitPRLookup
		if pr != nil {
			commitPR, _ = pr.(CommitPRLookup)
		}
		if unpushed, landed, defUnresolved, err := unpushedLanding(git, p, repo, commitPR, refs.names, pushed); err != nil {
			c.Unverifiable = append(c.Unverifiable, fmt.Sprintf("could not verify unpushed commits: %v", err))
		} else {
			c.Unpushed = unpushed
			c.SquashLanded = landed
			c.DefaultBranchUnresolved = defUnresolved
		}
	}

	// A worktree's stash belongs to the checkout's repository and stays with it.
	if !own {
		return c
	}
	if count, err := gitDirStash(git, p); err != nil {
		c.Unverifiable = append(c.Unverifiable, fmt.Sprintf("could not count its stash: %v", err))
	} else {
		c.Stashed = count
	}

	return c
}

// unpushedLanding walks the local refs names lists (walkedRefs) and decides,
// per ref, whether its not-yet-remote commits are genuinely unpushed or have already landed on the remote's default branch
// under a different sha (the squash-merge, auto-delete-on-merge shape
// facet#66 is about). It replaces an earlier per-commit patch-identity design
// (`git cherry`): that design could never recognize a multi-commit branch
// squashed into a single commit, because no individual commit's patch matches
// the combined diff -- an audited defect that made the fix work only for
// one-commit branches, the minority of real PRs.
//
// Ported from an earlier teardown script, which had already reached this
// design under real, repeated failures (facet#66).
//
// repo is the issue's home repo, needed for the merged-PR lookup; commitPR may
// be nil, in which case only the ancestor check runs and a squash-merge is not
// detected (fails safe toward "unpushed", never toward silently clearing a
// blocker).
//
// The counts are over DISTINCT COMMITS, not a sum of per-ref answers. A commit
// reachable from more than one local branch -- `git branch backup` before a
// rebase is enough -- was counted once per branch, so a workspace holding the
// only copy of one commit was told three commits were at risk (facet#77). The
// classification is still per ref, because "landed" is a property of the ref
// that was proven, not of an individual sha; only the accounting is a set.
//
// names are full ref names, and "HEAD" when HEAD's branch is not listed, so no
// branch or tag of the same name stands in for a ref.
func unpushedLanding(git gitx.Runner, dir, repo string, commitPR CommitPRLookup, names []string, pushed pushedRefs) (unpushed, landed int, defUnresolved bool, err error) {
	// Only a counted origin's default branch proves anything landed.
	def := ""
	if pushed.origin {
		def = remoteDefaultBranch(git, dir)
	}

	current, _, err := currentRef(git, dir)
	if err != nil {
		return 0, 0, false, err
	}

	unpushedShas := map[string]struct{}{}
	landedShas := map[string]struct{}{}
	for _, ref := range names {
		out, err := git.Run(dir, nil, append([]string{"rev-list", ref}, pushed.not()...)...)
		if err != nil {
			return 0, 0, false, err
		}
		shas := strings.Fields(out)
		if len(shas) == 0 {
			// This ref's commits are all already reachable from some remote
			// (pushed to its own branch, say) -- nothing to prove for it, and
			// nothing to add.
			continue
		}
		// A candidate exists, so the landing proof genuinely needed def. An
		// unresolvable default branch is only worth disclosing once it has
		// actually decided something (facet#80).
		defUnresolved = def == ""
		shaOut, err := git.Run(dir, nil, "rev-parse", ref)
		if err != nil {
			return 0, 0, false, err
		}
		tip := strings.TrimSpace(shaOut)
		isHead := ref == current || ref == "HEAD"
		into := unpushedShas
		if proofLanded(git, dir, tip, repo, def, commitPR, isHead) {
			into = landedShas
		}
		for _, s := range shas {
			into[s] = struct{}{}
		}
	}

	// A commit reachable from both a ref proven landed and one that is not
	// counts as UNPUSHED. The landing proof ran against the other ref's tip and
	// says nothing about a ref carrying further work on top, so the overlap is
	// resolved in the direction that holds the workspace -- and it keeps
	// SquashLanded's contract true: those commits are excluded from Unpushed,
	// never reported under both.
	for s := range unpushedShas {
		delete(landedShas, s)
	}
	return len(unpushedShas), len(landedShas), defUnresolved, nil
}

// proofLanded answers whether sha's content has already landed on def (the
// remote's resolved default branch, "" if unresolvable). Three independent
// checks; isHead selects which apply.
//
// isHead is true for the checked-out ref (a branch, or a detached HEAD -- the
// shape every auditor workspace is in, since auditing checks out the audited
// sha rather than a branch tip) and requires all three checks. isHead is false
// for a leftover local branch the workspace happens to still be carrying
// (an auditor's own working branch, say); that only needs the first two,
// because its content is allowed to differ from def -- a later round inside the
// same PR can supersede it, and requiring it to match def would then wrongly
// refuse a correct deletion.
func proofLanded(git gitx.Runner, dir, sha, repo, def string, commitPR CommitPRLookup, isHead bool) bool {
	if def == "" {
		return false
	}

	// (a) sha is already an ancestor of def -- no squash happened, and there
	// is nothing left to prove.
	if _, err := git.Run(dir, nil, "merge-base", "--is-ancestor", sha, def); err == nil {
		return true
	}

	// (b) a MERGED pull request had sha as a commit. With no lookup available,
	// nothing beyond (a) can be proven, and that is the safe direction to
	// fail in: the commit stays counted as unpushed.
	if commitPR == nil {
		return false
	}
	merged, err := commitPR.MergedPRForSHA(repo, sha)
	if err != nil || merged == nil {
		return false
	}
	if !isHead {
		return true
	}

	// (c) every file this ref itself touched must still read on def exactly as
	// it does here. A merged PR whose file then diverged on def would still be
	// a loss, and (b) alone would have called it safe -- that is why this check
	// exists and the property is unchanged.
	//
	// What changed (facet#78) is the scope. This compared the WHOLE tree to def,
	// so a squash-landed workspace reaped cleanly only until the next unrelated
	// pull request merged; from that moment it went back to claiming it held the
	// only copy of work demonstrably on def. Nothing about the workspace had
	// changed. On a repo where merges are frequent the clean window is minutes,
	// and a guard that cries wolf on the ordinary path teaches --force -- the one
	// flag that can actually destroy unpushed work.
	//
	// Asking "is MY content still on def" rather than "is the whole tree still
	// what I left" narrows the question to the files this ref changed, and
	// nothing else. A sibling touching some other file is no longer this
	// workspace's problem; a sibling touching one of these files still refuses.
	base, err := git.Run(dir, nil, "merge-base", sha, def)
	if err != nil {
		return false
	}
	names, err := git.Run(dir, nil, "diff", "--name-only", "-z", strings.TrimSpace(base), sha)
	if err != nil {
		return false
	}
	// -z because a path may contain anything but NUL, and a filename with a
	// newline in it must not be read as two paths -- one of which would then
	// name nothing and quietly narrow the comparison.
	var files []string
	for _, f := range strings.Split(names, "\x00") {
		if f != "" {
			files = append(files, f)
		}
	}
	if len(files) == 0 {
		// This ref changed no file relative to the merge base, so there is no
		// content of its own left to lose -- (a) and (b) have already proven
		// everything there is to prove. Guarded explicitly because an empty
		// pathspec would make the diff below compare the entire tree again,
		// silently restoring the behaviour this replaced.
		return true
	}
	diff, err := git.Run(dir, nil, append([]string{"diff", "--stat", sha, def, "--"}, files...)...)
	if err != nil {
		return false
	}
	return strings.TrimSpace(diff) == ""
}

// currentRef reports the checked-out branch name, or detached=true if HEAD is
// not on any branch.
func currentRef(git gitx.Runner, dir string) (branch string, detached bool, err error) {
	out, err := git.Run(dir, nil, "symbolic-ref", "--quiet", "HEAD")
	if err != nil {
		// Exit 1 here means "detached", not a failure -- HEAD simply is not a
		// symbolic ref. Anything else is a genuine problem reading the repo.
		var gitErr *gitx.Error
		if errors.As(err, &gitErr) && gitErr.ExitCode == 1 {
			return "", true, nil
		}
		return "", false, err
	}
	return strings.TrimSpace(out), false, nil
}

// remoteDefaultBranch resolves origin's default branch as its full ref,
// "refs/remotes/origin/<name>", or "" if it cannot be determined -- in which
// case callers must not guess. Never the short "origin/main": git resolves
// that to a local branch or tag of that name first, and a local branch named
// origin/main would then pass the landing proof with commits no remote has.
func remoteDefaultBranch(git gitx.Runner, dir string) string {
	if out, err := git.Run(dir, nil, "symbolic-ref", "refs/remotes/origin/HEAD"); err == nil {
		if s := strings.TrimSpace(out); strings.HasPrefix(s, "refs/remotes/origin/") {
			return s
		}
	}
	// refs/remotes/origin/HEAD is set by a normal clone, but is not
	// guaranteed (a hand-built remote, an old fetch config). "main" is the
	// default branch of every repository facet has been used on, not a bare
	// guess, so fall back to it if it exists.
	if _, err := git.Run(dir, nil, "rev-parse", "--verify", "refs/remotes/origin/main"); err == nil {
		return "refs/remotes/origin/main"
	}
	return ""
}

// ListIssues inspects every issue workspace under the workspaces root. One that
// cannot be inspected (a broken manifest) is not listed, but named in unchecked
// with the reason, so a listing never reads as complete when it is not.
func ListIssues(roots config.Roots, git gitx.Runner, pr PRLookup, mux LiveChecker) (out []*IssueState, unchecked []string, err error) {
	entries, err := os.ReadDir(roots.Workspaces)
	if err != nil {
		return nil, nil, err
	}
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), config.IssuePrefix) {
			continue
		}
		dir := filepath.Join(roots.Workspaces, e.Name())
		if _, err := os.Stat(manifest.Path(dir)); err != nil {
			continue
		}
		st, err := InspectIssue(dir, git, pr, mux)
		if err != nil {
			unchecked = append(unchecked, fmt.Sprintf("%s: %v", e.Name(), err))
			continue
		}
		out = append(out, st)
	}
	return out, unchecked, nil
}

// Reap deletes an issue workspace. It never touches the shared mirror: removing a
// hardlinked object file only drops that name, and the mirror keeps its own.
//
// Callers must check Blockers() first; Reap does not, so that --force has
// somewhere to go.
//
// A clone that is a worktree of a checkout is removed through that checkout
// first, so the checkout forgets it; its branch stays there.
func Reap(st *IssueState, git gitx.Runner) error {
	// reap is meant to be run from inside the workspace it deletes. Step out
	// first, so no git this process runs afterwards starts in a directory that
	// no longer exists.
	if wd, err := os.Getwd(); err == nil && isUnder(st.Dir, wd) {
		if err := os.Chdir(filepath.Dir(st.Dir)); err != nil {
			return fmt.Errorf("step out of %s before deleting it: %w", st.Dir, err)
		}
	}
	if st.Worktree {
		return removeWorktree(git, st.Clone[0].WorktreeOf, st.Dir)
	}
	for _, c := range st.Clone {
		if c.WorktreeOf == "" {
			continue
		}
		p := filepath.Join(st.Dir, c.Dir)
		// A clone entry that is a symlink is a link, not a clone: git would
		// follow it and remove the worktree it points at, outside the
		// workspace. removeAllForce below unlinks it and follows nothing.
		if fi, err := os.Lstat(p); err == nil && fi.Mode()&os.ModeSymlink != 0 {
			continue
		}
		if !isUnder(realPath(st.Dir), realPath(p)) {
			return fmt.Errorf("the clone %s leads to %s, outside the workspace; left alone\nfix: remove the link by hand, then reap again", p, realPath(p))
		}
		if err := removeWorktree(git, c.WorktreeOf, p); err != nil {
			return err
		}
	}
	return removeAllForce(st.Dir)
}

// isUnder reports whether path is root or lies beneath it.
func isUnder(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	// A name that only starts with ".." (`..wt`) is inside: only ".." itself,
	// or a path through it, leads out.
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// removeAllForce deletes a tree that may hold directories without write
// permission (a Go module cache, a tool that makes its output read-only), which
// plain os.RemoveAll cannot empty.
//
// DIRECTORIES ONLY. Deleting a file needs write permission on its directory,
// never on the file, so a file's own mode is left alone -- and it must be: a
// clone's objects are hardlinks to the mirror's, so a chmod on a pack here is a
// chmod on the mirror's pack (it once made every mirror pack writable on each
// reap). A directory is never hardlinked, so making one writable touches
// nothing outside the tree.
//
// WalkDir stats with Lstat, so a symlink to a directory arrives with IsDir() ==
// false and is skipped; os.Chmod follows symlinks and would otherwise change the
// target, outside the tree (facet#87).
func removeAllForce(root string) error {
	makeDirsWritable(root)
	return os.RemoveAll(root)
}

// makeDirsWritable gives the owner full permission on every directory in the
// tree at root, and changes nothing else; see removeAllForce for why only
// directories, and why a symlink is never followed. What it cannot read or
// change is left for the delete that follows to report.
func makeDirsWritable(root string) {
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if info, err := d.Info(); err == nil && info.Mode().Perm()&0o700 != 0o700 {
				_ = os.Chmod(path, info.Mode().Perm()|0o700)
			}
		}
		return nil
	})
}
