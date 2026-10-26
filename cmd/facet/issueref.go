package main

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/RiccardoCereghino/facet/internal/ghx"
	"github.com/RiccardoCereghino/facet/internal/routing"
	"github.com/RiccardoCereghino/facet/internal/scope"
)

// The helpers below are shared with cmd/facet-graph, which keeps its own copy:
// that binary is frozen, and the two should not be coupled through a package
// that exists only to hold them.

// loadRouting reads the routing file, treating its ABSENCE as an empty one.
//
// The issue commands read GitHub, not the lab: the routing file only ever
// tells them which comment kinds exist, and having none is a legitimate state.
//
// A routing file that EXISTS and is malformed still refuses. "There is no
// configuration" and "the configuration is broken" are different answers, and
// only the first is ordinary.
func loadRouting() (*routing.Routing, error) {
	route, err := routing.Load(roots.Routing)
	if errors.Is(err, os.ErrNotExist) {
		return &routing.Routing{}, nil
	}
	return route, err
}

// parseIssueRef reads "owner/repo#n".
//
// The validation is scope.ParseRef's, reused rather than written again so the
// two cannot drift on what a well-formed reference is. Only the sentence is
// this command's own: that function's wording names a scope file, which would
// send someone reading a refusal to look at entirely the wrong thing.
func parseIssueRef(s string) (ghx.IssueRef, error) {
	r, err := scope.ParseRef(s)
	if err != nil {
		return ghx.IssueRef{}, fmt.Errorf("%q is not an issue reference\nfix: write it as owner/repo#123", s)
	}
	owner, name, _ := strings.Cut(r.Repo, "/")
	return ghx.IssueRef{Owner: owner, Repo: name, Number: r.Number}, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
