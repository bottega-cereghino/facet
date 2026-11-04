//go:build !windows

package seat

import (
	"os"
	"os/exec"
	"syscall"
)

// configureProcessGroup puts the generator in a process group of its own, so
// that killProcessTree can name the group rather than the direct child.
func configureProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcessTree kills the GROUP, not the process. A negative pid is the
// group, and it is safe here only because configureProcessGroup gave the child
// one of its own -- without that it would name facet's own group.
func killProcessTree(p *os.Process) error {
	return syscall.Kill(-p.Pid, syscall.SIGKILL)
}
