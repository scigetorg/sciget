//go:build !unix

package main

import "os/exec"

// detach does nothing where sessions do not exist; containers run only on
// Unix anyway.
func detach(cmd *exec.Cmd) {}
