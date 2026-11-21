package main

import (
	"testing"
	"unicode/utf8"
)

// truncate cuts on characters, never inside one: a comment's first line is
// often not ASCII, and half a rune prints as garbage.
func TestTruncateCutsOnRunes(t *testing.T) {
	tests := []struct {
		s    string
		n    int
		want string
	}{
		{"short", 10, "short"},
		{"exactly", 7, "exactly"},
		{"abcdefgh", 5, "abcd…"},
		{"aébc", 3, "aé…"}, // a byte cut lands inside the é
		{"ééééé", 5, "ééééé"},
		{"ééééééé", 5, "éééé…"},
		{"日本語のテキスト", 4, "日本語…"},
	}
	for _, tt := range tests {
		got := truncate(tt.s, tt.n)
		if got != tt.want {
			t.Errorf("truncate(%q, %d) = %q, want %q", tt.s, tt.n, got, tt.want)
		}
		if !utf8.ValidString(got) {
			t.Errorf("truncate(%q, %d) = %q, not valid UTF-8", tt.s, tt.n, got)
		}
	}
}
