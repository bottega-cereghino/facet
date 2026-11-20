package ghx

import (
	"fmt"
	"strings"
)

// IssueRef names one issue anywhere on the forge. References in issue bodies
// and on the command line routinely cross repositories, so a bare number is
// never enough to identify one.
type IssueRef struct {
	Owner  string
	Repo   string
	Number int
}

// String renders the ref the way issue bodies and humans write it.
func (r IssueRef) String() string {
	return fmt.Sprintf("%s/%s#%d", r.Owner, r.Repo, r.Number)
}

// OwnerRepo is the "owner/name" half, which is what every other method here
// takes as its repo argument.
func (r IssueRef) OwnerRepo() string { return r.Owner + "/" + r.Repo }

// splitRepo takes "owner/name" apart, refusing anything else rather than
// guessing -- a half-formed repo silently becomes a 404 three calls later.
func splitRepo(what, repo string) (owner, name string, err error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" {
		return "", "", fmt.Errorf("%s: %q is not owner/repo", what, repo)
	}
	return owner, name, nil
}
