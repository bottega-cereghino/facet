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

// orderQueued returns issues in the order the queue should start them.
// oldest is createdAt, then repository, then number. milestone uses that
// same order inside a milestone, and ranks a dated milestone before an
// undated one, and any milestone before none.
func orderQueued(issues []queuedIssue, order string) []queuedIssue {
	out := append([]queuedIssue(nil), issues...)
	// Stable, so a tie that issueBefore does not break stays in the order
	// the issues were listed, which is the order of repos in the config.
	sort.SliceStable(out, func(i, j int) bool {
		return issueBefore(out[i], out[j], order)
	})
	return out
}

func issueBefore(a, b queuedIssue, order string) bool {
	if order == queueOrderMilestone {
		if c := compareMilestone(a.Issue.Milestone, b.Issue.Milestone); c != 0 {
			return c < 0
		}
	}
	if c := compareCreated(a.Issue.CreatedAt, b.Issue.CreatedAt); c != 0 {
		return c < 0
	}
	if a.Repo != b.Repo {
		return a.Repo < b.Repo
	}
	return a.Issue.Number < b.Issue.Number
}

// compareMilestone reports whether a should be taken before b.
// A nil milestone sorts last. A due date sorts before none, earlier first.
// With the same due-date standing, the lower milestone number wins, then the title.
func compareMilestone(a, b *ghx.Milestone) int {
	if a == nil && b == nil {
		return 0
	}
	if a == nil {
		return 1
	}
	if b == nil {
		return -1
	}
	ad, aok := milestoneDue(a)
	bd, bok := milestoneDue(b)
	switch {
	case aok && bok:
		if ad.Before(bd) {
			return -1
		}
		if bd.Before(ad) {
			return 1
		}
	case aok:
		return -1
	case bok:
		return 1
	}
	if a.Number != b.Number {
		if a.Number < b.Number {
			return -1
		}
		return 1
	}
	return strings.Compare(a.Title, b.Title)
}

func milestoneDue(m *ghx.Milestone) (time.Time, bool) {
	if m == nil || m.DueOn == nil || *m.DueOn == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, *m.DueOn)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

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
