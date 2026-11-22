package main

import (
	"fmt"
	"os"
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
// projects is the priority file, highest first. A repository named there
// comes before one named later, and before one the file does not name.
// Within a project, p1 comes before p2, p2 before p3, and p3 before an
// issue with none of those. Within one of those groups the oldest
// createdAt is first, then the repository name, then the issue number.
// A milestone does not move an issue.
func orderQueued(issues []queuedIssue, projects []string) []queuedIssue {
	out := append([]queuedIssue(nil), issues...)
	// Stable, so a tie that issueBefore does not break stays in the order
	// the issues were listed, which is the order of repos in the config.
	sort.SliceStable(out, func(i, j int) bool {
		return issueBefore(out[i], out[j], projects)
	})
	return out
}

func issueBefore(a, b queuedIssue, projects []string) bool {
	if c := projectRank(a.Repo, projects) - projectRank(b.Repo, projects); c != 0 {
		return c < 0
	}
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

// queuePriorities is the order the queue starts work inside one project.
var queuePriorities = []string{"p1", "p2", "p3"}

// projectRank is the index of the repository's name in projects, or
// len(projects) when the file does not name it. An empty projects list
// ranks every repository together.
func projectRank(repo string, projects []string) int {
	name := repo
	if _, n, ok := strings.Cut(repo, "/"); ok && n != "" {
		name = n
	}
	for i, p := range projects {
		if strings.EqualFold(p, name) {
			return i
		}
	}
	return len(projects)
}

// readPriorities reads a priority file. One project name per line, highest
// first. A # starts a comment, including after a name. Blank lines are
// skipped. The file is read again each time the queue picks an issue, so
// an edit applies to the next start.
func readPriorities(path string) ([]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("priorities %s: %w\nfix: the file is one project name per line, highest first, and a # starts a comment", path, err)
	}
	return parsePriorities(path, b)
}

func parsePriorities(path string, b []byte) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for i, line := range strings.Split(string(b), "\n") {
		if h := strings.IndexByte(line, '#'); h >= 0 {
			line = line[:h]
		}
		name := strings.TrimSpace(line)
		if name == "" {
			continue
		}
		if strings.Contains(name, "/") || strings.ContainsAny(name, " \t") {
			return nil, fmt.Errorf("priorities %s:%d: %q is not a project name\nfix: write the repository name, such as widgets for acme/widgets, one per line", path, i+1, name)
		}
		key := strings.ToLower(name)
		if seen[key] {
			return nil, fmt.Errorf("priorities %s:%d: project %s is listed twice\nfix: list each project once, highest first", path, i+1, name)
		}
		seen[key] = true
		out = append(out, name)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("priorities %s names no project\nfix: one project name per line, highest first; a # starts a comment", path)
	}
	return out, nil
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
