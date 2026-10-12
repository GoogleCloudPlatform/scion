/*
Copyright 2026 The Scion Authors.
*/

package dirfd

import (
	"fmt"
	"path/filepath"
	"strings"
	"syscall"
)

// OpenParentNoFollowUnder is OpenParentNoFollow for a path made of a
// trusted base directory and a relative path below it. base is opened
// with a plain O_DIRECTORY open, so symbolic links in base itself (for
// example a home directory reached through a linked prefix such as
// /home -> /var/home) are followed as usual. Every directory component of
// rel is then opened relative to that descriptor with
// O_DIRECTORY|O_NOFOLLOW, so a symbolic link below base is refused rather
// than followed.
//
// rel must be a non-empty relative path that stays below base: an
// absolute rel, or one containing a ".." component, is refused. The caller
// owns the returned descriptor and must close it; leaf is rel's final
// component, to be opened relative to that descriptor.
func OpenParentNoFollowUnder(base, rel string) (dirFd int, leaf string, err error) {
	if base == "" {
		return -1, "", fmt.Errorf("dirfd: empty base directory")
	}
	if rel == "" || filepath.IsAbs(rel) {
		return -1, "", fmt.Errorf("dirfd: %q is not a relative path", rel)
	}
	// Check the raw components, before any cleaning, so a ".." anywhere in
	// rel is refused as documented (even "a/../b", which cleans to "b").
	for _, p := range strings.Split(filepath.ToSlash(rel), "/") {
		if p == ".." {
			return -1, "", fmt.Errorf("dirfd: %q does not stay below its base", rel)
		}
	}
	parts := strings.Split(filepath.ToSlash(filepath.Clean(rel)), "/")
	if len(parts) == 1 && parts[0] == "." {
		return -1, "", fmt.Errorf("dirfd: %q does not name an entry below its base", rel)
	}
	leaf = parts[len(parts)-1]

	fd, err := syscall.Open(base, syscall.O_DIRECTORY|syscall.O_RDONLY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return -1, "", fmt.Errorf("dirfd: open %s: %w", base, err)
	}
	fd, err = walkNoFollow(fd, parts[:len(parts)-1])
	if err != nil {
		return -1, "", err
	}
	return fd, leaf, nil
}
