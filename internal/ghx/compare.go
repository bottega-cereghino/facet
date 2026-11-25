package ghx

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// CompareFileLimit is GitHub's cap on the files array of the compare endpoint.
// A response with this many files may have silently dropped the rest, so
// CompareFiles refuses it as truncated rather than treating it as complete.
const CompareFileLimit = 300

// DiffFile is one file in a compare of two commits. Patch is the unified
// hunks GitHub returned. It is empty when the file has no textual change.
type DiffFile struct {
	Name  string
	Patch string
}

// CompareFiles is the net diff of base...head in repo: the changes on head
// since it diverged from base. When head is a descendant of base, that is
// the commits after base. It pages through the compare endpoint and refuses a
// listing at GitHub's file cap as truncated. GitHub omits the patch of a
// large file; CompareFiles then reads the file at base and at head and diffs
// the two locally, so no changed file goes unjudged. A file that cannot be
// read is an *UnreadableError, because a missing patch cannot show that the
// change is clean.
func (CLI) CompareFiles(repo, base, head string) ([]DiffFile, error) {
	if _, _, err := splitRepo("CompareFiles", repo); err != nil {
		return nil, err
	}
	if base == "" || head == "" {
		return nil, fmt.Errorf("compare %s: base and head are required", repo)
	}
	out, err := run("api", "--paginate",
		fmt.Sprintf("repos/%s/compare/%s...%s?per_page=%d", repo, url.PathEscape(base), url.PathEscape(head), restPerPage))
	if err != nil {
		return nil, err
	}
	files, missing, err := parseCompareFiles(out)
	if err != nil {
		return nil, fmt.Errorf("parse compare %s %s...%s: %w", repo, base, head, err)
	}
	for _, m := range missing {
		patch, err := localPatch(repo, base, head, m)
		if err != nil {
			return nil, err
		}
		files[m.idx].Patch = patch
	}
	return files, nil
}

// patchless is a changed file GitHub returned without a patch.
type patchless struct {
	idx            int // index in the files slice
	name, previous string
	added          bool
}

func parseCompareFiles(body []byte) ([]DiffFile, []patchless, error) {
	dec := json.NewDecoder(strings.NewReader(string(body)))
	var out []DiffFile
	var missing []patchless
	pages := 0
	for dec.More() {
		var cmp struct {
			Files []struct {
				Filename string  `json:"filename"`
				Patch    *string `json:"patch"`
				Previous string  `json:"previous_filename"`
				Status   string  `json:"status"`
				Changes  int     `json:"changes"`
			} `json:"files"`
		}
		if err := dec.Decode(&cmp); err != nil {
			return nil, nil, err
		}
		pages++
		if len(cmp.Files) >= CompareFileLimit {
			return nil, nil, fmt.Errorf("diff truncated: reached GitHub's %d-file compare limit", CompareFileLimit)
		}
		for _, f := range cmp.Files {
			if f.Filename == "" {
				continue
			}
			if f.Patch == nil && f.Changes > 0 && f.Status != "removed" {
				prev := f.Previous
				if prev == "" {
					prev = f.Filename
				}
				missing = append(missing, patchless{idx: len(out), name: f.Filename, previous: prev, added: f.Status == "added"})
			}
			patch := ""
			if f.Patch != nil {
				patch = *f.Patch
			}
			out = append(out, DiffFile{Name: f.Filename, Patch: patch})
		}
	}
	if pages == 0 {
		return nil, nil, errors.New("empty compare response")
	}
	if len(out) >= CompareFileLimit {
		return nil, nil, fmt.Errorf("diff truncated: reached GitHub's %d-file compare limit", CompareFileLimit)
	}
	return out, missing, nil
}
