package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bottega-cereghino/facet/internal/ghx"
)

func recentRFC3339(ago time.Duration) string {
	return time.Now().Add(-ago).UTC().Format(time.RFC3339)
}

func TestMatchGlob(t *testing.T) {
	cases := []struct {
		pattern string
		path    string
		want    bool
	}{
		{"**/*baseline*", "baseline.json", true},
		{"**/*baseline*", "docs/lint-baseline.md", true},
		{"**/*baseline*", "a/b/my-baseline-file.txt", true},
		{"**/*baseline*", "a/b/other.txt", false},
		{"**/config.json", "config.json", true},
		{"**/config.json", "a/config.json", true},
		{"**/config.json", "a/b/config.json", true},
		{"**/config.json", "a/myconfig.json", false},
		{"**/*.lock", "uv.lock", true},
		{"**/*.lock", "app/Cargo.lock", true},
		{"**/*.lock", "lock.txt", false},
		{"package-lock.json", "package-lock.json", true},
		{"package-lock.json", "web/package-lock.json", true},
		{"/package-lock.json", "package-lock.json", true},
		{"/package-lock.json", "sub/package-lock.json", false},
		{"*.lock", "uv.lock", true},
		{"*.lock", "web/yarn.lock", true},
		{"go.sum", "go.sum", true},
		{"go.sum", "sub/go.sum", true},
		{"/go.sum", "go.sum", true},
		{"/go.sum", "sub/go.sum", false},
		{"a/**/b", "a/b", true},
		{"a/**/b", "a/x/b", true},
		{"a/**/b", "a/x/y/b", true},
		{"a/**/b", "a/x/c", false},
	}
	for _, tc := range cases {
		if got := matchGlob(tc.pattern, tc.path); got != tc.want {
			t.Errorf("matchGlob(%q, %q) = %v, want %v", tc.pattern, tc.path, got, tc.want)
		}
	}
}

func TestQueueSerializeConfigValidation(t *testing.T) {
	base := `{"repos":["acme/widgets"],"agents":[{"label":"","command":["echo","{prompt}"]}]`
	writeCfg := func(extra string) (queueConfig, error) {
		p := filepath.Join(t.TempDir(), "queue.json")
		if err := os.WriteFile(p, []byte(base+extra+"}"), 0o644); err != nil {
			t.Fatal(err)
		}
		return loadQueueConfig(p)
	}
	cfg, err := writeCfg("")
	if err != nil || cfg.Serialize != nil {
		t.Fatalf("omitted serialize: cfg=%+v err=%v", cfg.Serialize, err)
	}
	cfg, err = writeCfg(`,"serialize":{"paths":["**/config.json","go.sum"]}`)
	if err != nil || cfg.Serialize == nil || len(cfg.Serialize.Paths) != 2 {
		t.Fatalf("valid serialize: cfg=%+v err=%v", cfg.Serialize, err)
	}
	for _, bad := range []string{
		`,"serialize":{"paths":[]}`,
		`,"serialize":{"paths":["   "]}`,
		`,"serialize":{"paths":["[unclosed"]}`,
		`,"serialize":{"paths":["docs/"]}`,
		`,"serialize":{"paths":["/"]}`,
		`,"serialize":{"paths":["a//b"]}`,
		`,"serialize":{"paths":[".."]}`,
	} {
		if _, err := writeCfg(bad); err == nil || !strings.Contains(err.Error(), "fix:") {
			t.Errorf("bad %s: err = %v, want error with fix:", bad, err)
		}
	}
}

func TestQueueSerializesTwoGreenPRsTouchingASharedFile(t *testing.T) {
	cfg := fixQueue(t, filepath.Join(t.TempDir(), "log"))
	cfg.Serialize = &queueSerialize{Paths: []string{"**/config.json"}}
	seedState(t, cfg.State, []int{1, 2}, nil)

	var (
		mu       sync.Mutex
		merged11 bool
		updated2 bool
		merged12 bool
		checks2  int
	)
	land := &landCalls{
		merge: func(n int) error {
			mu.Lock()
			defer mu.Unlock()
			if n == 11 {
				merged11 = true
			}
			if n == 12 {
				merged12 = true
			}
			return nil
		},
		update: func(n int) error {
			mu.Lock()
			defer mu.Unlock()
			if n == 12 {
				updated2 = true
			}
			return nil
		},
		behind: func(_, _, head string) (bool, error) {
			mu.Lock()
			defer mu.Unlock()
			// Once #11 merges, main moves past head12; after update, head12b contains main.
			return merged11 && head == "head12", nil
		},
		files: func(_ string, n int) ([]string, error) {
			if n == 11 {
				return []string{"a.go", "config.json"}, nil
			}
			return []string{"b.go", "config.json"}, nil
		},
	}
	prs := func(string) ([]ghx.OpenPR, error) {
		mu.Lock()
		defer mu.Unlock()
		var out []ghx.OpenPR
		if !merged11 {
			pr11 := landPR(11, "feature/1-bell", "CLEAN")
			pr11.CreatedAt = recentRFC3339(2 * time.Hour)
			out = append(out, pr11)
		}
		if !merged12 {
			pr12 := landPR(12, "feature/2-horn", "CLEAN")
			pr12.CreatedAt = recentRFC3339(time.Hour)
			if updated2 {
				pr12.HeadRefOid = "head12b"
				checks2++
				if checks2 == 1 {
					pr12.Checks[0].Status = "IN_PROGRESS"
				}
			}
			out = append(out, pr12)
		}
		return out, nil
	}

	now := time.Now()
	var clockStep int
	s := startQueue(t, cfg, land.env(queueEnv{
		list: noIssues,
		prs:  prs,
		now: func() time.Time {
			mu.Lock()
			defer mu.Unlock()
			if checks2 >= 1 {
				clockStep++
				return now.Add(time.Duration(clockStep) * (prListTTL + time.Second))
			}
			return now
		},
	}))
	waitFor(t, "both PRs merged", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return merged11 && merged12
	})
	s.stop(t)

	want := "merge acme/widgets#11 squash head11|update acme/widgets#12|merge acme/widgets#12 squash head12b"
	if got := strings.Join(land.list(), "|"); got != want {
		t.Fatalf("landed = %q\nwant   = %q", got, want)
	}
	if !strings.Contains(s.buf.String(), "waits for #11, shared file config.json") {
		t.Fatalf("log missing wait line:\n%s", s.buf.String())
	}
}

func TestQueueLandsTwoPRsWithDisjointFiles(t *testing.T) {
	cfg := fixQueue(t, filepath.Join(t.TempDir(), "log"))
	cfg.Serialize = &queueSerialize{Paths: []string{"**/config.json"}}
	seedState(t, cfg.State, []int{1, 2}, nil)

	var (
		mu       sync.Mutex
		merged11 bool
		merged12 bool
	)
	land := &landCalls{
		merge: func(n int) error {
			mu.Lock()
			defer mu.Unlock()
			if n == 11 {
				merged11 = true
			}
			if n == 12 {
				merged12 = true
			}
			return nil
		},
		files: func(_ string, n int) ([]string, error) {
			if n == 11 {
				return []string{"a.go"}, nil
			}
			return []string{"b.go"}, nil
		},
	}
	prs := func(string) ([]ghx.OpenPR, error) {
		mu.Lock()
		defer mu.Unlock()
		var out []ghx.OpenPR
		if !merged11 {
			pr11 := landPR(11, "feature/1-bell", "CLEAN")
			pr11.CreatedAt = recentRFC3339(2 * time.Hour)
			if !merged12 {
				// #11 is still pending while #12 is green: #12 must not wait.
				pr11.Checks[0].Status = "IN_PROGRESS"
			}
			out = append(out, pr11)
		}
		if !merged12 {
			pr12 := landPR(12, "feature/2-horn", "CLEAN")
			pr12.CreatedAt = recentRFC3339(time.Hour)
			out = append(out, pr12)
		}
		return out, nil
	}

	s := startQueue(t, cfg, land.env(queueEnv{list: noIssues, prs: prs}))
	waitFor(t, "both disjoint PRs merged", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return merged11 && merged12
	})
	s.stop(t)

	want := "merge acme/widgets#12 squash head12|merge acme/widgets#11 squash head11"
	if got := strings.Join(land.list(), "|"); got != want {
		t.Fatalf("landed = %q\nwant   = %q", got, want)
	}
	if strings.Contains(s.buf.String(), "waits for") {
		t.Fatalf("disjoint PRs logged a wait:\n%s", s.buf.String())
	}
}

func TestQueueBlocksMergeWhenBaseMovedAfterChecks(t *testing.T) {
	cfg := fixQueue(t, filepath.Join(t.TempDir(), "log"))
	seedState(t, cfg.State, []int{1}, nil)

	var (
		mu      sync.Mutex
		updated bool
		merged  bool
	)
	land := &landCalls{
		update: func(int) error {
			mu.Lock()
			updated = true
			mu.Unlock()
			return nil
		},
		merge: func(int) error {
			mu.Lock()
			merged = true
			mu.Unlock()
			return nil
		},
		behind: func(_, _, head string) (bool, error) {
			// Base moved after checks ran on head11; head11b contains the base.
			return head == "head11", nil
		},
	}
	prs := func(string) ([]ghx.OpenPR, error) {
		mu.Lock()
		defer mu.Unlock()
		if merged {
			return nil, nil
		}
		pr := landPR(11, "feature/1-bell", "CLEAN")
		if updated {
			pr.HeadRefOid = "head11b"
		}
		return []ghx.OpenPR{pr}, nil
	}

	s := startQueue(t, cfg, land.env(queueEnv{list: noIssues, prs: prs}))
	waitFor(t, "update then merge", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return merged
	})
	s.stop(t)

	want := "update acme/widgets#11|merge acme/widgets#11 squash head11b"
	if got := strings.Join(land.list(), "|"); got != want {
		t.Fatalf("landed = %q\nwant   = %q", got, want)
	}
}

func TestQueueOmittedSerializeChangesNothing(t *testing.T) {
	cfg := fixQueue(t, filepath.Join(t.TempDir(), "log"))
	seedState(t, cfg.State, []int{1, 2}, nil)

	var (
		fileCalls atomic.Int32
		merged12  atomic.Bool
	)
	land := &landCalls{
		merge: func(n int) error {
			if n == 12 {
				merged12.Store(true)
			}
			return nil
		},
		files: func(string, int) ([]string, error) {
			fileCalls.Add(1)
			return []string{"config.json"}, nil
		},
	}
	pr11 := landPR(11, "feature/1-bell", "CLEAN")
	pr11.CreatedAt = recentRFC3339(2 * time.Hour)
	pr11.Checks[0].Status = "IN_PROGRESS"
	pr12 := landPR(12, "feature/2-horn", "CLEAN")
	pr12.CreatedAt = recentRFC3339(time.Hour)

	prs := func(string) ([]ghx.OpenPR, error) {
		if merged12.Load() {
			return []ghx.OpenPR{pr11}, nil
		}
		return []ghx.OpenPR{pr11, pr12}, nil
	}

	s := startQueue(t, cfg, land.env(queueEnv{list: noIssues, prs: prs}))
	waitFor(t, "PR #12 merged", func() bool { return merged12.Load() })
	s.stop(t)

	if n := fileCalls.Load(); n != 0 {
		t.Fatalf("prFiles called %d times with serialize omitted, want 0", n)
	}
	if got := strings.Join(land.list(), "|"); got != "merge acme/widgets#12 squash head12" {
		t.Fatalf("landed = %q", got)
	}
}

func TestQueueDryRunListsSerializeWaitsAndAtMostOneMergePerRepo(t *testing.T) {
	cfg := fixQueue(t, "unused")
	cfg.Serialize = &queueSerialize{Paths: []string{"**/config.json"}}
	seedState(t, cfg.State, []int{1, 2, 3}, nil)

	pr11 := landPR(11, "feature/1-bell", "CLEAN")
	pr11.CreatedAt = recentRFC3339(3 * time.Hour)
	pr12 := landPR(12, "feature/2-horn", "CLEAN")
	pr12.CreatedAt = recentRFC3339(2 * time.Hour)
	pr13 := landPR(13, "feature/3-drum", "CLEAN")
	pr13.CreatedAt = recentRFC3339(time.Hour)

	land := &landCalls{
		files: func(_ string, n int) ([]string, error) {
			if n == 13 {
				return []string{"c.go"}, nil
			}
			return []string{"config.json"}, nil
		},
	}
	var buf bytes.Buffer
	err := testDryQueue(context.Background(), cfg, land.env(queueEnv{
		list: noIssues,
		prs:  staticPRs(pr11, pr12, pr13),
		out:  &buf,
	}))
	got := buf.String()
	if err != nil {
		t.Fatalf("dryQueue err = %v", err)
	}
	if !strings.Contains(got, "dry-run: would merge acme/widgets#1 PR #11") {
		t.Fatalf("missing merge for #11:\n%s", got)
	}
	if !strings.Contains(got, "dry-run: acme/widgets#2 PR #12 waits for #11, shared file config.json") {
		t.Fatalf("missing wait for #12:\n%s", got)
	}
	if strings.Contains(got, "would merge acme/widgets#3 PR #13") {
		t.Fatalf("merged more than one PR per repo in one round:\n%s", got)
	}
	if len(land.list()) != 0 {
		t.Fatalf("dry-run landed %q", land.list())
	}
}

func TestQueueCachesPRFilesAndLogsWaitOncePerListing(t *testing.T) {
	cfg := fixQueue(t, filepath.Join(t.TempDir(), "log"))
	cfg.Serialize = &queueSerialize{Paths: []string{"**/config.json"}}
	seedState(t, cfg.State, []int{1, 2}, nil)

	fileCalls := &countCalls{}
	ticks := &countCalls{}
	land := &landCalls{
		files: func(_ string, n int) ([]string, error) {
			fileCalls.add(string(rune('0' + n)))
			return []string{"config.json"}, nil
		},
	}
	pr11 := landPR(11, "feature/1-bell", "CLEAN")
	pr11.CreatedAt = recentRFC3339(2 * time.Hour)
	pr11.Checks[0].Status = "IN_PROGRESS"
	pr12 := landPR(12, "feature/2-horn", "CLEAN")
	pr12.CreatedAt = recentRFC3339(time.Hour)

	s := startQueue(t, cfg, land.env(queueEnv{
		list: func(repo, _ string) ([]ghx.Issue, error) { ticks.add(repo); return nil, nil },
		prs:  staticPRs(pr11, pr12),
	}))
	waitFor(t, "five rounds", func() bool { return ticks.get("acme/widgets") >= 5 })
	s.stop(t)

	if n11, n12 := fileCalls.get(string(rune('0'+11))), fileCalls.get(string(rune('0'+12))); n11 != 1 || n12 != 1 {
		t.Fatalf("prFiles calls: #11=%d #12=%d, want 1 and 1", n11, n12)
	}
	if count := strings.Count(s.buf.String(), "waits for #11, shared file config.json"); count != 1 {
		t.Fatalf("wait logged %d times, want 1:\n%s", count, s.buf.String())
	}
}

func TestQueueSerializeSkipsUnlandableOlderPR(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, statePath string, pr11 *ghx.OpenPR)
	}{
		{
			name: "blocked older PR",
			setup: func(t *testing.T, statePath string, _ *ghx.OpenPR) {
				st, err := loadQueueState(statePath)
				if err != nil {
					t.Fatal(err)
				}
				st.Fixes["acme/widgets#1"] = &fixRecord{PR: 11, Runs: 2, Blocked: true}
				if err := st.save(statePath); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "stopped after five refused merges",
			setup: func(t *testing.T, statePath string, pr11 *ghx.OpenPR) {
				st, err := loadQueueState(statePath)
				if err != nil {
					t.Fatal(err)
				}
				st.Merges["acme/widgets#11"] = &mergeHold{
					Head:    pr11.HeadRefOid,
					Checks:  checksFingerprint(pr11.Checks),
					Count:   maxMergeRefusals,
					Stopped: true,
				}
				if err := st.save(statePath); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "draft older PR",
			setup: func(_ *testing.T, _ string, pr11 *ghx.OpenPR) {
				pr11.IsDraft = true
				pr11.MergeStateStatus = "DRAFT"
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := fixQueue(t, filepath.Join(t.TempDir(), "log"))
			cfg.Serialize = &queueSerialize{Paths: []string{"package-lock.json"}}
			seedState(t, cfg.State, []int{1, 2}, nil)

			pr11 := landPR(11, "feature/1-bell", "CLEAN")
			pr11.CreatedAt = recentRFC3339(2 * time.Hour)
			pr12 := landPR(12, "feature/2-horn", "CLEAN")
			pr12.CreatedAt = recentRFC3339(time.Hour)
			tc.setup(t, cfg.State, &pr11)

			var merged12 atomic.Bool
			land := &landCalls{
				merge: func(n int) error {
					if n == 12 {
						merged12.Store(true)
					}
					return nil
				},
				files: func(string, int) ([]string, error) {
					return []string{"web/package-lock.json"}, nil
				},
			}
			prs := func(string) ([]ghx.OpenPR, error) {
				if merged12.Load() {
					return []ghx.OpenPR{pr11}, nil
				}
				return []ghx.OpenPR{pr11, pr12}, nil
			}
			s := startQueue(t, cfg, land.env(queueEnv{list: noIssues, prs: prs}))
			waitFor(t, "younger PR #12 merged", func() bool { return merged12.Load() })
			s.stop(t)
			if got := strings.Join(land.list(), "|"); got != "merge acme/widgets#12 squash head12" {
				t.Fatalf("landed = %q, want only #12", got)
			}
			if strings.Contains(s.buf.String(), "waits for #11") {
				t.Fatalf("younger PR waited for unlandable #11:\n%s", s.buf.String())
			}
		})
	}
}

func TestQueueSerializesNestedLockfile(t *testing.T) {
	cfg := fixQueue(t, "unused")
	cfg.Serialize = &queueSerialize{Paths: []string{"*.lock", "package-lock.json"}}
	seedState(t, cfg.State, []int{1, 2}, nil)

	pr11 := landPR(11, "feature/1-bell", "CLEAN")
	pr11.CreatedAt = recentRFC3339(2 * time.Hour)
	pr11.Checks[0].Status = "IN_PROGRESS"
	pr12 := landPR(12, "feature/2-horn", "CLEAN")
	pr12.CreatedAt = recentRFC3339(time.Hour)

	land := &landCalls{
		files: func(string, int) ([]string, error) {
			return []string{"web/package-lock.json"}, nil
		},
	}
	var buf bytes.Buffer
	err := testDryQueue(context.Background(), cfg, land.env(queueEnv{
		list: noIssues,
		prs:  staticPRs(pr11, pr12),
		out:  &buf,
	}))
	if err != nil {
		t.Fatalf("dryQueue err = %v", err)
	}
	if !strings.Contains(buf.String(), "acme/widgets#2 PR #12 waits for #11, shared file web/package-lock.json") {
		t.Fatalf("expected nested lockfile wait:\n%s", buf.String())
	}
}
