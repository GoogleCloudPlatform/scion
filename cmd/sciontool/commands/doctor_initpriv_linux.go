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

package commands

import (
	"errors"
	"fmt"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

// What readInitPrivilege reads: the /proc mount, and names relative to
// it. Package vars only as test seams; production never changes them.
var (
	initProcRoot           = "/proc"
	initProcStatusName     = "1/status"
	initProcExeName        = "1/exe"
	initProcSelfExeName    = "self/exe"
	initProcInitPidNSName  = "1/ns/pid"
	initProcSelfPidNSName  = "self/ns/pid"
	initProcSelfUIDMapName = "self/uid_map"
)

// initProcReadMax bounds each procfs file read.
const initProcReadMax = 4 << 10

// statxMountID returns fd's mount ID and whether the kernel reported it
// (STATX_MNT_ID, Linux 5.8 and later). name is the file's name below the
// /proc root, "" for the root itself. A test seam.
var statxMountID = func(fd int, name string) (id uint64, reported bool, err error) {
	var stx unix.Statx_t
	if err := unix.Statx(fd, "", unix.AT_EMPTY_PATH, unix.STATX_MNT_ID, &stx); err != nil {
		return 0, false, err
	}
	return stx.Mnt_id, stx.Mask&unix.STATX_MNT_ID != 0, nil
}

// readInitPrivilege asks the kernel whether PID 1 is this sciontool
// binary running without root. The verdict is non-root only when all of
// these hold; an effective uid of 0 is root; anything else is unknown,
// with the reason:
//
//  1. /proc is a procfs directory (fstatfs), opened without following a
//     symlink, and every file below is opened relative to it and has the
//     same mount ID (statx): a bind mount over any of them has its own.
//     A kernel that does not report mount IDs gives unknown.
//  2. All four uids on the Uid line of 1/status are non-zero.
//  3. 1/exe and self/exe, both pinned, point at the same file: PID 1
//     runs this sciontool binary.
//  4. self/uid_map is the identity map: doctor runs in the initial user
//     namespace, not in one the workload made, where it could mount its
//     own procfs and run a non-root sciontool as PID 1.
//  5. doctor and PID 1 share the pid namespace.
//
// Making a root init read as non-root needs root or CAP_SYS_ADMIN over
// the initial user namespace's mounts, which the workload does not have.
// Out of scope, as for any in-process check: a process that can ptrace
// init or write its memory, or write the sciontool binary.
func readInitPrivilege() (initPrivilege, error) {
	p, err := openProcMount()
	if err != nil {
		return initPrivUnknown, err
	}
	defer p.close()

	status, err := p.readFile(initProcStatusName)
	if err != nil {
		return initPrivUnknown, err
	}
	switch parseInitStatusUIDs(status) {
	case initPrivRoot:
		return initPrivRoot, nil
	case initPrivUnknown:
		return initPrivUnknown, fmt.Errorf("%s: no readable Uid line, or a uid other than the effective uid is 0", initProcStatusName)
	}

	same, err := p.sameExecutable(initProcExeName)
	if err != nil {
		return initPrivUnknown, err
	}
	if !same {
		return initPrivUnknown, errors.New("PID 1 is not this sciontool binary")
	}

	uidMap, err := p.readFile(initProcSelfUIDMapName)
	if err != nil {
		return initPrivUnknown, err
	}
	if !uidMapIsIdentity(uidMap) {
		return initPrivUnknown, errors.New("doctor is not in the initial user namespace")
	}

	initNS, err := p.readLink(initProcInitPidNSName)
	if err != nil {
		return initPrivUnknown, err
	}
	selfNS, err := p.readLink(initProcSelfPidNSName)
	if err != nil {
		return initPrivUnknown, err
	}
	if initNS != selfNS {
		return initPrivUnknown, errors.New("doctor and PID 1 are in different pid namespaces")
	}
	return initPrivNonRoot, nil
}

// procMount is an open /proc directory and its mount ID.
type procMount struct {
	fd  int
	mnt uint64
}

// openProcMount opens initProcRoot and checks it is a procfs directory
// whose mount ID the kernel reports.
func openProcMount() (*procMount, error) {
	fd, err := unix.Open(initProcRoot, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", initProcRoot, err)
	}
	var fs unix.Statfs_t
	if err := unix.Fstatfs(fd, &fs); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("statfs %s: %w", initProcRoot, err)
	}
	if fs.Type != unix.PROC_SUPER_MAGIC {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("%s is not procfs", initProcRoot)
	}
	mnt, err := mountIDOf(fd, "")
	if err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	return &procMount{fd: fd, mnt: mnt}, nil
}

func (p *procMount) close() { _ = unix.Close(p.fd) }

// mountIDOf returns fd's mount ID, or an error when the kernel did not
// report one.
func mountIDOf(fd int, name string) (uint64, error) {
	id, reported, err := statxMountID(fd, name)
	if err != nil {
		return 0, fmt.Errorf("statx %q: %w", name, err)
	}
	if !reported {
		return 0, fmt.Errorf("statx %q: the kernel does not report mount IDs", name)
	}
	return id, nil
}

// openAt opens name relative to the /proc directory, without following a
// final symlink, and checks it is on the same mount.
func (p *procMount) openAt(name string, flags int) (int, error) {
	fd, err := unix.Openat(p.fd, name, flags|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, fmt.Errorf("open %s: %w", name, err)
	}
	mnt, err := mountIDOf(fd, name)
	if err == nil && mnt != p.mnt {
		err = fmt.Errorf("%s is not on the %s mount", name, initProcRoot)
	}
	if err != nil {
		_ = unix.Close(fd)
		return -1, err
	}
	return fd, nil
}

// readFile reads a pinned procfs file, at most initProcReadMax bytes.
func (p *procMount) readFile(name string) ([]byte, error) {
	fd, err := p.openAt(name, unix.O_RDONLY)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), name)
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, initProcReadMax+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", name, err)
	}
	if len(data) > initProcReadMax {
		return nil, fmt.Errorf("%s is too large", name)
	}
	return data, nil
}

// readLink reads a pinned procfs link (a namespace link).
func (p *procMount) readLink(name string) (string, error) {
	fd, err := p.openAt(name, unix.O_PATH)
	if err != nil {
		return "", err
	}
	defer func() { _ = unix.Close(fd) }()
	buf := make([]byte, 256)
	n, err := unix.Readlinkat(fd, "", buf)
	if err != nil {
		return "", fmt.Errorf("readlink %s: %w", name, err)
	}
	if n >= len(buf) {
		return "", fmt.Errorf("readlink %s: too long", name)
	}
	return string(buf[:n]), nil
}

// sameExecutable reports whether the procfs exe link name points at the
// same file as self/exe, this process's executable. Both sides are read
// through the pinned /proc directory (see linkTargetStat).
func (p *procMount) sameExecutable(name string) (bool, error) {
	target, err := p.linkTargetStat(name)
	if err != nil {
		return false, err
	}
	self, err := p.linkTargetStat(initProcSelfExeName)
	if err != nil {
		return false, err
	}
	return target.Dev == self.Dev && target.Ino == self.Ino, nil
}

// linkTargetStat stats the target of a procfs link. The link is opened
// without following it and pinned to the /proc mount, and stays open
// while its target is opened through the same directory; then the link
// is opened and pinned again and must be the same link as the one held.
// A mount placed over the link around the target open is seen: the
// re-opened path then has another mount ID or inode.
func (p *procMount) linkTargetStat(name string) (unix.Stat_t, error) {
	var target unix.Stat_t
	lfd, err := p.openAt(name, unix.O_PATH)
	if err != nil {
		return target, err
	}
	defer func() { _ = unix.Close(lfd) }()
	var held unix.Stat_t
	if err := unix.Fstat(lfd, &held); err != nil {
		return target, fmt.Errorf("stat %s: %w", name, err)
	}
	tfd, err := unix.Openat(p.fd, name, unix.O_PATH|unix.O_CLOEXEC, 0)
	if err != nil {
		return target, fmt.Errorf("open %s target: %w", name, err)
	}
	ferr := unix.Fstat(tfd, &target)
	_ = unix.Close(tfd)
	if ferr != nil {
		return target, fmt.Errorf("stat %s target: %w", name, ferr)
	}
	after, err := linkReopenStat(p, name)
	if err != nil {
		return target, err
	}
	if held.Dev != after.Dev || held.Ino != after.Ino {
		return target, fmt.Errorf("%s changed while it was read", name)
	}
	return target, nil
}

// linkReopenStat is the second, pinned stat of a link in linkTargetStat;
// a test seam.
var linkReopenStat = (*procMount).linkStat

// linkStat stats the procfs link name itself, pinned to the /proc mount.
func (p *procMount) linkStat(name string) (unix.Stat_t, error) {
	var st unix.Stat_t
	fd, err := p.openAt(name, unix.O_PATH)
	if err != nil {
		return st, err
	}
	defer func() { _ = unix.Close(fd) }()
	if err := unix.Fstat(fd, &st); err != nil {
		return st, fmt.Errorf("stat %s: %w", name, err)
	}
	return st, nil
}
