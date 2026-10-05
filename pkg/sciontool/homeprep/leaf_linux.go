//go:build linux

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

package homeprep

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

// Mode bits of the agent directory and of the home directory.
const (
	agentDirMode = unix.S_ISGID | 0o775
	homeDirMode  = unix.S_ISGID | 0o771
)

// fchownFn is unix.Fchown; tests replace it to simulate an export that
// maps root to another user.
var fchownFn = unix.Fchown

// kubeletUID is the owner of an agent directory the kubelet created (root).
// Tests replace it to exercise step 0 without root.
var kubeletUID uint32 = 0

// Leaf creates the agent's home directory in the agent directory. It runs
// as root with only the capabilities to change ownership and modes, so it
// does as little as possible, all relative to an fd for the agent
// directory, never following a symbolic link and never recursing:
//
//  0. If the agent directory is owned by root (the kubelet created it) and
//     lacks mode 2775 or the export's group, it gets them. A directory
//     owned by anyone else is left alone.
//  1. mkdir home-<id> with mode 2771; an existing entry is fine.
//  2. Open it as a directory without following links; anything else
//     fails.
//  3. If it already exists with the expected owner, group and mode, done.
//  4. If this call created it: remove inherited POSIX ACLs, then set the
//     owner and group, then the mode. A refused chown (EPERM, an export
//     that maps root to another user) fails with ErrClassLeafFailed.
//  5. If it existed with any other owner, group or mode: fail with
//     ErrClassLeafMatch and change nothing.
//
// It never reads, lists or writes inside the home or any sibling.
func Leaf(opts LeafOptions) error {
	if opts.HomeName == "" || opts.UID <= 0 || opts.GID <= 0 {
		return classErr(ErrClassLeafFailed, "home name, uid and gid are required")
	}
	if _, err := cleanRelPath(opts.HomeName); err != nil || opts.HomeName != cleanName(opts.HomeName) {
		return classErr(ErrClassLeafFailed, "invalid home directory name %q", opts.HomeName)
	}
	dirFd, err := unix.Open(opts.AgentDir, unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return classErr(ErrClassLeafFailed, "cannot open the agent directory: %v", err)
	}
	defer func() { _ = unix.Close(dirFd) }()

	// Step 0.
	var st unix.Stat_t
	if err := unix.Fstat(dirFd, &st); err != nil {
		return classErr(ErrClassLeafFailed, "cannot stat the agent directory: %v", err)
	}
	if st.Uid == kubeletUID && (st.Mode&0o7777 != agentDirMode || int(st.Gid) != opts.GID) {
		if err := fchownFn(dirFd, -1, opts.GID); err != nil {
			return leafChownErr("the agent directory", err)
		}
		if err := unix.Fchmod(dirFd, agentDirMode); err != nil {
			return classErr(ErrClassLeafFailed, "cannot set the mode of the agent directory: %v", err)
		}
	}

	// Step 1.
	created := true
	if err := unix.Mkdirat(dirFd, opts.HomeName, homeDirMode); err != nil {
		if !errors.Is(err, unix.EEXIST) {
			if errors.Is(err, unix.EPERM) || errors.Is(err, unix.EACCES) {
				return classErr(ErrClassLeafFailed, "cannot create %s: %v; configure the broker host mount or use an export that does not map root to another user", opts.HomeName, err)
			}
			return classErr(ErrClassLeafFailed, "cannot create %s: %v", opts.HomeName, err)
		}
		created = false
	}

	// Step 2.
	homeFd, err := unix.Openat(dirFd, opts.HomeName, unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return classErr(ErrClassLeafMatch, "%s is not a directory: %v", opts.HomeName, err)
	}
	defer func() { _ = unix.Close(homeFd) }()
	if err := unix.Fstat(homeFd, &st); err != nil {
		return classErr(ErrClassLeafFailed, "cannot stat %s: %v", opts.HomeName, err)
	}

	if !created {
		// Steps 3 and 5.
		if int(st.Uid) == opts.UID && int(st.Gid) == opts.GID && st.Mode&0o7777 == homeDirMode {
			return nil
		}
		return classErr(ErrClassLeafMatch, "%s has %d:%d mode %04o, expected %d:%d %04o",
			opts.HomeName, st.Uid, st.Gid, st.Mode&0o7777, opts.UID, opts.GID, homeDirMode)
	}

	// Step 4. A failure removes the empty directory this call created, so
	// a later start can create it again instead of meeting a mismatch.
	fail := func(err error) error {
		_ = unix.Unlinkat(dirFd, opts.HomeName, unix.AT_REMOVEDIR)
		return err
	}
	if err := stripACLs(homeFd); err != nil {
		return fail(classErr(ErrClassLeafFailed, "cannot remove inherited ACLs from %s: %v", opts.HomeName, err))
	}
	if err := fchownFn(homeFd, opts.UID, opts.GID); err != nil {
		return fail(leafChownErr(opts.HomeName, err))
	}
	if err := unix.Fchmod(homeFd, homeDirMode); err != nil {
		return fail(classErr(ErrClassLeafFailed, "cannot set the mode of %s: %v", opts.HomeName, err))
	}
	return nil
}

func leafChownErr(what string, err error) error {
	if errors.Is(err, unix.EPERM) || errors.Is(err, unix.EACCES) || errors.Is(err, unix.EINVAL) {
		return classErr(ErrClassLeafFailed, "cannot change the owner of %s: %v; the export maps root to another user, so configure the broker host mount (home_storage_leaf: broker) or use an export without root squashing", what, err)
	}
	return classErr(ErrClassLeafFailed, "cannot change the owner of %s: %v", what, err)
}

// stripACLs removes the default and access POSIX ACLs from fd, so the
// directory's permissions are exactly its mode bits. A file system without
// ACLs, or a directory without them, is fine.
func stripACLs(fd int) error {
	for _, name := range []string{"system.posix_acl_default", "system.posix_acl_access"} {
		if err := unix.Fremovexattr(fd, name); err != nil &&
			!errors.Is(err, unix.ENODATA) && !errors.Is(err, unix.ENOTSUP) && !errors.Is(err, unix.EOPNOTSUPP) {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	return nil
}

func cleanName(s string) string {
	for _, c := range s {
		if c == '/' || c == 0 {
			return ""
		}
	}
	if s == "." || s == ".." {
		return ""
	}
	return s
}
