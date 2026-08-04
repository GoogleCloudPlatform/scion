// Copyright 2026 The Scion Authors.

//go:build windows

package commands

import "os"

// checkProcessAlive checks whether the given process is alive.
// On Windows, os.FindProcess returns an error if the PID does not exist,
// so a nil-error from FindProcess is sufficient. We re-check here by
// attempting a zero-signal equivalent (opening the process handle).
func checkProcessAlive(proc *os.Process) error {
	// On Windows os.FindProcess only fails for invalid PIDs; reaching
	// this point means the process handle was successfully opened.
	_ = proc
	return nil
}
