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

package shareddirs

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// collectHandler is a minimal slog.Handler that collects records for test
// assertions, so handleACLSetError's warn-once behavior can be verified
// directly instead of only inferred from its return value.
type collectHandler struct {
	mu      sync.Mutex
	records *[]slog.Record
}

func (h *collectHandler) Enabled(_ context.Context, _ slog.Level) bool { return true }
func (h *collectHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	*h.records = append(*h.records, r)
	return nil
}
func (h *collectHandler) WithAttrs(_ []slog.Attr) slog.Handler { return h }
func (h *collectHandler) WithGroup(_ string) slog.Handler      { return h }

// captureSlog installs a collecting slog handler as the default for the
// duration of the calling test, restoring the previous default on cleanup.
func captureSlog(t *testing.T) *[]slog.Record {
	t.Helper()
	var records []slog.Record
	old := slog.Default()
	slog.SetDefault(slog.New(&collectHandler{records: &records}))
	t.Cleanup(func() { slog.SetDefault(old) })
	return &records
}

// aclSupportProbeACL is the same bytes encodeMinimalPosixACL(7, 7, 5)
// produces, but written out as a hard-coded literal, so aclSupported acts as
// an independent support check: a bug in encodeMinimalPosixACL must not be
// able to make the probe fail/succeed in lockstep with the very code the
// round-trip tests exist to check.
var aclSupportProbeACL = []byte{
	0x02, 0x00, 0x00, 0x00, // acl_ea_header, version 2
	0x01, 0x00, 0x07, 0x00, 0xff, 0xff, 0xff, 0xff, // ACL_USER_OBJ, rwx, undefined id
	0x04, 0x00, 0x07, 0x00, 0xff, 0xff, 0xff, 0xff, // ACL_GROUP_OBJ, rwx, undefined id
	0x20, 0x00, 0x05, 0x00, 0xff, 0xff, 0xff, 0xff, // ACL_OTHER, r-x, undefined id
}

// aclSupported attempts to set a probe ACL directly via Fsetxattr,
// bypassing SetLeafDefaultACL's ENOTSUP-swallowing, so a test filesystem
// that doesn't support POSIX ACLs produces a clear, explained skip instead
// of a false pass or a confusing failure.
func aclSupported(t *testing.T, dir string, fd int) bool {
	t.Helper()
	err := unix.Fsetxattr(fd, xattrPosixACLAccess, aclSupportProbeACL, 0)
	if err == nil {
		return true
	}
	if errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EOPNOTSUPP) {
		t.Skipf("filesystem at %s does not support POSIX ACLs (%v); skipping", dir, err)
		return false
	}
	require.NoError(t, err, "unexpected error probing ACL support")
	return false
}

// TestEncodeMinimalPosixACL_GoldenBytes is a pure, filesystem-free
// golden-bytes test of the wire format itself, which never skips (unlike
// the round-trip tests below, which depend on the test
// filesystem actually supporting POSIX ACLs). Pins the exact byte layout
// documented in the comment above the function: acl_ea_header (version 2,
// little-endian) followed by ACL_USER_OBJ, ACL_GROUP_OBJ, ACL_OTHER entries
// in that order, each (tag, perm, ACL_UNDEFINED_ID) little-endian.
func TestEncodeMinimalPosixACL_GoldenBytes(t *testing.T) {
	got := encodeMinimalPosixACL(7, 7, 5)
	want := []byte{
		0x02, 0x00, 0x00, 0x00, // acl_ea_header, version 2
		0x01, 0x00, 0x07, 0x00, 0xff, 0xff, 0xff, 0xff, // ACL_USER_OBJ, rwx, undefined id
		0x04, 0x00, 0x07, 0x00, 0xff, 0xff, 0xff, 0xff, // ACL_GROUP_OBJ, rwx, undefined id
		0x20, 0x00, 0x05, 0x00, 0xff, 0xff, 0xff, 0xff, // ACL_OTHER, r-x, undefined id
	}
	assert.Equal(t, want, got)
	assert.Len(t, got, 28, "4-byte header + 3 8-byte entries")
}

// TestSetLeafDefaultACL_RoundTrip reads the ACL back with Fgetxattr and
// confirms it decodes to exactly the minimal u::rwx,g::rwx,o::r-x entries.
//
// Only the DEFAULT ACL is checked this way. The Linux VFS treats a "minimal"
// access ACL (exactly u::/g::/o::, no named entries, no mask — precisely
// what SetLeafDefaultACL writes) as equivalent to plain mode bits and does
// not persist it as a retrievable xattr: it folds the entries into the
// inode's mode and Fgetxattr(system.posix_acl_access) then correctly
// returns ENODATA. This is standard behavior (the same thing `setfacl -m
// u::rwx,g::rwx,o::r-x` does — no `+` appears in `ls -l`), not a bug; only
// the DEFAULT ACL is guaranteed to persist for a minimal, no-named-entry
// ACL, because default-ACL semantics (inheritance) have no mode-bit
// equivalent to fold into. The access-ACL-equivalent effect is verified via
// the plain mode bits instead.
func TestSetLeafDefaultACL_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	require.NoError(t, err)
	defer func() { _ = unix.Close(fd) }()

	if !aclSupported(t, dir, fd) {
		return
	}

	require.NoError(t, SetLeafDefaultACL(fd))

	buf := make([]byte, 256)
	n, err := unix.Fgetxattr(fd, xattrPosixACLDefault, buf)
	require.NoError(t, err, "read back %s", xattrPosixACLDefault)
	assert.Equal(t, encodeMinimalPosixACL(7, 7, 5), buf[:n], "%s ACL bytes", xattrPosixACLDefault)

	// The "access ACL" write folded into the mode bits (see comment above):
	// confirm the directory's own mode reflects u::rwx,g::rwx,o::r-x (0775).
	var st unix.Stat_t
	require.NoError(t, unix.Fstat(fd, &st))
	assert.Equal(t, os.FileMode(0o775), os.FileMode(uint32(st.Mode)&0o777),
		"directory mode should reflect the minimal access ACL's permission bits")
}

// TestSetLeafDefaultACL_ChildInheritsGroupWriteDespiteUmask: a child created
// with a permissive requested mode (0666, the common case for a plain
// create call) under a restrictive umask (022, which alone would strip
// group+other write) must still come out group-writable, because the
// default ACL on the parent overrides umask-based stripping for its own g::
// entry.
func TestSetLeafDefaultACL_ChildInheritsGroupWriteDespiteUmask(t *testing.T) {
	dir := t.TempDir()
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	require.NoError(t, err)
	defer func() { _ = unix.Close(fd) }()

	if !aclSupported(t, dir, fd) {
		return
	}

	require.NoError(t, SetLeafDefaultACL(fd))

	oldUmask := unix.Umask(0o022)
	defer unix.Umask(oldUmask)

	childPath := filepath.Join(dir, "child")
	childFd, err := unix.Open(childPath, unix.O_RDWR|unix.O_CREAT|unix.O_CLOEXEC, 0o666)
	require.NoError(t, err)
	defer func() { _ = unix.Close(childFd) }()

	var st unix.Stat_t
	require.NoError(t, unix.Fstat(childFd, &st))
	perm := os.FileMode(uint32(st.Mode) & 0o777)
	assert.NotZero(t, perm&0o020,
		"child must be group-writable via the inherited default ACL despite umask 022 (got mode %o)", perm)
	// The design's explicit caveat: 0664 (rw-rw-r--) or better is the
	// expected outcome for a 0666-requested child under umask 022 with the
	// default ACL in effect (umask would otherwise have produced 0644).
	assert.True(t, perm&0o660 == 0o660, "expected at least 0664-equivalent group/owner write, got %o", perm)
}

// TestSetLeafDefaultACL_ExplicitRestrictiveMode_NotOverridden pins the
// caveat precisely: a creator that requests an explicit restrictive mode
// (0644, no group write bit at all) is NOT made group-writable by the
// default ACL — POSIX ACL inheritance only grants permissions the requested
// mode also allows for that class.
func TestSetLeafDefaultACL_ExplicitRestrictiveMode_NotOverridden(t *testing.T) {
	dir := t.TempDir()
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	require.NoError(t, err)
	defer func() { _ = unix.Close(fd) }()

	if !aclSupported(t, dir, fd) {
		return
	}

	require.NoError(t, SetLeafDefaultACL(fd))

	childPath := filepath.Join(dir, "child-restrictive")
	childFd, err := unix.Open(childPath, unix.O_RDWR|unix.O_CREAT|unix.O_CLOEXEC, 0o644)
	require.NoError(t, err)
	defer func() { _ = unix.Close(childFd) }()

	var st unix.Stat_t
	require.NoError(t, unix.Fstat(childFd, &st))
	perm := os.FileMode(uint32(st.Mode) & 0o777)
	assert.Zero(t, perm&0o020,
		"an explicit 0644 create must NOT be made group-writable by the default ACL (got mode %o)", perm)
}

// TestHandleACLSetError table-tests handleACLSetError across the documented
// "not a failure" errnos (ENOTSUP, EOPNOTSUPP) versus everything else
// (EINVAL, EPERM stand in for "any other error"), and confirms the
// ENOTSUP/EOPNOTSUPP case actually logs a warning via the process default
// slog logger, not just that it swallows the error.
func TestHandleACLSetError(t *testing.T) {
	tests := []struct {
		name         string
		err          error
		wantNil      bool
		wantsWarning bool
	}{
		{"ENOTSUP", unix.ENOTSUP, true, true},
		{"EOPNOTSUPP", unix.EOPNOTSUPP, true, true},
		{"EINVAL", unix.EINVAL, false, false},
		{"EPERM", unix.EPERM, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			aclUnsupportedWarnOnce = sync.Once{} // isolate each subtest from the others
			records := captureSlog(t)

			err := handleACLSetError("access", tt.err)
			if tt.wantNil {
				assert.NoError(t, err)
			} else {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "set access ACL")
			}
			if tt.wantsWarning {
				require.Len(t, *records, 1, "expected exactly one warning logged")
				assert.Equal(t, slog.LevelWarn, (*records)[0].Level)
			} else {
				assert.Empty(t, *records, "a real failure must not also log the ENOTSUP-style warning")
			}
		})
	}
}

// TestHandleACLSetError_WarnsOnlyOncePerProcess is the other half of the
// ENOTSUP/EOPNOTSUPP classification test above: the warning is deliberately
// process-wide (sync.Once), not per-call or per-shared-dir, to avoid log
// spam when many shared dirs are created against a filesystem/export that
// doesn't support POSIX ACLs at all. Three calls -- across both errnos and
// both ACL types -- must still produce exactly one log line.
func TestHandleACLSetError_WarnsOnlyOncePerProcess(t *testing.T) {
	aclUnsupportedWarnOnce = sync.Once{}
	records := captureSlog(t)

	require.NoError(t, handleACLSetError("access", unix.ENOTSUP))
	require.NoError(t, handleACLSetError("default", unix.EOPNOTSUPP))
	require.NoError(t, handleACLSetError("access", unix.ENOTSUP))

	assert.Len(t, *records, 1,
		"the process-wide ENOTSUP/EOPNOTSUPP warning must be logged exactly once regardless of how many ACL set calls hit it")
}
