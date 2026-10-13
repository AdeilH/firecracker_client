//go:build !unix

package main

import "os/exec"

// setupExecKill is a no-op where process groups are unavailable; the
// WaitDelay backstop in opRunInVM still bounds the wait.
func setupExecKill(cmd *exec.Cmd) {}
