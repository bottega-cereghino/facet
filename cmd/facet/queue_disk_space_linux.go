//go:build linux

package main

import "golang.org/x/sys/unix"

// statfsUnit is the size of one block in a linux statfs result. Frsize is
// the fragment size, which is what available bytes are counted in. Bsize
// there is the preferred I/O size, so it is only the fallback.
func statfsUnit(st unix.Statfs_t) int64 {
	if st.Frsize > 0 {
		return st.Frsize
	}
	return st.Bsize
}
