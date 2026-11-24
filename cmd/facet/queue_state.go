package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// queueState is the state file: the issues this queue started, and the fix
// runs it made per issue. It is what lets a restarted queue tell its own
// pull requests from a person's, and keep counting tries. Merges is the
// backoff after a merge GitHub refused, keyed by repo#pr, so a restart
// does not try again before the wait is up.
type queueState struct {
	Started map[string]startedIssue `json:"started"`
	Fixes   map[string]*fixRecord   `json:"fixes"`
	Merges  map[string]*mergeHold   `json:"merges,omitempty"`
	// Starts is every issue the queue started, kept after Started drops a
	// merged one, so the cap window still counts that run. Empty when the
	// config has no cap.
	Starts []startedIssue `json:"starts,omitempty"`
}

// mergeHold is one pull request's refused merges. Head and Checks are what
// the count is for: a new head, or a check that changed, starts the count
// again. Next is when another try is allowed. Stopped means five refusals
// of this head, and the queue is not trying that merge again.
type mergeHold struct {
	Head    string    `json:"head"`
	Checks  string    `json:"checks"`
	Count   int       `json:"count"`
	Next    time.Time `json:"next"`
	Stopped bool      `json:"stopped,omitempty"`
	Labeled bool      `json:"labeled,omitempty"`
	Reason  string    `json:"reason,omitempty"`
}

// startedIssue is one issue the queue started. The issue's fields are kept
// so a fix can be ordered like new work without listing the issue again.
type startedIssue struct {
	Repo      string    `json:"repo"`
	Number    int       `json:"number"`
	At        time.Time `json:"at"`
	Title     string    `json:"title,omitempty"`
	URL       string    `json:"url,omitempty"`
	CreatedAt string    `json:"createdAt,omitempty"`
	Labels    []string  `json:"labels,omitempty"`
	// PR is the queue's pull request for the issue, recorded the first
	// time one is seen after the start. From then on only that pull
	// request is the queue's.
	PR int `json:"pr,omitempty"`
	// Marker is the token of the trailer line the run's agent was told to
	// end its pull request's description with. Only a pull request that
	// carries it is the queue's. A start recorded without one has none.
	Marker string `json:"marker,omitempty"`
}

// fixRecord counts the fix runs made for one issue's pull request.
type fixRecord struct {
	PR      int       `json:"pr"`
	Runs    int       `json:"runs"`
	Last    time.Time `json:"last"`
	Blocked bool      `json:"blocked,omitempty"`
	// Head is the pull request's head when the first fix run of this
	// record started. The suppression check reads the diff from Head to
	// the head now, so a later try is judged with the earlier one.
	Head string `json:"head,omitempty"`
}

func newQueueState() *queueState {
	return &queueState{
		Started: map[string]startedIssue{},
		Fixes:   map[string]*fixRecord{},
		Merges:  map[string]*mergeHold{},
	}
}

// loadQueueState reads the state file. A missing file is an empty state. A
// file that cannot be parsed is a refusal: starting from empty would forget
// which pull requests are the queue's and how many tries each had.
func loadQueueState(path string) (*queueState, error) {
	st := newQueueState()
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return st, nil
		}
		return nil, fmt.Errorf("read queue state %s: %w\nfix: the queue keeps its state there; make the file readable", path, err)
	}
	if err := json.Unmarshal(b, st); err != nil {
		return nil, fmt.Errorf("queue state %s: %w\nfix: repair the file, or move it away to start with no record of earlier starts and fix runs", path, err)
	}
	if st.Started == nil {
		st.Started = map[string]startedIssue{}
	}
	if st.Fixes == nil {
		st.Fixes = map[string]*fixRecord{}
	}
	if st.Merges == nil {
		st.Merges = map[string]*mergeHold{}
	}
	return st, nil
}

// syncFile flushes a file or a directory to disk. Tests replace it to see
// what is synced, and when.
var syncFile = func(f *os.File) error { return f.Sync() }

// save writes the state atomically: a temp file beside it, flushed to disk,
// then a rename, then the directory flushed, so a crash or a power loss
// leaves the old file or the new one, never half of one or none.
func (st *queueState) save(path string) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp*")
	if err != nil {
		return fmt.Errorf("write queue state: %w\nfix: the directory of state must exist and be writable", err)
	}
	name := tmp.Name()
	_, werr := tmp.Write(append(b, '\n'))
	serr := syncFile(tmp)
	cerr := tmp.Close()
	if err := firstErr(werr, serr, cerr); err != nil {
		_ = os.Remove(name)
		return fmt.Errorf("write queue state %s: %w", path, err)
	}
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return fmt.Errorf("write queue state %s: %w", path, err)
	}
	return syncDir(filepath.Dir(path))
}

// syncDir flushes a directory, so a rename in it survives a power loss. A
// file system that cannot sync a directory (EINVAL, ENOTSUP) is not an
// error: the rename is then as durable as that file system makes it.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("write queue state: open %s to sync it: %w", dir, err)
	}
	err = syncFile(d)
	_ = d.Close()
	if err != nil && !errors.Is(err, syscall.EINVAL) && !errors.Is(err, syscall.ENOTSUP) {
		return fmt.Errorf("write queue state: sync %s: %w", dir, err)
	}
	return nil
}

func firstErr(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

// lockQueueState takes an exclusive lock on a file beside the state file,
// path + ".lock", for as long as the queue runs, and returns its release.
// Two queues on one state file would each start the same issue and each
// overwrite the other's record, so a second one refuses instead. The lock
// is flock(2): the kernel drops it when the process exits, however it
// exits, so a crash leaves no stale lock behind. The file stays.
func lockQueueState(path string) (func(), error) {
	lock := path + ".lock"
	f, err := os.OpenFile(lock, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("lock queue state: %w\nfix: the directory of state must exist and be writable", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("queue state %s is in use: another facet queue holds %s\nfix: stop the other queue first, or give this config a state file of its own", path, lock)
		}
		return nil, fmt.Errorf("lock queue state %s: %w", lock, err)
	}
	return func() { _ = f.Close() }, nil
}
