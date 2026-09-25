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
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"golang.org/x/sys/unix"
)

// Phase 2 item 2 (leaf modes/ACL hardening, ptone/scion#1794):
//   - No new "acl_group" setting: use the leaf's own OWNING group, which
//     setgid already pins to the export tree's group (scion:scion, design
//     §3.6). No named group ACL entry, no mask entry, no LookupGroup call.
//   - Set a minimal default ACL on the leaf: d:u::rwx, d:g::rwx, d:o::r-x,
//     plus the matching (non-default) access ACL entries. POSIX: once a
//     directory has ANY default ACL, new children inherit permissions from
//     it instead of from mode-minus-umask, and a default ACL's g:: entry
//     always reflects the owning group — this is what makes new files
//     group-writable regardless of the creating process's umask, without
//     needing a named group entry at all.
//   - Caveat: this only helps children
//     created with a permissive requested mode (the common case, e.g. a
//     plain `open(..., 0666)` or `mkdir(..., 0777)`, which is what every
//     libc/runtime does by default and lets the umask/ACL decide the
//     result). A creator that passes an explicit restrictive mode, e.g.
//     `open(..., 0644)`, is NOT overridden by the ACL — 0644 has no group
//     write bit to begin with, and POSIX ACL inheritance only fills in
//     permissions up to what both the ACL AND the requested mode allow.
//   - ACLs are set via raw Fsetxattr on the already-open leaf fd (no cgo),
//     so this is symlink-safe by construction, matching every other
//     operation in this package.
//   - ENOTSUP/EOPNOTSUPP (the mounted NFS export doesn't support POSIX ACLs
//     — this varies by NFS server and export configuration, not something
//     this code can assume either way) is not a failure: warn once per
//     process and keep Phase 1 behavior (the leaf stays plain 2775, setgid,
//     no ACL). Any other error fails closed, matching the existing
//     chmod-failure behavior.
//   - Only ever applied to a leaf THIS call created — never a pre-existing
//     shared dir (same rule as the chmod above it).

// POSIX ACL extended-attribute binary format (Linux `acl_ea_header` /
// `acl_ea_entry`, as read/written by libacl and the kernel's ACL xattr
// handlers via system.posix_acl_access / system.posix_acl_default). There is
// no portable Go stdlib or x/sys encoder for this, so it's encoded directly
// here rather than shelling out to setfacl (which would need a path, not an
// fd) or adding a cgo dependency on libacl.
const (
	aclEAVersion = 0x0002

	aclTagUserObj  = 0x01
	aclTagGroupObj = 0x04
	aclTagOther    = 0x20

	aclUndefinedID = 0xFFFFFFFF

	aclPermRead    = 0x04
	aclPermWrite   = 0x02
	aclPermExecute = 0x01

	xattrPosixACLAccess  = "system.posix_acl_access"
	xattrPosixACLDefault = "system.posix_acl_default"
)

// encodeMinimalPosixACL encodes a minimal, valid POSIX ACL containing
// exactly the three mandatory entries — ACL_USER_OBJ (owner), ACL_GROUP_OBJ
// (owning group), ACL_OTHER — in the kernel's expected canonical order. No
// named user/group entries means no ACL_MASK entry is required either. This
// shape is intentionally the minimum needed to express "the owning group
// gets these permissions, inherited by new children" (no named-entry form
// unless a concrete reason surfaces).
func encodeMinimalPosixACL(userObjPerm, groupObjPerm, otherPerm uint16) []byte {
	const headerSize = 4
	const entrySize = 8
	buf := make([]byte, headerSize+3*entrySize)
	binary.LittleEndian.PutUint32(buf[0:4], aclEAVersion)
	writeEntry := func(off int, tag, perm uint16) {
		binary.LittleEndian.PutUint16(buf[off:off+2], tag)
		binary.LittleEndian.PutUint16(buf[off+2:off+4], perm)
		binary.LittleEndian.PutUint32(buf[off+4:off+8], aclUndefinedID)
	}
	writeEntry(headerSize, aclTagUserObj, userObjPerm)
	writeEntry(headerSize+entrySize, aclTagGroupObj, groupObjPerm)
	writeEntry(headerSize+2*entrySize, aclTagOther, otherPerm)
	return buf
}

var (
	aclUnsupportedWarnOnce sync.Once
)

// SetLeafDefaultACL sets a minimal access+default POSIX ACL on the
// already-open leaf directory fd: u::rwx, g::rwx, o::r-x as the access ACL
// (mirroring the leaf's own 0o2775 mode, set by the caller just before this
// call), and d:u::rwx, d:g::rwx, d:o::r-x as the default ACL, so every new
// child created inside this leaf inherits group-write for the leaf's own
// (setgid-pinned) owning group regardless of the creating process's umask.
//
// Returns nil (not an error) if the mounted NFS export doesn't support
// POSIX ACLs at all (ENOTSUP/EOPNOTSUPP) — an accepted fallback, since
// support varies by NFS server and export configuration, logged once per
// process, not per shared dir, to avoid log spam. Any other error is
// returned as a real failure, matching how a chmod failure on this same fd
// already fails closed.
func SetLeafDefaultACL(fd int) error {
	rwx := uint16(aclPermRead | aclPermWrite | aclPermExecute)
	rx := uint16(aclPermRead | aclPermExecute)

	access := encodeMinimalPosixACL(rwx, rwx, rx)
	def := encodeMinimalPosixACL(rwx, rwx, rx)

	if err := unix.Fsetxattr(fd, xattrPosixACLAccess, access, 0); err != nil {
		return handleACLSetError("access", err)
	}
	if err := unix.Fsetxattr(fd, xattrPosixACLDefault, def, 0); err != nil {
		return handleACLSetError("default", err)
	}
	return nil
}

func handleACLSetError(which string, err error) error {
	if errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EOPNOTSUPP) {
		aclUnsupportedWarnOnce.Do(func() {
			slog.Warn("server.shared_dir_storage: filesystem/export does not support POSIX ACLs; "+
				"shared dirs will use setgid-only Phase 1 behavior (modes/ACL hardening degraded, ptone/scion#1794)",
				"acl_type", which, "error", err)
		})
		return nil
	}
	return fmt.Errorf("set %s ACL: %w", which, err)
}
