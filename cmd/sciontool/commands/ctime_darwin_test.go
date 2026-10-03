//go:build darwin

/*
Copyright 2026 The Scion Authors.
*/

package commands

import "syscall"

// statCtime returns st's change time in a field name that varies by GOOS:
// darwin's syscall.Stat_t names it Ctimespec; linux's names the equivalent
// field Ctim (see the linux-tagged sibling of this file). Centralizing the
// field access here is what lets the rest of this package's tests compare
// change times without themselves needing a build-tagged split.
func statCtime(st *syscall.Stat_t) syscall.Timespec {
	return st.Ctimespec
}
