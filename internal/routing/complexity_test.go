package routing

import "testing"

func TestParseComplexity(t *testing.T) {
	tests := []struct {
		name, body string
		want       string
		found      bool
		wantErr    bool
	}{
		{"no section", "### What\n\nthing\n", "", false, false},
		{"bare number", "### Complexity\n\n2\n", "2", true, false},
		{"tier form", "### Complexity\n\nc3\n", "3", true, false},
		{"label form", "### Complexity\n\ncomplexity/1\n", "1", true, false},
		{"a note after the number", "### Complexity\n\n2 (one repo, no migration)\n", "2", true, false},
		{"followed by another section", "### Complexity\n\n4\n\n### Repos in scope\n\nfacet\n", "4", true, false},
		{"says something else", "### Complexity\n\nsmall\n", "", true, true},
		{"empty section reads as absent", "### Complexity\n\n### Next\n\nx\n", "", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, found, err := ParseComplexity(tt.body)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want || found != tt.found {
				t.Errorf("ParseComplexity = (%q, %v), want (%q, %v)", got, found, tt.want, tt.found)
			}
		})
	}
}
