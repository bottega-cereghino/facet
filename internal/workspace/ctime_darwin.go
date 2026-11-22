package workspace

import (
	"io/fs"
	"syscall"
)

// changeTime is fi's status change time in nanoseconds, or 0 when the
// filesystem does not say. Unlike the modification time, no call sets it
// back: any write, chmod or touch moves it to now.
func changeTime(fi fs.FileInfo) int64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return st.Ctimespec.Nano()
	}
	return 0
}
