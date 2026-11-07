//go:build linux

/*
Copyright 2026 The Scion Authors.
*/

package netcap

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"unsafe"
)

// Kernel ABI constants (linux/prctl.h, linux/capability.h). Defined here
// rather than imported so this package depends only on the standard library.
const (
	prCapbsetDrop           = 24
	prCapAmbient            = 47
	prCapAmbientLower       = 3
	linuxCapabilityVersion3 = 0x20080522
)

type capHeader struct {
	version uint32
	pid     int32
}

type capData struct {
	effective   uint32
	permitted   uint32
	inheritable uint32
}

// Seams for tests. allThreadsSyscall6 applies a syscall to every OS thread
// of the process (syscall.AllThreadsSyscall6 returns ENOTSUP when cgo is
// linked in, which DropNetAdmin treats as a failure). capget reads the
// calling thread's sets; capsetAllThreads writes every thread's.
// readThreadStatuses returns /proc/self/task/*/status.
var (
	allThreadsSyscall6 = syscall.AllThreadsSyscall6
	capget             = realCapget
	capsetAllThreads   = realCapsetAllThreads
	readThreadStatuses = realReadThreadStatuses
)

func realCapget(hdr *capHeader, data *[2]capData) error {
	_, _, e := syscall.RawSyscall(syscall.SYS_CAPGET,
		uintptr(unsafe.Pointer(hdr)), uintptr(unsafe.Pointer(data)), 0)
	if e != 0 {
		return e
	}
	return nil
}

// realCapsetAllThreads runs capset(2) on every thread. The header's pid is
// 0, so each thread sets its own sets.
func realCapsetAllThreads(hdr *capHeader, data *[2]capData) syscall.Errno {
	_, _, e := allThreadsSyscall6(syscall.SYS_CAPSET,
		uintptr(unsafe.Pointer(hdr)), uintptr(unsafe.Pointer(data)), 0, 0, 0, 0)
	runtime.KeepAlive(hdr)
	runtime.KeepAlive(data)
	return e
}

func realReadThreadStatuses() ([]string, error) {
	paths, err := filepath.Glob("/proc/self/task/*/status")
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			// A thread can exit between Glob and ReadFile; its status is
			// gone with it and it can no longer start anything.
			if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
				continue
			}
			return nil, err
		}
		out = append(out, string(b))
	}
	return out, nil
}

// Held reports whether any thread of this process holds CAP_NET_ADMIN in
// any capability set (bounding included).
func Held() (bool, error) {
	statuses, err := readThreadStatuses()
	if err != nil {
		return false, fmt.Errorf("check %s: read thread status: %w", CapabilityName, err)
	}
	return anyHolds(statuses, capNetAdmin)
}

// Usable reports whether every thread of this process has CAP_NET_ADMIN in
// its effective and bounding sets, that is, whether this process (and a
// program it executes as root) can change network configuration now.
func Usable() (bool, error) {
	statuses, err := readThreadStatuses()
	if err != nil {
		return false, fmt.Errorf("check %s: read thread status: %w", CapabilityName, err)
	}
	return allUsable(statuses, capNetAdmin)
}

// DropNetAdmin removes CAP_NET_ADMIN from every capability set of every
// thread, then verifies the result from /proc. When no thread holds the
// capability in any set it returns nil without making any prctl or capset
// call, so a start that was never granted the capability (gVisor, Cloud
// Run, restricted pods) cannot fail here.
//
// Order matters: the bounding-set drop needs CAP_SETPCAP in the effective
// set, which the capset step leaves untouched (only bit 12 is cleared).
func DropNetAdmin() error {
	held, err := Held()
	if err != nil {
		return err
	}
	if !held {
		return nil
	}

	// Ambient first: harmless if already clear. EINVAL means the kernel
	// predates ambient capabilities, so there is nothing to lower; the
	// verify step below still checks CapAmb if the kernel reports it.
	if _, _, e := allThreadsSyscall6(syscall.SYS_PRCTL, prCapAmbient, prCapAmbientLower, capNetAdmin, 0, 0, 0); e != 0 && e != syscall.EINVAL {
		return fmt.Errorf("drop %s from ambient set: %w", CapabilityName, e)
	}

	if _, _, e := allThreadsSyscall6(syscall.SYS_PRCTL, prCapbsetDrop, capNetAdmin, 0, 0, 0, 0); e != 0 {
		return fmt.Errorf("drop %s from bounding set: %w", CapabilityName, e)
	}

	hdr := capHeader{version: linuxCapabilityVersion3}
	var data [2]capData
	if err := capget(&hdr, &data); err != nil {
		return fmt.Errorf("read capabilities before dropping %s: %w", CapabilityName, err)
	}
	mask := ^uint32(1 << capNetAdmin) // bit 12 lives in the low word
	data[0].effective &= mask
	data[0].permitted &= mask
	data[0].inheritable &= mask
	hdr = capHeader{version: linuxCapabilityVersion3} // pid 0: each thread sets itself
	if e := capsetAllThreads(&hdr, &data); e != 0 {
		return fmt.Errorf("drop %s from effective, permitted and inheritable sets: %w", CapabilityName, e)
	}

	statuses, err := readThreadStatuses()
	if err != nil {
		return fmt.Errorf("verify %s: read thread status: %w", CapabilityName, err)
	}
	return verifyNone(statuses, capNetAdmin)
}
