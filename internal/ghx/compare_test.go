package ghx

import (
	"strings"
	"testing"
)

func TestCompareFilesReadsPatches(t *testing.T) {
	body := `{"files":[{"filename":"a.ts","status":"modified","changes":2,"patch":"@@ -1 +1 @@\n-foo\n+foo!\n"},{"filename":"gone.ts","status":"removed","changes":1}]}`
	args := fakeGh(t, body, "", 0)
	got, err := (CLI{}).CompareFiles("acme/widgets", "abc", "def")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Name != "a.ts" || !strings.Contains(got[0].Patch, "+foo!") || got[1].Name != "gone.ts" || got[1].Patch != "" {
		t.Fatalf("files = %+v", got)
	}
	want := "api repos/acme/widgets/compare/abc...def"
	if strings.Join(args(), " ") != want {
		t.Errorf("gh args = %q, want %q", args(), want)
	}
}

func TestCompareFilesRefusesAMissingPatch(t *testing.T) {
	fakeGh(t, `{"files":[{"filename":"big.ts","status":"modified","changes":9}]}`, "", 0)
	_, err := (CLI{}).CompareFiles("acme/widgets", "abc", "def")
	if err == nil || !strings.Contains(err.Error(), "no patch for big.ts") {
		t.Fatalf("err = %v", err)
	}
}
