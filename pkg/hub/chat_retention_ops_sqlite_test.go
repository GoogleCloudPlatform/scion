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

// TestChatRetention_ReconcileInterruptedRunsSQLite: at start, the preview
// and manual runs still "running" from before this process are marked
// failed (interrupted); a sweep run, a finished preview and a preview this
// process started are left alone. The panel then reports the failure
// instead of a preview running forever.
func TestChatRetention_ReconcileInterruptedRunsSQLite(t *testing.T) {
	f := newChatRetentionHubFixture(t)
	setChatRetentionSettings(t, f.srv, true, `{"enabled":true,"messages_retention_days":30}`)
	f.srv.startTime = time.Now().UTC()
	before := f.srv.startTime.Add(-time.Hour)
	fin := before.Add(time.Minute)
	save := func(trigger string, dry bool, started time.Time, status string, finished *time.Time) string {
		t.Helper()
		r := &store.ChatRetentionRun{Trigger: trigger, DryRun: dry, StartedAt: started, Status: status, FinishedAt: finished, Actor: f.alice.ID}
		require.NoError(t, f.crs.SaveChatRetentionRun(f.ctx, r))
		return r.ID
	}
	stuckPreview := save(store.ChatRetentionTriggerPreview, true, before, store.ChatRetentionStatusRunning, nil)
	stuckManual := save(store.ChatRetentionTriggerManual, false, before, store.ChatRetentionStatusRunning, nil)
	openSweep := save(store.ChatRetentionTriggerSweep, false, before, store.ChatRetentionStatusRunning, nil)
	donePreview := save(store.ChatRetentionTriggerPreview, true, before.Add(-time.Hour), store.ChatRetentionStatusOK, &fin)
	livePreview := save(store.ChatRetentionTriggerPreview, true, f.srv.startTime.Add(time.Second), store.ChatRetentionStatusRunning, nil)

	assert.Equal(t, 2, f.srv.ReconcileInterruptedChatRetentionRuns(f.ctx))

	runs, err := f.crs.ListChatRetentionRuns(f.ctx, store.ChatRetentionRunFilter{})
	require.NoError(t, err)
	byID := map[string]store.ChatRetentionRun{}
	for _, r := range runs {
		byID[r.ID] = r
	}
	for _, id := range []string{stuckPreview, stuckManual} {
		assert.Equal(t, store.ChatRetentionStatusFailed, byID[id].Status, id)
		assert.Equal(t, chatRetentionInterruptedError, byID[id].Error, id)
		assert.NotNil(t, byID[id].FinishedAt, id)
	}
	assert.Equal(t, store.ChatRetentionStatusRunning, byID[openSweep].Status, "the open sweep run is the next tick's")
	assert.Equal(t, store.ChatRetentionStatusOK, byID[donePreview].Status)
	assert.Equal(t, store.ChatRetentionStatusRunning, byID[livePreview].Status, "a run of this process is not interrupted")
	assert.Zero(t, f.srv.ReconcileInterruptedChatRetentionRuns(f.ctx), "idempotent")

	got := decodeChatRetentionGET(t, f)
	require.NotNil(t, got.LastPreview)
	assert.Equal(t, livePreview, got.LastPreview.ID)
}

// decodeChatRetentionGET reads the settings GET as the fixture's admin view
// would, through the handler's own history assembly.
func decodeChatRetentionGET(t *testing.T, f *chatRetentionHubFixture) chatRetentionResponse {
	t.Helper()
	resp := chatRetentionResponseFor(f.srv.chatRetentionConfig(), 0, nil)
	f.srv.chatRetentionHistory(f.ctx, &resp)
	return resp
}

// TestChatRetention_PreviewDeadlineIsPartialSQLite: a preview that reaches
// its own time limit (not the caller's) is recorded as partial with the
// counts so far, and the operation does not fail.
func TestChatRetention_PreviewDeadlineIsPartialSQLite(t *testing.T) {
	f := newChatRetentionHubFixture(t)
	// A pause far longer than the walk's time: the walk ends at its deadline
	// during the pause after the first selection.
	setChatRetentionSettings(t, f.srv, true, `{"enabled":false,"messages_retention_days":30,"sweep":{"chunk_size":25,"pause_ms":10000}}`)
	old := time.Now().UTC().Add(-40 * 24 * time.Hour)
	for i := 0; i < 30; i++ {
		f.message(chatRetentionSeed{created: old.Add(time.Duration(i) * time.Second)})
	}
	// The caller's deadline leaves the walk about 1 s (chatRetentionWalkEnd
	// keeps a chunk timeout and a margin before it).
	ctx, cancel := context.WithTimeout(f.ctx, chatRetentionChunkTimeout+chatRetentionDeadlineMargin+time.Second)
	defer cancel()
	var log bytes.Buffer
	run, err := f.srv.runChatRetentionPreview(ctx, &log, 0, f.alice.ID)
	require.NoError(t, err, log.String())
	assert.Equal(t, store.ChatRetentionStatusPartial, run.Status)
	assert.Empty(t, run.Error)
	assert.Equal(t, int64(25), run.Counts[f.project.ID].Main, "the first chunk's count is kept")
	assert.Contains(t, log.String(), "stopped at its time limit")
	assert.NoError(t, ctx.Err(), "the caller's context is still live")
	assert.Equal(t, 30, f.count(`SELECT COUNT(*) FROM messages`), "nothing deleted")
}

// chunkTimeoutChatRetentionStore fails the main walk's selection the way a
// selection that runs past its chunk timeout fails (a wrapped
// context.DeadlineExceeded), while the walk's own deadline is far away.
type chunkTimeoutChatRetentionStore struct {
	store.ChatRetentionStore
}

func (s chunkTimeoutChatRetentionStore) SelectChatRetentionCandidates(context.Context, time.Time, store.ChatRetentionPosition, int) ([]store.ChatRetentionCandidate, error) {
	return nil, fmt.Errorf("chat retention: select candidates: %w", context.DeadlineExceeded)
}

// TestChatRetention_PreviewChunkTimeoutIsFailedSQLite: a selection that
// times out on its chunk timeout is a failure, not a partial preview: the
// run is recorded failed with the error and the operation reports it. Only
// the walk's own deadline makes a preview partial
// (TestChatRetention_PreviewDeadlineIsPartialSQLite).
func TestChatRetention_PreviewChunkTimeoutIsFailedSQLite(t *testing.T) {
	f := newChatRetentionHubFixture(t)
	setChatRetentionSettings(t, f.srv, true, `{"enabled":false,"messages_retention_days":30,"sweep":{"pause_ms":0}}`)
	f.message(chatRetentionSeed{created: time.Now().UTC().Add(-40 * 24 * time.Hour)})
	f.srv.SetChatRetentionStore(chunkTimeoutChatRetentionStore{f.crs})

	var log bytes.Buffer
	run, err := f.srv.runChatRetentionPreview(f.ctx, &log, 0, f.alice.ID)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	require.NotNil(t, run)
	assert.Equal(t, store.ChatRetentionStatusFailed, run.Status)
	assert.Contains(t, run.Error, "deadline exceeded")
	assert.NotContains(t, log.String(), "stopped at its time limit")

	stored, err := f.crs.ListChatRetentionRuns(f.ctx, store.ChatRetentionRunFilter{Trigger: store.ChatRetentionTriggerPreview})
	require.NoError(t, err)
	require.Len(t, stored, 1)
	assert.Equal(t, store.ChatRetentionStatusFailed, stored[0].Status)
}

// lockHeldStore is the hub store with LockChatRetention held by another
// replica (a live manual run or a sweep tick there).
type lockHeldStore struct {
	store.Store
}

func (lockHeldStore) TryAdvisoryLock(context.Context, store.AdvisoryLockKey) (bool, func() error, error) {
	return false, func() error { return nil }, nil
}

func (lockHeldStore) TryAdvisoryLockObject(context.Context, store.AdvisoryLockKey, int32) (bool, func() error, error) {
	return false, func() error { return nil }, nil
}

// TestChatRetention_ReconcileWhileLockHeldSQLite: while another replica
// holds LockChatRetention, a manual run started before this process may be
// live there and is left running; only a manual row older than any run can
// last is failed. Previews take no lock and are reconciled by start time.
func TestChatRetention_ReconcileWhileLockHeldSQLite(t *testing.T) {
	f := newChatRetentionHubFixture(t)
	installStoreFault(t, f.srv, func(inner store.Store, _ *storeFaultSwitch) lockHeldStore { return lockHeldStore{inner} })
	f.srv.startTime = time.Now().UTC()
	save := func(trigger string, started time.Time) string {
		t.Helper()
		r := &store.ChatRetentionRun{Trigger: trigger, DryRun: trigger == store.ChatRetentionTriggerPreview,
			StartedAt: started, Status: store.ChatRetentionStatusRunning}
		require.NoError(t, f.crs.SaveChatRetentionRun(f.ctx, r))
		return r.ID
	}
	recentManual := save(store.ChatRetentionTriggerManual, f.srv.startTime.Add(-time.Hour))
	staleManual := save(store.ChatRetentionTriggerManual, f.srv.startTime.Add(-chatRetentionRunMaxAge-time.Hour))
	preview := save(store.ChatRetentionTriggerPreview, f.srv.startTime.Add(-time.Hour))

	assert.Equal(t, 2, f.srv.ReconcileInterruptedChatRetentionRuns(f.ctx))

	runs, err := f.crs.ListChatRetentionRuns(f.ctx, store.ChatRetentionRunFilter{})
	require.NoError(t, err)
	status := map[string]string{}
	for _, r := range runs {
		status[r.ID] = r.Status
	}
	assert.Equal(t, store.ChatRetentionStatusRunning, status[recentManual], "may be live on the replica holding the lock")
	assert.Equal(t, store.ChatRetentionStatusFailed, status[staleManual], "older than any run can last")
	assert.Equal(t, store.ChatRetentionStatusFailed, status[preview])
}
