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

package telegram

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

const sharedDirTestProjectID = "abcd1234-ef56-7890-abcd-ef1234567890"

// stubSharedDirSettings replaces the global settings loader with gs.
func stubSharedDirSettings(t *testing.T, gs *config.VersionedSettings) {
	t.Helper()
	orig := loadSharedDirSettings
	loadSharedDirSettings = func() (*config.VersionedSettings, error) { return gs, nil }
	t.Cleanup(func() { loadSharedDirSettings = orig })
}

// nfsSettings returns fake settings selecting the nfs backend with share
// "share1" under mountRoot.
func nfsSettings(mountRoot string) *config.VersionedSettings {
	return &config.VersionedSettings{
		Server: &config.V1ServerConfig{
			SharedDirStorage: &config.V1SharedDirStorageConfig{
				Backend: "nfs",
				NFS: &config.V1NFSConfig{
					MountRoot: mountRoot,
					Shares:    []config.V1NFSShare{{ID: "share1"}},
				},
			},
		},
	}
}

// mountedNFSRoot returns a temp mount root with share1 present, and the
// expected (symlink-resolved) host path of the project's shared dir name.
func mountedNFSRoot(t *testing.T, name string) (mountRoot, sharedDir string) {
	t.Helper()
	mountRoot = t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(mountRoot, "share1"), 0o755))
	resolved, err := filepath.EvalSymlinks(mountRoot)
	require.NoError(t, err)
	return mountRoot, filepath.Join(resolved, "share1", "projects", sharedDirTestProjectID, "shared-dirs", name)
}

func localSharedDir(home, name string) string {
	return filepath.Join(home, ".scion", "project-configs", "my-project__abcd1234", "shared-dirs", name)
}

func downloadTestFile(t *testing.T) (string, error) {
	t.Helper()
	b := newTestBrokerV2(t, newFakeTGServerV2(t))
	b.downloadsPath = ""
	tgMsg := &TGMessage{Document: &TGDocument{FileID: "doc1", FileUniqueID: "u1", FileName: "note.txt", FileSize: 10}}
	agentPath, _, err := b.downloadTelegramFile(context.Background(), tgMsg, "my-project", sharedDirTestProjectID)
	return agentPath, err
}

func TestDownloadTelegramFile_LocalBackend(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	stubSharedDirSettings(t, &config.VersionedSettings{})

	agentPath, err := downloadTestFile(t)
	require.NoError(t, err)
	assert.Contains(t, agentPath, "/scion-volumes/scratchpad/.attachments/_telegram/")

	entries, err := os.ReadDir(filepath.Join(localSharedDir(home, "scratchpad"), ".attachments", "_telegram"))
	require.NoError(t, err)
	assert.Len(t, entries, 1)
}

func TestDownloadTelegramFile_NFSBackend(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	mountRoot, nfsDir := mountedNFSRoot(t, "scratchpad")
	stubSharedDirSettings(t, nfsSettings(mountRoot))

	agentPath, err := downloadTestFile(t)
	require.NoError(t, err)
	assert.Contains(t, agentPath, "/scion-volumes/scratchpad/.attachments/_telegram/")

	entries, err := os.ReadDir(filepath.Join(nfsDir, ".attachments", "_telegram"))
	require.NoError(t, err, "attachment must be staged under the nfs shared dir")
	assert.Len(t, entries, 1)
	_, err = os.Stat(localSharedDir(home, "scratchpad"))
	assert.True(t, os.IsNotExist(err), "local shared dir must not be written for nfs")
}

func TestDownloadTelegramFile_NFSUnavailable(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	stubSharedDirSettings(t, nfsSettings(filepath.Join(t.TempDir(), "not-mounted")))

	_, err := downloadTestFile(t)
	require.Error(t, err)
	assert.True(t, isSharedDirStorageUnavailable(err), "err = %v", err)
	_, statErr := os.Stat(localSharedDir(home, "scratchpad"))
	assert.True(t, os.IsNotExist(statErr), "must not fall back to the local shared dir")

	text := sharedDirUnavailableText(telegramAttachmentName(&TGMessage{Document: &TGDocument{FileName: "note.txt"}}))
	assert.Contains(t, text, "note.txt")
	assert.NotContains(t, text, "not-mounted", "the sender message must not name host paths")
}

func TestResolveSharedDirAttachmentPath_Backends(t *testing.T) {
	newBroker := func(t *testing.T) *TelegramBrokerV2 {
		b := newTestBrokerV2(t, newFakeTGServerV2(t))
		b.projectSlugMap = map[string]string{sharedDirTestProjectID: "my-project"}
		return b
	}
	const attach = "/scion-volumes/scratchpad/out/a.png"

	t.Run("local", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		stubSharedDirSettings(t, &config.VersionedSettings{})
		got := newBroker(t).resolveSharedDirAttachmentPath(context.Background(), nil, attach, sharedDirTestProjectID)
		assert.Equal(t, filepath.Join(localSharedDir(home, "scratchpad"), "out", "a.png"), got)
	})

	t.Run("nfs", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		mountRoot, nfsDir := mountedNFSRoot(t, "scratchpad")
		stubSharedDirSettings(t, nfsSettings(mountRoot))
		got := newBroker(t).resolveSharedDirAttachmentPath(context.Background(), nil, attach, sharedDirTestProjectID)
		assert.Equal(t, filepath.Join(nfsDir, "out", "a.png"), got)
	})

	t.Run("nfs unavailable", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		stubSharedDirSettings(t, nfsSettings(filepath.Join(t.TempDir(), "not-mounted")))
		got := newBroker(t).resolveSharedDirAttachmentPath(context.Background(), nil, attach, sharedDirTestProjectID)
		assert.Equal(t, attach, got, "unresolved paths are returned unchanged, never mapped to the local dir")
	})
}
