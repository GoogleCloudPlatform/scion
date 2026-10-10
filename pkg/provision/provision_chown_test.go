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

package provision

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubLchown replaces lchownFile with one that fails every call with errno,
// wrapped in an *os.PathError the way os.Lchown reports it. The files keep
// whatever owner they already have (the test process's own uid:gid), which
// is what a squashing NFS export looks like to root in the init container.
func stubLchown(t *testing.T, errno syscall.Errno) {
	t.Helper()
	orig := lchownFile
	lchownFile = func(name string, uid, gid int) error {
		return &os.PathError{Op: "lchown", Path: name, Err: errno}
	}
	t.Cleanup(func() { lchownFile = orig })
}

func chownTestTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.txt"), []byte("a"), 0644))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "sub"), 0770))
	require.NoError(t, os.WriteFile(filepath.Join(root, "sub", "b.txt"), []byte("b"), 0644))
	// A dangling symlink: the owner check must Lstat the link itself, never
	// follow it, or this entry would fail with ENOENT.
	require.NoError(t, os.Symlink(filepath.Join(root, "missing"), filepath.Join(root, "dangling")))
	return root
}

func TestChownProjectTree_EPERM_OwnerMatches_Tolerated(t *testing.T) {
	root := chownTestTree(t)
	stubLchown(t, syscall.EPERM)

	err := chownProjectTree(context.Background(), root, "", os.Getuid(), os.Getgid())
	assert.NoError(t, err)
}

func TestChownProjectTree_EPERM_OwnerMismatch_Fails(t *testing.T) {
	root := chownTestTree(t)
	stubLchown(t, syscall.EPERM)
	wantUID, wantGID := os.Getuid()+1, os.Getgid()

	err := chownProjectTree(context.Background(), root, "", wantUID, wantGID)
	require.Error(t, err)
	assert.ErrorIs(t, err, syscall.EPERM)
	assert.Contains(t, err.Error(), root)
	assert.Contains(t, err.Error(), fmt.Sprintf("owner is %d:%d, want %d:%d",
		os.Getuid(), os.Getgid(), wantUID, wantGID))
}

func TestChownProjectTree_EPERM_GroupMismatch_Fails(t *testing.T) {
	root := chownTestTree(t)
	stubLchown(t, syscall.EPERM)

	err := chownProjectTree(context.Background(), root, "", os.Getuid(), os.Getgid()+1)
	require.Error(t, err)
	assert.ErrorIs(t, err, syscall.EPERM)
	assert.Contains(t, err.Error(), "owner is")
}

// -1 is lchown's "leave unchanged" value, so an EPERM on an entry whose
// other ID already matches is tolerated, while a mismatch on the ID that was
// actually requested still fails.
func TestChownProjectTree_EPERM_UnchangedID_MatchesAny(t *testing.T) {
	root := chownTestTree(t)
	stubLchown(t, syscall.EPERM)

	assert.NoError(t, chownProjectTree(context.Background(), root, "", -1, os.Getgid()))
	assert.NoError(t, chownProjectTree(context.Background(), root, "", os.Getuid(), -1))
	assert.NoError(t, chownProjectTree(context.Background(), root, "", -1, -1))

	err := chownProjectTree(context.Background(), root, "", -1, os.Getgid()+1)
	require.Error(t, err)
	assert.ErrorIs(t, err, syscall.EPERM)
}

// captureSlog routes the default slog logger into a buffer at Info level
// for the rest of the test.
func captureSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(orig) })
	return &buf
}

const toleratedSummaryMsg = "chown not permitted on some entries; ownership already matched"

func TestChownProjectTree_EPERM_OwnerMatches_LogsOneSummary(t *testing.T) {
	root := chownTestTree(t)
	stubLchown(t, syscall.EPERM)
	buf := captureSlog(t)

	require.NoError(t, chownProjectTree(context.Background(), root, "", os.Getuid(), os.Getgid()))

	out := buf.String()
	assert.Equal(t, 1, strings.Count(out, toleratedSummaryMsg), out)
	// root, a.txt, sub, sub/b.txt, dangling.
	assert.Contains(t, out, "count=5")
	assert.Contains(t, out, "root="+root)
}

func TestChownProjectTree_Success_NoSummary(t *testing.T) {
	root := chownTestTree(t)
	orig := lchownFile
	lchownFile = func(string, int, int) error { return nil }
	t.Cleanup(func() { lchownFile = orig })
	buf := captureSlog(t)

	require.NoError(t, chownProjectTree(context.Background(), root, "", os.Getuid(), os.Getgid()))
	assert.NotContains(t, buf.String(), toleratedSummaryMsg)
}

func TestChownProjectTree_NonEPERM_OwnerMatches_StillFails(t *testing.T) {
	for _, errno := range []syscall.Errno{syscall.EACCES, syscall.EIO, syscall.EROFS} {
		t.Run(errno.Error(), func(t *testing.T) {
			root := chownTestTree(t)
			stubLchown(t, errno)

			err := chownProjectTree(context.Background(), root, "", os.Getuid(), os.Getgid())
			require.Error(t, err)
			assert.ErrorIs(t, err, errno)
		})
	}
}

// TestProvisionShared_RequireChownSuccess_EPERM_OwnerMatches is the
// end-to-end form of the all_squash case: chown is refused, but the
// workspace is already owned by the requested uid:gid, so provisioning
// succeeds and writes the sentinel.
func TestProvisionShared_RequireChownSuccess_EPERM_OwnerMatches(t *testing.T) {
	stubLchown(t, syscall.EPERM)
	hostPath := filepath.Join(t.TempDir(), "workspace")

	err := ProvisionShared(ProvisionInput{
		Resolved:            ResolvedWorkspace{HostPath: hostPath},
		ProjectID:           "proj-chown-eperm-match",
		Mode:                store.SharingModeSharedPlain,
		NFSUID:              os.Getuid(),
		NFSGID:              os.Getgid(),
		SentinelDir:         hostPath,
		RequireChownSuccess: true,
	})
	require.NoError(t, err)
	_, statErr := os.Stat(filepath.Join(hostPath, ProvisionSentinelFile))
	assert.NoError(t, statErr, "sentinel must be written")
}

func TestProvisionShared_RequireChownSuccess_EPERM_OwnerMismatch(t *testing.T) {
	stubLchown(t, syscall.EPERM)
	hostPath := filepath.Join(t.TempDir(), "workspace")

	err := ProvisionShared(ProvisionInput{
		Resolved:            ResolvedWorkspace{HostPath: hostPath},
		ProjectID:           "proj-chown-eperm-mismatch",
		Mode:                store.SharingModeSharedPlain,
		NFSUID:              os.Getuid() + 1,
		NFSGID:              os.Getgid(),
		SentinelDir:         hostPath,
		RequireChownSuccess: true,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "owner is")
	_, statErr := os.Stat(filepath.Join(hostPath, ProvisionSentinelFile))
	assert.Error(t, statErr, "sentinel must not be written")
}

// 0 (unset) means the default owner id 1000; any other value is kept.
func TestDefaultOwnerID(t *testing.T) {
	for _, tc := range []struct{ in, want int }{
		{0, 1000},
		{1000, 1000},
		{2000, 2000},
	} {
		if got := DefaultOwnerID(tc.in); got != tc.want {
			t.Errorf("DefaultOwnerID(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}
