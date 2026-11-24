package workspace

import (
	"errors"
	"strings"

	"github.com/bottega-cereghino/facet/internal/gitx"
)

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
//
// A symbolic ref is not a default branch either. fetch --prune never deletes
// one and does not overwrite it, so refs/remotes/origin/HEAD can name
// refs/remotes/origin/main while that name points at refs/heads/wip. git
// symbolic-ref follows the chain, and rev-parse --verify follows it too, so
// either name would make the landing proof compare that branch with itself
// (#408). Only a ref symbolic-ref -q refuses to read as symbolic is returned.
func remoteDefaultBranch(git gitx.Runner, dir string) string {
	if out, err := git.Run(dir, nil, "symbolic-ref", "refs/remotes/origin/HEAD"); err == nil {
		if s := strings.TrimSpace(out); strings.HasPrefix(s, "refs/remotes/origin/") && !symbolicRef(git, dir, s) {
			return s
		}
	}
	// refs/remotes/origin/HEAD is set by a normal clone, but is not
	// guaranteed (a hand-built remote, an old fetch config). "main" is the
	// default branch of every repository facet has been used on, not a bare
	// guess, so fall back to it when it exists as a direct ref.
	const main = "refs/remotes/origin/main"
	if _, err := git.Run(dir, nil, "rev-parse", "--verify", main); err == nil && !symbolicRef(git, dir, main) {
		return main
	}
	return ""
}

// symbolicRef reports whether git symbolic-ref -q reads ref as a symbolic
// ref. A probe that fails for any other reason counts as one too: a default
// branch that cannot be told from a symref must not clear the landing proof.
func symbolicRef(git gitx.Runner, dir, ref string) bool {
	_, err := git.Run(dir, nil, "symbolic-ref", "-q", ref)
	if err == nil {
		return true
	}
	var gitErr *gitx.Error
	return !errors.As(err, &gitErr) || gitErr.ExitCode != 1
}
