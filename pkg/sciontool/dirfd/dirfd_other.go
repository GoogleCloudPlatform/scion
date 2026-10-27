//go:build !unix

/*
Copyright 2026 The Scion Authors.
*/

package dirfd

// The dirfd primitives rely on openat(2)-style directory file descriptors
// and O_NOFOLLOW, which this platform does not offer. They guard the
// in-container (sciontool) and broker host paths, neither of which runs
// here; on this platform every operation fails with errors.ErrUnsupported,
// so a caller that reaches one by mistake gets a clear refusal rather than
// an unchecked path-based fallback.

import (
	"errors"
	"fmt"
	"os"
)

var (
	ErrNotSingleLinkRegular           = errors.New("dirfd: not a single-link regular file")
	ErrTooLarge                       = errors.New("dirfd: content exceeds size limit")
	ErrPathEscapesRoot                = errors.New("dirfd: path escapes root")
	ErrTooManySymlinksUnderRoot       = errors.New("dirfd: too many symlinks resolving path under root")
	ErrTooManyTrustedAncestorSymlinks = errors.New("dirfd: too many symlinks resolving a trusted ancestor chain")
	ErrHardlinkedRegularFile          = errors.New("dirfd: regular file has more than one hard link, refusing to chown")
	ErrMaxWalkDepthExceeded           = errors.New("dirfd: max walk depth exceeded, not descending further")
	ErrUnlinked                       = errors.New("dirfd: file has no link left")
	ErrNotOwned                       = errors.New("dirfd: not owned by the required user, or writable by others")
)

// LeafPolicy mirrors the unix type so callers compile; see safeio.go.
type LeafPolicy int

const (
	LeafPolicyUnset LeafPolicy = iota
	ReplaceLeaf
	RefuseSymlink
	TruncateInPlaceOrCreate
)

// Stat carries the file owner fields callers read from ReadAtNoFollowStat.
type Stat struct {
	Uid uint32
	Gid uint32
}

func unsupported(op, path string) error {
	return fmt.Errorf("dirfd: %s %s: %w on this platform", op, path, errors.ErrUnsupported)
}

func OpenParentNoFollow(path string) (dirFd int, leaf string, err error) {
	return -1, "", unsupported("OpenParentNoFollow", path)
}

func CreateExclAt(dirFd int, name string, mode os.FileMode) (*os.File, error) {
	return nil, unsupported("CreateExclAt", name)
}

func OpenAt(dirFd int, name string, flags int, mode os.FileMode) (*os.File, error) {
	return nil, unsupported("OpenAt", name)
}

func EnsureDirNoFollow(path string, mode os.FileMode) (*os.File, error) {
	return nil, unsupported("EnsureDirNoFollow", path)
}

func EnsureDirNoFollowUnderRoot(root, path string, mode os.FileMode, uid, gid int) (dirFd int, underRoot bool, err error) {
	return -1, false, unsupported("EnsureDirNoFollowUnderRoot", path)
}

func RenameAt(dirFd int, oldName, newName string) error {
	return unsupported("RenameAt", oldName)
}

func UnlinkAt(dirFd int, name string) error {
	return unsupported("UnlinkAt", name)
}

func RefuseSymlinkOrNonRegularAt(dirFd int, name string) error {
	return unsupported("RefuseSymlinkOrNonRegularAt", name)
}

func ReadAtNoFollow(dirFd int, name string, max int64) ([]byte, error) {
	return nil, unsupported("ReadAtNoFollow", name)
}

func ReadAtNoFollowStat(dirFd int, name string, max int64) ([]byte, Stat, error) {
	return nil, Stat{}, unsupported("ReadAtNoFollowStat", name)
}

func ReadFileNoFollow(path string, max int64) ([]byte, error) {
	return nil, unsupported("ReadFileNoFollow", path)
}

func WriteFileNoFollow(path string, data []byte, mode os.FileMode, uid, gid int, policy LeafPolicy) error {
	return unsupported("WriteFileNoFollow", path)
}

func WriteFileNoFollowWithChown(path string, data []byte, mode os.FileMode, uid, gid int, policy LeafPolicy, chown func(fd, uid, gid int) error) error {
	return unsupported("WriteFileNoFollowWithChown", path)
}

func WriteAtNoFollowWithChown(dirFd int, leaf, pathForErrors string, data []byte, mode os.FileMode, uid, gid int, policy LeafPolicy, chown func(fd, uid, gid int) error) error {
	return unsupported("WriteAtNoFollowWithChown", pathForErrors)
}

func ReadUnderRootNoFollow(root, path string, max int64) ([]byte, error) {
	return nil, unsupported("ReadUnderRootNoFollow", path)
}

func SetTrustedAncestorOwnerUIDForTest(uid int) (restore func()) {
	return func() {}
}

func EnsureDirTrustedAncestorFollow(path string) (dirFd int, err error) {
	return -1, unsupported("EnsureDirTrustedAncestorFollow", path)
}

func OpenParentNoFollowRootOwned(path string) (dirFd int, leaf string, err error) {
	return -1, "", unsupported("OpenParentNoFollowRootOwned", path)
}

func OpenNoFollowRootOwnedFile(path string) (*os.File, error) {
	return nil, unsupported("OpenNoFollowRootOwnedFile", path)
}

func VerifyRootOwnedExecutable(candidate string) error {
	return unsupported("VerifyRootOwnedExecutable", candidate)
}

func OpenParentNoFollowOwnedBy(base, path string, uid uint32) (dirFd int, leaf string, err error) {
	return -1, "", unsupported("OpenParentNoFollowOwnedBy", path)
}

func OpenParentNoFollowUnder(base, rel string) (dirFd int, leaf string, err error) {
	return -1, "", unsupported("OpenParentNoFollowUnder", rel)
}

func OpenDirNoFollow(path string) (*os.File, error) {
	return nil, unsupported("OpenDirNoFollow", path)
}

func RemoveContentsNoFollow(dir *os.File, keep func(name string) bool, onErr func(name string, err error)) (removed int, err error) {
	name := ""
	if dir != nil {
		name = dir.Name()
	}
	return 0, unsupported("RemoveContentsNoFollow", name)
}

// Close is a no-op: this package never returns a real descriptor here.
func Close(fd int) error { return nil }

// Fchown is unsupported on this platform.
func Fchown(fd, uid, gid int) error { return unsupported("Fchown", "") }
