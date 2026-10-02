//go:build !linux

/*
Copyright 2026 The Scion Authors.
*/

package dirfd

import (
	"errors"
	"fmt"
)

// ChownTreeNoFollow's real implementation walks a directory tree using
// openat(O_PATH) and fchownat(..., AT_EMPTY_PATH), both Linux-specific
// kernel features (see walk_linux.go) with no equivalent on this platform.
// There is no root/workload uid privilege boundary to enforce here — this
// platform never runs the enforced privilege-drop container path this
// function exists for — so callers get a clear, typed refusal instead of a
// silent no-op that would look indistinguishable from "nothing needed
// chowning".
func ChownTreeNoFollow(root string, uid, gid int, shouldChown func(entryUID uint32) bool, guardHardlinks bool, onErr func(name string, err error)) (walked, changed int, err error) {
	return 0, 0, fmt.Errorf("dirfd: ChownTreeNoFollow %s: %w on this platform", root, errors.ErrUnsupported)
}
