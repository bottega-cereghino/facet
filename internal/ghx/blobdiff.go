package ghx

import (
	"fmt"
	"net/url"
	"strings"
)

// UnreadableError says a changed file has no patch in the compare and could
// not be read at base or at head either. Reading it again will not help.
type UnreadableError struct {
	File string
	Err  error
}

func (e *UnreadableError) Error() string { return fmt.Sprintf("cannot read %s: %v", e.File, e.Err) }
func (e *UnreadableError) Unwrap() error { return e.Err }

// lcsCells caps the table of the line diff. Past it, localPatch pairs lines
// by count instead of by order.
const lcsCells = 4_000_000

// localPatch builds the patch of one file GitHub sent without a patch, from
// the file's two versions.
func localPatch(repo, base, head string, m patchless) (string, error) {
	var before, after string
	if !m.added {
		b, err := readBlob(repo, m.previous, base)
		if err != nil {
			return "", err
		}
		before = b
	}
	a, err := readBlob(repo, m.name, head)
	if err != nil {
		return "", err
	}
	after = a
	return unifiedDiff(before, after), nil
}

// readBlob reads a file at a ref. A missing or too large file is an
// *UnreadableError; any other failure (network, rate limit) is returned as
// it is, because it can pass.
func readBlob(repo, file, ref string) (string, error) {
	segs := strings.Split(file, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	out, err := run("api", "-H", "Accept: application/vnd.github.raw+json",
		fmt.Sprintf("repos/%s/contents/%s?ref=%s", repo, strings.Join(segs, "/"), url.QueryEscape(ref)))
	if err != nil {
		msg := err.Error()
		if strings.Contains(msg, "HTTP 404") || strings.Contains(msg, "too_large") || strings.Contains(msg, "too large") {
			return "", &UnreadableError{File: file, Err: err}
		}
		return "", err
	}
	return string(out), nil
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

// unifiedDiff is one hunk holding the whole file: context, removed and added
// lines. Only the line prefixes matter to the readers of a patch.
func unifiedDiff(before, after string) string {
	a, b := splitLines(before), splitLines(after)
	pre := 0
	for pre < len(a) && pre < len(b) && a[pre] == b[pre] {
		pre++
	}
	suf := 0
	for suf < len(a)-pre && suf < len(b)-pre && a[len(a)-1-suf] == b[len(b)-1-suf] {
		suf++
	}
	midA, midB := a[pre:len(a)-suf], b[pre:len(b)-suf]
	var sb strings.Builder
	fmt.Fprintf(&sb, "@@ -1,%d +1,%d @@\n", len(a), len(b))
	emit := func(prefix string, lines []string) {
		for _, l := range lines {
			sb.WriteString(prefix + l + "\n")
		}
	}
	emit(" ", a[:pre])
	if len(midA)*len(midB) <= lcsCells {
		lcsHunk(&sb, midA, midB)
	} else {
		countHunk(&sb, midA, midB)
	}
	emit(" ", a[len(a)-suf:])
	return sb.String()
}

func lcsHunk(sb *strings.Builder, a, b []string) {
	w := len(b) + 1
	t := make([]int32, (len(a)+1)*w)
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			if a[i] == b[j] {
				t[i*w+j] = t[(i+1)*w+j+1] + 1
			} else {
				t[i*w+j] = max(t[(i+1)*w+j], t[i*w+j+1])
			}
		}
	}
	i, j := 0, 0
	for i < len(a) || j < len(b) {
		switch {
		case i < len(a) && j < len(b) && a[i] == b[j]:
			sb.WriteString(" " + a[i] + "\n")
			i++
			j++
		case i < len(a) && (j == len(b) || t[(i+1)*w+j] >= t[i*w+j+1]):
			sb.WriteString("-" + a[i] + "\n")
			i++
		default:
			sb.WriteString("+" + b[j] + "\n")
			j++
		}
	}
}

// countHunk pairs lines by how often they occur, not by order: a line is
// added when head has more copies of it than base.
func countHunk(sb *strings.Builder, a, b []string) {
	left := map[string]int{}
	for _, l := range a {
		left[l]++
	}
	var added []string
	for _, l := range b {
		if left[l] > 0 {
			left[l]--
			continue
		}
		added = append(added, l)
	}
	for _, l := range a {
		if left[l] > 0 {
			left[l]--
			sb.WriteString("-" + l + "\n")
		}
	}
	for _, l := range added {
		sb.WriteString("+" + l + "\n")
	}
}
