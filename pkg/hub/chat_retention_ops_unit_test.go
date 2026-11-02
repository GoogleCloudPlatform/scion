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

package hub

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// seedPreviewFixture fills h with messages of every kind the two phases
// select or skip: old and new messages of live conversations in two
// projects, a deleted conversation with old and new messages, a deleted
// legacy topic's messages (thread_id, no conversation), a deleted topic
// whose conversation is live, and pending and deferred messages.
func seedPreviewFixture(h *chatRetentionUnitHarness) {
	now := h.clock.t
	day := 24 * time.Hour
	old := now.Add(-100 * day)
	deleted := now.Add(-40 * day)
	h.st.convDeleted = map[string]time.Time{"gone": deleted, "inside-grace": now.Add(-5 * day)}
	h.st.topics = []fakeChatRetentionTopic{
		{id: "legacy-topic", deleted: deleted},
		{id: "linked-topic", conv: "live-linked", deleted: deleted},
		{id: "live-topic", conv: "live"},
	}
	for i := 0; i < 70; i++ {
		at := old.Add(time.Duration(i) * time.Minute)
		h.st.addConv(fmt.Sprintf("p1-old-%03d", i), at, "p1", "delivered", "live", "")
		h.st.addConv(fmt.Sprintf("p2-old-%03d", i), at, "p2", "delivered", "", "")
	}
	for i := 0; i < 30; i++ {
		h.st.addConv(fmt.Sprintf("gone-old-%03d", i), old.Add(time.Duration(i)*time.Second), "p1", "delivered", "gone", "")
		h.st.addConv(fmt.Sprintf("gone-new-%03d", i), now.Add(-time.Hour), "p2", "delivered", "gone", "")
		h.st.addConv(fmt.Sprintf("legacy-old-%03d", i), old.Add(time.Duration(i)*time.Second), "p2", "delivered", "", "legacy-topic")
	}
	h.st.addConv("linked-1", now.Add(-time.Hour), "p1", "delivered", "live-linked", "linked-topic")
	h.st.addConv("linked-legacy-1", old, "p1", "delivered", "", "linked-topic")
	h.st.addConv("grace-1", now.Add(-time.Hour), "p1", "delivered", "inside-grace", "")
	h.st.addConv("pending-old", old, "p1", "pending", "live", "")
	h.st.addConv("deferred-gone", old, "p1", "deferred", "gone", "")
	h.st.addConv("p1-new", now.Add(-time.Hour), "p1", "delivered", "live", "")
}

// TestChatRetentionPreview_CountsEqualPurge: on the same fixture the
// preview counts exactly what a manual sweep then deletes, per project and
// phase, a message of a deleted conversation older than the window is
// counted once (under the deleted-conversation phase), and the preview
// deletes nothing, writes no cursor and keeps the pause between selections.
func TestChatRetentionPreview_CountsEqualPurge(t *testing.T) {
	h := newChatRetentionUnitHarness()
	seedPreviewFixture(h)
	before := len(h.st.msgs)
	cfg := chatRetentionTestConfig(func(c *opsettings.ChatRetentionConfig) { c.ChunkSize = 25 })
	p := h.policy(cfg)

	preview, err := h.sw.Preview(context.Background(), p)
	require.NoError(t, err)
	assert.True(t, preview.DeletedDone)
	assert.True(t, preview.MainDone)
	assert.Len(t, h.st.msgs, before, "the preview deletes nothing")
	assert.False(t, h.st.hasCursor, "the preview writes no cursor")
	assert.Zero(t, h.st.purgeCalls)
	assert.Len(t, h.pauses, preview.Selects-1, "a pause between selections, none after the last")
	for _, d := range h.pauses {
		assert.Equal(t, cfg.Pause(), d)
	}
	// The old messages of "gone" count under the deleted-conversation
	// phase, not the window; a deleted topic's live conversation and its
	// legacy rows count there too.
	assert.Equal(t, map[string]store.ChatRetentionCounts{
		"p1": {Main: 70, DeletedConv: 30 + 1 + 1},
		"p2": {Main: 70, DeletedConv: 30 + 30},
	}, preview.ByProject)

	run := &store.ChatRetentionRun{Trigger: store.ChatRetentionTriggerManual, StartedAt: h.clock.t}
	var res chatRetentionTickResult
	start := h.clock.t
	require.NoError(t, h.sw.sweepAndRecord(context.Background(), p, run, &res, start, start.Add(chatRetentionOpMaxDuration), false))
	for project, want := range preview.ByProject {
		got := run.Counts[project]
		assert.Equal(t, want.Main, got.Main, "project %s main", project)
		assert.Equal(t, want.DeletedConv, got.DeletedConv, "project %s deleted_conv", project)
	}
	assert.Equal(t, store.ChatRetentionStatusOK, run.Status)
	for _, id := range []string{"pending-old", "deferred-gone", "p1-new", "grace-1"} {
		assert.True(t, h.st.has(id), "kept %s", id)
	}
}

// TestChatRetentionPreview_HypotheticalWindowAndNoWindow: the preview
// policy replaces the saved window with the requested one, runs even when
// the saved section is disabled, and without any window counts only the
// deleted-conversation phase.
func TestChatRetentionPreview_HypotheticalWindowAndNoWindow(t *testing.T) {
	h := newChatRetentionUnitHarness()
	seedPreviewFixture(h)
	saved := opsettings.DefaultChatRetentionConfig() // disabled, no window

	p := chatRetentionPreviewPolicy(saved, 0, h.clock.t)
	assert.True(t, p.purges(), "a preview runs whatever enabled says")
	assert.True(t, p.MainCutoff.IsZero())
	res, err := h.sw.Preview(context.Background(), p)
	require.NoError(t, err)
	assert.Zero(t, res.ByProject["p1"].Main)
	assert.Zero(t, res.ByProject["p2"].Main)
	assert.Equal(t, int64(32), res.ByProject["p1"].DeletedConv)

	p = chatRetentionPreviewPolicy(saved, 365, h.clock.t)
	assert.Equal(t, 365, p.Config.MessagesRetentionDays)
	res, err = h.sw.Preview(context.Background(), p)
	require.NoError(t, err)
	assert.Zero(t, res.ByProject["p1"].Main, "nothing is older than 365 days")

	p = chatRetentionPreviewPolicy(saved, 30, h.clock.t)
	res, err = h.sw.Preview(context.Background(), p)
	require.NoError(t, err)
	assert.Equal(t, int64(70), res.ByProject["p1"].Main)
}

// TestChatRetentionPreview_ErrorAndContextDone: a failing selection or a
// cancelled context ends the preview with the error and the counts so far.
func TestChatRetentionPreview_ErrorAndContextDone(t *testing.T) {
	h := newChatRetentionUnitHarness()
	seedPreviewFixture(h)
	h.st.topicListErr = errors.New("topic list failed")
	_, err := h.sw.Preview(context.Background(), h.policy(chatRetentionTestConfig(nil)))
	require.ErrorContains(t, err, "topic list failed")

	h = newChatRetentionUnitHarness()
	seedPreviewFixture(h)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = h.sw.Preview(ctx, h.policy(chatRetentionTestConfig(nil)))
	require.ErrorIs(t, err, context.Canceled)
}

// TestChatRetentionPreviewDays: the window parameter is optional, and a
// value outside 7..36500 or not a whole number is refused before a run.
func TestChatRetentionPreviewDays(t *testing.T) {
	days, err := chatRetentionPreviewDays(nil)
	require.NoError(t, err)
	assert.Zero(t, days)
	days, err = chatRetentionPreviewDays(map[string]string{chatRetentionParamDays: "365"})
	require.NoError(t, err)
	assert.Equal(t, 365, days)
	for _, bad := range []string{"6", "36501", "abc", "7.5", "-30"} {
		_, err := chatRetentionPreviewDays(map[string]string{chatRetentionParamDays: bad})
		require.Error(t, err, bad)
		assert.Contains(t, err.Error(), "from 7 to 36500")
	}
}

// TestChatRetentionSweeper_DeletedListErrorKeepsFirstError: when a
// candidate's purge fails and a later candidate list fails too, the tick
// reports the candidate's error, the first one.
func TestChatRetentionSweeper_DeletedListErrorKeepsFirstError(t *testing.T) {
	h := newChatRetentionUnitHarness()
	old := h.clock.t.Add(-40 * 24 * time.Hour)
	h.st.convDeleted = map[string]time.Time{"a-broken": old}
	h.st.addConv("broken-1", h.clock.t.Add(-time.Hour), "p1", "delivered", "a-broken", "")
	h.st.conversationSelectFail = map[string]bool{"a-broken": true}
	h.st.topicListErr = errors.New("topic list failed")
	cfg := chatRetentionTestConfig(func(c *opsettings.ChatRetentionConfig) { c.MessagesRetentionDays = 0 })

	res, err := h.sw.RunTick(context.Background(), h.policy(cfg))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "select timed out")
	assert.NotContains(t, err.Error(), "topic list failed")
	assert.False(t, res.DeletedDone)
	require.Len(t, h.st.runs, 1)
	assert.Contains(t, h.st.runs[0].Error, "select timed out")
}

// TestFailedPurge_DeletesUnlinkedAttachments: the failed-message purge
// deletes the bytes and then the row of each attachment it leaves with no
// link, counting them; one whose byte delete fails keeps its row; one
// still linked by a surviving message is untouched.
func TestFailedPurge_DeletesUnlinkedAttachments(t *testing.T) {
	const (
		proj     = "11111111-1111-4111-8111-111111111111"
		attOK    = "22222222-2222-4222-8222-222222222222"
		attFail  = "33333333-3333-4333-8333-333333333333"
		attShare = "44444444-4444-4444-8444-444444444444"
	)
	h := newChatRetentionUnitHarness()
	h.seedFailed(3, proj)
	h.st.add("kept", h.clock.t.Add(-time.Hour), proj, "delivered")
	ids := []string{proj + "-f000000", proj + "-f000001", proj + "-f000002"}
	h.st.attach(attOK, proj, 100, ids[0])
	h.st.attach(attFail, proj, 7, ids[1])
	h.st.attach(attShare, proj, 9, ids[2], "kept")
	blobs := &fakeChatRetentionBlobs{fail: map[string]bool{attFail: true}}
	h.sw.attachments = blobs

	res, err := h.sw.PurgeFailed(context.Background(), h.clock.t.Add(-7*24*time.Hour), chatRetentionTestConfig(nil))
	require.NoError(t, err)
	assert.Equal(t, []string{proj + "/" + attOK}, blobs.deleted)
	assert.NotContains(t, h.st.attachments, attOK)
	assert.Contains(t, h.st.attachments, attFail, "a failed byte delete keeps the row for the attachment GC")
	assert.Contains(t, h.st.attachments, attShare, "still linked by a surviving message")
	assert.Equal(t, 1, res.AttachmentDeleteFailures)
	total := res.Total()
	assert.Equal(t, int64(3), total.Failed)
	assert.Equal(t, int64(1), total.AttachmentRows)
	assert.Equal(t, int64(100), total.AttachmentBytes)

	// Without a blob store every row stays for the attachment GC.
	h = newChatRetentionUnitHarness()
	h.seedFailed(1, proj)
	h.st.attach(attOK, proj, 100, proj+"-f000000")
	res, err = h.sw.PurgeFailed(context.Background(), h.clock.t.Add(-7*24*time.Hour), chatRetentionTestConfig(nil))
	require.NoError(t, err)
	assert.Contains(t, h.st.attachments, attOK)
	assert.Zero(t, res.Total().AttachmentRows)
}

// TestChatRetentionRunJSON: a run in the admin API carries its totals, the
// projects with messages and the policy's days, and never a project ID.
func TestChatRetentionRunJSON(t *testing.T) {
	fin := time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)
	got := chatRetentionRunJSONFor(store.ChatRetentionRun{
		ID: "r1", Trigger: store.ChatRetentionTriggerPreview, DryRun: true, Status: store.ChatRetentionStatusOK,
		StartedAt: fin.Add(-time.Hour), FinishedAt: &fin, Actor: "u1",
		Policy: store.ChatRetentionPolicy{MessagesRetentionDays: 30, DeletedConversationGraceDays: 7},
		Counts: map[string]store.ChatRetentionCounts{
			"p1": {Main: 3, DeletedConv: 1, Mentions: 9},
			"p2": {Main: 2},
			"p3": {AttachmentRows: 1},
		},
	})
	assert.Equal(t, int64(6), got.Messages)
	assert.Equal(t, 2, got.Projects)
	assert.Equal(t, int64(9), got.Totals.Mentions)
	assert.Equal(t, 30, got.MessagesRetentionDays)
	assert.Equal(t, 7, got.DeletedConversationGraceDays)
	assert.Equal(t, "u1", got.Actor)
}
