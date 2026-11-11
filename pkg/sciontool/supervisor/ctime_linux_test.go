//go:build linux

/*
Copyright 2026 The Scion Authors.
*/

package supervisor

import "syscall"

// statCtime returns st's change time in a field name that varies by GOOS:
// linux's syscall.Stat_t names it Ctim; darwin's names the equivalent field
// Ctimespec (see the darwin-tagged sibling of this file).
func statCtime(st *syscall.Stat_t) syscall.Timespec {
	return st.Ctim
}
