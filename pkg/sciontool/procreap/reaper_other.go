//go:build !unix

/*
Copyright 2025 The Scion Authors.
*/

package procreap

// StartReaper is a no-op: reaping orphaned children is the job of PID 1 in
// an agent container, which never runs on this platform.
func StartReaper() {}
