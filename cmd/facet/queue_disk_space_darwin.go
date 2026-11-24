//go:build darwin

package main

import "golang.org/x/sys/unix"

// statfsUnit is the size of one block in a darwin statfs result. Bsize is
// the fundamental block size; available bytes are Bavail of them.
func statfsUnit(st unix.Statfs_t) int64 {
	return int64(st.Bsize)
}
