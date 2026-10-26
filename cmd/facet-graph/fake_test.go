package main

import (
	"fmt"
	"strconv"

	"github.com/RiccardoCereghino/facet/internal/ghx"
)

// A copy of cmd/facet's fakeGH (file_test.go), kept here so this frozen binary
// does not share test code with facet.

// fakeGH is a scripted ghx.Client. It records every IssueID/AddBlockedBy call
// so tests can assert exactly which edges runFile attempted, without ever
// shelling out to gh.
type fakeGH struct {
	addedLabels []string
	addLabelErr error

	removedLabels  []string
	removeLabelErr error

	// The repository-side label definitions, which is what AddLabel needs to
	// already exist. nil means "defines nothing", which is only ever read by
	// the tests that care.
	// labelNotFound makes AddLabel fail the way gh does for a label the
	// repository does not define, and succeed once it does.
	labelNotFound  bool
	repoLabels     map[string][]ghx.RepoLabel
	repoLabelErrs  map[string]error
	createdLabels  []string
	createLabelErr error
	createURL      string

	issueIDs  map[string]int64 // "owner/repo#n" -> id
	issueErrs map[string]error // "owner/repo#n" -> error from IssueID

	addBlockedByCalls []string // "repo#number<-id" per call
	addBlockedByErr   error

	// auth is what Auth returns. Nil means "a sound credential": tests that are
	// not about the preflight should not have to script one, and the zero value
	// of a scripted status is logged out, which would fail every one of them.
	auth *ghx.AuthStatus

	// The issue graph. Keys are "owner/repo#n" throughout.
	parents    map[string]ghx.IssueRef   // a miss means "no parent", not an error
	parentErrs map[string]error          // "could not tell", which is a third state
	children   map[string][]ghx.IssueRef // eventually consistent in reality
	childErrs  map[string]error

	addSubIssueCalls []string // "repo#number<-childID" per call
	addSubIssueErr   error

	removeSubIssueCalls []string // "repo#number<-childID" per call
	removeSubIssueErr   error

	// The dependency graph, which is not the issue graph.
	blockedBy map[string][]ghx.IssueRef
	blocking  map[string][]ghx.IssueRef

	comments       map[string][]ghx.Comment
	postedComments []string // "repo#number: body" per call
	editedComments []string // "commentID: body" per call

	statuses    map[string]string // "owner/repo#n" -> board status
	statusesErr error
}

// Auth returns the scripted status, defaulting to a credential that satisfies
// ghx.DefaultRequirements so unrelated tests are unaffected by the preflight.
func (f *fakeGH) Auth() (*ghx.AuthStatus, error) {
	if f.auth != nil {
		return f.auth, nil
	}
	return &ghx.AuthStatus{
		Host: "github.com", State: ghx.StateConfirmed,
		Account: "RiccardoCereghino", Active: true,
		TokenType: "ghp_", Scopes: []string{"read:org", "repo", "workflow"},
		GitProtocol: "ssh", ConfigSource: "/dev/null",
	}, nil
}

func (f *fakeGH) key(repo string, number int) string {
	return repo + "#" + strconv.Itoa(number)
}

func (f *fakeGH) ViewIssue(_ string, _ int) (*ghx.Issue, error) { return nil, nil }
func (f *fakeGH) DevelopBranch(_ string, _ int, _, _ string) (string, error) {
	return "", nil
}
func (f *fakeGH) BranchesFor(_ string, _ int) ([]string, error) { return nil, nil }
func (f *fakeGH) ViewPR(_, _ string) (*ghx.PR, error)           { return nil, nil }
func (f *fakeGH) MergedPRForSHA(_, _ string) (*ghx.PRForCommit, error) {
	return nil, nil
}
func (f *fakeGH) SetIssueStatus(_ ghx.ProjectTarget, _ string) error { return nil }
func (f *fakeGH) SetIssueBody(_ string, _ int, _ string) error       { return nil }
func (f *fakeGH) SearchIssues(_, _ string) ([]ghx.Issue, error)      { return nil, nil }
func (f *fakeGH) CreateIssue(_, _, _ string, _ []string) (string, error) {
	return f.createURL, nil
}

// IssueID errors on a map miss, matching the real CLI's behaviour on a
// nonexistent or inaccessible issue (a 404 via `gh api`) -- a test relying on
// "an unscripted issue is skipped" must see the same failure mode the real
// client produces, not a fake id of 0 that happens to also look like an error
// path today but wouldn't catch a caller that started treating 0 as valid.
func (f *fakeGH) IssueID(repo string, number int) (int64, error) {
	k := f.key(repo, number)
	if err, ok := f.issueErrs[k]; ok {
		return 0, err
	}
	id, ok := f.issueIDs[k]
	if !ok {
		return 0, fmt.Errorf("fakeGH.IssueID: no issue id scripted for %s", k)
	}
	return id, nil
}

// addedLabels records every label write, so a test can assert that `tree wire`
// RECORDED the level rather than merely derived it.
func (f *fakeGH) AddLabel(repo string, number int, label string) error {
	// The facet#139 case, scripted SEPARATELY from addLabelErr because the two
	// must be told apart without reading gh's sentence: a label the repository
	// has not DEFINED cannot be applied, and can be applied the moment it is.
	if f.labelNotFound && !f.definesLabel(repo, label) {
		return fmt.Errorf("failed to update %s#%d: '%s' not found", repo, number, label)
	}
	f.addedLabels = append(f.addedLabels, f.key(repo, number)+"+"+label)
	return f.addLabelErr
}

// RemoveLabel records every removal, so a test can assert `tree wire` dropped
// the label of the rung a moved node left.
func (f *fakeGH) RemoveLabel(repo string, number int, label string) error {
	f.removedLabels = append(f.removedLabels, f.key(repo, number)+"-"+label)
	return f.removeLabelErr
}

func (f *fakeGH) definesLabel(repo, label string) bool {
	for _, l := range f.repoLabels[repo] {
		if l.Name == label {
			return true
		}
	}
	return false
}

func (f *fakeGH) AddBlockedBy(repo string, number int, blockingID int64) error {
	f.addBlockedByCalls = append(f.addBlockedByCalls, f.key(repo, number)+"<-"+strconv.FormatInt(blockingID, 10))
	return f.addBlockedByErr
}

// --- the issue graph -------------------------------------------------------
//
// Scripted separately from the dependency edges above because they are
// different graphs: parents say what a thing is part of, blockers say what must
// land first. A fake that conflated them would let a test pass that only works
// because the two happen to agree.

// IssueParent returns the scripted parent. A miss is "asked, and there is
// none" -- NOT an error -- because that is the honest majority case and the
// one the no-parent-is-valid rule turns on. An unreadable parent is scripted
// explicitly via parentErrs.
func (f *fakeGH) IssueParent(repo string, number int) (ghx.IssueRef, bool, error) {
	k := f.key(repo, number)
	if err, ok := f.parentErrs[k]; ok {
		return ghx.IssueRef{}, false, err
	}
	p, ok := f.parents[k]
	return p, ok, nil
}

// IssueChildren returns each scripted child ref with no title/state/labels
// attached -- fakeGH's own ViewIssue is a no-op stub, so a bare fakeGH has
// never been able to name a child's fields; the empty State reads as
// "unreadable" exactly as it did before facet#105, when a child's Node came
// from a separate node()/ViewIssue call that this same stub also blanked.
// treeFake (below) overrides this to attach real fields from its own issues
// map.
// OpenIssueParents is the complement read. The default answer is an empty
// repository rather than an error, so the tests that are not about orphans do
// not have to script one.
func (f *fakeGH) OpenIssueParents(_ string) ([]ghx.Parentage, error) { return nil, nil }

// The label DEFINITIONS in a repository, as opposed to the labels an issue
// carries. A test that does not script them gets a repository defining every
// label the structure declares -- the state facet#139 says is not universal,
// but is the right default for the tests that are not about it.
func (f *fakeGH) RepoLabels(repo string) ([]ghx.RepoLabel, error) {
	if err, ok := f.repoLabelErrs[repo]; ok {
		return nil, err
	}
	if f.repoLabels == nil {
		return nil, nil
	}
	return f.repoLabels[repo], nil
}

func (f *fakeGH) CreateLabel(repo string, l ghx.RepoLabel) error {
	f.createdLabels = append(f.createdLabels, repo+": "+l.Name+" #"+l.Color)
	if f.createLabelErr != nil {
		return f.createLabelErr
	}
	if f.repoLabels == nil {
		f.repoLabels = map[string][]ghx.RepoLabel{}
	}
	f.repoLabels[repo] = append(f.repoLabels[repo], l)
	return nil
}

func (f *fakeGH) IssueChildren(repo string, number int) ([]ghx.SubIssue, error) {
	k := f.key(repo, number)
	if err, ok := f.childErrs[k]; ok {
		return nil, err
	}
	refs := f.children[k]
	out := make([]ghx.SubIssue, 0, len(refs))
	for _, r := range refs {
		out = append(out, ghx.SubIssue{Ref: r})
	}
	return out, nil
}

func (f *fakeGH) AddSubIssue(repo string, number int, childID int64) error {
	f.addSubIssueCalls = append(f.addSubIssueCalls,
		f.key(repo, number)+"<-"+strconv.FormatInt(childID, 10))
	return f.addSubIssueErr
}

func (f *fakeGH) RemoveSubIssue(repo string, number int, childID int64) error {
	f.removeSubIssueCalls = append(f.removeSubIssueCalls,
		f.key(repo, number)+"<-"+strconv.FormatInt(childID, 10))
	return f.removeSubIssueErr
}

func (f *fakeGH) BlockedBy(repo string, number int) ([]ghx.IssueRef, error) {
	return f.blockedBy[f.key(repo, number)], nil
}

func (f *fakeGH) Blocking(repo string, number int) ([]ghx.IssueRef, error) {
	return f.blocking[f.key(repo, number)], nil
}

func (f *fakeGH) IssueComments(repo string, number int) ([]ghx.Comment, error) {
	return f.comments[f.key(repo, number)], nil
}

func (f *fakeGH) PostComment(repo string, number int, body string) (string, error) {
	f.postedComments = append(f.postedComments, f.key(repo, number)+": "+body)
	return "https://github.com/" + f.key(repo, number) + "-comment", nil
}

func (f *fakeGH) EditComment(_ string, commentID int64, body string) (string, error) {
	f.editedComments = append(f.editedComments,
		strconv.FormatInt(commentID, 10)+": "+body)
	return "https://github.com/edited-comment", nil
}

func (f *fakeGH) ProjectStatuses(_ string, _ int, _ string) (map[string]string, error) {
	return f.statuses, f.statusesErr
}

var _ ghx.Client = (*fakeGH)(nil)
