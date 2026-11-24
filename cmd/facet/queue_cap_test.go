package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bottega-cereghino/facet/internal/ghx"
)

func capQueue(t *testing.T, repos []string) queueConfig {
	t.Helper()
	cfg := oneAgentConfig(repos)
	cfg.Max = 4
	cfg.State = filepath.Join(t.TempDir(), "state.json")
	cfg.Cap = &queueCap{
		Labels: []string{"standards", "ci"},
		Num:    1,
		Den:    3,
		Passes: []string{"passes", "{repo}"},
	}
	return cfg
}

// passCount is the passes command. fail makes it exit non-zero.
type passCount struct {
	mu   sync.Mutex
	n    map[string]int
	fail bool
}

func (p *passCount) run(_ context.Context, _ string, argv []string) error {
	if len(argv) == 0 || argv[0] != "passes" {
		return nil
	}
	repo := ""
	if len(argv) > 1 {
		repo = argv[1]
	}
	p.mu.Lock()
	if p.n == nil {
		p.n = map[string]int{}
	}
	p.n[repo]++
	p.mu.Unlock()
	if p.fail {
		return errors.New("standards check failed")
	}
	return nil
}

func (p *passCount) get(repo string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.n[repo]
}

func seedWindow(t *testing.T, path string, rows []startedIssue) {
	t.Helper()
	st := newQueueState()
	for _, s := range rows {
		if s.Marker == "" {
			s.Marker = testMarker(s.Number)
		}
		st.Started[s.Repo+"#"+strconv.Itoa(s.Number)] = s
	}
	if err := st.save(path); err != nil {
		t.Fatal(err)
	}
}

func productStarts(repo string, at time.Time) []startedIssue {
	return []startedIssue{
		{Repo: repo, Number: 1, At: at, Labels: []string{"feat"}},
		{Repo: repo, Number: 2, At: at.Add(time.Hour), Labels: []string{"feat"}},
		{Repo: repo, Number: 3, At: at.Add(2 * time.Hour), Labels: []string{"feat"}},
	}
}

func dryStartIDs(log string) []string {
	var out []string
	for _, line := range strings.Split(log, "\n") {
		if strings.HasPrefix(line, "dry-run: would start ") {
			out = append(out, strings.Fields(line)[3])
		}
	}
	return out
}

func TestQueueCapConfigDefaultsAndRefusals(t *testing.T) {
	base := `"repos": ["acme/widgets"], "agents": [{"command": ["agent", "{prompt}"]}]`
	cfg, err := loadTempConfig(t, `{"state": "q.json", "cap": {}, `+base+`}`)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Cap == nil || cfg.Cap.Num != 1 || cfg.Cap.Den != 3 || len(cfg.Cap.Passes) != 0 {
		t.Fatalf("default cap = %+v", cfg.Cap)
	}
	if !cfg.Cap.matches([]string{"Standards"}) || !cfg.Cap.matches([]string{"CI"}) || cfg.Cap.matches([]string{"standards-extra"}) || cfg.Cap.matches([]string{"science"}) {
		t.Fatalf("default labels = %v", cfg.Cap.Labels)
	}
	if !filepath.IsAbs(cfg.State) || filepath.Base(cfg.State) != "q.json" {
		t.Fatalf("state = %q", cfg.State)
	}
	only, err := loadTempConfig(t, `{"state": "q.json", "cap": {"labels": ["lint"], "share": [2, 5], "passes": ["check", "{repo}"]}, `+base+`}`)
	if err != nil || only.Cap.Labels[0] != "lint" || only.Cap.Num != 2 || only.Cap.Den != 5 || only.Cap.Passes[0] != "check" {
		t.Fatalf("explicit cap = %+v, %v", only.Cap, err)
	}
	cases := []struct {
		name string
		body string
		want string
	}{
		{"no state", `{"cap": {}, ` + base + `}`, "state is not"},
		{"bad share", `{"state": "q.json", "cap": {"share": [3, 1]}, ` + base + `}`, "share"},
		{"duplicate label", `{"state": "q.json", "cap": {"labels": ["CI", "ci"]}, ` + base + `}`, "twice"},
		{"empty passes", `{"state": "q.json", "cap": {"passes": [""]}, ` + base + `}`, "empty command"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadTempConfig(t, tc.body)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
		})
	}
}

func TestQueueCapSecondStandardsIssueWaitsBehindProduct(t *testing.T) {
	cfg := capQueue(t, nil)
	cfg.Max = 1
	at := time.Now().Add(-3 * time.Hour)
	rows := productStarts("acme/widgets", at)
	rows[2].Labels = []string{"standards"}
	seedWindow(t, cfg.State, rows)
	root := t.TempDir()
	var mu sync.Mutex
	var spawned []int
	pass := &passCount{}
	s := startQueue(t, cfg, queueEnv{
		list: func(string, string) ([]ghx.Issue, error) {
			return []ghx.Issue{
				labelled(10, "2026-01-01T00:00:00Z", "Standards", "next", "standards"),
				labelled(11, "2026-01-02T00:00:00Z", "Product", "next", "feat"),
			}, nil
		},
		run: pass.run,
		spawn: func(_ string, n int) (string, error) {
			mu.Lock()
			spawned = append(spawned, n)
			mu.Unlock()
			return os.MkdirTemp(root, "ws")
		},
	})
	waitFor(t, "the product issue and later rounds", func() bool {
		mu.Lock()
		got11 := false
		for _, n := range spawned {
			if n == 11 {
				got11 = true
			}
		}
		mu.Unlock()
		// Each round runs passes once. Several rounds show the skip is logged once.
		return got11 && pass.get("acme/widgets") >= 3
	})
	s.stop(t)
	mu.Lock()
	got := append([]int(nil), spawned...)
	mu.Unlock()
	for _, n := range got {
		if n == 10 {
			t.Fatalf("started the capped standards issue: %v\n%s", got, s.buf.String())
		}
	}
	if strings.Count(s.buf.String(), "capped: acme/widgets#10, 1 of the last 3 runs in acme/widgets was standards work") != 1 {
		t.Fatalf("log =\n%s", s.buf.String())
	}
}

func TestQueueCapStartsStandardsWhenTheWindowHasNone(t *testing.T) {
	cfg := capQueue(t, nil)
	seedWindow(t, cfg.State, productStarts("acme/widgets", time.Now().Add(-3*time.Hour)))
	pass := &passCount{}
	var buf bytes.Buffer
	err := testDryQueue(context.Background(), cfg, queueEnv{
		list: func(string, string) ([]ghx.Issue, error) {
			return []ghx.Issue{labelled(10, "2026-01-01T00:00:00Z", "Standards", "next", "standards")}, nil
		},
		run: pass.run,
		out: &buf,
	})
	if err != nil {
		t.Fatal(err)
	}
	if ids := dryStartIDs(buf.String()); len(ids) != 1 || ids[0] != "acme/widgets#10" {
		t.Fatalf("starts = %v\n%s", ids, buf.String())
	}
	if strings.Contains(buf.String(), "capped:") {
		t.Fatalf("capped a window with no standards run:\n%s", buf.String())
	}
}

func TestQueueCapPassesFailingDisablesTheCap(t *testing.T) {
	cfg := capQueue(t, nil)
	at := time.Now().Add(-3 * time.Hour)
	rows := productStarts("acme/widgets", at)
	rows[2].Labels = []string{"standards"}
	seedWindow(t, cfg.State, rows)
	pass := &passCount{fail: true}
	var buf bytes.Buffer
	err := testDryQueue(context.Background(), cfg, queueEnv{
		list: func(string, string) ([]ghx.Issue, error) {
			return []ghx.Issue{labelled(10, "2026-01-01T00:00:00Z", "Standards", "next", "standards")}, nil
		},
		run: pass.run,
		out: &buf,
	})
	if err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	if ids := dryStartIDs(got); len(ids) != 1 || ids[0] != "acme/widgets#10" || strings.Contains(got, "capped:") {
		t.Fatalf("dry-run =\n%s", got)
	}
	if !strings.Contains(got, "standards check did not pass") || pass.get("acme/widgets") != 1 {
		t.Fatalf("passes %d, dry-run =\n%s", pass.get("acme/widgets"), got)
	}
}

func TestQueueCapLabelsMatchWholeAndIgnoreCase(t *testing.T) {
	cases := []struct {
		name   string
		labels []string
		start  bool
	}{
		{"upper case standards", []string{"Standards"}, false},
		{"upper case ci", []string{"CI"}, false},
		{"a longer label is not standards", []string{"standards-extra"}, true},
		{"a word that contains ci is not ci", []string{"science"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := capQueue(t, nil)
			at := time.Now().Add(-3 * time.Hour)
			seedWindow(t, cfg.State, []startedIssue{
				{Repo: "acme/widgets", Number: 1, At: at, Labels: []string{"feat"}},
				{Repo: "acme/widgets", Number: 2, At: at.Add(time.Hour), Labels: []string{"feat"}},
				{Repo: "acme/widgets", Number: 3, At: at.Add(2 * time.Hour), Labels: []string{"ci"}},
			})
			pass := &passCount{}
			var buf bytes.Buffer
			err := testDryQueue(context.Background(), cfg, queueEnv{
				list: func(string, string) ([]ghx.Issue, error) {
					return []ghx.Issue{labelled(10, "2026-01-01T00:00:00Z", "Work", append([]string{"next"}, tc.labels...)...)}, nil
				},
				run: pass.run,
				out: &buf,
			})
			if err != nil {
				t.Fatal(err)
			}
			got := buf.String()
			started := dryStartIDs(got)
			capped := strings.Contains(got, "capped: acme/widgets#10, 1 of the last 3 runs in acme/widgets was standards work")
			if tc.start {
				if len(started) != 1 || started[0] != "acme/widgets#10" || capped {
					t.Fatalf("dry-run =\n%s", got)
				}
				return
			}
			if len(started) != 0 || !capped {
				t.Fatalf("dry-run =\n%s", got)
			}
		})
	}
}

func TestQueueCapOmittedChangesNothing(t *testing.T) {
	cfg := capQueue(t, nil)
	cfg.Cap = nil
	at := time.Now().Add(-3 * time.Hour)
	rows := productStarts("acme/widgets", at)
	rows[0].Labels = []string{"standards"}
	rows[1].Labels = []string{"ci"}
	rows[2].Labels = []string{"standards"}
	seedWindow(t, cfg.State, rows)
	var buf bytes.Buffer
	err := testDryQueue(context.Background(), cfg, queueEnv{
		list: func(string, string) ([]ghx.Issue, error) {
			return []ghx.Issue{labelled(10, "2026-01-01T00:00:00Z", "Standards", "next", "standards")}, nil
		},
		run: func(context.Context, string, []string) error {
			return errors.New("passes ran with no cap")
		},
		out: &buf,
	})
	if err != nil {
		t.Fatal(err)
	}
	if ids := dryStartIDs(buf.String()); len(ids) != 1 || ids[0] != "acme/widgets#10" || strings.Contains(buf.String(), "capped:") {
		t.Fatalf("dry-run =\n%s", buf.String())
	}
}

func TestQueueCapRestartKeepsTheWindow(t *testing.T) {
	cfg := capQueue(t, nil)
	at := time.Now().Add(-3 * time.Hour)
	rows := productStarts("acme/widgets", at)
	rows[2].Labels = []string{"CI"}
	seedWindow(t, cfg.State, rows)
	before := readFile(t, cfg.State)
	pass := &passCount{}
	var buf bytes.Buffer
	err := testDryQueue(context.Background(), cfg, queueEnv{
		list: func(string, string) ([]ghx.Issue, error) {
			return []ghx.Issue{
				labelled(10, "2026-01-01T00:00:00Z", "Standards", "next", "standards"),
				labelled(11, "2026-01-02T00:00:00Z", "Product", "next"),
			}, nil
		},
		run: pass.run,
		out: &buf,
	})
	if err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	if !strings.Contains(got, "capped: acme/widgets#10, 1 of the last 3 runs in acme/widgets was standards work") {
		t.Fatalf("dry-run =\n%s", got)
	}
	if ids := dryStartIDs(got); len(ids) != 1 || ids[0] != "acme/widgets#11" {
		t.Fatalf("starts = %v\n%s", ids, got)
	}
	if readFile(t, cfg.State) != before {
		t.Fatal("dry-run wrote the state file")
	}
}

func TestQueueCapDryRunSkipsTheSecondStandardsIssue(t *testing.T) {
	cfg := capQueue(t, nil)
	pass := &passCount{}
	var buf bytes.Buffer
	err := testDryQueue(context.Background(), cfg, queueEnv{
		list: func(string, string) ([]ghx.Issue, error) {
			return []ghx.Issue{
				labelled(1, "2026-01-01T00:00:00Z", "Standards", "next", "standards"),
				labelled(2, "2026-01-02T00:00:00Z", "More standards", "next", "ci"),
				labelled(3, "2026-01-03T00:00:00Z", "Product", "next", "feat"),
			}, nil
		},
		run: pass.run,
		out: &buf,
	})
	if err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	if ids := strings.Join(dryStartIDs(got), ","); ids != "acme/widgets#1,acme/widgets#3" {
		t.Fatalf("starts = %s\n%s", ids, got)
	}
	if !strings.Contains(got, "capped: acme/widgets#2, 1 of the last 3 runs in acme/widgets was standards work") {
		t.Fatalf("dry-run =\n%s", got)
	}
	if pass.get("acme/widgets") != 1 {
		t.Fatalf("passes ran %d times, want once per round", pass.get("acme/widgets"))
	}
}

func TestQueueCapPassesRunsOncePerRepository(t *testing.T) {
	cfg := capQueue(t, []string{"acme/widgets", "acme/other"})
	at := time.Now().Add(-3 * time.Hour)
	rows := append(productStarts("acme/widgets", at), productStarts("acme/other", at)...)
	rows[2].Labels = []string{"standards"}
	rows[5].Labels = []string{"ci"}
	seedWindow(t, cfg.State, rows)
	pass := &passCount{}
	var buf bytes.Buffer
	err := testDryQueue(context.Background(), cfg, queueEnv{
		list: func(repo, _ string) ([]ghx.Issue, error) {
			switch repo {
			case "acme/widgets":
				return []ghx.Issue{
					labelled(10, "2026-01-01T00:00:00Z", "One", "next", "standards"),
					labelled(11, "2026-01-02T00:00:00Z", "Two", "next", "ci"),
				}, nil
			case "acme/other":
				return []ghx.Issue{labelled(12, "2026-01-01T00:00:00Z", "Other", "next", "Standards")}, nil
			default:
				return nil, errors.New(repo)
			}
		},
		run: pass.run,
		out: &buf,
	})
	if err != nil {
		t.Fatal(err)
	}
	if pass.get("acme/widgets") != 1 || pass.get("acme/other") != 1 {
		t.Fatalf("passes widgets %d other %d\n%s", pass.get("acme/widgets"), pass.get("acme/other"), buf.String())
	}
	if strings.Contains(buf.String(), "would start") {
		t.Fatalf("started a capped issue:\n%s", buf.String())
	}
}

func TestQueueCapFixAndLandAreNotCapped(t *testing.T) {
	seed := func(t *testing.T, path string) {
		t.Helper()
		at := time.Now().Add(-3 * time.Hour)
		rows := productStarts("acme/widgets", at)
		rows[0].Labels = []string{"standards"}
		seedWindow(t, path, rows)
	}
	issues := func(string, string) ([]ghx.Issue, error) {
		return []ghx.Issue{labelled(9, "2026-01-04T00:00:00Z", "More", "next", "standards")}, nil
	}
	t.Run("fix", func(t *testing.T) {
		cfg := fixQueue(t, "unused")
		cfg.Cap = capQueue(t, nil).Cap
		cfg.Max = 4
		seed(t, cfg.State)
		pass := &passCount{}
		var buf bytes.Buffer
		err := testDryQueue(context.Background(), cfg, queueEnv{
			list: issues,
			prs:  staticPRs(redPR(11, "feature/1-bell")),
			run:  pass.run,
			out:  &buf,
		})
		if err != nil {
			t.Fatal(err)
		}
		got := buf.String()
		if !strings.Contains(got, "would fix acme/widgets#1 PR #11") || !strings.Contains(got, "capped: acme/widgets#9") || strings.Contains(got, "would start acme/widgets#9") {
			t.Fatalf("dry-run =\n%s", got)
		}
	})
	t.Run("land", func(t *testing.T) {
		cfg := fixQueue(t, "unused")
		cfg.Cap = capQueue(t, nil).Cap
		cfg.Max = 4
		seed(t, cfg.State)
		pass := &passCount{}
		land := &landCalls{}
		var buf bytes.Buffer
		err := testDryQueue(context.Background(), cfg, land.env(queueEnv{
			list: issues,
			prs:  staticPRs(landPR(11, "feature/1-bell", "CLEAN")),
			run:  pass.run,
			out:  &buf,
		}))
		if err != nil {
			t.Fatal(err)
		}
		got := buf.String()
		if !strings.Contains(got, "would merge acme/widgets#1 PR #11") || !strings.Contains(got, "capped: acme/widgets#9") {
			t.Fatalf("dry-run =\n%s", got)
		}
		if len(land.list()) != 0 {
			t.Fatalf("dry run landed %q", land.list())
		}
	})
}

func TestQueueCapKeepsAStartAfterThePullRequestIsGone(t *testing.T) {
	cfg := fixQueue(t, filepath.Join(t.TempDir(), "log"))
	cfg.Cap = &queueCap{Labels: []string{"standards", "ci"}, Num: 1, Den: 3}
	st := newQueueState()
	st.Started["acme/widgets#1"] = startedIssue{
		Repo: "acme/widgets", Number: 1, At: time.Now().Add(-48 * time.Hour), Labels: []string{"CI"},
	}
	if err := st.save(cfg.State); err != nil {
		t.Fatal(err)
	}
	s := startQueue(t, cfg, queueEnv{
		list: noIssues,
		prs:  func(string) ([]ghx.OpenPR, error) { return nil, nil },
	})
	waitFor(t, "the start to outlive the pull request", func() bool {
		got, err := loadQueueState(cfg.State)
		if err != nil {
			return false
		}
		if _, ok := got.Started["acme/widgets#1"]; ok {
			return false
		}
		w := got.window("acme/widgets", 3)
		return len(w) == 1 && w[0].Number == 1 && w[0].Labels[0] == "CI"
	})
	s.stop(t)
}

func TestCapWindowReadsStartedAndKeepsTheNewest(t *testing.T) {
	st := newQueueState()
	repo := "acme/widgets"
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 1; i <= 4; i++ {
		s := startedIssue{Repo: repo, Number: i, At: at.Add(time.Duration(i) * time.Hour), Labels: []string{"feat"}}
		st.Started[repo+"#"+strconv.Itoa(i)] = s
	}
	third := st.Started[repo+"#3"]
	third.Labels = []string{"Standards"}
	st.Started[repo+"#3"] = third
	// A newer copy of issue 4 lives only in the log.
	st.noteStart(startedIssue{Repo: repo, Number: 4, At: at.Add(5 * time.Hour), Labels: []string{"ci"}})
	rule := &queueCap{Labels: []string{"standards", "ci"}, Num: 1, Den: 3}
	got := st.window(repo, rule.Den)
	if len(got) != 3 || got[0].Number != 4 || got[1].Number != 3 || got[2].Number != 2 {
		t.Fatalf("window = %+v", got)
	}
	n := 0
	for _, s := range got {
		if rule.matches(s.Labels) {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("standards in window = %d, want 2", n)
	}
	if len(st.window("acme/other", 3)) != 0 {
		t.Fatal("other repository leaked into the window")
	}
}
