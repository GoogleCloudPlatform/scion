//go:build !windows

/*
Copyright 2026 The Scion Authors.
*/

package commands

import "syscall"

// setProvisionUmask sets umask 002 for the provisioning init container's
// worktree step and returns a func that restores the previous umask.
func setProvisionUmask() (restore func()) {
	old := syscall.Umask(0o002)
	return func() { syscall.Umask(old) }
}
