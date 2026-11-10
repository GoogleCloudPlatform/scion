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
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// sharedTreeDir makes dir carry mode (a mode_t value, setgid and sticky
// included) and a non-zero owner and group, and returns that uid and gid.
// Run as root it gives dir a fixed non-root owner and group; otherwise dir
// keeps this process's uid and its own group, and the test is skipped when
// that group is 0 or the kernel drops a requested setgid bit.
func sharedTreeDir(t *testing.T, dir string, mode uint32) (uid, gid int) {
	t.Helper()
	if unix.Geteuid() == 0 {
		require.NoError(t, os.Chown(dir, 4243, 4242))
	}
	require.NoError(t, unix.Chmod(dir, mode))
	var st unix.Stat_t
	require.NoError(t, unix.Stat(dir, &st))
	if st.Gid == 0 || st.Uid == 0 {
		t.Skip("needs a directory with a non-zero owner and group")
	}
	if uint32(st.Mode)&0o7777 != mode {
		t.Skipf("the kernel kept mode %#o, not %#o", uint32(st.Mode)&0o7777, mode)
	}
	return int(st.Uid), int(st.Gid)
}

// walkWarnPrefix starts the walk's warning about an existing upper
// directory; other warnings (such as the once-per-process ACL warning) are
// not counted.
const walkWarnPrefix = "NFS directory walk: an existing upper directory is group/other-writable"

// warnRecords returns the walk's upper-directory warnings among records.
func warnRecords(records []slog.Record) []slog.Record {
	var out []slog.Record
	for _, r := range records {
		if r.Level == slog.LevelWarn && strings.HasPrefix(r.Message, walkWarnPrefix) {
			out = append(out, r)
		}
	}
	return out
}

// TestWarnIfIntermediateUnsafe_ExpectedSharedLayout: an existing upper
// directory is not warned about only when it is not other-writable, has the
// setgid bit and the expected share gid, and is owned by this process or the
// expected nfs uid. Every row other than the two quiet ones breaks exactly
// one of those conditions and keeps the existing warning.
//
// Kill sets: dropping the other-writable condition fails "other-writable"
// and "other-writable with sticky"; dropping the owner condition fails
// "foreign owner". TestHasExpectedSharedLayout checks the same conditions,
// and the zero-gid and zero-uid guards, without a filesystem.
func TestWarnIfIntermediateUnsafe_ExpectedSharedLayout(t *testing.T) {
	const (
		self = iota
		nfsUID
		foreign
	)
	for _, tc := range []struct {
		name     string
		mode     uint32
		owner    int  // who owns the directory: this process, the nfs uid or neither
		gidDelta int  // expected gid = the directory's gid + gidDelta
		noOwner  bool // pass the zero Owner (unknown share gid)
		wantWarn bool
	}{
		{name: "expected layout, owned by this process", mode: 0o2775, owner: self},
		{name: "expected layout, owned by the nfs uid", mode: 0o2775, owner: nfsUID},
		{name: "other-writable", mode: 0o2777, owner: self, wantWarn: true},
		{name: "other-writable with sticky", mode: 0o3777, owner: self, wantWarn: true},
		{name: "expected owner with the wrong gid", mode: 0o2775, owner: self, gidDelta: 1, wantWarn: true},
		{name: "nfs-uid owner with the wrong gid", mode: 0o2775, owner: nfsUID, gidDelta: 1, wantWarn: true},
		{name: "no setgid", mode: 0o0775, owner: self, wantWarn: true},
		{name: "foreign owner", mode: 0o2775, owner: foreign, wantWarn: true},
		{name: "zero Owner", mode: 0o2775, owner: self, noOwner: true, wantWarn: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetIntermediateUnsafeWarnOnce(t)
			records := captureSlog(t)
			dir := t.TempDir()
			dirUID, dirGID := sharedTreeDir(t, dir, tc.mode)

			// The directory's owner is this process (geteuid), the nfs
			// uid (expected.UID) or neither.
			euid, expectedUID := dirUID, dirUID+1
			switch tc.owner {
			case nfsUID:
				euid, expectedUID = dirUID+1, dirUID
			case foreign:
				euid, expectedUID = dirUID+1, dirUID+2
			}
			oldEuid := geteuid
			geteuid = func() int { return euid }
			t.Cleanup(func() { geteuid = oldEuid })

			expected := Owner{UID: expectedUID, GID: dirGID + tc.gidDelta}
			if tc.noOwner {
				expected = Owner{}
			}
			fd := openDirFd(t, dir)
			quiet := warnIfIntermediateUnsafe(fd, "projects", expected)

			warns := warnRecords(*records)
			if tc.wantWarn {
				assert.False(t, quiet)
				require.Len(t, warns, 1)
				assert.Contains(t, warns[0].Message, "group/other-writable or not owned by this broker")
				assertRecordHasAttr(t, warns[0], "component", "projects")
			} else {
				assert.True(t, quiet, "the expected shared layout is reported as such")
				assert.Empty(t, warns, "the expected shared layout is not warned about")
			}
		})
	}
}

// TestEnsureLeafExpecting_ExpectedLayoutThroughRealWalk: the walk does not
// warn about an existing upper directory with the expected shared layout,
// logs one Debug line with only a count, and creates the leaf exactly as
// EnsureLeaf does: nothing is chowned even for a root creator. EnsureLeaf
// (no expected owner) still warns about the same tree.
func TestEnsureLeafExpecting_ExpectedLayoutThroughRealWalk(t *testing.T) {
	rel := filepath.Join("projects", "pid-1", "shared-dirs", "artifacts")
	setup := func(t *testing.T) (base string, expected Owner) {
		base = t.TempDir()
		projects := filepath.Join(base, "projects")
		require.NoError(t, os.Mkdir(projects, 0o755))
		uid, gid := sharedTreeDir(t, projects, 0o2775)
		return base, Owner{UID: uid, GID: gid}
	}

	t.Run("expected owner", func(t *testing.T) {
		base, expected := setup(t)
		resetIntermediateUnsafeWarnOnce(t)
		records := captureSlog(t)
		calls := recordChowns(t, 0)

		fd, existed, err := EnsureLeafExpecting(base, rel, expected)
		require.NoError(t, err)
		t.Cleanup(func() { _ = CloseFd(fd) })
		assert.False(t, existed)

		assert.Empty(t, warnRecords(*records), "the expected shared layout is not warned about")
		var debug []slog.Record
		for _, r := range *records {
			if r.Level == slog.LevelDebug {
				debug = append(debug, r)
			}
		}
		require.Len(t, debug, 1, "one Debug line per walk")
		assertRecordHasAttr(t, debug[0], "count", "1")
		debug[0].Attrs(func(a slog.Attr) bool {
			assert.Equal(t, "count", a.Key, "the Debug line carries only a count")
			return true
		})
		assert.Empty(t, *calls, "a check-only expected owner chowns nothing")
		info, err := os.Stat(filepath.Join(base, rel))
		require.NoError(t, err)
		assert.Equal(t, os.ModeDir|os.ModeSetgid|0o775, info.Mode(), "the leaf is created as EnsureLeaf creates it")
	})

	t.Run("EnsureLeaf still warns", func(t *testing.T) {
		base, _ := setup(t)
		resetIntermediateUnsafeWarnOnce(t)
		records := captureSlog(t)

		fd, _, err := EnsureLeaf(base, rel)
		require.NoError(t, err)
		t.Cleanup(func() { _ = CloseFd(fd) })

		warns := warnRecords(*records)
		require.Len(t, warns, 1)
		assertRecordHasAttr(t, warns[0], "component", "projects")
	})
}

// statFor returns a directory Stat_t with mode (a mode_t value), uid and
// gid. Mode is set through reflection because its width differs between
// platforms (uint32 on Linux, uint16 on Darwin and the BSDs).
func statFor(mode uint32, uid, gid uint32) unix.Stat_t {
	var st unix.Stat_t
	reflect.ValueOf(&st).Elem().FieldByName("Mode").SetUint(uint64(unix.S_IFDIR | mode))
	st.Uid, st.Gid = uid, gid
	return st
}

// TestHasExpectedSharedLayout checks the expected-layout conditions on
// synthetic stat results, so no row depends on the test host's ids or on
// the kernel keeping a setgid bit. This process's effective uid is 1000
// (the geteuid seam); the share is nfs uid 2000, gid 3000.
//
// Kill sets: dropping the other-writable condition fails "other-writable"
// and "other-writable with sticky"; dropping the owner condition fails
// "foreign owner" and "uid 0 with expected uid 0"; dropping the zero-gid
// guard fails "gid 0 with the zero Owner"; dropping only the zero-uid guard
// fails "uid 0 with expected uid 0".
func TestHasExpectedSharedLayout(t *testing.T) {
	oldEuid := geteuid
	geteuid = func() int { return 1000 }
	t.Cleanup(func() { geteuid = oldEuid })
	share := Owner{UID: 2000, GID: 3000}

	for _, tc := range []struct {
		name     string
		st       unix.Stat_t
		expected Owner
		want     bool
	}{
		{name: "owned by this process", st: statFor(0o2775, 1000, 3000), expected: share, want: true},
		{name: "owned by the nfs uid", st: statFor(0o2775, 2000, 3000), expected: share, want: true},
		{name: "not group-writable", st: statFor(0o2755, 2000, 3000), expected: share, want: true},
		{name: "other-writable", st: statFor(0o2777, 1000, 3000), expected: share},
		{name: "other-writable with sticky", st: statFor(0o3777, 1000, 3000), expected: share},
		{name: "wrong gid", st: statFor(0o2775, 1000, 3001), expected: share},
		{name: "nfs-uid owner with the wrong gid", st: statFor(0o2775, 2000, 3001), expected: share},
		{name: "no setgid", st: statFor(0o0775, 1000, 3000), expected: share},
		{name: "foreign owner", st: statFor(0o2775, 4000, 3000), expected: share},
		{name: "gid 0 with the zero Owner", st: statFor(0o2775, 1000, 0), expected: Owner{}},
		{name: "uid 0 with expected uid 0", st: statFor(0o2775, 0, 3000), expected: Owner{UID: 0, GID: 3000}},
		{name: "negative expected gid", st: statFor(0o2775, 1000, 3000), expected: Owner{UID: 2000, GID: -1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, hasExpectedSharedLayout(tc.st, tc.expected))
		})
	}
}
