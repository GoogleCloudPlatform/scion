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

package provisioner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/projectfs"
	"github.com/GoogleCloudPlatform/scion/pkg/provision"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testPID = "550e8400-e29b-41d4-a716-446655440000"

func nfsLayout(hostBase string) projectfs.Layout {
	return projectfs.Layout{Backend: projectfs.BackendNFS, HostBase: hostBase, SubPathRoot: "projects"}
}

func permBits(t *testing.T, path string) uint32 {
	t.Helper()
	var st syscall.Stat_t
	require.NoError(t, syscall.Stat(path, &st))
	// Stat_t.Mode is uint32 on linux and uint16 on darwin.
	return uint32(st.Mode) & 0o7777
}

// OpenStateDir creates <ProjectRoot>/provision with the leaf contract and
// sweeps its aged lock litter on every open (ptone/scion#5836), never the
// live lock or fresh litter.
func TestProvisioner_OpenStateDir_SweepsAgedLitter(t *testing.T) {
	base := t.TempDir()
	p := Provisioner{Layout: nfsLayout(base)}
	dir, err := p.OpenStateDir(testPID)
	require.NoError(t, err)
	realBase, err := filepath.EvalSymlinks(base)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(realBase, "projects", testPID, "provision"), dir)
	assert.Equal(t, uint32(0o2775), permBits(t, dir))

	longAgo := time.Now().Add(-time.Hour)
	aged := filepath.Join(dir, ".scion-provision.lock.evict-aged")
	fresh := filepath.Join(dir, ".scion-provision.lock.evict-fresh")
	live := filepath.Join(dir, ".scion-provision.lock")
	for _, d := range []string{aged, fresh, live} {
		require.NoError(t, os.Mkdir(d, 0o770))
	}
	for _, d := range []string{aged, fresh} {
		require.NoError(t, os.WriteFile(filepath.Join(d, "evicted-at"), nil, 0o660))
	}
	require.NoError(t, os.Chtimes(filepath.Join(aged, "evicted-at"), longAgo, longAgo))
	require.NoError(t, os.Chtimes(live, longAgo, longAgo))

	_, err = p.OpenStateDir(testPID)
	require.NoError(t, err)
	assert.NoDirExists(t, aged)
	assert.DirExists(t, fresh)
	assert.DirExists(t, live, "the live lock is never removed")
}

// ProvisionSharedWorkspace is the one shared-plain provisioning path of the
// hub and the brokers: the workspace leaf and the state directory are
// created with the leaf contract, and provision.ProvisionShared gets the
// state directory as its only sentinel and lock directory.
func TestProvisioner_ProvisionSharedWorkspace_Input(t *testing.T) {
	base := t.TempDir()
	var got []provision.ProvisionInput
	p := Provisioner{
		Layout:          nfsLayout(base),
		ProvisionShared: func(in provision.ProvisionInput) error { got = append(got, in); return nil },
	}
	gc := &api.GitCloneConfig{URL: "https://github.example/org/repo.git"}
	created, err := p.ProvisionSharedWorkspace(SharedWorkspace{
		Ctx: context.Background(), ProjectID: testPID, AgentID: "agent-1",
		GitClone: gc, CloneToken: "tok", CloneTokenRemote: gc.URL,
	})
	require.NoError(t, err)
	assert.True(t, created, "the project root did not exist before")
	require.Len(t, got, 1)
	in := got[0]
	realBase, err := filepath.EvalSymlinks(base)
	require.NoError(t, err)
	root := filepath.Join(realBase, "projects", testPID)
	assert.Equal(t, filepath.Join(root, "workspace"), in.Resolved.HostPath)
	assert.Equal(t, filepath.Join("projects", testPID, "workspace"), in.Resolved.ServerRelativePath)
	assert.Equal(t, filepath.Join(root, "provision"), in.SentinelDir)
	assert.Equal(t, store.SharingModeSharedPlain, in.Mode)
	assert.True(t, in.HostGitHardened)
	assert.False(t, in.RequireChownSuccess)
	assert.Equal(t, 1000, in.NFSUID)
	assert.Equal(t, 1000, in.NFSGID)
	assert.Equal(t, "tok", in.CloneToken)
	assert.Equal(t, gc.URL, in.CloneTokenRemote)
	assert.Equal(t, uint32(0o2775), permBits(t, filepath.Join(root, "workspace")))
	assert.Equal(t, uint32(0o2775), permBits(t, filepath.Join(root, "provision")))

	created, err = p.ProvisionSharedWorkspace(SharedWorkspace{Ctx: context.Background(), ProjectID: testPID})
	require.NoError(t, err)
	assert.False(t, created, "the project root existed")
}

// With the real provisioner: the sentinel is written in the state
// directory only, nothing in the workspace root (AC-L3).
func TestProvisioner_ProvisionSharedWorkspace_RealNoClone(t *testing.T) {
	base := t.TempDir()
	l := nfsLayout(base)
	l.UID, l.GID = os.Getuid(), os.Getgid()
	_, err := Provisioner{Layout: l}.ProvisionSharedWorkspace(SharedWorkspace{Ctx: context.Background(), ProjectID: testPID})
	require.NoError(t, err)
	root := filepath.Join(base, "projects", testPID)
	assert.FileExists(t, filepath.Join(root, "provision", provision.ProvisionSentinelFile))
	entries, err := os.ReadDir(filepath.Join(root, "workspace"))
	require.NoError(t, err)
	assert.Empty(t, entries, "nothing of the provisioning in the workspace root")
}

// A storage step that fails wraps ErrStorage; a provisioning
// failure is returned as is.
func TestProvisioner_ProvisionSharedWorkspace_Errors(t *testing.T) {
	missing := Provisioner{Layout: nfsLayout(filepath.Join(t.TempDir(), "not-mounted"))}
	_, err := missing.ProvisionSharedWorkspace(SharedWorkspace{Ctx: context.Background(), ProjectID: testPID})
	require.ErrorIs(t, err, ErrStorage)

	boom := errors.New("clone failed")
	failing := Provisioner{Layout: nfsLayout(t.TempDir()), ProvisionShared: func(provision.ProvisionInput) error { return boom }}
	created, err := failing.ProvisionSharedWorkspace(SharedWorkspace{Ctx: context.Background(), ProjectID: testPID})
	require.ErrorIs(t, err, boom)
	assert.False(t, errors.Is(err, ErrStorage))
	assert.True(t, created)

	_, err = Provisioner{Layout: projectfs.Layout{Backend: projectfs.BackendLocal, HostBase: t.TempDir()}}.ProvisionSharedWorkspace(SharedWorkspace{ProjectID: testPID})
	require.Error(t, err)
}
