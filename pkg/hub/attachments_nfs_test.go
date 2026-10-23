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

//go:build !no_sqlite

package hub

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/projectfs"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Chat attachments on NFS-backed shared dirs (ptone/scion#5827,
// ptone/scion#5829). Phase 1 left these tests pinning the old refusal; phase
// 4 flips them: the scratchpad resolves through the layout authority to the
// uuid tree every agent mounts, projection writes and ingest reads go
// through Resolved.Open (pkg/shareddirs' anchored O_NOFOLLOW walk), and a
// resolution failure still never falls back to a local directory.

// nfsAttachmentServer mirrors agentAttachmentServer, but with
// server.shared_dir_storage: nfs in the hub's server config (and in the
// global settings file under a fresh HOME, set BEFORE testServer(t) runs;
// see setNFSSharedDirStorageGlobalSettings's own docstring).
func nfsAttachmentServer(t *testing.T) (srv *Server, project *store.Project, hostBase string) {
	t.Helper()

	hostBase = setNFSSharedDirStorageGlobalSettings(t)
	srv, s := testServer(t)
	useNFSSharedDirStorage(srv, hostBase)

	db := openTestMemorySQLite(t, "sqlite")

	wcs := NewWebChatStore(db, "sqlite")
	require.NoError(t, wcs.Init())
	srv.SetWebChatStore(wcs)

	as, err := NewLocalDiskAttachmentStore(t.TempDir())
	require.NoError(t, err)
	srv.SetAttachmentStore(as)

	project = &store.Project{
		ID:         api.NewUUID(),
		Name:       "attach-nfs-project",
		Slug:       "attach-nfs-project",
		SharedDirs: []api.SharedDir{{Name: attachmentSharedDirName}},
	}
	require.NoError(t, s.CreateProject(context.Background(), project))

	return srv, project, hostBase
}

// nfsScratchpadLeaf is the scratchpad leaf agents mount for project.
func nfsScratchpadLeaf(hostBase string, project *store.Project) string {
	return filepath.Join(hostBase, "projects", project.ID, "shared-dirs", attachmentSharedDirName)
}

// TestResolveScratchpad_NFSBackend_UUIDTree: an NFS-backed scratchpad
// resolves to the uuid tree agents mount, and staging is set to create the
// leaf when it is missing.
func TestResolveScratchpad_NFSBackend_UUIDTree(t *testing.T) {
	srv, project, hostBase := nfsAttachmentServer(t)

	sp := srv.resolveScratchpad(context.Background(), project.ID)
	require.NotNil(t, sp, "an NFS-backed scratchpad is resolved, not refused")
	assert.Equal(t, projectfs.BackendNFS, sp.resolution.Resolved.Backend)
	assert.Equal(t, nfsScratchpadLeaf(hostBase, project), sp.resolution.Resolved.HostPath)

	staging := srv.resolveAttachmentStaging(context.Background(), project.ID)
	require.NotNil(t, staging)
	assert.True(t, staging.create, "staging creates the NFS leaf when missing")
	assert.Equal(t, "/scion-volumes/scratchpad/.attachments/_webchat", staging.agentDir())
}

// TestAttachmentStaging_NFSBackend_ProjectsIntoUUIDTree: staging writes the
// projection into the uuid tree (creating the missing leaf), and returns the
// container path computed from the scratchpad Mount's Target.
func TestAttachmentStaging_NFSBackend_ProjectsIntoUUIDTree(t *testing.T) {
	srv, project, hostBase := nfsAttachmentServer(t)
	leaf := nfsScratchpadLeaf(hostBase, project)
	require.NoDirExists(t, leaf)

	staging := srv.resolveAttachmentStaging(context.Background(), project.ID)
	require.NotNil(t, staging)

	src := writeTempFile(t, "notes.txt", "hello sibling")
	agentPath, err := staging.stage(src, "att-1", "notes.txt")
	require.NoError(t, err)
	assert.Equal(t, "/scion-volumes/scratchpad/.attachments/_webchat/att-1/notes.txt", agentPath)

	got, err := os.ReadFile(filepath.Join(leaf, ".attachments", "_webchat", "att-1", "notes.txt"))
	require.NoError(t, err, "the projection must land in the leaf agents mount")
	assert.Equal(t, "hello sibling", string(got))
}

// TestAttachmentStaging_WorkspaceStorageNFS_ProjectsIntoUUIDTree is the T4
// case (only server.workspace_storage: nfs): the projection lands under the
// workspace export's uuid tree, as phase 1 does for web uploads.
func TestAttachmentStaging_WorkspaceStorageNFS_ProjectsIntoUUIDTree(t *testing.T) {
	f := newSharedDirLayoutFixture(t)
	t.Setenv("K_SERVICE", "scion-hub") // a Cloud Run Service: NFS is durable
	f.srv.config.WorkspaceStorageConfig = nfsConfig(f.mountRoot)
	require.NoError(t, os.MkdirAll(filepath.Join(f.mountRoot, "share1"), 0o755))

	staging := f.srv.resolveAttachmentStaging(context.Background(), f.project.ID)
	require.NotNil(t, staging)
	src := writeTempFile(t, "a.png", "png")
	agentPath, err := staging.stage(src, "att-t4", "a.png")
	require.NoError(t, err)
	assert.Equal(t, "/scion-volumes/scratchpad/.attachments/_webchat/att-t4/a.png", agentPath)

	leaf := filepath.Join(f.mountRoot, "share1", "projects", f.project.ID, "shared-dirs", "scratchpad")
	assert.FileExists(t, filepath.Join(leaf, ".attachments", "_webchat", "att-t4", "a.png"))
	_, err = os.Stat(resolveTestSharedDirPath(t, f.project, "scratchpad"))
	assert.True(t, os.IsNotExist(err), "nothing may be written to the hub's local slug-keyed tree")
}

// TestResolveScratchpad_CloudRunLocalLayout_NotProjected: on a Cloud Run
// Service whose shared dirs resolve to local disk, nothing is projected
// (no agent mounts the instance disk, and a recycle discards it).
func TestResolveScratchpad_CloudRunLocalLayout_NotProjected(t *testing.T) {
	f := newSharedDirLayoutFixture(t)
	t.Setenv("K_SERVICE", "scion-hub")
	dir := resolveTestSharedDirPath(t, f.project, "scratchpad")
	require.NoError(t, os.MkdirAll(dir, 0o755))

	assert.Nil(t, f.srv.resolveAttachmentStaging(context.Background(), f.project.ID))
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Empty(t, entries)
}

// TestAttachmentStaging_NFSBackend_NoFallbackToStaleLocalDir: a stale local
// project-configs directory, left over from before the project moved to
// NFS, is never used: the projection goes to the NFS leaf only.
func TestAttachmentStaging_NFSBackend_NoFallbackToStaleLocalDir(t *testing.T) {
	srv, project, hostBase := nfsAttachmentServer(t)

	home, err := os.UserHomeDir()
	require.NoError(t, err)
	staleLocalDir := config.SharedDirHostPath(home, project.Slug, project.ID, attachmentSharedDirName)
	require.NoError(t, os.MkdirAll(staleLocalDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(staleLocalDir, "marker.txt"), []byte("stale"), 0o644))

	staging := srv.resolveAttachmentStaging(context.Background(), project.ID)
	require.NotNil(t, staging)
	_, err = staging.stage(writeTempFile(t, "n.txt", "x"), "att-1", "n.txt")
	require.NoError(t, err)
	assert.FileExists(t, filepath.Join(nfsScratchpadLeaf(hostBase, project), ".attachments", "_webchat", "att-1", "n.txt"))

	entries, err := os.ReadDir(staleLocalDir)
	require.NoError(t, err)
	require.Len(t, entries, 1, "the stale local dir must be untouched, only the marker file present")
	assert.Equal(t, "marker.txt", entries[0].Name())
}

// TestIngestAgentAttachments_NFSBackend_ReadsThroughAuthority: a file the
// CLI staged in the NFS scratchpad is ingested (read through the anchored
// root), so agent --attach works on NFS projects.
func TestIngestAgentAttachments_NFSBackend_ReadsThroughAuthority(t *testing.T) {
	srv, project, hostBase := nfsAttachmentServer(t)
	leaf := nfsScratchpadLeaf(hostBase, project)
	require.NoError(t, os.MkdirAll(leaf, 0o2775))
	agentPath := stageAgentFile(t, leaf, "report.txt", "from the agent")

	refs, warnings := srv.ingestAgentAttachments(context.Background(), project.ID, "agent-1", []string{agentPath})
	require.Empty(t, warnings)
	require.Len(t, refs, 1)
	assert.Equal(t, "report.txt", refs[0].Name)
}

// TestIngestAgentAttachments_NFSBackend_SymlinkNoLeak: a directory symlink
// planted inside the NFS leaf, pointing at a victim outside the export, is
// never followed: os.Root refuses the escape, so nothing outside is read.
func TestIngestAgentAttachments_NFSBackend_SymlinkNoLeak(t *testing.T) {
	srv, project, hostBase := nfsAttachmentServer(t)
	outside := newOutsideTree(t)

	leaf := nfsScratchpadLeaf(hostBase, project)
	require.NoError(t, os.MkdirAll(leaf, 0o2775))
	require.NoError(t, os.Symlink(outside.dir, filepath.Join(leaf, "escape")))

	before := outside.snapshot(t)
	refs, warnings := srv.ingestAgentAttachments(context.Background(), project.ID, "agent-1", []string{
		"/scion-volumes/" + attachmentSharedDirName + "/escape/secret.txt",
	})
	assert.Empty(t, refs, "a path through a planted symlink must not be ingested")
	assert.Len(t, warnings, 1)
	outside.assertIntact(t, before)
}

// TestAttachmentStaging_NFSBackend_SymlinkedAttachmentsDirNoOutsideWrite: a
// planted .attachments symlink in the NFS leaf is not followed by staging.
func TestAttachmentStaging_NFSBackend_SymlinkedAttachmentsDirNoOutsideWrite(t *testing.T) {
	srv, project, hostBase := nfsAttachmentServer(t)
	outside := newOutsideTree(t)
	leaf := nfsScratchpadLeaf(hostBase, project)
	require.NoError(t, os.MkdirAll(leaf, 0o2775))
	require.NoError(t, os.Symlink(filepath.Join(outside.dir, "landing"), filepath.Join(leaf, ".attachments")))
	before := outside.snapshot(t)

	staging := srv.resolveAttachmentStaging(context.Background(), project.ID)
	require.NotNil(t, staging)
	_, err := staging.stage(writeTempFile(t, "n.txt", "x"), "att-1", "n.txt")
	assert.Error(t, err)
	outside.assertIntact(t, before)
}

// TestResolveScratchpad_NFSResolutionError_SymlinkedPid_NoFallback: when the
// NFS resolution itself errors -- a symlinked <pid> component with an
// existing victim leaf, caught by the symlink-escape backstop -- nothing is
// resolved and no stale local directory is used.
func TestResolveScratchpad_NFSResolutionError_SymlinkedPid_NoFallback(t *testing.T) {
	srv, project, hostBase := nfsAttachmentServer(t)

	victim := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(victim, "shared-dirs", attachmentSharedDirName), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(hostBase, "projects"), 0o755))
	require.NoError(t, os.Symlink(victim, filepath.Join(hostBase, "projects", project.ID)))

	home, err := os.UserHomeDir()
	require.NoError(t, err)
	staleLocalDir := config.SharedDirHostPath(home, project.Slug, project.ID, attachmentSharedDirName)
	require.NoError(t, os.MkdirAll(staleLocalDir, 0o755))

	assert.Nil(t, srv.resolveScratchpad(context.Background(), project.ID))
	assert.Nil(t, srv.resolveAttachmentStaging(context.Background(), project.ID))
}

// TestResolveScratchpad_NFSResolutionError_InvalidBlock_NoFallback: the same
// fail-closed requirement for an nfs block that fails Validate (no
// mount_root).
func TestResolveScratchpad_NFSResolutionError_InvalidBlock_NoFallback(t *testing.T) {
	srv, project, _ := nfsAttachmentServer(t)
	srv.config.SharedDirStorageConfig = invalidNFSSharedDirStorageTestConfig()

	home, err := os.UserHomeDir()
	require.NoError(t, err)
	staleLocalDir := config.SharedDirHostPath(home, project.Slug, project.ID, attachmentSharedDirName)
	require.NoError(t, os.MkdirAll(staleLocalDir, 0o755))

	assert.Nil(t, srv.resolveScratchpad(context.Background(), project.ID))
}

// TestResolveScratchpad_ResolveErrorWarnLoggedOncePerProject: a storage
// resolution failure is logged once per project, not once per dispatch.
func TestResolveScratchpad_ResolveErrorWarnLoggedOncePerProject(t *testing.T) {
	buf := captureSlog(t)
	srv, project, _ := nfsAttachmentServer(t)
	srv.config.SharedDirStorageConfig = invalidNFSSharedDirStorageTestConfig()

	_ = srv.resolveScratchpad(context.Background(), project.ID)
	_ = srv.resolveScratchpad(context.Background(), project.ID)

	count := strings.Count(buf.String(), "shared dir storage resolution failed")
	assert.Equal(t, 1, count, "the resolution-error Warn must be emitted once per project")
}
