//go:build unix

package main

import (
	"os/exec"
	"syscall"
)

// detach makes cmd start in its own session, so closing the terminal or the
// TUI does not stop it.
func detach(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }
