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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/maintenance"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
)

// seedPreviewSQLite stores, in two projects, messages of every kind the
// two phases select or skip, and returns the IDs a sweep must keep.
func (f *chatRetentionHubFixture) seedPreviewSQLite() (otherProject string, keep []string) {
	f.t.Helper()
	other := &store.Project{ID: api.NewUUID(), Name: "other", Slug: "other-" + api.NewUUID()[:8], Created: time.Now(), Updated: time.Now()}
	require.NoError(f.t, f.s.CreateProject(f.ctx, other))
	inOther := func(sd chatRetentionSeed) string {
		f.t.Helper()
		if sd.state == "" {
			sd.state = store.MessageDispatchDispatched
		}
		m := &store.Message{ID: api.NewUUID(), ProjectID: other.ID, Sender: "user:alice@test.com", SenderID: f.alice.ID,
			Recipient: "agent:bot", Msg: "hello", Type: "chat", Channel: sd.channel, ThreadID: sd.thread,
			ConversationID: sd.conv, DispatchState: sd.state, CreatedAt: sd.created}
		require.NoError(f.t, f.s.CreateMessage(f.ctx, m))
		f.dependents(m.ID)
		return m.ID
	}
	now := time.Now().UTC()
	day := 24 * time.Hour
	old := now.Add(-40 * day)
	recent := now.Add(-time.Hour)
	live := api.NewUUID()
	gone := f.deletedConversation(now.Add(-40 * day))
	legacy := f.rawTopic("", now.Add(-31*day))

	for i := 0; i < 7; i++ {
		at := old.Add(time.Duration(i) * time.Minute)
		f.message(chatRetentionSeed{created: at, channel: "web", conv: live})
		inOther(chatRetentionSeed{created: at}) // agent-to-agent, no conversation
		f.message(chatRetentionSeed{created: at, channel: "web", conv: gone})
		inOther(chatRetentionSeed{created: recent, channel: "web", conv: gone})
		inOther(chatRetentionSeed{created: at, channel: "web", thread: legacy})
	}
	keep = []string{
		f.message(chatRetentionSeed{created: old, channel: "web", conv: live, state: store.MessageDispatchPending}),
		f.message(chatRetentionSeed{created: old, channel: "web", conv: gone, state: store.MessageDispatchDeferred}),
		f.message(chatRetentionSeed{created: recent, channel: "web", conv: live}),
	}
	return other.ID, keep
}

// TestChatRetention_PreviewEqualsRunSQLite: on the real tables the preview
// counts exactly what a manual run then deletes, per project and phase; the
// preview deletes nothing and records a dry-run preview row with its actor
// and policy, and the manual run records a manual row with its actor.
func TestChatRetention_PreviewEqualsRunSQLite(t *testing.T) {
	f := newChatRetentionHubFixture(t)
	setChatRetentionSettings(t, f.srv, true, `{"enabled":true,"messages_retention_days":30,"sweep":{"chunk_size":25,"pause_ms":0}}`)
	other, keep := f.seedPreviewSQLite()
	total := f.count(`SELECT COUNT(*) FROM messages`)

	var log bytes.Buffer
	preview, err := f.srv.runChatRetentionPreview(f.ctx, &log, 0, f.alice.ID)
	require.NoError(t, err, log.String())
	assert.Equal(t, total, f.count(`SELECT COUNT(*) FROM messages`), "the preview deletes nothing")
	assert.Equal(t, store.ChatRetentionTriggerPreview, preview.Trigger)
	assert.True(t, preview.DryRun)
	assert.Equal(t, store.ChatRetentionStatusOK, preview.Status)
	assert.Equal(t, f.alice.ID, preview.Actor)
	assert.Equal(t, 30, preview.Policy.MessagesRetentionDays)
	assert.Equal(t, store.ChatRetentionCounts{Main: 7, DeletedConv: 7}, preview.Counts[f.project.ID])
	assert.Equal(t, store.ChatRetentionCounts{Main: 7, DeletedConv: 14}, preview.Counts[other])
	assert.Contains(t, log.String(), "Total: would delete 35 messages (14 by the window, 21 of deleted conversations) in 2 projects")

	stored, err := f.crs.ListChatRetentionRuns(f.ctx, store.ChatRetentionRunFilter{Trigger: store.ChatRetentionTriggerPreview})
	require.NoError(t, err)
	require.Len(t, stored, 1)
	assert.True(t, stored[0].DryRun)
	assert.Equal(t, f.alice.ID, stored[0].Actor)
	latestSweep, err := f.crs.LatestChatRetentionRun(f.ctx, store.ChatRetentionTriggerPreview, time.Time{})
	require.NoError(t, err)
	assert.Nil(t, latestSweep, "dry runs never count as the open run")

	run, _, err := f.srv.runChatRetentionManual(f.ctx, &log, f.alice.ID)
	require.NoError(t, err, log.String())
	assert.Equal(t, store.ChatRetentionTriggerManual, run.Trigger)
	assert.False(t, run.DryRun)
	assert.Equal(t, f.alice.ID, run.Actor)
	assert.Equal(t, store.ChatRetentionStatusOK, run.Status)
	for project, want := range preview.Counts {
		got := run.Counts[project]
		assert.Equal(t, want.Main, got.Main, "project %s main", project)
		assert.Equal(t, want.DeletedConv, got.DeletedConv, "project %s deleted_conv", project)
	}
	assert.Equal(t, len(keep), f.count(`SELECT COUNT(*) FROM messages`))
	for _, id := range keep {
		assert.True(t, f.exists(id), "kept %s", id)
	}
	assert.Equal(t, chatRetentionNoOrphans(), chatRetentionOrphans(t, f.db))
	cur, err := f.crs.GetChatRetentionCursor(f.ctx, store.ChatRetentionPhaseMain)
	require.NoError(t, err)
	assert.NotNil(t, cur.LastAppliedCutoff, "a manual run that reaches the cutoff records it")
}

// TestChatRetention_ManualRunWaitsForTickSQLite: a manual run waits while a
// sweep tick holds the guard, then runs; with the sweep disabled it is
// refused.
func TestChatRetention_ManualRunWaitsForTickSQLite(t *testing.T) {
	prev := chatRetentionLockPoll
	chatRetentionLockPoll = 10 * time.Millisecond
	t.Cleanup(func() { chatRetentionLockPoll = prev })

	f := newChatRetentionHubFixture(t)
	setChatRetentionSettings(t, f.srv, true, `{"enabled":false,"messages_retention_days":30,"sweep":{"pause_ms":0}}`)
	err := (&ChatRetentionRunExecutor{srv: f.srv}).ValidateParams(nil)
	re, ok := maintenance.IsRequestError(err)
	require.True(t, ok, "refused before a run: %v", err)
	assert.Equal(t, chatRetentionCodeDisabled, re.Code)
	assert.Equal(t, http.StatusConflict, re.HTTPStatus())

	setChatRetentionSettings(t, f.srv, true, `{"enabled":true,"messages_retention_days":30,"sweep":{"pause_ms":0}}`)
	id := f.message(chatRetentionSeed{created: time.Now().Add(-40 * 24 * time.Hour)})
	require.True(t, f.srv.chatRetention.running.CompareAndSwap(false, true), "a tick holds the guard")
	done := make(chan error, 1)
	var log bytes.Buffer
	go func() {
		_, _, err := f.srv.runChatRetentionManual(f.ctx, &log, f.alice.ID)
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("the manual run did not wait for the tick: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	assert.True(t, f.exists(id), "nothing is purged while the tick holds the guard")
	f.srv.chatRetention.running.Store(false)
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("the manual run did not start after the tick")
	}
	assert.False(t, f.exists(id))
	assert.Contains(t, log.String(), "Waiting for the running chat retention sweep to finish")
	assert.False(t, f.srv.chatRetention.running.Load(), "the manual run releases the guard")
}

// chatRetentionOpsServer is chatRetentionAdminServer with the chat
// retention store and the web chat tables wired, and quick sweep pacing.
func chatRetentionOpsServer(t *testing.T, on bool) (*Server, store.Store, *store.User, *store.User) {
	t.Helper()
	srv, s, admin, member := chatRetentionAdminServer(t, on)
	p, ok := s.(chatRetentionProvider)
	require.True(t, ok, "the test store provides the chat retention store")
	wcs := NewWebChatStore(p.DB(), "sqlite")
	require.NoError(t, wcs.Init())
	srv.SetWebChatStore(wcs)
	srv.SetChatRetentionStore(p.ChatRetention())
	return srv, s, admin, member
}

func setChatRetentionSection(t *testing.T, srv *Server, doc string) {
	t.Helper()
	ops := srv.GetOperationalSettings()
	_, err := ops.Update(context.Background(), opsettings.ChatRetentionSectionName, json.RawMessage(doc), "test",
		ops.SectionRevision(opsettings.ChatRetentionSectionName), "managed")
	require.NoError(t, err)
}

func chatRetentionOpPath(key string) string {
	return "/api/v1/admin/maintenance/operations/" + key + "/run"
}

// TestChatRetentionOps_HTTP: the preview and manual-run operations through
// the routed API. While hub.chat_retention is off they are not listed and
// answer 404; a hub member is refused; an admin's preview records a
// dry-run row with the admin as actor (a non-string window is refused); a
// manual run is refused while the sweep is disabled; and the settings GET
// lists the runs, the last preview and the messages purged since.
func TestChatRetentionOps_HTTP(t *testing.T) {
	t.Run("experiment off", func(t *testing.T) {
		srv, _, admin, _ := chatRetentionOpsServer(t, false)
		for _, key := range []string{entadapter.ChatRetentionPreviewKey, entadapter.ChatRetentionRunKey} {
			rec := doRequestAsUser(t, srv, admin, http.MethodPost, chatRetentionOpPath(key), map[string]any{})
			assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
			rec = doRequestAsUser(t, srv, admin, http.MethodGet, "/api/v1/admin/maintenance/operations/"+key, nil)
			assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
		}
		rec := doRequestAsUser(t, srv, admin, http.MethodGet, "/api/v1/admin/maintenance/operations", nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.NotContains(t, rec.Body.String(), entadapter.ChatRetentionPreviewKey)
		assert.NotContains(t, rec.Body.String(), entadapter.ChatRetentionRunKey)
	})

	t.Run("experiment on", func(t *testing.T) {
		srv, s, admin, member := chatRetentionOpsServer(t, true)
		setChatRetentionSection(t, srv, `{"enabled":false,"messages_retention_days":30,"sweep":{"pause_ms":0}}`)
		ctx := context.Background()
		project := &store.Project{ID: api.NewUUID(), Name: "ops", Slug: "ops-" + api.NewUUID()[:8], Created: time.Now(), Updated: time.Now()}
		require.NoError(t, s.CreateProject(ctx, project))
		for i := 0; i < 3; i++ {
			require.NoError(t, s.CreateMessage(ctx, &store.Message{ID: api.NewUUID(), ProjectID: project.ID, Sender: "agent:a",
				Recipient: "agent:b", Msg: "x", Type: "chat", DispatchState: store.MessageDispatchDispatched,
				CreatedAt: time.Now().Add(-40 * 24 * time.Hour)}))
		}

		rec := doRequestAsUser(t, srv, admin, http.MethodGet, "/api/v1/admin/maintenance/operations", nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), entadapter.ChatRetentionPreviewKey)

		for _, key := range []string{entadapter.ChatRetentionPreviewKey, entadapter.ChatRetentionRunKey} {
			rec := doRequestAsUser(t, srv, member, http.MethodPost, chatRetentionOpPath(key), map[string]any{})
			assert.Equal(t, http.StatusForbidden, rec.Code, "member runs %s: %s", key, rec.Body.String())
		}

		rec = doRequestAsUser(t, srv, admin, http.MethodPost, chatRetentionOpPath(entadapter.ChatRetentionPreviewKey),
			map[string]any{"params": map[string]any{"messages_retention_days": 30}})
		require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), maintenance.CodeInvalidParam)
		rec = doRequestAsUser(t, srv, admin, http.MethodPost, chatRetentionOpPath(entadapter.ChatRetentionPreviewKey),
			map[string]any{"params": map[string]any{"messages_retention_days": "3"}})
		require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		rec = doRequestAsUser(t, srv, admin, http.MethodPost, chatRetentionOpPath(entadapter.ChatRetentionRunKey), map[string]any{})
		require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), chatRetentionCodeDisabled)

		runOp := func(key string, body any) *store.MaintenanceOperationRun {
			t.Helper()
			rec := doRequestAsUser(t, srv, admin, http.MethodPost, chatRetentionOpPath(key), body)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			var resp struct {
				RunID string `json:"runId"`
			}
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
			run := waitForRunStatus(t, s, resp.RunID, func(r *store.MaintenanceOperationRun) bool {
				return r.Status != store.MaintenanceStatusRunning
			})
			require.Equal(t, store.MaintenanceStatusCompleted, run.Status, "log: %s result: %s", run.Log, run.Result)
			return run
		}
		op := runOp(entadapter.ChatRetentionPreviewKey, map[string]any{"params": map[string]any{"messages_retention_days": "60"}})
		assert.Contains(t, op.Log, "messages older than 60 days")
		assert.Contains(t, op.Log, "Total: would delete 0 messages")
		op = runOp(entadapter.ChatRetentionPreviewKey, map[string]any{})
		assert.Contains(t, op.Log, "Total: would delete 3 messages (3 by the window, 0 of deleted conversations) in 1 projects")

		setChatRetentionSection(t, srv, `{"enabled":true,"messages_retention_days":30,"sweep":{"pause_ms":0}}`)
		op = runOp(entadapter.ChatRetentionRunKey, map[string]any{})
		assert.Contains(t, op.Log, "Total: deleted 3 messages")

		got := decodeChatRetention(t, doRequestAsUser(t, srv, admin, http.MethodGet, chatRetentionAdminPath, nil))
		require.Len(t, got.Runs, 3)
		assert.Equal(t, store.ChatRetentionTriggerManual, got.Runs[0].Trigger)
		assert.Equal(t, admin.ID, got.Runs[0].Actor)
		assert.Equal(t, int64(3), got.Runs[0].Messages)
		assert.False(t, got.Runs[0].DryRun)
		require.NotNil(t, got.LastPreview)
		assert.True(t, got.LastPreview.DryRun)
		assert.Equal(t, int64(3), got.LastPreview.Messages)
		assert.Equal(t, 1, got.LastPreview.Projects)
		assert.Equal(t, 30, got.LastPreview.MessagesRetentionDays)
		assert.Equal(t, int64(3), got.PurgedSincePreview)
		raw := doRequestAsUser(t, srv, admin, http.MethodGet, chatRetentionAdminPath, nil).Body.String()
		assert.NotContains(t, raw, project.ID, "the history holds no project IDs")
	})
}

// TestChatHistory_AroundPurgedMessageUnavailable: jumping to a message the
// sweep purged answers the usual 404 with code message_unavailable, as
// does an ID that never existed; a message that exists still opens.
func TestChatHistory_AroundPurgedMessageUnavailable(t *testing.T) {
	f := newChatRetentionHubFixture(t)
	topicID := api.NewUUID()
	require.NoError(t, f.wcs.CreateTopic(f.ctx, WebChatTopic{
		ID: topicID, ProjectID: f.project.ID, Name: "jump", CreatedBy: f.alice.ID,
		CreatedAt: time.Now().UTC().Add(-90 * 24 * time.Hour),
	}))
	setTopicConversationID(t, f.db, f.s, topicID, f.project.ID)
	conv := topicConversationID(t, f.wcs, topicID)
	purged := f.message(chatRetentionSeed{created: time.Now().UTC().Add(-40 * 24 * time.Hour), channel: "web", conv: conv, thread: topicID})
	kept := f.message(chatRetentionSeed{created: time.Now().UTC().Add(-time.Hour), channel: "web", conv: conv, thread: topicID})
	_, err := f.crs.PurgeChatMessages(f.ctx, []string{purged}, nil, store.ChatRetentionCursor{Phase: "test"})
	require.NoError(t, err)

	around := func(id string) (int, map[string]any) {
		rec := doRequestAsUser(t, f.srv, f.alice, http.MethodGet,
			"/api/v1/chat/conversations/"+topicID+"/messages?around="+url.QueryEscape(id), nil)
		var body map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		return rec.Code, body
	}
	for name, id := range map[string]string{"purged": purged, "never existed": api.NewUUID(), "not a UUID": "x"} {
		code, body := around(id)
		require.Equal(t, http.StatusNotFound, code, "%s: %v", name, body)
		e, _ := body["error"].(map[string]any)
		assert.Equal(t, errCodeMessageUnavailable, e["code"], name)
		assert.Equal(t, "Message not found", e["message"], name)
	}
	code, body := around(kept)
	require.Equal(t, http.StatusOK, code, fmt.Sprint(body))
}
