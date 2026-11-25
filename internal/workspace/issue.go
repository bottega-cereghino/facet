package workspace

import (
	"time"

	"github.com/bottega-cereghino/facet/internal/ghx"
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
	// finish within fetchTimeout, before Unpushed was computed. When non-empty,
	// Blockers holds the workspace because a branch deleted or force-pushed on
	// that remote since the last fetch would still read as pushed.
	StaleRemotes []string
	// DefaultBranchUnresolved is set when origin's default branch could not be
	// determined -- no refs/remotes/origin/HEAD and no direct origin/main, which
	// is a hand-built remote, an old fetch config, a repo whose default is not
	// main, or a symref standing where that direct ref should be (#408) -- *and*
	// there were unpushed candidates that needed it. Nothing can be proven to
	// have landed on a branch that cannot be named, so proofLanded fails safe
	// and every candidate is counted unpushed. Without this the operator is
	// told about unpushed commits and never about the real cause (facet#80).
	// It is a disclaimer on that line and never a blocker of
	// its own: it is set only when the fail-safe actually decided something, so
	// a clean workspace on such a remote still reaps.
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
	// PRErr is the error of the failed lookup behind PRUnknown, or of a lookup
	// reap --closed ran itself. Nil when no lookup failed.
	PRErr error
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
