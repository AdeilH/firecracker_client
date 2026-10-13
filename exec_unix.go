//go:build unix

package main

import (
	"os/exec"
	"syscall"
)

// setupExecKill puts the child in its own process group and arranges for a
// context timeout to kill the whole group. Killing only the ssh client can
// leave shell children holding the output pipes open, which would stall
// CombinedOutput long past the deadline.
func setupExecKill(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
