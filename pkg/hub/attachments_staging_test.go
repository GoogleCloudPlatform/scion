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
	"io"
	"os"
	"path"
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

// hostDir names the staging directory as a path on the hub host. Production
// code has no use for it — stage() resolves through Resolved.Open, and a host
// path is exactly what must not be handed to a path-based os call — but a test
// asserting on the result has to stand outside the root to look in.
func (a *attachmentStaging) hostDir() string {
	return filepath.Join(a.resolved.HostPath, projectfs.AttachmentsDir, a.channel)
}

// stage is stageFrom with the bytes read from a host file (test helper; the
// production legacy path reads the hub-private store through
// LocalDiskAttachmentStore.Get).
func (a *attachmentStaging) stage(srcPath, id, filename string) (string, error) {
	return a.stageFrom(func() (io.ReadCloser, error) { return os.Open(srcPath) }, id, filename)
}

// agentDir is the container directory staged files of this target land in.
func (a *attachmentStaging) agentDir() string {
	return path.Join(a.mount.Target, projectfs.AttachmentsDir, a.channel)
}

// newLocalTestStaging builds a staging target on a local-backend scratchpad
// at dir, as resolveAttachmentStaging does for a workstation hub.
func newLocalTestStaging(dir string, inWorkspace bool) *attachmentStaging {
	mount := projectfs.SharedDirMount(api.SharedDir{Name: attachmentSharedDirName, InWorkspace: inWorkspace}, "")
	return newAttachmentStaging(projectfs.Resolved{
		Backend: projectfs.BackendLocal, HostPath: dir,
		Target: mount.Target, Kind: mount.Kind, Name: mount.Name,
	}, mount, false)
}

func TestNewAttachmentStaging_Paths(t *testing.T) {
	st := newLocalTestStaging("/home/scion/.scion/project-configs/proj__abcd1234/shared-dirs/scratchpad", false)
	require.NotNil(t, st)
	assert.Equal(t,
		"/home/scion/.scion/project-configs/proj__abcd1234/shared-dirs/scratchpad/.attachments/_webchat",
		st.hostDir())
	assert.Equal(t, "/scion-volumes/scratchpad/.attachments/_webchat", st.agentDir())
}

func TestNewAttachmentStaging_InWorkspace(t *testing.T) {
	st := newLocalTestStaging("/host/shared-dirs/scratchpad", true)
	require.NotNil(t, st)
	assert.Equal(t, "/workspace/.scion-volumes/scratchpad/.attachments/_webchat", st.agentDir())
	p, err := st.agentPath("att-1", "a.txt")
	require.NoError(t, err)
	assert.Equal(t, "/workspace/.scion-volumes/scratchpad/.attachments/_webchat/att-1/a.txt", p)
}

// TestAttachmentStaging_LocalDirMissingNotCreated: on the local backend a
// staging target never vivifies the scratchpad (create is false).
func TestAttachmentStaging_LocalDirMissingNotCreated(t *testing.T) {
	src := writeTempFile(t, "notes.txt", "hello agent")
	dir := filepath.Join(t.TempDir(), "scratchpad")
	st := newLocalTestStaging(dir, false)
	_, err := st.stage(src, "att-1", "notes.txt")
	require.Error(t, err)
	assert.NoDirExists(t, dir)
}

func TestAttachmentStaging_StageCopiesFile(t *testing.T) {
	src := writeTempFile(t, "notes.txt", "hello agent")
	st := newLocalTestStaging(t.TempDir(), false)
	require.NotNil(t, st)

	agentPath, err := st.stage(src, "att-1", "notes.txt")
	require.NoError(t, err)
	assert.Equal(t, "/scion-volumes/scratchpad/.attachments/_webchat/att-1/notes.txt", agentPath)

	staged := filepath.Join(st.hostDir(), "att-1", "notes.txt")
	content, err := os.ReadFile(staged)
	require.NoError(t, err)
	assert.Equal(t, "hello agent", string(content))
}

func TestAttachmentStaging_StageIsIdempotent(t *testing.T) {
	src := writeTempFile(t, "notes.txt", "hello agent")
	st := newLocalTestStaging(t.TempDir(), false)

	first, err := st.stage(src, "att-1", "notes.txt")
	require.NoError(t, err)
	second, err := st.stage(src, "att-1", "notes.txt")
	require.NoError(t, err)
	assert.Equal(t, first, second)

	content, err := os.ReadFile(filepath.Join(st.hostDir(), "att-1", "notes.txt"))
	require.NoError(t, err)
	assert.Equal(t, "hello agent", string(content))
}

// TestAttachmentStaging_ReusesOnlyIdenticalCopy: an existing file at the
// destination is reused only when it holds the attachment's exact bytes;
// a planted file with other content is refused and left as it is.
func TestAttachmentStaging_ReusesOnlyIdenticalCopy(t *testing.T) {
	src := writeTempFile(t, "notes.txt", "hello agent")
	st := newLocalTestStaging(t.TempDir(), false)
	dest := filepath.Join(st.hostDir(), "att-1", "notes.txt")
	require.NoError(t, os.MkdirAll(filepath.Dir(dest), 0o755))

	require.NoError(t, os.WriteFile(dest, []byte("planted by an agent"), 0o644))
	_, err := st.stage(src, "att-1", "notes.txt")
	require.ErrorIs(t, err, errStagedCopyMismatch)
	got, err := os.ReadFile(dest)
	require.NoError(t, err)
	assert.Equal(t, "planted by an agent", string(got), "the planted file is not touched")

	// Same length, other bytes: still refused.
	require.NoError(t, os.WriteFile(dest, []byte("HELLO AGENT"), 0o644))
	_, err = st.stage(src, "att-1", "notes.txt")
	require.ErrorIs(t, err, errStagedCopyMismatch)

	// A prefix of the attachment: refused.
	require.NoError(t, os.WriteFile(dest, []byte("hello"), 0o644))
	_, err = st.stage(src, "att-1", "notes.txt")
	require.ErrorIs(t, err, errStagedCopyMismatch)

	// The exact bytes: reused.
	require.NoError(t, os.WriteFile(dest, []byte("hello agent"), 0o644))
	p, err := st.stage(src, "att-1", "notes.txt")
	require.NoError(t, err)
	assert.Equal(t, "/scion-volumes/scratchpad/.attachments/_webchat/att-1/notes.txt", p)

	// A directory planted at the name: refused.
	dirDest := filepath.Join(st.hostDir(), "att-2", "notes.txt")
	require.NoError(t, os.MkdirAll(dirDest, 0o755))
	_, err = st.stage(src, "att-2", "notes.txt")
	require.Error(t, err)
}

func TestSameContent(t *testing.T) {
	big := strings.Repeat("x", 70<<10)
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"", "", true}, {"a", "a", true}, {"a", "b", false}, {"a", "", false}, {"", "a", false},
		{"ab", "a", false}, {big, big, true}, {big, big + "y", false}, {big + "y", big, false},
	} {
		got, err := sameContent(strings.NewReader(tc.a), strings.NewReader(tc.b))
		require.NoError(t, err)
		assert.Equal(t, tc.want, got, "%d vs %d bytes", len(tc.a), len(tc.b))
	}
}

func TestAttachmentStaging_StageRejectsTraversal(t *testing.T) {
	src := writeTempFile(t, "notes.txt", "hello agent")
	base := t.TempDir()
	st := newLocalTestStaging(base, false)

	_, err := st.stage(src, "../escape", "notes.txt")
	require.Error(t, err)

	_, err = st.stage(src, "att-1", "../../escape.txt")
	// filepath.Base reduces the name to "escape.txt", which stays inside the
	// staging dir — the point is that nothing lands outside base.
	require.NoError(t, err)
	assert.FileExists(t, filepath.Join(st.hostDir(), "att-1", "escape.txt"))

	_, err = st.stage(src, "att-2", "..")
	require.Error(t, err)
}

func TestAttachmentStaging_StageMissingSource(t *testing.T) {
	st := newLocalTestStaging(t.TempDir(), false)
	_, err := st.stage(filepath.Join(t.TempDir(), "gone.txt"), "att-1", "gone.txt")
	require.Error(t, err)
	// A failed copy must not leave a truncated file behind.
	assert.NoFileExists(t, filepath.Join(st.hostDir(), "att-1", "gone.txt"))
}

func TestAttachmentStaging_StageDoesNotFollowSymlink(t *testing.T) {
	src := writeTempFile(t, "notes.txt", "hello agent")
	target := writeTempFile(t, "secret.txt", "do not overwrite")
	st := newLocalTestStaging(t.TempDir(), false)

	require.NoError(t, os.MkdirAll(filepath.Join(st.hostDir(), "att-1"), 0o755))
	require.NoError(t, os.Symlink(target, filepath.Join(st.hostDir(), "att-1", "notes.txt")))

	_, err := st.stage(src, "att-1", "notes.txt")
	require.ErrorIs(t, err, errStagedCopyMismatch, "a planted link is never reused as the staged copy")

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
	assert.Equal(t, filepath.Join(sharedDir, ".attachments", "_webchat"), staging.hostDir())
	assert.Equal(t, "/scion-volumes/scratchpad/.attachments/_webchat", staging.agentDir())
}

func TestResolveAttachmentStaging_UnknownProject(t *testing.T) {
	srv, _, _ := newStagingTestProject(t, []api.SharedDir{{Name: "scratchpad"}})
	assert.Nil(t, srv.resolveAttachmentStaging(context.Background(), "no-such-project"))
	assert.Nil(t, srv.resolveAttachmentStaging(context.Background(), ""))
}
