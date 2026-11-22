//go:build !darwin

package mux

import "os"

// execPath is the path of the executable pid runs, or "" when it cannot be
// read.
func execPath(pid string) string {
	p, err := os.Readlink("/proc/" + pid + "/exe")
	if err != nil {
		return ""
	}
	return p
}
