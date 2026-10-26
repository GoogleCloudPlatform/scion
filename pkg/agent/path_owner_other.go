//go:build !unix

// Copyright 2026 The Scion Authors.

package agent

// pathOwner reports no owner: this platform has no numeric file owner.
func pathOwner(path string) (uid, gid int, ok bool) {
	return 0, 0, false
}
