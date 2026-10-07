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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	scionruntime "github.com/GoogleCloudPlatform/scion/pkg/runtime"
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
		got, err := newBroker(t).resolveSharedDirAttachmentPath(context.Background(), nil, attach, sharedDirTestProjectID)
		require.NoError(t, err)
		assert.Equal(t, filepath.Join(localSharedDir(home, "scratchpad"), "out", "a.png"), got)
	})

	t.Run("nfs", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		mountRoot, nfsDir := mountedNFSRoot(t, "scratchpad")
		stubSharedDirSettings(t, nfsSettings(mountRoot))
		got, err := newBroker(t).resolveSharedDirAttachmentPath(context.Background(), nil, attach, sharedDirTestProjectID)
		require.NoError(t, err)
		assert.Equal(t, filepath.Join(nfsDir, "out", "a.png"), got)
	})

	t.Run("nfs unavailable", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		stubSharedDirSettings(t, nfsSettings(filepath.Join(t.TempDir(), "not-mounted")))
		got, err := newBroker(t).resolveSharedDirAttachmentPath(context.Background(), nil, attach, sharedDirTestProjectID)
		require.Error(t, err)
		assert.True(t, isSharedDirStorageUnavailable(err), "err = %v", err)
		assert.Empty(t, got, "an unresolved path is never returned, as the container path or the local dir")
	})
}

// TestPublish_UnresolvedSharedDirAttachment_NeverOpened checks that an
// outbound attachment whose shared dir cannot be resolved is skipped: no
// path is opened on this host, no document is sent, and the message text
// is still delivered.
func TestPublish_UnresolvedSharedDirAttachment_NeverOpened(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	stubSharedDirSettings(t, nfsSettings(filepath.Join(t.TempDir(), "not-mounted")))

	var opened []string
	orig := openAttachmentFile
	t.Cleanup(func() { openAttachmentFile = orig })
	openAttachmentFile = func(name string) (*os.File, error) {
		opened = append(opened, name)
		return orig(name)
	}

	tgSrv := newFakeTGServerV2(t)
	b := newTestBrokerV2(t, tgSrv)
	b.projectSlugMap = map[string]string{sharedDirTestProjectID: "my-project"}

	msg := messages.NewInstruction("agent:coder", "user:alice", "report attached")
	msg.Attachments = []string{"/scion-volumes/scratchpad/out/report.txt"}
	msg.Metadata = map[string]string{"telegram_chat_id": "-300"}

	err := b.Publish(context.Background(), "scion.project."+sharedDirTestProjectID+".agent.coder.messages", msg)
	require.NoError(t, err)

	assert.Empty(t, opened, "no attachment path may be opened when the shared dir is unresolved")
	sent := tgSrv.getSentMessages()
	require.Len(t, sent, 1, "the message text is delivered without the attachment")
	assert.Contains(t, sent[0].Text, "report attached")
}

// TestPublish_ResolvedSharedDirAttachment_Opened is the control for the
// test above: with the shared dir mounted, the resolved host path is the
// one opened.
func TestPublish_ResolvedSharedDirAttachment_Opened(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	mountRoot, nfsDir := mountedNFSRoot(t, "scratchpad")
	stubSharedDirSettings(t, nfsSettings(mountRoot))

	var opened []string
	orig := openAttachmentFile
	t.Cleanup(func() { openAttachmentFile = orig })
	openAttachmentFile = func(name string) (*os.File, error) {
		opened = append(opened, name)
		return orig(name)
	}

	tgSrv := newFakeTGServerV2(t)
	b := newTestBrokerV2(t, tgSrv)
	b.projectSlugMap = map[string]string{sharedDirTestProjectID: "my-project"}

	msg := messages.NewInstruction("agent:coder", "user:alice", "report attached")
	msg.Attachments = []string{"/scion-volumes/scratchpad/out/report.txt"}
	msg.Metadata = map[string]string{"telegram_chat_id": "-300"}

	// The file does not exist, so the send fails after the open; only the
	// opened path matters here.
	_ = b.Publish(context.Background(), "scion.project."+sharedDirTestProjectID+".agent.coder.messages", msg)
	assert.Equal(t, []string{filepath.Join(nfsDir, "out", "report.txt")}, opened)
}

// TestHandleGroupMessage_AttachmentErrorTextHasNoDetails checks that a
// failed inbound attachment tells the sender only fixed text: no path and
// none of the underlying error.
func TestHandleGroupMessage_AttachmentErrorTextHasNoDetails(t *testing.T) {
	b, tgSrv, hub := newRoutingTestBroker(t)
	hub.agents["proj-1"] = []AgentInfo{{Slug: "coder"}}
	linkTestUser(t, b.store, 456, "alice@example.com")

	// A downloads path below a regular file cannot be created; the
	// resulting error names that path.
	blocker := filepath.Join(t.TempDir(), "blocker")
	require.NoError(t, os.WriteFile(blocker, []byte("x"), 0o644))
	b.downloadsPath = filepath.Join(blocker, "downloads")

	msg := plainGroupMessage(456, "")
	msg.Document = &TGDocument{FileID: "doc1", FileUniqueID: "u1", FileName: "note.txt", FileSize: 10}
	b.handleGroupMessage(msg)

	sent := tgSrv.getSentMessages()
	require.Len(t, sent, 1, "the sender is told the attachment failed")
	text := sent[0].Text
	assert.Contains(t, text, "note.txt")
	assert.Contains(t, text, "could not be processed")
	assert.NotContains(t, text, "/", "the sender message must not contain a path")
	assert.NotContains(t, text, "blocker")
	assert.NotContains(t, text, "not a directory")
}

func TestAttachmentFailureText(t *testing.T) {
	raw := errors.New("open /srv/private/file: permission denied")
	generic := attachmentFailureText("a.txt", raw)
	assert.NotContains(t, generic, "/")
	assert.NotContains(t, generic, "permission denied")

	unavailable := attachmentFailureText("a.txt", fmt.Errorf("%w: mount missing at /srv/x", scionruntime.ErrSharedDirStorageUnavailable))
	assert.Equal(t, sharedDirUnavailableText("a.txt"), unavailable)
	assert.NotContains(t, unavailable, "/")
}
