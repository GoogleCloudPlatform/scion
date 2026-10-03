//go:build darwin

/*
Copyright 2026 The Scion Authors.
*/

package supervisor

import "syscall"

// statCtime returns st's change time in a field name that varies by GOOS:
// darwin's syscall.Stat_t names it Ctimespec; linux's names the equivalent
// field Ctim (see the linux-tagged sibling of this file).
func statCtime(st *syscall.Stat_t) syscall.Timespec {
	return st.Ctimespec
}
