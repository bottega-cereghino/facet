package lockfile

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestWithReleasesLock(t *testing.T) {
	lock := filepath.Join(t.TempDir(), "x.lock")
	ran := false
	if err := With(lock, Options{}, func() error { ran = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if !ran {
		t.Error("fn did not run")
	}
	if _, err := os.Stat(lock); !os.IsNotExist(err) {
		t.Error("lockfile survived With")
	}
}

// Only one holder may run the guarded work at a time.
func TestWithSerialises(t *testing.T) {
	lock := filepath.Join(t.TempDir(), "x.lock")
	var mu sync.Mutex
	inside, peak := 0, 0
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = With(lock, Options{Poll: time.Millisecond}, func() error {
				mu.Lock()
				inside++
				if inside > peak {
					peak = inside
				}
				mu.Unlock()
				time.Sleep(20 * time.Millisecond)
				mu.Lock()
				inside--
				mu.Unlock()
				return nil
			})
		}()
	}
	wg.Wait()
	if peak != 1 {
		t.Errorf("lock allowed %d concurrent holders, want 1", peak)
	}
}

// A lock whose holder crashed (ancient mtime, no heartbeat) is broken so a later
// waiter is not blocked forever.
func TestWithBreaksStaleLock(t *testing.T) {
	lock := filepath.Join(t.TempDir(), "x.lock")
	if err := os.WriteFile(lock, []byte("pid 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Minute)
	if err := os.Chtimes(lock, old, old); err != nil {
		t.Fatal(err)
	}
	ran, warned := false, false
	err := With(lock, Options{StaleAge: time.Minute, Warn: func(string, ...any) { warned = true }},
		func() error { ran = true; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if !ran {
		t.Error("stale lock was not broken")
	}
	if !warned {
		t.Error("no warning on breaking a stale lock")
	}
}

// A live holder's heartbeat keeps its lock fresh, so a peer does not judge it
// abandoned while the work legitimately outlives StaleAge.
func TestHeartbeatKeepsLockFresh(t *testing.T) {
	lock := filepath.Join(t.TempDir(), "x.lock")
	err := With(lock, Options{StaleAge: 50 * time.Millisecond, Heartbeat: 10 * time.Millisecond},
		func() error {
			// Backdate as if aged; the heartbeat must bring it forward. Poll
			// rather than sleep a fixed time and require a fresh mtime: under
			// -race on a loaded runner a 10ms tick can come late (#221).
			old := time.Now().Add(-time.Hour)
			if err := os.Chtimes(lock, old, old); err != nil {
				return err
			}
			deadline := time.Now().Add(5 * time.Second)
			for {
				fi, err := os.Stat(lock)
				if err != nil {
					return err
				}
				if fi.ModTime().After(old.Add(time.Minute)) {
					return nil
				}
				if time.Now().After(deadline) {
					t.Errorf("heartbeat did not refresh the lock in 5s; mtime %s", fi.ModTime())
					return nil
				}
				time.Sleep(10 * time.Millisecond)
			}
		})
	if err != nil {
		t.Fatal(err)
	}
}

// createLock claims a free path and reports contention as os.IsExist so the
// acquire loop's stale/poll path takes over. On Windows it additionally retries
// the delete-pending ERROR_ACCESS_DENIED window; that path is exercised by the
// Windows CI leg via internal/mirror's TestHeldLockBlocksUntilReleased. Here we
// pin the cross-platform contract.
func TestCreateLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")

	f, err := createLock(path)
	if err != nil {
		t.Fatalf("first createLock: %v", err)
	}
	_ = f.Close()

	// A second create of the same live path is contention, reported as IsExist.
	if _, err := createLock(path); !os.IsExist(err) {
		t.Fatalf("second createLock err = %v, want os.IsExist", err)
	}

	// After removal the path is free again.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	f2, err := createLock(path)
	if err != nil {
		t.Fatalf("createLock after remove: %v", err)
	}
	_ = f2.Close()
}

// A holder that stalls past StaleAge can have its lock broken and taken by a
// successor. Its release must then leave the successor's lock in place, or a
// third process could take the lock beside the successor.
func TestReleaseLeavesASuccessorsLock(t *testing.T) {
	lock := filepath.Join(t.TempDir(), "x.lock")
	err := With(lock, Options{}, func() error {
		// What a waiter that broke this lock, and then took it, leaves behind.
		if err := os.Remove(lock); err != nil {
			return err
		}
		return os.WriteFile(lock, []byte("successor\n"), 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(lock)
	if err != nil {
		t.Fatalf("release removed the successor's lock: %v", err)
	}
	if string(b) != "successor\n" {
		t.Errorf("lock holds %q, want the successor's", b)
	}
}

// Two waiters can judge the same lock stale. When the second comes to break
// it, the first may already have broken it and created its own fresh lock at
// the same path; breakStale must leave that lock, the same file, in place.
func TestBreakStaleLeavesAFreshLock(t *testing.T) {
	dir := t.TempDir()
	lock := filepath.Join(dir, "x.lock")
	if err := os.WriteFile(lock, []byte("pid 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(lock, old, old); err != nil {
		t.Fatal(err)
	}
	stale, err := os.Stat(lock)
	if err != nil {
		t.Fatal(err)
	}
	// The first waiter breaks the stale lock and takes its own.
	if err := os.Remove(lock); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lock, []byte("first waiter\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fresh, err := os.Stat(lock)
	if err != nil {
		t.Fatal(err)
	}

	if breakStale(lock, stale, time.Minute) {
		t.Error("breakStale reported breaking a lock that was not the stale one")
	}
	now, err := os.Stat(lock)
	if err != nil {
		t.Fatalf("the first waiter's fresh lock was removed: %v", err)
	}
	if !os.SameFile(now, fresh) {
		t.Error("the lock at the path is no longer the first waiter's file")
	}
	assertOnlyLock(t, dir)
}

// The stale file itself is removed, with nothing left beside it.
func TestBreakStaleRemovesTheStaleLock(t *testing.T) {
	dir := t.TempDir()
	lock := filepath.Join(dir, "x.lock")
	if err := os.WriteFile(lock, []byte("pid 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(lock, old, old); err != nil {
		t.Fatal(err)
	}
	stale, err := os.Stat(lock)
	if err != nil {
		t.Fatal(err)
	}
	if !breakStale(lock, stale, time.Minute) {
		t.Error("breakStale did not break a stale lock")
	}
	if _, err := os.Stat(lock); !os.IsNotExist(err) {
		t.Errorf("stale lock survived: %v", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("breakStale left %d files behind", len(entries))
	}
}

func assertOnlyLock(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "x.lock" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("want only x.lock in the directory, got %v", names)
	}
}
