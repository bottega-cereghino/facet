//go:build windows

package seat

import (
	"os"
	"os/exec"
	"strconv"
)

// configureProcessGroup is a no-op here. Windows has no setpgid: a child is
// tied to its parent only through a job object it does not get by default, so
// there is nothing to arrange at Start time. The guarantee the Unix side buys
// with a process group is bought at kill time instead, in killProcessTree.
func configureProcessGroup(_ *exec.Cmd) {}

// killProcessTree walks and kills the whole tree. `taskkill /T` is what
// internal/gitx already uses for the same reason (facet#52): this codebase
// shells out to real tools rather than reimplement OS process-tree semantics
// in Go, and on Windows an orphaned grandchild is worse than on Unix because
// it keeps a lock on every file it has open.
func killProcessTree(p *os.Process) error {
	return exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(p.Pid)).Run()
}
