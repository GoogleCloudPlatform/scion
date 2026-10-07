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

package discord

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/bwmarrin/discordgo"
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

func attachmentServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("attachment-bytes"))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func downloadTestAttachment(t *testing.T) (string, error) {
	t.Helper()
	srv := attachmentServer(t)
	b := &DiscordBroker{log: discardLogger(), httpClient: srv.Client()}
	att := &discordgo.MessageAttachment{ID: "att-1", Filename: "note.txt", URL: srv.URL + "/note.txt", Size: 16}
	agentPath, _, err := b.downloadDiscordAttachment(context.Background(), att, "my-project", sharedDirTestProjectID)
	return agentPath, err
}

func TestDownloadDiscordAttachment_LocalBackend(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	stubSharedDirSettings(t, &config.VersionedSettings{})

	agentPath, err := downloadTestAttachment(t)
	require.NoError(t, err)
	assert.Contains(t, agentPath, "/scion-volumes/scratchpad/.attachments/_discord/")

	entries, err := os.ReadDir(filepath.Join(localSharedDir(home, "scratchpad"), ".attachments", "_discord"))
	require.NoError(t, err)
	assert.Len(t, entries, 1)
}

func TestDownloadDiscordAttachment_NFSBackend(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	mountRoot, nfsDir := mountedNFSRoot(t, "scratchpad")
	stubSharedDirSettings(t, nfsSettings(mountRoot))

	agentPath, err := downloadTestAttachment(t)
	require.NoError(t, err)
	assert.Contains(t, agentPath, "/scion-volumes/scratchpad/.attachments/_discord/")

	entries, err := os.ReadDir(filepath.Join(nfsDir, ".attachments", "_discord"))
	require.NoError(t, err, "attachment must be staged under the nfs shared dir")
	assert.Len(t, entries, 1)
	_, err = os.Stat(localSharedDir(home, "scratchpad"))
	assert.True(t, os.IsNotExist(err), "local shared dir must not be written for nfs")
}

func TestDownloadDiscordAttachment_NFSUnavailable(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	stubSharedDirSettings(t, nfsSettings(filepath.Join(t.TempDir(), "not-mounted")))

	_, err := downloadTestAttachment(t)
	require.Error(t, err)
	assert.True(t, isSharedDirStorageUnavailable(err), "err = %v", err)
	_, statErr := os.Stat(localSharedDir(home, "scratchpad"))
	assert.True(t, os.IsNotExist(statErr), "must not fall back to the local shared dir")
	assert.Contains(t, sharedDirUnavailableText("note.txt"), "note.txt")
}

func TestTranslateContainerPath_LocalBackend(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	stubSharedDirSettings(t, &config.VersionedSettings{})

	got, err := translateContainerPath("/scion-volumes/scratchpad/out/report.md", "my-project", sharedDirTestProjectID)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(localSharedDir(home, "scratchpad"), "out", "report.md"), got)
}

func TestTranslateContainerPath_NFSBackend(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	mountRoot, nfsDir := mountedNFSRoot(t, "scratchpad")
	stubSharedDirSettings(t, nfsSettings(mountRoot))

	got, err := translateContainerPath("/workspace/.scion-volumes/scratchpad/out/report.md", "my-project", sharedDirTestProjectID)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(nfsDir, "out", "report.md"), got)
}

func TestTranslateContainerPath_NFSUnavailable(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	stubSharedDirSettings(t, nfsSettings(filepath.Join(t.TempDir(), "not-mounted")))

	_, err := translateContainerPath("/scion-volumes/scratchpad/out/report.md", "my-project", sharedDirTestProjectID)
	require.Error(t, err)
	assert.True(t, isSharedDirStorageUnavailable(err), "err = %v", err)
}

func TestResolveSharedDirAttachmentPath_Backends(t *testing.T) {
	newBroker := func() *DiscordBroker {
		return &DiscordBroker{
			log:            discardLogger(),
			projectSlugMap: map[string]string{sharedDirTestProjectID: "my-project"},
		}
	}

	t.Run("local", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		stubSharedDirSettings(t, &config.VersionedSettings{})
		got := newBroker().resolveSharedDirAttachmentPath(context.Background(), "/scion-volumes/scratchpad/a.png", sharedDirTestProjectID)
		assert.Equal(t, filepath.Join(localSharedDir(home, "scratchpad"), "a.png"), got)
	})

	t.Run("nfs", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		mountRoot, nfsDir := mountedNFSRoot(t, "scratchpad")
		stubSharedDirSettings(t, nfsSettings(mountRoot))
		got := newBroker().resolveSharedDirAttachmentPath(context.Background(), "/scion-volumes/scratchpad/a.png", sharedDirTestProjectID)
		assert.Equal(t, filepath.Join(nfsDir, "a.png"), got)
	})

	t.Run("nfs unavailable", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		stubSharedDirSettings(t, nfsSettings(filepath.Join(t.TempDir(), "not-mounted")))
		got := newBroker().resolveSharedDirAttachmentPath(context.Background(), "/scion-volumes/scratchpad/a.png", sharedDirTestProjectID)
		assert.Empty(t, got)
	})
}
