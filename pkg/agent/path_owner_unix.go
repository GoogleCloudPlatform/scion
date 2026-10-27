//go:build unix

// Copyright 2026 The Scion Authors.

package agent

import (
	"os"
	"syscall"
)

func pathOwner(path string) (uid, gid int, ok bool) {
	fi, err := os.Lstat(path)
	if err != nil {
		return 0, 0, false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return int(st.Uid), int(st.Gid), true
}
