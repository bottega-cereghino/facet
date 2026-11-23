package ghx

import (
	"encoding/json"
	"fmt"
	"net/url"
)

// DiffFile is one file in a compare of two commits. Patch is the unified
// hunks GitHub returned. It is empty when the file has no textual change.
type DiffFile struct {
	Name  string
	Patch string
}

// CompareFiles is the net diff of base...head in repo: the changes on head
// since it diverged from base. When head is a descendant of base, that is
// the commits after base. A changed file with no patch is an error, because
// a missing patch cannot show that the change is clean.
func (CLI) CompareFiles(repo, base, head string) ([]DiffFile, error) {
	if _, _, err := splitRepo("CompareFiles", repo); err != nil {
		return nil, err
	}
	if base == "" || head == "" {
		return nil, fmt.Errorf("compare %s: base and head are required", repo)
	}
	out, err := run("api", fmt.Sprintf("repos/%s/compare/%s...%s", repo, url.PathEscape(base), url.PathEscape(head)))
	if err != nil {
		return nil, err
	}
	files, err := parseCompareFiles(out)
	if err != nil {
		return nil, fmt.Errorf("parse compare %s %s...%s: %w", repo, base, head, err)
	}
	return files, nil
}

func parseCompareFiles(body []byte) ([]DiffFile, error) {
	var cmp struct {
		Files []struct {
			Filename string  `json:"filename"`
			Patch    *string `json:"patch"`
			Status   string  `json:"status"`
			Changes  int     `json:"changes"`
		} `json:"files"`
	}
	if err := json.Unmarshal(body, &cmp); err != nil {
		return nil, err
	}
	out := make([]DiffFile, 0, len(cmp.Files))
	for _, f := range cmp.Files {
		if f.Filename == "" {
			continue
		}
		if f.Patch == nil && f.Changes > 0 && f.Status != "removed" {
			return nil, fmt.Errorf("no patch for %s", f.Filename)
		}
		patch := ""
		if f.Patch != nil {
			patch = *f.Patch
		}
		out = append(out, DiffFile{Name: f.Filename, Patch: patch})
	}
	return out, nil
}
