// Copyright 2026 The Scion Authors.

//go:build !windows

package commands

import (
	"os"
	"syscall"
)

// checkProcessAlive checks whether the given process is alive using signal 0.
// On Unix, os.FindProcess always succeeds, so we probe with signal 0.
func checkProcessAlive(proc *os.Process) error {
	return proc.Signal(syscall.Signal(0))
}
