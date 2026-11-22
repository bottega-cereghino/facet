package main

import (
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/bottega-cereghino/facet/internal/ghx"
)

// queuedIssue is one open issue plus the repository it was listed from.
// The issue payload does not carry the repository.
type queuedIssue struct {
	Repo  string
	Issue ghx.Issue
}

func (q queuedIssue) key() string {
	return q.Repo + "#" + strconv.Itoa(q.Issue.Number)
}

// orderQueued returns issues in the order the queue starts them.
// An issue labelled p1 comes before p2, p2 before p3, and p3 before an
// issue with none of those. Within one of those groups the oldest
// createdAt is first, then the repository name, then the issue number.
// The order is fixed: a milestone does not move an issue.
func orderQueued(issues []queuedIssue) []queuedIssue {
	out := append([]queuedIssue(nil), issues...)
	// Stable, so a tie that issueBefore does not break stays in the order
	// the issues were listed, which is the order of repos in the config.
	sort.SliceStable(out, func(i, j int) bool {
		return issueBefore(out[i], out[j])
	})
	return out
}

func issueBefore(a, b queuedIssue) bool {
	if c := comparePriority(a.Issue, b.Issue); c != 0 {
		return c < 0
	}
	if c := compareCreated(a.Issue.CreatedAt, b.Issue.CreatedAt); c != 0 {
		return c < 0
	}
	if a.Repo != b.Repo {
		return a.Repo < b.Repo
	}
	return a.Issue.Number < b.Issue.Number
}

// comparePriority ranks p1, then p2, then p3, then anything else.
// An issue that carries more than one of those labels takes the earliest.
// The match ignores case. A label such as p10 is not p1.
func comparePriority(a, b ghx.Issue) int {
	return priorityRank(a) - priorityRank(b)
}

func priorityRank(iss ghx.Issue) int {
	best := len(queuePriorities)
	for _, name := range iss.LabelNames() {
		for i, p := range queuePriorities {
			if strings.EqualFold(name, p) && i < best {
				best = i
			}
		}
	}
	return best
}

// queuePriorities is the order the queue starts work, earliest first.
var queuePriorities = []string{"p1", "p2", "p3"}

func compareCreated(a, b string) int {
	at, aerr := time.Parse(time.RFC3339, a)
	bt, berr := time.Parse(time.RFC3339, b)
	switch {
	case aerr == nil && berr == nil:
		if at.Before(bt) {
			return -1
		}
		if bt.Before(at) {
			return 1
		}
		return 0
	case aerr == nil:
		return -1
	case berr == nil:
		return 1
	default:
		return strings.Compare(a, b)
	}
}
