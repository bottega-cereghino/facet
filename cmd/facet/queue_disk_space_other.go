//go:build !linux && !darwin

package main

import "fmt"

// diskFreeBytes has no probe on this system. The caller refuses new work.
func diskFreeBytes(path string) (int64, error) {
	return 0, fmt.Errorf("free space of %s is not available on this system\nfix: run the queue on the machine whose disk it watches", path)
}
