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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// disableWriteDenySwitch turns the conversation envelope switch off on srv.
func disableWriteDenySwitch(t *testing.T, srv *Server) {
	t.Helper()
	fakeStore := newFakeHubSettingStore()
	ops := NewOperationalSettings(fakeStore, emptyKoanf(), emptyKoanf())
	fakeStore.seed("messaging", json.RawMessage(`{"conversation_envelope_switch":false}`))
	_, err := ops.Refresh(context.Background())
	require.NoError(t, err)
	srv.SetOperationalSettings(ops)
	require.False(t, srv.writeDenyEnabled(), "write-deny must be off")
}

// externalRefFixture is the routed inbound fixture plus a conversation that
// a chat thread reference already names in another project.
type externalRefFixture struct {
	routedTestEnv
	otherProject *store.Project
	otherConv    *store.Conversation
	surface      string
	ref          string
}

func newExternalRefFixture(t *testing.T) externalRefFixture {
	t.Helper()
	env := setupRoutedTestEnv(t)
	ctx := context.Background()

	other := &store.Project{
		ID:        tid("proj-extref-other"),
		Slug:      "extref-other",
		Name:      "External Ref Other Project",
		OwnerID:   env.user.ID,
		CreatedBy: env.user.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, env.store.CreateProject(ctx, other))

	const surface, ref = "slack", "C0EXTREF:1700000000.000100"
	otherProjectID := other.ID
	conv, err := env.store.UpsertConversationByExternalRef(ctx, &store.Conversation{
		Kind:        "group",
		Surface:     surface,
		ExternalRef: ref,
		ParentRef:   "C0EXTREF",
		DisplayName: "other project thread",
		DriftState:  "active",
		ProjectID:   &otherProjectID,
	})
	require.NoError(t, err)
	// Read back so later comparisons use the stored values.
	conv, err = env.store.GetConversation(ctx, conv.ID)
	require.NoError(t, err)

	return externalRefFixture{routedTestEnv: env, otherProject: other, otherConv: conv, surface: surface, ref: ref}
}

// postLegacyInbound posts to /api/v1/broker/inbound for agent alpha.
func (f externalRefFixture) postLegacyInbound(t *testing.T, surface, ref string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(inboundMessageRequest{
		Topic: "scion.project." + f.project.ID + ".agent." + f.agent1.Slug + ".messages",
		Message: &messages.StructuredMessage{
			Version:   messages.Version,
			Timestamp: time.Now().UTC().Format(time.RFC3339),
			Channel:   "slack",
			Sender:    "user:" + f.user.Email,
			Recipient: "agent:" + f.agent1.Slug,
			Msg:       "hello from the thread",
			Type:      messages.TypeInstruction,
		},
		Surface:     surface,
		ExternalRef: ref,
	})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/broker/inbound", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithBrokerIdentity(req.Context(), NewBrokerIdentity("test-broker")))
	rec := httptest.NewRecorder()
	f.srv.mux.ServeHTTP(rec, req)
	return rec
}

// postRoutedInbound posts to /api/v1/broker/inbound/routed for agent alpha.
func (f externalRefFixture) postRoutedInbound(t *testing.T, surface, ref string) *httptest.ResponseRecorder {
	t.Helper()
	return f.doRoutedRequest(t, routedInboundRequest{
		ProjectID:    f.project.ID,
		DefaultAgent: f.agent1.Slug,
		Surface:      surface,
		ExternalRef:  ref,
		Message: &messages.StructuredMessage{
			Version: messages.Version,
			Channel: "slack",
			Sender:  "user:" + f.user.Email,
			Msg:     "hello from the thread",
			Type:    messages.TypeInstruction,
		},
	})
}

// routedBodyWithoutMessageIDs decodes a routed response and blanks the
// per-request message IDs so two answers can be compared.
func routedBodyWithoutMessageIDs(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var resp ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp), "body: %s", rec.Body.String())
	if results, ok := resp.Error.Details["results"].([]interface{}); ok {
		for _, r := range results {
			if m, ok := r.(map[string]interface{}); ok {
				if _, has := m["message_id"]; has {
					m["message_id"] = "<message-id>"
				}
			}
		}
	}
	out, err := json.Marshal(resp)
	require.NoError(t, err)
	return string(out)
}

// requireNothingDelivered asserts that no message reached agent alpha and the
// other project's conversation is unchanged.
func (f externalRefFixture) requireNothingDelivered(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	assert.Empty(t, f.dispatcher.getCalls(), "nothing dispatched")
	msgs, err := f.store.ListMessages(ctx, store.MessageFilter{AgentID: f.agent1.ID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	assert.Empty(t, msgs.Items, "no message stored")
	got, err := f.store.GetConversation(ctx, f.otherConv.ID)
	require.NoError(t, err)
	require.NotNil(t, got.ProjectID)
	assert.Equal(t, f.otherProject.ID, *got.ProjectID, "conversation keeps its project")
	assert.Nil(t, got.DefaultAgentID, "conversation default agent unchanged")
	assert.True(t, f.otherConv.LastActivityAt.Equal(got.LastActivityAt), "conversation not touched")
}

// TestBrokerInbound_ExternalRefOfOtherProjectNotReused: a chat thread
// reference that already names a conversation of another project is not
// reused. Both inbound endpoints answer exactly as for any other conversation
// resolution failure, and nothing is delivered or stored.
func TestBrokerInbound_ExternalRefOfOtherProjectNotReused(t *testing.T) {
	t.Run("inbound", func(t *testing.T) {
		f := newExternalRefFixture(t)
		unresolved := f.postLegacyInbound(t, f.surface, "thread:bad")
		require.Equal(t, http.StatusConflict, unresolved.Code, "baseline body: %s", unresolved.Body.String())

		rec := f.postLegacyInbound(t, f.surface, f.ref)
		require.Equal(t, http.StatusConflict, rec.Code, "body: %s", rec.Body.String())
		assert.Equal(t, unresolved.Body.String(), rec.Body.String(), "same answer as an unresolved conversation")
		f.requireNothingDelivered(t)
	})

	t.Run("routed", func(t *testing.T) {
		f := newExternalRefFixture(t)
		unresolved := f.postRoutedInbound(t, f.surface, "thread:bad")
		require.Equal(t, http.StatusConflict, unresolved.Code, "baseline body: %s", unresolved.Body.String())

		rec := f.postRoutedInbound(t, f.surface, f.ref)
		require.Equal(t, http.StatusConflict, rec.Code, "body: %s", rec.Body.String())
		assert.Contains(t, rec.Body.String(), `"status":"conversation_not_resolved"`)
		assert.Equal(t, routedBodyWithoutMessageIDs(t, unresolved), routedBodyWithoutMessageIDs(t, rec),
			"same answer as an unresolved conversation")
		f.requireNothingDelivered(t)
	})
}

// TestBrokerInbound_ExternalRefMismatchRefusedWithWriteDenyOff: with the
// conversation envelope switch off, other resolution failures continue
// without a conversation, but a reference of another project is still
// refused with the same answer.
func TestBrokerInbound_ExternalRefMismatchRefusedWithWriteDenyOff(t *testing.T) {
	t.Run("inbound", func(t *testing.T) {
		f := newExternalRefFixture(t)
		unresolved := f.postLegacyInbound(t, f.surface, "thread:bad")
		require.Equal(t, http.StatusConflict, unresolved.Code)

		disableWriteDenySwitch(t, f.srv)
		rec := f.postLegacyInbound(t, f.surface, f.ref)
		require.Equal(t, http.StatusConflict, rec.Code, "body: %s", rec.Body.String())
		assert.Equal(t, unresolved.Body.String(), rec.Body.String())
		f.requireNothingDelivered(t)
	})

	t.Run("routed", func(t *testing.T) {
		f := newExternalRefFixture(t)
		unresolved := f.postRoutedInbound(t, f.surface, "thread:bad")
		require.Equal(t, http.StatusConflict, unresolved.Code)

		disableWriteDenySwitch(t, f.srv)
		rec := f.postRoutedInbound(t, f.surface, f.ref)
		require.Equal(t, http.StatusConflict, rec.Code, "body: %s", rec.Body.String())
		assert.True(t, strings.Contains(rec.Body.String(), `"status":"conversation_not_resolved"`), "body: %s", rec.Body.String())
		assert.Equal(t, routedBodyWithoutMessageIDs(t, unresolved), routedBodyWithoutMessageIDs(t, rec))
		f.requireNothingDelivered(t)
	})
}
