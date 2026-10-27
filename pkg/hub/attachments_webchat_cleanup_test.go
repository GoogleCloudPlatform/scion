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
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// cleanupCopy creates a web chat copy of attachment id in dir (the
// scratchpad's _webchat directory).
func cleanupCopy(t *testing.T, dir, id string) string {
	t.Helper()
	p := filepath.Join(dir, id)
	require.NoError(t, os.MkdirAll(p, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(p, "file.txt"), []byte(id), 0o644))
	return p
}

// cleanupAttachment records a legacy attachment row in f's chat store and
// links it to one stored message per thread key; it returns the id.
func cleanupAttachment(t *testing.T, f *refFixture, keys ...string) string {
	t.Helper()
	ctx := context.Background()
	id := api.NewUUID()
	require.NoError(t, f.wcs.CreateAttachment(ctx, AttachmentMeta{ID: id, ProjectID: f.projA.ID, Filename: "file.txt",
		MimeType: "text/plain", UploadedBy: f.ua.ID, CreatedAt: time.Now()}))
	for _, key := range keys {
		msgID := api.NewUUID()
		require.NoError(t, f.st.CreateMessage(ctx, &store.Message{
			ID: msgID, ProjectID: f.projA.ID, Sender: "user:" + f.ua.ID, SenderID: f.ua.ID, Recipient: "agent:aa", Msg: "m",
			Type: messages.TypeInstruction, Channel: "web", ThreadID: key, CreatedAt: time.Now().UTC(),
			DispatchState: store.MessageDispatchDispatched,
		}))
		require.NoError(t, f.wcs.LinkAttachmentToMessage(ctx, msgID, id))
	}
	return id
}

// TestWebChatCopyCleanup_RemovesOnlyNonProjectAudienceCopies: the one-shot
// cleanup removes exactly the web chat copies whose referencing messages
// are all in direct or group conversations (or another project's thread),
// keeps project-audience copies and copies it cannot attribute, records
// completion, and removes nothing on a second run.
func TestWebChatCopyCleanup_RemovesOnlyNonProjectAudienceCopies(t *testing.T) {
	f := newRefFixture(t)
	ctx := context.Background()
	webchat := f.enableScratchpad(t)
	dm := dmKeyFor(t, "user", f.ua.ID, "agent", f.aa.ID)
	group := seedGroupConversation(t, f.st, f.projA.ID, "crew")

	dmCopy := cleanupCopy(t, webchat, cleanupAttachment(t, f, dm))
	groupCopy := cleanupCopy(t, webchat, cleanupAttachment(t, f, group))
	foreignTopicCopy := cleanupCopy(t, webchat, cleanupAttachment(t, f, f.topicB))
	topicCopy := cleanupCopy(t, webchat, cleanupAttachment(t, f, f.topicA))
	mixedCopy := cleanupCopy(t, webchat, cleanupAttachment(t, f, dm, f.topicA))
	unlinkedCopy := cleanupCopy(t, webchat, cleanupAttachment(t, f))
	noRowCopy := cleanupCopy(t, webchat, api.NewUUID())
	// Other channels are never touched.
	otherChannel := filepath.Join(filepath.Dir(webchat), "_telegram", "tg_1")
	require.NoError(t, os.MkdirAll(otherChannel, 0o755))

	removed, done := f.srv.runWebChatCopyCleanup(ctx)
	require.True(t, done)
	assert.Equal(t, 3, removed)
	for _, gone := range []string{dmCopy, groupCopy, foreignTopicCopy} {
		assert.NoDirExists(t, gone)
	}
	for _, kept := range []string{topicCopy, mixedCopy, unlinkedCopy, noRowCopy, otherChannel} {
		assert.DirExists(t, kept)
	}
	_, err := f.st.GetHubSetting(ctx, webChatCopyCleanupMarker)
	require.NoError(t, err, "completion is recorded")

	// A second run is a no-op, and so is a rerun without the marker.
	removed, done = f.srv.runWebChatCopyCleanup(ctx)
	assert.True(t, done)
	assert.Zero(t, removed)
	require.NoError(t, f.st.DeleteHubSetting(ctx, webChatCopyCleanupMarker))
	removed, done = f.srv.runWebChatCopyCleanup(ctx)
	assert.True(t, done)
	assert.Zero(t, removed)
	assert.DirExists(t, topicCopy)
}

// TestWebChatCopyCleanup_SkipsSymlinkedAttachmentsDir: a .attachments that
// is a link (out of the scratchpad, or to the scratchpad itself) is never
// walked; nothing it points at is removed.
func TestWebChatCopyCleanup_SkipsSymlinkedAttachmentsDir(t *testing.T) {
	f := newRefFixture(t)
	ctx := context.Background()
	webchat := f.enableScratchpad(t)
	scratchpad := filepath.Dir(filepath.Dir(webchat))
	dm := dmKeyFor(t, "user", f.ua.ID, "agent", f.aa.ID)
	id := cleanupAttachment(t, f, dm)

	outside := t.TempDir()
	victim := cleanupCopy(t, filepath.Join(outside, "_webchat"), id)
	require.NoError(t, os.RemoveAll(filepath.Join(scratchpad, ".attachments")))
	require.NoError(t, os.Symlink(outside, filepath.Join(scratchpad, ".attachments")))

	removed, done := f.srv.runWebChatCopyCleanup(ctx)
	assert.True(t, done)
	assert.Zero(t, removed)
	assert.DirExists(t, victim, "nothing is removed through the link")
}

// TestWebChatCopyCleanupOnce_FromMaintenancePass: the maintenance pass runs
// the cleanup until it completes, then not again in the process.
func TestWebChatCopyCleanupOnce_FromMaintenancePass(t *testing.T) {
	f := newRefFixture(t)
	ctx := context.Background()
	webchat := f.enableScratchpad(t)
	dm := dmKeyFor(t, "user", f.ua.ID, "agent", f.aa.ID)
	dmCopy := cleanupCopy(t, webchat, cleanupAttachment(t, f, dm))

	f.srv.sweepAttachmentRetention(ctx, time.Now())
	assert.NoDirExists(t, dmCopy)
	f.srv.attachmentRetention.mu.Lock()
	done := f.srv.attachmentRetention.copyCleanupDone
	f.srv.attachmentRetention.mu.Unlock()
	assert.True(t, done)
}

// cleanupPassDone reports whether this process recorded the one-shot
// cleanup as complete.
func cleanupPassDone(f *refFixture) bool {
	f.srv.attachmentRetention.mu.Lock()
	defer f.srv.attachmentRetention.mu.Unlock()
	return f.srv.attachmentRetention.copyCleanupDone
}

// TestWebChatCopyCleanupOnce_PartialRunRetried: a run that does not finish
// (here, its pass is cancelled) records nothing, neither the hub setting
// nor the in-process flag, so the next pass runs it again; that pass
// removes the DM and group copies, keeps the project-audience copy and
// records completion, and a later pass removes nothing.
func TestWebChatCopyCleanupOnce_PartialRunRetried(t *testing.T) {
	f := newRefFixture(t)
	ctx := context.Background()
	webchat := f.enableScratchpad(t)
	dm := dmKeyFor(t, "user", f.ua.ID, "agent", f.aa.ID)
	group := seedGroupConversation(t, f.st, f.projA.ID, "crew")
	dmCopy := cleanupCopy(t, webchat, cleanupAttachment(t, f, dm))
	groupCopy := cleanupCopy(t, webchat, cleanupAttachment(t, f, group))
	topicCopy := cleanupCopy(t, webchat, cleanupAttachment(t, f, f.topicA))

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	f.srv.webChatCopyCleanupOnce(cancelled)
	assert.False(t, cleanupPassDone(f), "a partial run is not recorded in the process")
	_, err := f.st.GetHubSetting(ctx, webChatCopyCleanupMarker)
	require.ErrorIs(t, err, store.ErrNotFound, "a partial run is not recorded in the hub settings")

	f.srv.webChatCopyCleanupOnce(ctx)
	assert.True(t, cleanupPassDone(f))
	_, err = f.st.GetHubSetting(ctx, webChatCopyCleanupMarker)
	require.NoError(t, err)
	assert.NoDirExists(t, dmCopy)
	assert.NoDirExists(t, groupCopy)
	assert.DirExists(t, topicCopy)

	// A later pass removes nothing, even a copy that appears afterwards.
	lateCopy := cleanupCopy(t, webchat, cleanupAttachment(t, f, dm))
	f.srv.webChatCopyCleanupOnce(ctx)
	assert.DirExists(t, lateCopy)
	assert.DirExists(t, topicCopy)
}

// TestWebChatCopyCleanupOnce_ReplicaReadsTheHubSetting: replicas share the
// hub setting, not the in-process flag. A replica whose own run has not
// completed keeps checking the setting on every pass: while another
// replica's run has failed (no setting) it runs the cleanup itself; once a
// run anywhere has recorded completion it removes nothing and stops.
func TestWebChatCopyCleanupOnce_ReplicaReadsTheHubSetting(t *testing.T) {
	t.Run("another replica completed", func(t *testing.T) {
		f := newRefFixture(t)
		ctx := context.Background()
		webchat := f.enableScratchpad(t)
		dmCopy := cleanupCopy(t, webchat, cleanupAttachment(t, f, dmKeyFor(t, "user", f.ua.ID, "agent", f.aa.ID)))
		_, err := f.st.UpsertHubSetting(ctx, webChatCopyCleanupMarker, []byte(`{"completed_at":"2026-10-27T00:00:00Z"}`), "system", -1, "seeded")
		require.NoError(t, err)

		f.srv.webChatCopyCleanupOnce(ctx)
		assert.True(t, cleanupPassDone(f))
		assert.DirExists(t, dmCopy, "the cleanup ran once already, elsewhere")
	})
	t.Run("another replica failed", func(t *testing.T) {
		f := newRefFixture(t)
		ctx := context.Background()
		webchat := f.enableScratchpad(t)
		dmCopy := cleanupCopy(t, webchat, cleanupAttachment(t, f, dmKeyFor(t, "user", f.ua.ID, "agent", f.aa.ID)))

		// Another replica's run failed: no hub setting was written. This
		// replica's pass runs the cleanup.
		_, err := f.st.GetHubSetting(ctx, webChatCopyCleanupMarker)
		require.ErrorIs(t, err, store.ErrNotFound)
		f.srv.webChatCopyCleanupOnce(ctx)
		assert.True(t, cleanupPassDone(f))
		assert.NoDirExists(t, dmCopy)
	})
}
