package ghx

import (
	"fmt"
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
	want := "api --paginate repos/acme/widgets/compare/abc...def?per_page=100"
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

func TestCompareFilesPagesThroughAllFiles(t *testing.T) {
	var page1 strings.Builder
	page1.WriteString(`{"files":[`)
	for i := 0; i < restPerPage; i++ {
		if i > 0 {
			page1.WriteByte(',')
		}
		fmt.Fprintf(&page1, `{"filename":"f%03d.ts","status":"modified","changes":1,"patch":"@@ -1 +1 @@\n-old\n+new\n"}`, i)
	}
	page1.WriteString(`]}`)
	page2 := `{"files":[{"filename":"z.ts","status":"modified","changes":1,"patch":"@@ -1 +1 @@\n-foo\n+foo!\n"}]}`
	fakeGh(t, page1.String()+page2, "", 0)
	got, err := (CLI{}).CompareFiles("acme/widgets", "abc", "def")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != restPerPage+1 {
		t.Fatalf("got %d files, want %d", len(got), restPerPage+1)
	}
	last := got[len(got)-1]
	if last.Name != "z.ts" || !strings.Contains(last.Patch, "+foo!") {
		t.Fatalf("last file = %+v", last)
	}
}

func TestCompareFilesRefusesTruncatedListing(t *testing.T) {
	var body strings.Builder
	body.WriteString(`{"files":[`)
	for i := 0; i < CompareFileLimit; i++ {
		if i > 0 {
			body.WriteByte(',')
		}
		fmt.Fprintf(&body, `{"filename":"f%03d.ts","status":"modified","changes":1,"patch":"@@ -1 +1 @@\n-old\n+new\n"}`, i)
	}
	body.WriteString(`]}`)
	fakeGh(t, body.String(), "", 0)
	_, err := (CLI{}).CompareFiles("acme/widgets", "abc", "def")
	if err == nil || !strings.Contains(err.Error(), "truncated") {
		t.Fatalf("err = %v, want truncated error", err)
	}
}

func TestCompareFilesKeepsEveryEntryOfARepeatedName(t *testing.T) {
	fakeGh(t, `{"files":[`+
		`{"filename":"a.ts","status":"modified","changes":1,"patch":"@@ -1 +1 @@\n-a\n+b\n"},`+
		`{"filename":"a.ts","status":"modified","changes":1,"patch":"@@ -1 +1 @@\n-a\n+// @ts-ignore\n"}]}`, "", 0)
	got, err := (CLI{}).CompareFiles("acme/widgets", "abc", "def")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || !strings.Contains(got[1].Patch, "@ts-ignore") {
		t.Fatalf("got %+v, want both entries of a.ts", got)
	}
	fakeGh(t, `{"files":[`+
		`{"filename":"a.ts","status":"modified","changes":1,"patch":"@@ -1 +1 @@\n-a\n+b\n"},`+
		`{"filename":"a.ts","status":"modified","changes":4}]}`, "", 0)
	if _, err := (CLI{}).CompareFiles("acme/widgets", "abc", "def"); err == nil || !strings.Contains(err.Error(), "no patch for a.ts") {
		t.Fatalf("err = %v, want the second entry's missing patch refused", err)
	}
}
