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
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

const chatRetentionAdminPath = "/api/v1/admin/chat-retention"

// chatRetentionAdminServer is a server with live, store-backed operational
// settings, and its hub admin and member users. The experiment is turned on
// when on is true.
func chatRetentionAdminServer(t *testing.T, on bool) (*Server, store.Store, *store.User, *store.User) {
	t.Helper()
	srv, s := testServerWithOps(t, nil)
	ctx := context.Background()
	admin, err := s.GetUser(ctx, hubConfigTokenUser(t, s, "cr-admin", store.SystemRoleSuperAdmin))
	require.NoError(t, err)
	member, err := s.GetUser(ctx, hubConfigTokenUser(t, s, "cr-member", store.SystemRoleHubMember))
	require.NoError(t, err)
	if on {
		ops := srv.GetOperationalSettings()
		doc := fmt.Sprintf(`{"overrides":{%q:true}}`, experiments.ChatRetention)
		_, err := ops.Update(ctx, "experiments", json.RawMessage(doc), "test", ops.SectionRevision("experiments"), "managed")
		require.NoError(t, err)
	}
	return srv, s, admin, member
}

func decodeChatRetention(t *testing.T, rec *httptest.ResponseRecorder) chatRetentionResponse {
	t.Helper()
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var body chatRetentionResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	return body
}

// TestAdminChatRetention_ExperimentOff404: while hub.chat_retention is off
// the route answers 404 to an admin, for GET and PUT, and nothing is
// stored.
func TestAdminChatRetention_ExperimentOff404(t *testing.T) {
	srv, _, admin, _ := chatRetentionAdminServer(t, false)
	rec := doRequestAsUser(t, srv, admin, http.MethodGet, chatRetentionAdminPath, nil)
	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	rec = doRequestAsUser(t, srv, admin, http.MethodPut, chatRetentionAdminPath, map[string]any{"enabled": true, "expected_revision": 0})
	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	assert.Equal(t, int64(0), srv.GetOperationalSettings().SectionRevision(opsettings.ChatRetentionSectionName))
}

// TestAdminChatRetention_AdminRoundTrip: defaults, a full update, partial
// updates, null resets, and the JSON shape of the response.
func TestAdminChatRetention_AdminRoundTrip(t *testing.T) {
	srv, _, admin, _ := chatRetentionAdminServer(t, true)

	rec := doRequestAsUser(t, srv, admin, http.MethodGet, chatRetentionAdminPath, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.JSONEq(t, `{"enabled":false,"messages_retention_days":null,"sweep":{"chunk_size":200,"pause_ms":500,"tick_budget_seconds":30,"tick_interval_minutes":10},"last_applied_cutoff":null,"revision":0}`, rec.Body.String())

	got := decodeChatRetention(t, doRequestAsUser(t, srv, admin, http.MethodPut, chatRetentionAdminPath, map[string]any{
		"enabled": true, "messages_retention_days": 30, "sweep": map[string]any{"chunk_size": 100}, "expected_revision": 0,
	}))
	assert.True(t, got.Enabled)
	require.NotNil(t, got.MessagesRetentionDays)
	assert.Equal(t, 30, *got.MessagesRetentionDays)
	assert.Equal(t, chatRetentionSweepJSON{ChunkSize: 100, PauseMS: 500, TickBudgetSeconds: 30, TickIntervalMinutes: 10}, got.Sweep)
	require.Positive(t, got.Revision)
	cfg := srv.GetOperationalSettings().ChatRetention()
	assert.True(t, cfg.Enabled)
	assert.Equal(t, 30, cfg.MessagesRetentionDays)

	// Sweep tuning alone needs no expected_revision.
	got = decodeChatRetention(t, doRequestAsUser(t, srv, admin, http.MethodPut, chatRetentionAdminPath, map[string]any{
		"sweep": map[string]any{"pause_ms": 100, "chunk_size": nil},
	}))
	assert.Equal(t, chatRetentionSweepJSON{ChunkSize: 200, PauseMS: 100, TickBudgetSeconds: 30, TickIntervalMinutes: 10}, got.Sweep)
	assert.True(t, got.Enabled, "omitted fields are unchanged")

	// null window = keep forever; "sweep": null resets the tuning.
	got = decodeChatRetention(t, doRequestAsUser(t, srv, admin, http.MethodPut, chatRetentionAdminPath, map[string]any{
		"messages_retention_days": nil, "sweep": nil, "expected_revision": got.Revision,
	}))
	assert.Nil(t, got.MessagesRetentionDays)
	assert.Equal(t, chatRetentionSweepJSON{ChunkSize: 200, PauseMS: 500, TickBudgetSeconds: 30, TickIntervalMinutes: 10}, got.Sweep)
	assert.True(t, got.Enabled)
}

// TestAdminChatRetention_CASAndValidation: changing enabled or the window
// needs the current expected_revision; out-of-range values, unknown fields
// and later-phase keys are refused.
func TestAdminChatRetention_CASAndValidation(t *testing.T) {
	srv, _, admin, _ := chatRetentionAdminServer(t, true)
	put := func(body map[string]any) *httptest.ResponseRecorder {
		return doRequestAsUser(t, srv, admin, http.MethodPut, chatRetentionAdminPath, body)
	}

	rec := put(map[string]any{"enabled": true})
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	rec = put(map[string]any{"messages_retention_days": 30})
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	rec = put(map[string]any{"enabled": true, "expected_revision": 5})
	assert.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.Equal(t, "revision_conflict", errorCode(t, rec))

	for _, body := range []map[string]any{
		{"messages_retention_days": 6, "expected_revision": 0},
		{"messages_retention_days": 0, "expected_revision": 0},
		{"sweep": map[string]any{"chunk_size": 24}},
		{"sweep": map[string]any{"chunk_size": 1001}},
		{"sweep": map[string]any{"pause_ms": -1}},
		{"sweep": map[string]any{"tick_budget_seconds": 0}},
		{"sweep": map[string]any{"tick_budget_seconds": 41}},
		{"sweep": map[string]any{"pause_ms": 10001}},
		{"sweep": map[string]any{"tick_interval_minutes": 0}},
		{"deleted_conversation_grace_days": 30},
		{"failed_message_retention_days": 7},
		{"sweep": map[string]any{"bogus": 1}},
	} {
		rec := put(body)
		assert.Equal(t, http.StatusBadRequest, rec.Code, "%v: %s", body, rec.Body.String())
	}
	assert.Equal(t, int64(0), srv.GetOperationalSettings().SectionRevision(opsettings.ChatRetentionSectionName), "nothing was stored")
	assert.False(t, srv.GetOperationalSettings().ChatRetention().Enabled)
}

// TestAdminChatRetention_MemberRefused: a hub member can neither read nor
// write the section.
func TestAdminChatRetention_MemberRefused(t *testing.T) {
	srv, _, _, member := chatRetentionAdminServer(t, true)
	rec := doRequestAsUser(t, srv, member, http.MethodGet, chatRetentionAdminPath, nil)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	rec = doRequestAsUser(t, srv, member, http.MethodPut, chatRetentionAdminPath, map[string]any{"enabled": true, "expected_revision": 0})
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.False(t, srv.GetOperationalSettings().ChatRetention().Enabled)
}

// TestAdminChatRetention_TokenRefused: a user access token is refused on
// the route, even a hub admin's token carrying the hub configuration
// selectors, with the session-only reason; nothing changes.
func TestAdminChatRetention_TokenRefused(t *testing.T) {
	srv, _, admin, _ := chatRetentionAdminServer(t, true)
	key := mintHubConfigToken(t, srv, admin.ID, hubBoundary(), "hub_config:read", "hub_config:update")
	for _, method := range []string{http.MethodPut, http.MethodGet} {
		var body any
		if method == http.MethodPut {
			body = map[string]any{"enabled": true, "messages_retention_days": 7, "expected_revision": 0}
		}
		rec := doRequestWithToken(t, srv, key, method, chatRetentionAdminPath, body)
		require.Equal(t, http.StatusForbidden, rec.Code, "%s: %s", method, rec.Body.String())
		_, details := errorCodeAndDetails(t, rec)
		require.Equal(t, "IRREVERSIBLE_CASCADE", details["reason"], "%s: %s", method, rec.Body.String())
	}
	assert.False(t, srv.GetOperationalSettings().ChatRetention().Enabled)
}

// TestAdminChatRetention_SaveIsAudited: a change writes a mutation audit
// record with the caller and the before/after values; a save that changes
// nothing writes none.
func TestAdminChatRetention_SaveIsAudited(t *testing.T) {
	srv, s, admin, _ := chatRetentionAdminServer(t, true)
	ctx := context.Background()
	records := func() []*store.MutationAuditRecord {
		recs, _, err := s.ListMutationAudits(ctx, store.MutationAuditFilter{TargetID: opsettings.ChatRetentionSectionName, Limit: 100})
		require.NoError(t, err)
		return recs
	}

	got := decodeChatRetention(t, doRequestAsUser(t, srv, admin, http.MethodPut, chatRetentionAdminPath, map[string]any{
		"enabled": true, "messages_retention_days": 30, "expected_revision": 0,
	}))
	recs := records()
	require.Len(t, recs, 1)
	r := recs[0]
	assert.Equal(t, "hub_setting_update", r.MutationType)
	assert.Equal(t, "hub_setting", r.TargetType)
	assert.Equal(t, admin.ID, r.ActorPrincipalID)
	assert.Equal(t, "enabled=false,messages_retention_days=null,chunk_size=200,pause_ms=500,tick_budget_seconds=30,tick_interval_minutes=10", r.BeforeSummary)
	assert.Equal(t, "enabled=true,messages_retention_days=30,chunk_size=200,pause_ms=500,tick_budget_seconds=30,tick_interval_minutes=10", r.AfterSummary)

	decodeChatRetention(t, doRequestAsUser(t, srv, admin, http.MethodPut, chatRetentionAdminPath, map[string]any{
		"enabled": true, "expected_revision": got.Revision,
	}))
	assert.Len(t, records(), 1, "an unchanged save writes no audit record")
}

// TestAdminChatRetention_GenericSectionResetRefused: the generic section
// reset (DELETE /api/v1/admin/server-config/sections/chat_retention) is
// refused, because it would skip the experiment gate, the revision check
// and the audit record; the stored section is unchanged.
func TestAdminChatRetention_GenericSectionResetRefused(t *testing.T) {
	srv, s, admin, _ := chatRetentionAdminServer(t, true)
	srv.dbDriver = "postgres" // unlock the postgres-only generic-reset route for this test
	decodeChatRetention(t, doRequestAsUser(t, srv, admin, http.MethodPut, chatRetentionAdminPath, map[string]any{
		"enabled": true, "messages_retention_days": 30, "expected_revision": 0,
	}))
	before, err := s.GetHubSetting(context.Background(), opsettings.ChatRetentionSectionName)
	require.NoError(t, err)

	rec := doRequestAsUser(t, srv, admin, http.MethodDelete, "/api/v1/admin/server-config/sections/chat_retention", nil)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Equal(t, "validation_failed", errorCode(t, rec))
	assert.Contains(t, rec.Body.String(), "/api/v1/admin/chat-retention")

	after, err := s.GetHubSetting(context.Background(), opsettings.ChatRetentionSectionName)
	require.NoError(t, err)
	assert.Equal(t, string(before.Value), string(after.Value))
	assert.Equal(t, before.Revision, after.Revision)
	assert.True(t, srv.GetOperationalSettings().ChatRetention().Enabled)
}
