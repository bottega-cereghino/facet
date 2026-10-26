package ghx

import "testing"

// A failed GraphQL call names its request without printing the whole query
// ahead of the reason (facet#143); every other argument is kept as given.
func TestCommandLineCutsAMultiLineQueryToItsFirstLine(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{
			"graphql",
			[]string{"api", "graphql", "-f", "query=query($owner: String!) {\n  repository(owner: $owner) { id }\n}", "-f", "owner=o"},
			"api graphql -f query=query($owner: String!) { … -f owner=o",
		},
		{
			"one-line query",
			[]string{"api", "graphql", "-f", "query={ viewer { login } }"},
			"api graphql -f query={ viewer { login } }",
		},
		{
			"rest",
			[]string{"api", "repos/o/r/issues/1"},
			"api repos/o/r/issues/1",
		},
	} {
		if got := commandLine(tc.args); got != tc.want {
			t.Errorf("%s: commandLine = %q, want %q", tc.name, got, tc.want)
		}
	}
}
