//go:build linux || darwin

package main

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// diskFreeBytes is the free space, in bytes, on the volume that holds path.
// A missing path or a failed probe is an error: the caller refuses new work
// rather than treating "could not tell" as room.
func diskFreeBytes(path string) (int64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, fmt.Errorf("statfs %s: %w\nfix: set disk.path to a directory on the volume the queue should watch", path, err)
	}
	return freeBlocks(st.Bavail, statfsUnit(st)), nil
}

// freeBlocks is avail * unit, capped so a huge volume does not overflow.
func freeBlocks(avail uint64, unit int64) int64 {
	if unit <= 0 || avail == 0 {
		return 0
	}
	u := uint64(unit)
	const maxInt64 = uint64(1<<63 - 1)
	if avail > maxInt64/u {
		return int64(maxInt64)
	}
	return int64(avail * u)
}
