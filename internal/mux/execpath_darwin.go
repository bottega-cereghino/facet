package mux

import (
	"bytes"
	"strconv"

	"golang.org/x/sys/unix"
)

// execPath is the path of the executable pid runs, as the kernel recorded it
// at exec (kern.procargs2 starts with argc, then that path), or "" when it
// cannot be read: a process of another user, or one gone.
func execPath(pid string) string {
	n, err := strconv.Atoi(pid)
	if err != nil {
		return ""
	}
	buf, err := unix.SysctlRaw("kern.procargs2", n)
	if err != nil || len(buf) < 4 {
		return ""
	}
	path, _, _ := bytes.Cut(buf[4:], []byte{0})
	return string(path)
}
