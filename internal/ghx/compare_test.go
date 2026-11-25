package ghx

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
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

// routedGh puts a gh on PATH that answers by the first route whose key is in
// the arguments. A route is "stdout" or "!stderr" (exit 1).
func routedGh(t *testing.T, routes [][2]string) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\nargs=\"$*\"\n"
	for i, r := range routes {
		out := filepath.Join(dir, fmt.Sprintf("r%d", i))
		body, exit := r[1], "0"
		if strings.HasPrefix(body, "!") {
			body, exit = body[1:], "1"
		}
		if err := os.WriteFile(out, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		redirect := ""
		if exit == "1" {
			redirect = " >&2"
		}
		script += fmt.Sprintf("case \"$args\" in *'%s'*) /bin/cat '%s'%s; exit %s;; esac\n", r[0], out, redirect, exit)
	}
	script += "echo unrouted >&2; exit 2\n"
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
}

const noPatchCompare = `{"files":[{"filename":"dir/big file.ts","status":"modified","changes":9}]}`

func TestCompareFilesDiffsTheBlobsOfAFileWithNoPatch(t *testing.T) {
	routedGh(t, [][2]string{
		{"ref=abc", "keep\nold\nkeep2\n"},
		{"ref=def", "keep\nnew\nkeep2\n"},
		{"compare/", noPatchCompare},
	})
	got, err := (CLI{}).CompareFiles("acme/widgets", "abc", "def")
	if err != nil {
		t.Fatal(err)
	}
	want := "@@ -1,3 +1,3 @@\n keep\n-old\n+new\n keep2\n"
	if len(got) != 1 || got[0].Name != "dir/big file.ts" || got[0].Patch != want {
		t.Fatalf("files = %+v, want patch %q", got, want)
	}
}

func TestCompareFilesReadsTheOldNameOfARenamedFile(t *testing.T) {
	routedGh(t, [][2]string{
		{"contents/old.ts?ref=abc", "a\n"},
		{"contents/new.ts?ref=def", "a\nb\n"},
		{"compare/", `{"files":[{"filename":"new.ts","previous_filename":"old.ts","status":"renamed","changes":9}]}`},
	})
	got, err := (CLI{}).CompareFiles("acme/widgets", "abc", "def")
	if err != nil || len(got) != 1 || got[0].Patch != "@@ -1,1 +1,2 @@\n a\n+b\n" {
		t.Fatalf("got %+v, err %v", got, err)
	}
}

func TestCompareFilesReadsOnlyHeadOfAnAddedFile(t *testing.T) {
	routedGh(t, [][2]string{
		{"ref=def", "x\n"},
		{"compare/", `{"files":[{"filename":"n.ts","status":"added","changes":1}]}`},
	})
	got, err := (CLI{}).CompareFiles("acme/widgets", "abc", "def")
	if err != nil || len(got) != 1 || got[0].Patch != "@@ -1,0 +1,1 @@\n+x\n" {
		t.Fatalf("got %+v, err %v", got, err)
	}
}

func TestCompareFilesRefusesAnUnreadableFile(t *testing.T) {
	routedGh(t, [][2]string{
		{"contents/", "!gh: Not Found (HTTP 404)"},
		{"compare/", noPatchCompare},
	})
	_, err := (CLI{}).CompareFiles("acme/widgets", "abc", "def")
	var u *UnreadableError
	if !errors.As(err, &u) || u.File != "dir/big file.ts" {
		t.Fatalf("err = %v, want an UnreadableError for the file", err)
	}
}

func TestCompareFilesRetriesATransientReadFailure(t *testing.T) {
	routedGh(t, [][2]string{
		{"contents/", "!gh: connection reset"},
		{"compare/", noPatchCompare},
	})
	_, err := (CLI{}).CompareFiles("acme/widgets", "abc", "def")
	var u *UnreadableError
	if err == nil || errors.As(err, &u) {
		t.Fatalf("err = %v, want a plain error that can pass", err)
	}
}

func TestUnifiedDiffByCountPastTheTableCap(t *testing.T) {
	var sb strings.Builder
	big := 2100 // 2100 x 2100 lines is past the cap
	for i := 0; i < big; i++ {
		fmt.Fprintf(&sb, "line%d\n", i)
	}
	before := "first\n" + sb.String() + "last\n"
	after := "first!\n" + sb.String() + "extra as T\nlast!\n"
	got := unifiedDiff(before, after)
	if !strings.Contains(got, "+extra as T\n") || !strings.Contains(got, "+first!\n") {
		t.Fatalf("an added line is missing from the diff")
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
	routedGh(t, [][2]string{
		{"ref=abc", "a\n"},
		{"ref=def", "a\n// @ts-ignore\n"},
		{"compare/", `{"files":[` +
			`{"filename":"a.ts","status":"modified","changes":1,"patch":"@@ -1 +1 @@\n-a\n+b\n"},` +
			`{"filename":"a.ts","status":"modified","changes":4}]}`},
	})
	got, err = (CLI{}).CompareFiles("acme/widgets", "abc", "def")
	if err != nil || len(got) != 2 || !strings.Contains(got[1].Patch, "+// @ts-ignore") || !strings.Contains(got[0].Patch, "+b") {
		t.Fatalf("got %+v, err %v, want the second entry read from its blobs", got, err)
	}
}
