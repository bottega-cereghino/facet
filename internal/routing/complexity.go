package routing

import (
	"fmt"
	"regexp"
	"strings"
)

// complexityValue matches the first word of a "### Complexity" section: a
// number, written bare ("2"), as a tier ("c2") or as the label itself
// ("complexity/2"). Anything after it on the line is a note and is ignored.
var complexityValue = regexp.MustCompile(`(?i)^(?:complexity/|c)?(\d+)\b`)

// ParseComplexity reads the body's "### Complexity" section, the field the
// issue form uses for the work's size. found is false when the body has no such
// section. A section that is present but does not start with a number is an
// error: it says something, and guessing what would be worse than asking.
func ParseComplexity(body string) (n string, found bool, err error) {
	section := findSection(body, "Complexity")
	if section == "" {
		return "", false, nil
	}
	first, _, _ := strings.Cut(section, "\n")
	m := complexityValue.FindStringSubmatch(strings.TrimSpace(first))
	if m == nil {
		return "", true, fmt.Errorf("the ### Complexity section says %q; start it with a number, such as 2", strings.TrimSpace(first))
	}
	return m[1], true, nil
}
