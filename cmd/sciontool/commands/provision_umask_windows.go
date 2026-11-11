//go:build windows

/*
Copyright 2026 The Scion Authors.
*/

package commands

// setProvisionUmask is a no-op on Windows, which has no umask.
func setProvisionUmask() (restore func()) { return func() {} }
