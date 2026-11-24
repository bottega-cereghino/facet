package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

const queuePIDRelativePath = ".local/state/fleet/queue.pid"

type reopenableQueueLog struct {
	mu   sync.Mutex
	path string
	file *os.File
}

func openQueueLog(path string) (*reopenableQueueLog, error) {
	log := &reopenableQueueLog{path: path}
	if err := log.reopen(); err != nil {
		return nil, err
	}
	return log, nil
}

func (l *reopenableQueueLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.file.Write(p)
}

func (l *reopenableQueueLog) reopen() error {
	file, err := os.OpenFile(l.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open queue log %s: %w", l.path, err)
	}
	l.mu.Lock()
	old := l.file
	l.file = file
	l.mu.Unlock()
	if old != nil {
		if err := old.Close(); err != nil {
			return fmt.Errorf("close old queue log: %w", err)
		}
	}
	return nil
}

func (l *reopenableQueueLog) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.file.Close()
}

func runQueueWithLog(ctx context.Context, cfg queueConfig, env queueEnv, logPath string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("find home directory for queue pid file: %w", err)
	}
	pidPath := filepath.Join(home, queuePIDRelativePath)
	if err := writeQueuePID(pidPath, os.Getpid()); err != nil {
		return err
	}
	defer removeQueuePID(pidPath, os.Getpid())

	if logPath == "" {
		return runQueue(ctx, cfg, env)
	}
	log, err := openQueueLog(logPath)
	if err != nil {
		return err
	}
	defer func() { _ = log.Close() }()
	env.out = log

	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-ctx.Done():
				return
			case <-hup:
				if err := log.reopen(); err != nil {
					_, _ = fmt.Fprintf(env.out, "queue: reopen log: %v\n", err)
				}
			}
		}
	}()
	err = runQueue(ctx, cfg, env)
	<-done
	return err
}

func writeQueuePID(path string, pid int) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create queue pid directory: %w", err)
	}
	if err := os.WriteFile(path, []byte(strconv.Itoa(pid)+"\n"), 0o644); err != nil {
		return fmt.Errorf("write queue pid file %s: %w", path, err)
	}
	return nil
}

func removeQueuePID(path string, pid int) {
	b, err := os.ReadFile(path)
	if err != nil || strings.TrimSpace(string(b)) != strconv.Itoa(pid) {
		return
	}
	_ = os.Remove(path)
}

var _ io.WriteCloser = (*reopenableQueueLog)(nil)
