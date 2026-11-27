// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build unix

package shipper

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/GoogleCloudPlatform/scion/pkg/util/privatefs"
	"golang.org/x/sys/unix"
)

// Injectable seams for cross-UID and owner-UID testing (production defaults
// call the real OS syscalls using os.Geteuid consistently).
var (
	geteuidFn = os.Geteuid

	statOwnerFn = func(_ string, st *unix.Stat_t) (uint32, uint32) {
		return st.Uid, st.Gid
	}

	fchownFD = func(fd int, uid, gid int, _ string) error {
		return unix.Fchown(fd, uid, gid)
	}

	mkdiratFD = func(dirfd int, path string, mode uint32) error {
		return unix.Mkdirat(dirfd, path, mode)
	}

	renameatFD = func(olddirfd int, oldpath string, newdirfd int, newpath string, _, _ string) error {
		return unix.Renameat(olddirfd, oldpath, newdirfd, newpath)
	}
)

// wrapRootChownErr annotates EPERM/EACCES from root filesystem operations
// (fchown, mkdirat, openat) with ErrRootSquashChownRefused so root-squashed
// NFS exports fail with a distinct, actionable error.
func wrapRootChownErr(label string, err error) error {
	if errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES) {
		return fmt.Errorf("%s: %w: %w", label, ErrRootSquashChownRefused, err)
	}
	return fmt.Errorf("%s: %w", label, err)
}

// checkOwnerUID enforces the single-owner contract using geteuidFn:
//   - Root (euid == 0) accepts objects owned by root (0) or by homeUID.
//   - Non-root accepts only objects owned by its own euid.
func checkOwnerUID(label string, ownerUID, homeUID uint32) error {
	euid := geteuidFn()
	if euid == 0 {
		if ownerUID != 0 && ownerUID != homeUID {
			return fmt.Errorf("%s owned by uid %d, want 0 or %d: %w",
				label, ownerUID, homeUID, privatefs.ErrNotPrivate)
		}
		return nil
	}
	if int(ownerUID) != euid {
		return fmt.Errorf("%s owned by uid %d, want %d: %w",
			label, ownerUID, euid, privatefs.ErrNotPrivate)
	}
	return nil
}

// openPinnedStateDir opens <home>/.scion/trajectory-shipper relative to a
// pinned file descriptor on homeDir using openat(O_DIRECTORY|O_NOFOLLOW),
// eliminating path-based TOCTOU races. When create is true, missing
// directories are created via mkdirat(0700) and chowned to the home directory's
// owner when euid == 0.
//
// If create is false and either directory does not exist, it returns
// (-1, 0, 0, os.ErrNotExist).
func openPinnedStateDir(homeDir string, create bool) (dirFD int, homeUID, homeGID uint32, err error) {
	homeFD, err := unix.Open(homeDir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, 0, 0, fmt.Errorf("open home dir %s: %w", homeDir, err)
	}

	var homeStat unix.Stat_t
	if err := unix.Fstat(homeFD, &homeStat); err != nil {
		_ = unix.Close(homeFD)
		return -1, 0, 0, fmt.Errorf("fstat home dir %s: %w", homeDir, err)
	}
	homeUID, homeGID = statOwnerFn(homeDir, &homeStat)

	scionPath := filepath.Join(homeDir, ".scion")
	scionFD, err := openOrCreateDirNoFollow(homeFD, ".scion", scionPath, homeUID, homeGID, create)
	_ = unix.Close(homeFD)
	if err != nil {
		return -1, 0, 0, err
	}

	shipperPath := StateDir(homeDir)
	shipperFD, err := openOrCreateDirNoFollow(scionFD, "trajectory-shipper", shipperPath, homeUID, homeGID, create)
	_ = unix.Close(scionFD)
	if err != nil {
		return -1, 0, 0, err
	}

	return shipperFD, homeUID, homeGID, nil
}

func openOrCreateDirNoFollow(parentFD int, name, displayPath string, homeUID, homeGID uint32, create bool) (int, error) {
	created := false
	if create {
		if err := mkdiratFD(parentFD, name, 0o700); err == nil {
			created = true
		} else if !errors.Is(err, syscall.EEXIST) {
			if geteuidFn() == 0 {
				return -1, wrapRootChownErr("mkdirat "+displayPath, err)
			}
			return -1, fmt.Errorf("mkdirat %s: %w", displayPath, err)
		}
	}

	fd, err := unix.Openat(parentFD, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, syscall.ENOENT) {
			return -1, os.ErrNotExist
		}
		if errors.Is(err, syscall.ELOOP) || errors.Is(err, syscall.ENOTDIR) {
			return -1, fmt.Errorf("refusing state dir ancestor %s: %w", displayPath, privatefs.ErrReparsePoint)
		}
		if geteuidFn() == 0 {
			return -1, wrapRootChownErr("openat "+displayPath, err)
		}
		return -1, fmt.Errorf("openat %s: %w", displayPath, err)
	}

	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		_ = unix.Close(fd)
		return -1, fmt.Errorf("fstat %s: %w", displayPath, err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		_ = unix.Close(fd)
		return -1, fmt.Errorf("state dir ancestor %s is not a directory", displayPath)
	}

	ownerUID, _ := statOwnerFn(displayPath, &st)
	if err := checkOwnerUID("state dir ancestor "+displayPath, ownerUID, homeUID); err != nil {
		_ = unix.Close(fd)
		return -1, err
	}

	if created && geteuidFn() == 0 && ownerUID != homeUID {
		if cerr := fchownFD(fd, int(homeUID), int(homeGID), displayPath); cerr != nil {
			_ = unix.Close(fd)
			_ = unix.Unlinkat(parentFD, name, unix.AT_REMOVEDIR)
			return -1, wrapRootChownErr("fchown "+displayPath, cerr)
		}
	}

	return fd, nil
}

// openStateLockFile opens or creates <home>/.scion/trajectory-shipper/state.lock
// through a pinned directory descriptor (O_NOFOLLOW), enforcing private
// ownership and chowning to the home owner when created as root.
func openStateLockFile(homeDir string) (*os.File, error) {
	dirFD, homeUID, homeGID, err := openPinnedStateDir(homeDir, true)
	if err != nil {
		return nil, err
	}
	defer func() { _ = unix.Close(dirFD) }()

	lockPath := LockPath(homeDir)
	created := false
	fd, err := unix.Openat(dirFD, LockFileName, unix.O_RDWR|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0o600)
	if err == nil {
		created = true
	} else if errors.Is(err, syscall.EEXIST) {
		fd, err = unix.Openat(dirFD, LockFileName, unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
		if errors.Is(err, syscall.EACCES) || errors.Is(err, syscall.EPERM) {
			fd, err = unix.Openat(dirFD, LockFileName, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
		}
	}
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return nil, fmt.Errorf("refusing lock file %s: %w", lockPath, privatefs.ErrReparsePoint)
		}
		return nil, fmt.Errorf("openat lock file %s: %w", lockPath, err)
	}

	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("fstat lock file %s: %w", lockPath, err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("lock path %s is not a regular file", lockPath)
	}

	if created {
		if geteuidFn() == 0 && st.Nlink != 1 {
			_ = unix.Close(fd)
			_ = unix.Unlinkat(dirFD, LockFileName, 0)
			return nil, fmt.Errorf("refusing hard-linked lock file %s (nlink=%d): %w", lockPath, st.Nlink, ErrHardLinkRefused)
		}
		if err := unix.Fchmod(fd, 0o600); err != nil {
			_ = unix.Close(fd)
			return nil, fmt.Errorf("fchmod lock file %s: %w", lockPath, err)
		}
		if geteuidFn() == 0 {
			if err := fchownFD(fd, int(homeUID), int(homeGID), lockPath); err != nil {
				_ = unix.Close(fd)
				return nil, wrapRootChownErr("fchown lock file "+lockPath, err)
			}
		}
	} else {
		if geteuidFn() == 0 && st.Nlink != 1 {
			_ = unix.Close(fd)
			return nil, fmt.Errorf("refusing hard-linked lock file %s (nlink=%d): %w", lockPath, st.Nlink, ErrHardLinkRefused)
		}
		ownerUID, _ := statOwnerFn(lockPath, &st)
		if err := checkOwnerUID("lock file "+lockPath, ownerUID, homeUID); err != nil {
			_ = unix.Close(fd)
			return nil, err
		}
		// Never fchown a pre-existing lock file: root only chowns files it
		// freshly created via O_CREAT|O_EXCL with Nlink == 1.
	}

	f := os.NewFile(uintptr(fd), lockPath)
	if f == nil {
		_ = unix.Close(fd)
		return nil, errors.New("failed to wrap lock file descriptor")
	}
	return f, nil
}

// readStateFile reads <home>/.scion/trajectory-shipper/state.json through a
// pinned directory descriptor (O_NOFOLLOW), verifying regular-file type,
// 0600-compatible permissions, and owner UID.
func readStateFile(homeDir string) ([]byte, bool, error) {
	dirFD, homeUID, _, err := openPinnedStateDir(homeDir, false)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, err
	}
	defer func() { _ = unix.Close(dirFD) }()

	statePath := StatePath(homeDir)
	fd, err := unix.Openat(dirFD, StateFileName, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, syscall.ENOENT) {
			return nil, false, nil
		}
		if errors.Is(err, syscall.ELOOP) {
			return nil, false, fmt.Errorf("refusing state file %s: %w", statePath, privatefs.ErrReparsePoint)
		}
		return nil, false, fmt.Errorf("openat state file %s: %w", statePath, err)
	}

	f := os.NewFile(uintptr(fd), statePath)
	if f == nil {
		_ = unix.Close(fd)
		return nil, false, errors.New("failed to wrap state file descriptor")
	}
	defer func() { _ = f.Close() }()

	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return nil, false, fmt.Errorf("fstat state file %s: %w", statePath, err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		return nil, false, fmt.Errorf("refusing %s: not a regular file", statePath)
	}
	if st.Mode&0o077 != 0 {
		return nil, false, fmt.Errorf("%w: %s is group- or world-accessible (mode %04o)",
			privatefs.ErrNotPrivate, statePath, st.Mode&0o777)
	}
	ownerUID, _ := statOwnerFn(statePath, &st)
	if geteuidFn() == 0 {
		if st.Nlink != 1 {
			return nil, false, fmt.Errorf("refusing hard-linked state file %s (nlink=%d): %w", statePath, st.Nlink, ErrHardLinkRefused)
		}
		if homeUID != 0 && ownerUID != homeUID {
			return nil, false, fmt.Errorf("state file %s owned by uid %d, want %d: %w",
				statePath, ownerUID, homeUID, privatefs.ErrNotPrivate)
		}
	}
	if err := checkOwnerUID("state file "+statePath, ownerUID, homeUID); err != nil {
		return nil, false, err
	}
	if st.Size > MaxStateFileBytes {
		return nil, false, fmt.Errorf("shipper state %s exceeds maximum size of %d bytes (size=%d): %w",
			statePath, MaxStateFileBytes, st.Size, ErrStateTooLarge)
	}

	data, err := io.ReadAll(io.LimitReader(f, MaxStateFileBytes+1))
	if err != nil {
		return nil, false, fmt.Errorf("read shipper state %s: %w", statePath, err)
	}
	if int64(len(data)) > MaxStateFileBytes {
		return nil, false, fmt.Errorf("shipper state %s exceeds maximum size of %d bytes: %w",
			statePath, MaxStateFileBytes, ErrStateTooLarge)
	}
	return data, true, nil
}

// writeStateFile atomically writes data to <home>/.scion/trajectory-shipper/state.json
// through a pinned directory descriptor (temp file + fchmod 0600 + fchown to
// home owner when root + fsync + renameat).
func writeStateFile(homeDir string, data []byte) error {
	dirFD, homeUID, homeGID, err := openPinnedStateDir(homeDir, true)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(dirFD) }()

	stateDir := StateDir(homeDir)
	statePath := StatePath(homeDir)

	var (
		tmpFD   int
		tmpName string
		tmpPath string
	)
	for attempt := 0; attempt < 16; attempt++ {
		cand, rerr := randomStateTmpName()
		if rerr != nil {
			return fmt.Errorf("generate temp state filename: %w", rerr)
		}
		fd, oerr := unix.Openat(dirFD, cand, unix.O_RDWR|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
		if oerr == nil {
			tmpFD = fd
			tmpName = cand
			tmpPath = filepath.Join(stateDir, cand)
			break
		}
		if errors.Is(oerr, syscall.EEXIST) {
			continue
		}
		return fmt.Errorf("create temp shipper state file: %w", oerr)
	}
	if tmpName == "" {
		return errors.New("create temp shipper state file: exhausted name attempts")
	}

	cleanup := true
	tmpFile := os.NewFile(uintptr(tmpFD), tmpPath)
	defer func() {
		if cleanup {
			_ = unix.Unlinkat(dirFD, tmpName, 0)
		}
		_ = tmpFile.Close()
	}()

	if err := unix.Fchmod(tmpFD, 0o600); err != nil {
		return fmt.Errorf("fchmod temp shipper state file: %w", err)
	}
	if geteuidFn() == 0 {
		var tmpStat unix.Stat_t
		if err := unix.Fstat(tmpFD, &tmpStat); err != nil {
			return fmt.Errorf("fstat temp shipper state file: %w", err)
		}
		if tmpStat.Mode&unix.S_IFMT != unix.S_IFREG || tmpStat.Nlink != 1 {
			return fmt.Errorf("refusing hard-linked temp state file %s (nlink=%d): %w", tmpPath, tmpStat.Nlink, ErrHardLinkRefused)
		}
		if err := fchownFD(tmpFD, int(homeUID), int(homeGID), tmpPath); err != nil {
			return wrapRootChownErr("fchown temp shipper state file", err)
		}
	}

	if _, err := tmpFile.Write(data); err != nil {
		return fmt.Errorf("write temp shipper state file: %w", err)
	}
	if err := tmpFile.Sync(); err != nil {
		return fmt.Errorf("fsync temp shipper state file: %w", err)
	}
	if err := renameatFD(dirFD, tmpName, dirFD, StateFileName, tmpPath, statePath); err != nil {
		return fmt.Errorf("rename shipper state file: %w", err)
	}
	cleanup = false
	return nil
}

func randomStateTmpName() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "state-" + hex.EncodeToString(b[:]) + ".tmp", nil
}
