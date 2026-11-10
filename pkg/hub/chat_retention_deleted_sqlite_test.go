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
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// deletedConversation stores a conversation soft-deleted at deleted.
func (f *chatRetentionHubFixture) deletedConversation(deleted time.Time) string {
	f.t.Helper()
	pid := f.project.ID
	conv := &store.Conversation{ID: api.NewUUID(), ProjectID: &pid, Kind: "group", Surface: "native",
		DriftState: "active", LastActivityAt: deleted, CreatedAt: deleted.Add(-time.Hour), DeletedAt: &deleted}
	require.NoError(f.t, f.s.CreateConversation(f.ctx, conv))
	return conv.ID
}

// rawTopic stores a web chat topic directly, as the web chat store writes
// it (RFC 3339 text times); conv "" leaves it unlinked (a legacy topic)
// and a zero deleted keeps it live.
func (f *chatRetentionHubFixture) rawTopic(conv string, deleted time.Time) string {
	f.t.Helper()
	id := api.NewUUID()
	var convArg, deletedArg any
	if conv != "" {
		convArg = conv
	}
	if !deleted.IsZero() {
		deletedArg = deleted.UTC().Format(time.RFC3339Nano)
	}
	f.exec(`INSERT INTO webchat_topic (id, project_id, name, is_general, conversation_id, created_by, created_at, deleted_at) VALUES (?, ?, ?, 0, ?, ?, ?, ?)`,
		id, f.project.ID, "t-"+id, convArg, f.alice.ID, time.Now().UTC().Add(-400*24*time.Hour).Format(time.RFC3339Nano), deletedArg)
	return id
}

// TestChatRetention_DeletedConversationsSQLite: on the real tables, with
// the experiment on and enabled, the messages of a conversation and of
// topics deleted more than the grace ago are purged with their dependent
// rows (a legacy topic's by thread_id), whether or not a window is set;
// a conversation deleted inside the grace, a live conversation's old
// messages (without a window) and pending messages are kept. With the
// experiment or enabled off nothing is purged.
func TestChatRetention_DeletedConversationsSQLite(t *testing.T) {
	tests := []struct {
		name       string
		experiment bool
		doc        string
		purges     bool
		window     bool
	}{
		{"window null", true, `{"enabled":true,"sweep":{"pause_ms":0}}`, true, false},
		{"long window", true, `{"enabled":true,"messages_retention_days":3650,"sweep":{"pause_ms":0}}`, true, true},
		{"enabled off", true, `{"enabled":false,"sweep":{"pause_ms":0}}`, false, false},
		{"experiment off", false, `{"enabled":true,"sweep":{"pause_ms":0}}`, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newChatRetentionHubFixture(t)
			setChatRetentionSettings(t, f.srv, tt.experiment, tt.doc)
			now := time.Now().UTC()
			day := 24 * time.Hour
			recent := now.Add(-time.Hour) // newer than any window

			goneConv := f.deletedConversation(now.Add(-40 * day))
			graceConv := f.deletedConversation(now.Add(-10 * day))
			legacyTopic := f.rawTopic("", now.Add(-31*day))
			liveConv := api.NewUUID()
			linkedTopic := f.rawTopic(liveConv, now.Add(-60*day))
			liveTopic := f.rawTopic(api.NewUUID(), time.Time{})

			purge := []string{
				f.message(chatRetentionSeed{created: recent, channel: "web", conv: goneConv}),
				f.message(chatRetentionSeed{created: recent, channel: "web", conv: goneConv, state: store.MessageDispatchFailed}),
				f.message(chatRetentionSeed{created: recent, channel: "web", thread: legacyTopic}),
				f.message(chatRetentionSeed{created: recent, channel: "web", conv: liveConv, thread: linkedTopic}),
				f.message(chatRetentionSeed{created: recent, channel: "web", thread: linkedTopic}), // pre-stamp row
			}
			keep := []string{
				f.message(chatRetentionSeed{created: recent, channel: "web", conv: goneConv, state: store.MessageDispatchPending}),
				f.message(chatRetentionSeed{created: recent, channel: "web", thread: legacyTopic, state: store.MessageDispatchDeferred}),
				f.message(chatRetentionSeed{created: recent, channel: "web", conv: graceConv}),
				f.message(chatRetentionSeed{created: now.Add(-400 * day), channel: "web", thread: liveTopic}),
				f.message(chatRetentionSeed{created: recent, channel: "web", conv: api.NewUUID(), thread: linkedTopic}), // re-keyed elsewhere
			}

			f.srv.chatRetentionHandler()(f.ctx)

			if !tt.purges {
				for _, id := range append(purge, keep...) {
					assert.True(t, f.exists(id), "kept %s", id)
				}
				assert.Equal(t, 0, f.count(`SELECT COUNT(*) FROM chat_retention_runs`))
				return
			}
			for _, id := range purge {
				assert.False(t, f.exists(id), "purged %s", id)
			}
			for _, id := range keep {
				assert.True(t, f.exists(id), "kept %s", id)
			}
			assert.Equal(t, chatRetentionNoOrphans(), chatRetentionOrphans(t, f.db))

			run, err := f.crs.LatestChatRetentionRun(f.ctx, store.ChatRetentionTriggerSweep, now.Add(-time.Hour))
			require.NoError(t, err)
			require.NotNil(t, run)
			assert.Equal(t, store.ChatRetentionStatusOK, run.Status)
			got := run.Counts[f.project.ID]
			assert.Equal(t, int64(len(purge)), got.DeletedConv)
			assert.Zero(t, got.Main, "nothing is older than the window")
			assert.Equal(t, int64(len(purge)), got.Mentions)
			assert.Equal(t, 30, run.Policy.DeletedConversationGraceDays)
			assert.WithinDuration(t, now.Add(-30*day), run.Policy.GraceCutoff, time.Minute)
			if !tt.window {
				assert.True(t, run.Policy.MainCutoff.IsZero())
			}
		})
	}
}

// failingDeleteAttachmentStore is a local attachment store whose byte
// delete always fails (object storage down, a permission error).
type failingDeleteAttachmentStore struct {
	*LocalDiskAttachmentStore
}

func (failingDeleteAttachmentStore) Delete(context.Context, string, string) error {
	return errors.New("attachment storage unavailable")
}

// savedAttachment stores an attachment's bytes and metadata row and links
// it to messageID; it returns the attachment ID and its blob directory.
func (f *chatRetentionHubFixture) savedAttachment(local *LocalDiskAttachmentStore, base, messageID string) (string, string) {
	f.t.Helper()
	meta, err := local.Save(f.ctx, f.project.ID, "notes.txt", strings.NewReader("retention bytes"), 15, "text/plain")
	require.NoError(f.t, err)
	meta.UploadedBy = f.alice.ID
	require.NoError(f.t, f.wcs.CreateAttachment(f.ctx, meta))
	require.NoError(f.t, f.wcs.LinkAttachmentToMessage(f.ctx, messageID, meta.ID))
	dir := filepath.Join(base, f.project.ID, meta.ID)
	_, err = os.Stat(dir)
	require.NoError(f.t, err)
	return meta.ID, dir
}

// TestChatRetention_AttachmentBytesSQLite: after the purge commits, an
// attachment no message links any more loses its bytes and then its
// metadata row, and the run counts both; an attachment a surviving message
// links keeps both. When the byte delete fails, the row stays, and the
// existing attachment GC then removes the bytes (design §6.3).
func TestChatRetention_AttachmentBytesSQLite(t *testing.T) {
	setup := func(t *testing.T) (*chatRetentionHubFixture, *LocalDiskAttachmentStore, string) {
		f := newChatRetentionHubFixture(t)
		setChatRetentionSettings(t, f.srv, true, `{"enabled":true,"messages_retention_days":30,"sweep":{"pause_ms":0}}`)
		base := t.TempDir()
		local, err := NewLocalDiskAttachmentStore(base)
		require.NoError(t, err)
		return f, local, base
	}
	now := time.Now().UTC()
	old := now.Add(-40 * 24 * time.Hour)

	t.Run("bytes then row", func(t *testing.T) {
		f, local, base := setup(t)
		f.srv.SetAttachmentStore(local)
		oldMsg := f.message(chatRetentionSeed{created: old, channel: "web"})
		newMsg := f.message(chatRetentionSeed{created: now.Add(-time.Hour), channel: "web"})
		gone, goneDir := f.savedAttachment(local, base, oldMsg)
		kept, keptDir := f.savedAttachment(local, base, newMsg)

		f.srv.chatRetentionHandler()(f.ctx)

		assert.False(t, f.exists(oldMsg))
		_, err := os.Stat(goneDir)
		assert.True(t, os.IsNotExist(err), "the unlinked attachment's bytes are deleted: %v", err)
		assert.Equal(t, 0, f.count(`SELECT COUNT(*) FROM webchat_attachment WHERE id = ?`, gone))
		_, err = os.Stat(keptDir)
		assert.NoError(t, err, "a linked attachment keeps its bytes")
		assert.Equal(t, 1, f.count(`SELECT COUNT(*) FROM webchat_attachment WHERE id = ?`, kept))

		run, err := f.crs.LatestChatRetentionRun(f.ctx, store.ChatRetentionTriggerSweep, now.Add(-time.Hour))
		require.NoError(t, err)
		require.NotNil(t, run)
		c := run.Counts[f.project.ID]
		// The message's fixture attachment (no bytes on disk) and the
		// saved one.
		assert.Equal(t, int64(2), c.AttachmentRows)
		assert.Equal(t, int64(1+15), c.AttachmentBytes)
		assert.Equal(t, int64(2), c.UnlinkedAttachments)
	})

	t.Run("byte delete fails, then the attachment GC", func(t *testing.T) {
		f, local, base := setup(t)
		f.srv.SetAttachmentStore(failingDeleteAttachmentStore{local})
		oldMsg := f.message(chatRetentionSeed{created: old, channel: "web"})
		newMsg := f.message(chatRetentionSeed{created: now.Add(-time.Hour), channel: "web"})
		gone, goneDir := f.savedAttachment(local, base, oldMsg)
		_, keptDir := f.savedAttachment(local, base, newMsg)

		f.srv.chatRetentionHandler()(f.ctx)

		assert.False(t, f.exists(oldMsg))
		_, err := os.Stat(goneDir)
		assert.NoError(t, err, "the bytes are still there")
		assert.Equal(t, 1, f.count(`SELECT COUNT(*) FROM webchat_attachment WHERE id = ?`, gone), "the row stays for the attachment GC")
		run, err := f.crs.LatestChatRetentionRun(f.ctx, store.ChatRetentionTriggerSweep, now.Add(-time.Hour))
		require.NoError(t, err)
		require.NotNil(t, run)
		assert.Equal(t, store.ChatRetentionStatusOK, run.Status, "a byte delete failure does not fail the run")
		assert.Zero(t, run.Counts[f.project.ID].AttachmentRows)

		// The existing legacy blob GC finds the unlinked attachment by its
		// row and removes the bytes; the linked one is kept.
		f.srv.SetAttachmentStore(local)
		assert.Equal(t, 1, f.srv.sweepLegacyAttachmentBlobs(f.ctx, time.Now().Add(time.Hour)))
		_, err = os.Stat(goneDir)
		assert.True(t, os.IsNotExist(err), "the attachment GC removed the bytes: %v", err)
		_, err = os.Stat(keptDir)
		assert.NoError(t, err)
	})
}

// chatRetentionDMs lists the caller's DMs.
func (f *chatRetentionHubFixture) dms() map[string]chatDMEntry {
	f.t.Helper()
	rec := doRequestAsUser(f.t, f.srv, f.alice, http.MethodGet, "/api/v1/chat/dms", nil)
	require.Equal(f.t, http.StatusOK, rec.Code, rec.Body.String())
	var resp chatDMListResponse
	require.NoError(f.t, json.Unmarshal(rec.Body.Bytes(), &resp))
	out := map[string]chatDMEntry{}
	for _, e := range resp.DMs {
		out[e.ConversationKey] = e
	}
	return out
}

// TestChatRetention_ListsAndUnreadAfterPurgeSQLite covers design §8.5 and
// §8.6 after a purge:
//   - a DM whose messages are all purged is still listed, with no preview
//     line, its last_activity_at kept, and no unread dot;
//   - a DM whose read watermark was purged but which has a newer message
//     shows that message and is unread, until it is read;
//   - an unread mention after a purged watermark still counts, and one
//     before the new watermark does not.
func TestChatRetention_ListsAndUnreadAfterPurgeSQLite(t *testing.T) {
	f := newChatRetentionHubFixture(t)
	setChatRetentionSettings(t, f.srv, true, `{"enabled":true,"messages_retention_days":30,"sweep":{"pause_ms":0}}`)
	require.True(t, f.srv.GetOperationalSettings().ConversationEnvelopeSwitch(), "the DM list reads by conversation")
	now := time.Now().UTC()
	old := now.Add(-40 * 24 * time.Hour)

	keyGone, err := messages.DMConversationKey("agent", api.NewUUID(), "user", f.alice.ID)
	require.NoError(t, err)
	keyNew, err := messages.DMConversationKey("agent", api.NewUUID(), "user", f.alice.ID)
	require.NoError(t, err)
	peerOf := func(key string) string { return strings.Split(key, ":")[2] }
	// The DM list reads a DM's last message through its conversation (the
	// conversation envelope switch is on by default), so each DM has its
	// native conversation and its messages carry it.
	convGone := seedConversation(t, f.s, "native", keyGone, "direct")
	convNew := seedConversation(t, f.s, "native", keyNew, "direct")

	g1 := f.message(chatRetentionSeed{created: old, channel: "web", conv: convGone, thread: keyGone})
	g2 := f.message(chatRetentionSeed{created: old.Add(time.Minute), channel: "web", conv: convGone, thread: keyGone})
	goneActivity := old.Add(time.Minute).Truncate(time.Millisecond)
	require.NoError(t, f.wcs.UpsertDM(f.ctx, WebChatDM{ConversationKey: keyGone, ParticipantID: f.alice.ID,
		PeerID: peerOf(keyGone), PeerKind: "agent", LastMessageID: g2, LastActivityAt: goneActivity}))
	require.NoError(t, f.wcs.SetReadState(f.ctx, f.alice.ID, keyGone, g1))

	n1 := f.message(chatRetentionSeed{created: old, channel: "web", conv: convNew, thread: keyNew})
	n2 := f.message(chatRetentionSeed{created: now.Add(-time.Hour), channel: "web", conv: convNew, thread: keyNew})
	require.NoError(t, f.wcs.UpsertDM(f.ctx, WebChatDM{ConversationKey: keyNew, ParticipantID: f.alice.ID,
		PeerID: peerOf(keyNew), PeerKind: "agent", LastMessageID: n2, LastActivityAt: now.Add(-time.Hour)}))
	require.NoError(t, f.wcs.SetReadState(f.ctx, f.alice.ID, keyNew, n1))

	// A topic with a mention of alice on an old and on a new message, and
	// her read watermark on another old message.
	topicKey := api.NewUUID()
	t1 := f.message(chatRetentionSeed{created: old, channel: "web", thread: topicKey})
	t2 := f.message(chatRetentionSeed{created: old.Add(time.Minute), channel: "web", thread: topicKey})
	t3 := f.message(chatRetentionSeed{created: now.Add(-time.Hour), channel: "web", thread: topicKey})
	// The fixture wrote one mention of alice per message; move those of t2
	// and t3 to the topic.
	f.exec(`UPDATE webchat_mention SET conversation_key = ? WHERE user_id = ? AND message_id IN (?, ?)`, topicKey, f.alice.ID, t2, t3)
	require.NoError(t, f.wcs.SetReadState(f.ctx, f.alice.ID, topicKey, t1))

	before := f.dms()
	require.Contains(t, before, keyGone)
	assert.Equal(t, g2, before[keyGone].LastMessageID)
	assert.NotEmpty(t, before[keyGone].LastMessagePreview)
	assert.True(t, before[keyGone].HasUnread)

	f.srv.chatRetentionHandler()(f.ctx)
	for _, id := range []string{g1, g2, n1, t1, t2} {
		require.False(t, f.exists(id), "purged %s", id)
	}

	after := f.dms()
	require.Contains(t, after, keyGone, "a DM whose messages are all purged is still listed")
	gone := after[keyGone]
	assert.Empty(t, gone.LastMessageID)
	assert.Empty(t, gone.LastMessagePreview, "no preview line")
	assert.Empty(t, gone.LastMessageSender)
	assert.True(t, gone.LastActivityAt.Equal(goneActivity), "last_activity_at kept: %v, want %v", gone.LastActivityAt, goneActivity)
	assert.False(t, gone.HasUnread)

	require.Contains(t, after, keyNew)
	assert.Equal(t, n2, after[keyNew].LastMessageID)
	assert.Equal(t, n1, after[keyNew].LastReadMessageID, "the purged watermark is not rewritten")
	assert.True(t, after[keyNew].HasUnread, "a newer message after a purged watermark is unread")
	require.NoError(t, f.wcs.SetReadState(f.ctx, f.alice.ID, keyNew, n2))
	assert.False(t, f.dms()[keyNew].HasUnread)

	mentions, err := f.wcs.UnreadMentionKeys(f.ctx, f.alice.ID, []string{topicKey})
	require.NoError(t, err)
	assert.True(t, mentions[topicKey], "the mention after the purged watermark is unread")
	assert.Equal(t, 1, f.count(`SELECT COUNT(*) FROM webchat_mention WHERE conversation_key = ?`, topicKey), "the purged message's mention went with it")
	require.NoError(t, f.wcs.SetReadState(f.ctx, f.alice.ID, topicKey, t3))
	mentions, err = f.wcs.UnreadMentionKeys(f.ctx, f.alice.ID, []string{topicKey})
	require.NoError(t, err)
	assert.False(t, mentions[topicKey])
}
