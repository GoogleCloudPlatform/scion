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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeTempFile writes content to a new file under t.TempDir() and returns its path.
func writeTempFile(t *testing.T, name, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(p, []byte(content), 0o644))
	return p
}

func TestNewAttachmentStaging_Paths(t *testing.T) {
	st := newAttachmentStaging("/home/scion/.scion/project-configs/proj__abcd1234/shared-dirs/scratchpad", false)
	require.NotNil(t, st)
	assert.Equal(t,
		"/home/scion/.scion/project-configs/proj__abcd1234/shared-dirs/scratchpad/.attachments/_webchat",
		st.hostDir)
	assert.Equal(t, "/scion-volumes/scratchpad/.attachments/_webchat", st.agentDir)
}

func TestNewAttachmentStaging_InWorkspace(t *testing.T) {
	st := newAttachmentStaging("/host/shared-dirs/scratchpad", true)
	require.NotNil(t, st)
	assert.Equal(t, "/workspace/.scion-volumes/scratchpad/.attachments/_webchat", st.agentDir)
}

func TestNewAttachmentStaging_EmptyHostPath(t *testing.T) {
	assert.Nil(t, newAttachmentStaging("", false))
}

func TestAttachmentStaging_StageCopiesFile(t *testing.T) {
	src := writeTempFile(t, "notes.txt", "hello agent")
	st := newAttachmentStaging(t.TempDir(), false)
	require.NotNil(t, st)

	agentPath, err := st.stage(src, "att-1", "notes.txt")
	require.NoError(t, err)
	assert.Equal(t, "/scion-volumes/scratchpad/.attachments/_webchat/att-1/notes.txt", agentPath)

	staged := filepath.Join(st.hostDir, "att-1", "notes.txt")
	content, err := os.ReadFile(staged)
	require.NoError(t, err)
	assert.Equal(t, "hello agent", string(content))
}

func TestAttachmentStaging_StageIsIdempotent(t *testing.T) {
	src := writeTempFile(t, "notes.txt", "hello agent")
	st := newAttachmentStaging(t.TempDir(), false)

	first, err := st.stage(src, "att-1", "notes.txt")
	require.NoError(t, err)
	second, err := st.stage(src, "att-1", "notes.txt")
	require.NoError(t, err)
	assert.Equal(t, first, second)

	content, err := os.ReadFile(filepath.Join(st.hostDir, "att-1", "notes.txt"))
	require.NoError(t, err)
	assert.Equal(t, "hello agent", string(content))
}

func TestAttachmentStaging_StageRejectsTraversal(t *testing.T) {
	src := writeTempFile(t, "notes.txt", "hello agent")
	base := t.TempDir()
	st := newAttachmentStaging(base, false)

	_, err := st.stage(src, "../escape", "notes.txt")
	require.Error(t, err)

	_, err = st.stage(src, "att-1", "../../escape.txt")
	// filepath.Base reduces the name to "escape.txt", which stays inside the
	// staging dir — the point is that nothing lands outside base.
	require.NoError(t, err)
	assert.FileExists(t, filepath.Join(st.hostDir, "att-1", "escape.txt"))

	_, err = st.stage(src, "att-2", "..")
	require.Error(t, err)
}

func TestAttachmentStaging_StageMissingSource(t *testing.T) {
	st := newAttachmentStaging(t.TempDir(), false)
	_, err := st.stage(filepath.Join(t.TempDir(), "gone.txt"), "att-1", "gone.txt")
	require.Error(t, err)
	// A failed copy must not leave a truncated file behind.
	assert.NoFileExists(t, filepath.Join(st.hostDir, "att-1", "gone.txt"))
}

func TestAttachmentStaging_StageDoesNotFollowSymlink(t *testing.T) {
	src := writeTempFile(t, "notes.txt", "hello agent")
	target := writeTempFile(t, "secret.txt", "do not overwrite")
	st := newAttachmentStaging(t.TempDir(), false)

	require.NoError(t, os.MkdirAll(filepath.Join(st.hostDir, "att-1"), 0o755))
	require.NoError(t, os.Symlink(target, filepath.Join(st.hostDir, "att-1", "notes.txt")))

	_, err := st.stage(src, "att-1", "notes.txt")
	require.NoError(t, err) // treated as already staged

	content, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, "do not overwrite", string(content))
}

// ---------------------------------------------------------------------------
// Server.resolveAttachmentStaging
// ---------------------------------------------------------------------------

// newStagingTestProject creates a project with the given shared dirs and points
// HOME at a temp dir so shared dir host paths resolve inside the test sandbox.
func newStagingTestProject(t *testing.T, sharedDirs []api.SharedDir) (*Server, *store.Project, string) {
	t.Helper()
	srv, st := testServer(t)
	home := t.TempDir()
	t.Setenv("HOME", home)

	project := &store.Project{
		ID:         "11111111-2222-3333-4444-555555555555",
		Name:       "Staging Project",
		Slug:       "staging-project",
		SharedDirs: sharedDirs,
	}
	require.NoError(t, st.CreateProject(context.Background(), project))
	return srv, project, home
}

func TestResolveAttachmentStaging_NoScratchpadSharedDir(t *testing.T) {
	srv, project, _ := newStagingTestProject(t, nil)
	assert.Nil(t, srv.resolveAttachmentStaging(context.Background(), project.ID))
}

func TestResolveAttachmentStaging_HostDirMissing(t *testing.T) {
	srv, project, _ := newStagingTestProject(t, []api.SharedDir{{Name: "scratchpad"}})
	// The shared dir is declared but nothing backs it on this host — the caller
	// must keep using the hub-local attachment path.
	assert.Nil(t, srv.resolveAttachmentStaging(context.Background(), project.ID))
}

func TestResolveAttachmentStaging_UsesProjectConfigsSharedDir(t *testing.T) {
	srv, project, home := newStagingTestProject(t, []api.SharedDir{{Name: "scratchpad"}})
	sharedDir := config.SharedDirHostPath(home, project.Slug, project.ID, "scratchpad")
	require.NoError(t, os.MkdirAll(sharedDir, 0o755))

	staging := srv.resolveAttachmentStaging(context.Background(), project.ID)
	require.NotNil(t, staging)
	assert.Equal(t, filepath.Join(sharedDir, ".attachments", "_webchat"), staging.hostDir)
	assert.Equal(t, "/scion-volumes/scratchpad/.attachments/_webchat", staging.agentDir)
}

func TestResolveAttachmentStaging_UnknownProject(t *testing.T) {
	srv, _, _ := newStagingTestProject(t, []api.SharedDir{{Name: "scratchpad"}})
	assert.Nil(t, srv.resolveAttachmentStaging(context.Background(), "no-such-project"))
	assert.Nil(t, srv.resolveAttachmentStaging(context.Background(), ""))
}
