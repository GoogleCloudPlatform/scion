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
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/eventbus"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func TestParseAgentMessageTopic(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		topic     string
		projectID string
		agentSlug string
		wantErr   bool
	}{
		{
			name:      "valid topic",
			topic:     "scion.project.my-project-123.agent.coder.messages",
			projectID: "my-project-123",
			agentSlug: "coder",
		},
		{
			name:      "valid topic with uuid project",
			topic:     "scion.project.abc-def-123.agent.code-reviewer.messages",
			projectID: "abc-def-123",
			agentSlug: "code-reviewer",
		},
		{
			name:    "legacy grove topic",
			topic:   "scion.grove.my-project-123.agent.coder.messages",
			wantErr: true,
		},
		{
			name:    "too few segments",
			topic:   "scion.project.g1.agent.coder",
			wantErr: true,
		},
		{
			name:    "too many segments",
			topic:   "scion.project.g1.agent.coder.messages.extra",
			wantErr: true,
		},
		{
			name:    "wrong prefix",
			topic:   "other.project.g1.agent.coder.messages",
			wantErr: true,
		},
		{
			name:    "wrong structure",
			topic:   "scion.topic.g1.agent.coder.messages",
			wantErr: true,
		},
		{
			name:    "broadcast topic not agent",
			topic:   "scion.project.g1.broadcast.all.messages",
			wantErr: true,
		},
		{
			name:    "empty string",
			topic:   "",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			projectID, agentSlug, err := parseAgentMessageTopic(tt.topic)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.projectID, projectID)
			assert.Equal(t, tt.agentSlug, agentSlug)
		})
	}
}

func TestHandleBrokerInbound_RejectsNonRunningAgent(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		phase       state.Phase
		wantMessage string
	}{
		{
			name:        "error phase",
			phase:       state.PhaseError,
			wantMessage: "is in error state",
		},
		{
			name:        "stopped phase",
			phase:       state.PhaseStopped,
			wantMessage: "is stopped",
		},
		{
			name:        "suspended phase",
			phase:       state.PhaseSuspended,
			wantMessage: "is suspended",
		},
		{
			name:        "created phase (not yet running)",
			phase:       state.PhaseCreated,
			wantMessage: "is not yet running",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, s := testServer(t)
			ctx := context.Background()

			// Create a normal member user so the sender resolves and passes
			// authorization via project membership + agent's project message mode.
			user := &store.User{
				ID:          tid("user-phase-test"),
				Email:       "phase-test@example.com",
				DisplayName: "Phase Test User",
				Role:        store.UserRoleMember,
				Status:      "active",
				Created:     time.Now(),
			}
			require.NoError(t, s.CreateUser(ctx, user))
			ensureHubMembership(ctx, s, user.ID)

			// Create project with members group and policy.
			project := &store.Project{
				ID:        tid("proj-broker-inbound"),
				Slug:      "broker-inbound-proj",
				Name:      "Broker Inbound Test Project",
				OwnerID:   user.ID,
				CreatedBy: user.ID,
				Created:   time.Now(),
				Updated:   time.Now(),
			}
			require.NoError(t, s.CreateProject(ctx, project))
			srv.seedProjectCreatorMembership(ctx, project)
			msgAuthzAddProjectMember(t, s, user.ID, project.ID, project.Slug, store.GroupMemberRoleMember)

			// Create agent with the specified phase and project message mode
			// so normal project members can message it.
			agent := &store.Agent{
				ID:           tid("agent-errstate"),
				Slug:         "errstate-agent",
				Name:         "Error State Agent",
				ProjectID:    project.ID,
				Phase:        string(tt.phase),
				MessageMode:  store.MessageModeProject,
				StateVersion: 1,
				Created:      time.Now(),
				Updated:      time.Now(),
			}
			require.NoError(t, s.CreateAgent(ctx, agent))

			// Build broker inbound request with a user: sender that resolves.
			topic := "scion.project." + project.ID + ".agent." + agent.Slug + ".messages"
			payload := inboundMessageRequest{
				Topic: topic,
				Message: &messages.StructuredMessage{
					Version:   messages.Version,
					Timestamp: time.Now().UTC().Format(time.RFC3339),
					Channel:   "discord",
					Sender:    "user:" + user.Email,
					Recipient: "agent:" + agent.Slug,
					Msg:       "hello from discord",
					Type:      messages.TypeInstruction,
				},
			}
			body, err := json.Marshal(payload)
			require.NoError(t, err)

			req := httptest.NewRequest(http.MethodPost, "/api/v1/broker/inbound", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			// Inject broker identity directly to bypass HMAC middleware.
			req = req.WithContext(contextWithBrokerIdentity(req.Context(), NewBrokerIdentity("test-broker")))

			rec := httptest.NewRecorder()
			srv.mux.ServeHTTP(rec, req)

			// Verify 409 Conflict with agent_not_running error code.
			assert.Equal(t, http.StatusConflict, rec.Code)

			var errResp ErrorResponse
			require.NoError(t, json.NewDecoder(rec.Body).Decode(&errResp))
			assert.Equal(t, ErrCodeAgentNotRunning, errResp.Error.Code)
			assert.Contains(t, errResp.Error.Message, tt.wantMessage)
		})
	}
}

func TestHandleBrokerInbound_ConversationResolution(t *testing.T) {
	t.Parallel()
	srv, s := testServer(t)
	ctx := context.Background()

	// Create a normal member user so the sender resolves and passes
	// authorization via project membership + agent's project message mode.
	user := &store.User{
		ID:          tid("user-conv-resolve"),
		Email:       "conv-resolve@example.com",
		DisplayName: "Conv Resolve User",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, user))
	ensureHubMembership(ctx, s, user.ID)

	// Create project with members group and policy.
	project := &store.Project{
		ID:        tid("proj-conv-resolve"),
		Slug:      "conv-resolve-proj",
		Name:      "Conversation Resolution Test Project",
		OwnerID:   user.ID,
		CreatedBy: user.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))
	srv.seedProjectCreatorMembership(ctx, project)
	msgAuthzAddProjectMember(t, s, user.ID, project.ID, project.Slug, store.GroupMemberRoleMember)

	// Create a running agent with project message mode.
	agent := &store.Agent{
		ID:           tid("agent-conv-resolve"),
		Slug:         "conv-agent",
		Name:         "Conversation Agent",
		ProjectID:    project.ID,
		Phase:        string(state.PhaseRunning),
		MessageMode:  store.MessageModeProject,
		StateVersion: 1,
		Created:      time.Now(),
		Updated:      time.Now(),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	senderRef := "user:" + user.Email
	topic := "scion.project." + project.ID + ".agent." + agent.Slug + ".messages"

	tests := []struct {
		name        string
		surface     string
		externalRef string
		parentRef   string
		wantStatus  int
		wantConv    bool // expect a conversation to be created
		wantErrMsg  string
	}{
		{
			name:        "discord conversation created",
			surface:     "discord",
			externalRef: "123456789",
			parentRef:   "987654321",
			wantStatus:  http.StatusServiceUnavailable, // no dispatcher in test
			wantConv:    true,
		},
		{
			name:        "slack conversation created",
			surface:     "slack",
			externalRef: "C0123ABC:1234567890.123456",
			parentRef:   "C0123ABC",
			wantStatus:  http.StatusServiceUnavailable,
			wantConv:    true,
		},
		{
			name:        "telegram conversation created",
			surface:     "telegram",
			externalRef: "42",
			parentRef:   "-1001234567890",
			wantStatus:  http.StatusServiceUnavailable,
			wantConv:    true,
		},
		{
			name:        "gchat conversation created",
			surface:     "gchat",
			externalRef: "spaces/AAAA/threads/BBBB",
			parentRef:   "spaces/AAAA",
			wantStatus:  http.StatusServiceUnavailable,
			wantConv:    true,
		},
		{
			name:        "teams conversation created",
			surface:     "teams",
			externalRef: "19:meeting_abc@thread.v2",
			parentRef:   "19:abc@thread.v2",
			wantStatus:  http.StatusServiceUnavailable,
			wantConv:    true,
		},
		{
			name:        "no conversation fields - passes through",
			surface:     "",
			externalRef: "",
			parentRef:   "",
			wantStatus:  http.StatusServiceUnavailable,
			wantConv:    false,
		},
		{
			name:        "AC-8 regression: external_ref without surface rejected",
			surface:     "",
			externalRef: "some-thread-id",
			parentRef:   "",
			wantStatus:  http.StatusBadRequest,
			wantConv:    false,
			wantErrMsg:  "external_ref requires surface to be set",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payload := inboundMessageRequest{
				Topic: topic,
				Message: &messages.StructuredMessage{
					Version:   messages.Version,
					Timestamp: time.Now().UTC().Format(time.RFC3339),
					Channel:   "discord",
					Sender:    senderRef,
					Recipient: "agent:" + agent.Slug,
					Msg:       "hello",
					Type:      messages.TypeInstruction,
				},
				Surface:     tt.surface,
				ExternalRef: tt.externalRef,
				ParentRef:   tt.parentRef,
			}
			body, err := json.Marshal(payload)
			require.NoError(t, err)

			req := httptest.NewRequest(http.MethodPost, "/api/v1/broker/inbound", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req = req.WithContext(contextWithBrokerIdentity(req.Context(), NewBrokerIdentity("test-broker")))

			rec := httptest.NewRecorder()
			srv.mux.ServeHTTP(rec, req)

			assert.Equal(t, tt.wantStatus, rec.Code)

			if tt.wantErrMsg != "" {
				var errResp ErrorResponse
				require.NoError(t, json.NewDecoder(rec.Body).Decode(&errResp))
				assert.Contains(t, errResp.Error.Message, tt.wantErrMsg)
			}

			if tt.wantConv {
				// Verify conversation was created in the store under the canonical
				// thread:<projectID>:<externalRef> key so inbound and outbound share one row.
				wantKey, keyErr := messaging.ThreadConversationExternalRef(project.ID, tt.externalRef)
				require.NoError(t, keyErr)

				convs, err := s.ListConversations(ctx, store.ConversationFilter{
					Surface: tt.surface,
				}, store.ListOptions{Limit: 10})
				require.NoError(t, err)

				found := false
				for _, c := range convs.Items {
					if c.ExternalRef == wantKey && c.Surface == tt.surface {
						found = true
						assert.Equal(t, tt.parentRef, c.ParentRef)
						assert.Equal(t, "group", c.Kind)
						assert.Equal(t, "active", c.DriftState)
						assert.Equal(t, project.ID, *c.ProjectID)
					}
				}
				assert.True(t, found, "expected conversation with surface=%s external_ref=%s to exist", tt.surface, wantKey)
			}
		})
	}
}

// TestHandleBrokerInbound_ConversationResolution_PerPlugin_Regression tests the
// AC-8 regression case for each platform surface: external_ref present with
// empty surface must be rejected at the broker edge.
func TestHandleBrokerInbound_ConversationResolution_PerPlugin_Regression(t *testing.T) {
	t.Parallel()
	surfaces := []struct {
		name        string
		externalRef string
	}{
		{"discord", "123456789012345678"},
		{"slack", "C0123ABC:1234567890.123456"},
		{"telegram", "42"},
		{"gchat", "spaces/AAAA/threads/BBBB"},
		{"teams", "19:meeting_abc@thread.v2"},
	}

	for _, surf := range surfaces {
		t.Run(surf.name+"_missing_surface_rejected", func(t *testing.T) {
			srv, s := testServer(t)
			ctx := context.Background()

			// Create a normal member user so the sender resolves and passes
			// authorization via project membership + agent's project message mode.
			user := &store.User{
				ID:          tid("user-regr-" + surf.name),
				Email:       "regr-" + surf.name + "@example.com",
				DisplayName: "Regression User",
				Role:        store.UserRoleMember,
				Status:      "active",
				Created:     time.Now(),
			}
			require.NoError(t, s.CreateUser(ctx, user))
			ensureHubMembership(ctx, s, user.ID)

			project := &store.Project{
				ID:        tid("proj-regr-" + surf.name),
				Slug:      "regr-" + surf.name,
				Name:      "Regression " + surf.name,
				OwnerID:   user.ID,
				CreatedBy: user.ID,
				Created:   time.Now(),
				Updated:   time.Now(),
			}
			require.NoError(t, s.CreateProject(ctx, project))
			srv.seedProjectCreatorMembership(ctx, project)
			msgAuthzAddProjectMember(t, s, user.ID, project.ID, project.Slug, store.GroupMemberRoleMember)

			agent := &store.Agent{
				ID:           tid("agent-regr-" + surf.name),
				Slug:         "regr-agent-" + surf.name,
				Name:         "Regression Agent",
				ProjectID:    project.ID,
				Phase:        string(state.PhaseRunning),
				MessageMode:  store.MessageModeProject,
				StateVersion: 1,
				Created:      time.Now(),
				Updated:      time.Now(),
			}
			require.NoError(t, s.CreateAgent(ctx, agent))

			topic := "scion.project." + project.ID + ".agent." + agent.Slug + ".messages"
			payload := inboundMessageRequest{
				Topic: topic,
				Message: &messages.StructuredMessage{
					Version:   messages.Version,
					Timestamp: time.Now().UTC().Format(time.RFC3339),
					Channel:   surf.name,
					Sender:    "user:" + user.Email,
					Recipient: "agent:" + agent.Slug,
					Msg:       "regression test",
					Type:      messages.TypeInstruction,
				},
				// Surface intentionally omitted — only ExternalRef set.
				ExternalRef: surf.externalRef,
			}
			body, err := json.Marshal(payload)
			require.NoError(t, err)

			req := httptest.NewRequest(http.MethodPost, "/api/v1/broker/inbound", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req = req.WithContext(contextWithBrokerIdentity(req.Context(), NewBrokerIdentity("test-broker")))

			rec := httptest.NewRecorder()
			srv.mux.ServeHTTP(rec, req)

			assert.Equal(t, http.StatusBadRequest, rec.Code)
			var errResp ErrorResponse
			require.NoError(t, json.NewDecoder(rec.Body).Decode(&errResp))
			assert.Contains(t, errResp.Error.Message, "external_ref requires surface to be set")
		})
	}
}

func TestHandleBrokerInbound_AllowsRunningAgent(t *testing.T) {
	t.Parallel()
	srv, s := testServer(t)
	ctx := context.Background()

	// Create a normal member user so the sender resolves and passes
	// authorization via project membership + agent's project message mode.
	user := &store.User{
		ID:          tid("user-running-test"),
		Email:       "running-test@example.com",
		DisplayName: "Running Test User",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, user))
	ensureHubMembership(ctx, s, user.ID)

	// Create project with members group and policy.
	project := &store.Project{
		ID:        tid("proj-broker-running"),
		Slug:      "broker-running-proj",
		Name:      "Broker Running Test Project",
		OwnerID:   user.ID,
		CreatedBy: user.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))
	srv.seedProjectCreatorMembership(ctx, project)
	msgAuthzAddProjectMember(t, s, user.ID, project.ID, project.Slug, store.GroupMemberRoleMember)

	// Create a running agent with project message mode.
	agent := &store.Agent{
		ID:           tid("agent-running"),
		Slug:         "running-agent",
		Name:         "Running Agent",
		ProjectID:    project.ID,
		Phase:        string(state.PhaseRunning),
		MessageMode:  store.MessageModeProject,
		StateVersion: 1,
		Created:      time.Now(),
		Updated:      time.Now(),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	// Build broker inbound request with a user: sender that resolves.
	topic := "scion.project." + project.ID + ".agent." + agent.Slug + ".messages"
	payload := inboundMessageRequest{
		Topic: topic,
		Message: &messages.StructuredMessage{
			Version:   messages.Version,
			Timestamp: time.Now().UTC().Format(time.RFC3339),
			Channel:   "discord",
			Sender:    "user:" + user.Email,
			Recipient: "agent:" + agent.Slug,
			Msg:       "hello",
			Type:      messages.TypeInstruction,
		},
	}
	body, err := json.Marshal(payload)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/broker/inbound", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithBrokerIdentity(req.Context(), NewBrokerIdentity("test-broker")))

	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)

	// Running agent passes the phase check but gets 503 because no
	// dispatcher is configured in the test.
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

// TestHandleBrokerInbound_AgentSenderDenied verifies that agent-prefixed
// senders are denied.
func TestHandleBrokerInbound_AgentSenderDenied(t *testing.T) {
	t.Parallel()
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID:      tid("proj-agent-denied"),
		Slug:    "agent-denied-proj",
		Name:    "Agent Denied Test",
		Created: time.Now(),
		Updated: time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))

	agent := &store.Agent{
		ID:           tid("agent-denied-target"),
		Slug:         "denied-agent",
		Name:         "Denied Agent",
		ProjectID:    project.ID,
		Phase:        string(state.PhaseRunning),
		StateVersion: 1,
		Created:      time.Now(),
		Updated:      time.Now(),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	topic := "scion.project." + project.ID + ".agent." + agent.Slug + ".messages"
	payload := inboundMessageRequest{
		Topic: topic,
		Message: &messages.StructuredMessage{
			Version:   messages.Version,
			Timestamp: time.Now().UTC().Format(time.RFC3339),
			Channel:   "discord",
			Sender:    "agent:some-other-agent",
			Recipient: "agent:" + agent.Slug,
			Msg:       "hello from agent",
			Type:      messages.TypeInstruction,
		},
	}
	body, err := json.Marshal(payload)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/broker/inbound", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithBrokerIdentity(req.Context(), NewBrokerIdentity("test-broker")))

	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)

	// Agent-prefixed senders are denied.
	assert.Equal(t, http.StatusForbidden, rec.Code)

	var errResp ErrorResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&errResp))
	assert.Equal(t, ErrCodeMessageDenied, errResp.Error.Code)
}

// TestHandleBrokerInbound_ConvResolutionFailure_WriteDenyOff verifies that when
// conversation resolution fails and write-deny is OFF (the default), the handler
// completes without panicking and the message still dispatches (fail-open).
// TestHandleBrokerInbound_UnmappedExternalSenderDenied verifies that an
// unmapped external-channel sender (e.g. "discord:someuser") is denied.
func TestHandleBrokerInbound_UnmappedExternalSenderDenied(t *testing.T) {
	t.Parallel()
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID:      tid("proj-unmapped"),
		Slug:    "unmapped-proj",
		Name:    "Unmapped Sender Test",
		Created: time.Now(),
		Updated: time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))

	agent := &store.Agent{
		ID:           tid("agent-unmapped"),
		Slug:         "unmapped-agent",
		Name:         "Unmapped Agent",
		ProjectID:    project.ID,
		Phase:        string(state.PhaseRunning),
		StateVersion: 1,
		Created:      time.Now(),
		Updated:      time.Now(),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	topic := "scion.project." + project.ID + ".agent." + agent.Slug + ".messages"
	payload := inboundMessageRequest{
		Topic: topic,
		Message: &messages.StructuredMessage{
			Version:   messages.Version,
			Timestamp: time.Now().UTC().Format(time.RFC3339),
			Channel:   "discord",
			Sender:    "discord:someuser",
			Recipient: "agent:" + agent.Slug,
			Msg:       "hello from unmapped user",
			Type:      messages.TypeInstruction,
		},
	}
	body, err := json.Marshal(payload)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/broker/inbound", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithBrokerIdentity(req.Context(), NewBrokerIdentity("test-broker")))

	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusForbidden, rec.Code)

	var errResp ErrorResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&errResp))
	assert.Equal(t, ErrCodeMessageDenied, errResp.Error.Code)
}

// TestHandleBrokerInbound_CrossChannelDM_AdvancesSenderReadState: a human
// posting into a web-visible agent DM from a non-web channel (e.g. Discord)
// must not see their own message as unread when they later open that same
// DM in native web chat. The v2 DM last-message watermark and the sender's
// v2 read state must both advance — the legacy Wave-1 TouchThread watermark
// is a different mechanism and does not feed the v2 unread computation
// (LastMessageID != LastReadMessageID).
func TestHandleBrokerInbound_CrossChannelDM_AdvancesSenderReadState(t *testing.T) {
	t.Parallel()
	srv, s := testServer(t)
	ctx := context.Background()

	db := openTestMemorySQLite(t, "sqlite")
	wcs := NewWebChatStore(db, "sqlite")
	require.NoError(t, wcs.Init())
	srv.SetWebChatStore(wcs)
	srv.SetDispatcher(&brokerMockDispatcher{})

	// Assert the watermarks at the exact moment PublishUserMessage is
	// called, not after the handler returns: asserting only after-the-fact
	// cannot distinguish "advanced before publish" (correct — the sender's
	// own client refetches unread state on this event) from "advanced after
	// publish" (both leave the same final state).
	spy := &readStateAtPublishSpy{wcs: wcs}
	srv.SetEventPublisher(spy)

	user := &store.User{
		ID:          tid("user-xchan-dm"),
		Email:       "xchan-dm@example.com",
		DisplayName: "Cross Channel User",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, user))
	ensureHubMembership(ctx, s, user.ID)

	project := &store.Project{
		ID:        tid("proj-xchan-dm"),
		Slug:      "xchan-dm-proj",
		Name:      "Cross Channel DM Test Project",
		OwnerID:   user.ID,
		CreatedBy: user.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))
	srv.seedProjectCreatorMembership(ctx, project)
	msgAuthzAddProjectMember(t, s, user.ID, project.ID, project.Slug, store.GroupMemberRoleMember)

	agent := &store.Agent{
		ID:           tid("agent-xchan-dm"),
		Slug:         "xchan-agent",
		Name:         "Cross Channel Agent",
		ProjectID:    project.ID,
		Phase:        string(state.PhaseRunning),
		MessageMode:  store.MessageModeProject,
		StateVersion: 1,
		Created:      time.Now(),
		Updated:      time.Now(),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	// The web DM key for this agent+user pair — the same key native web chat
	// uses to view this conversation and to compute its unread state.
	dmKey := "dm:agent:" + agent.ID + ":user:" + user.ID

	topic := "scion.project." + project.ID + ".agent." + agent.Slug + ".messages"
	payload := inboundMessageRequest{
		Topic: topic,
		Message: &messages.StructuredMessage{
			Version:   messages.Version,
			Timestamp: time.Now().UTC().Format(time.RFC3339),
			Channel:   "discord",
			Sender:    "user:" + user.Email,
			Recipient: "agent:" + agent.Slug,
			Msg:       "hello from discord",
			Type:      messages.TypeInstruction,
			ThreadID:  dmKey,
		},
	}
	body, err := json.Marshal(payload)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/broker/inbound", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithBrokerIdentity(req.Context(), NewBrokerIdentity("test-broker")))

	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	// Find the persisted message to get its server-assigned ID.
	msgs, err := s.ListMessages(ctx, store.MessageFilter{ThreadID: dmKey}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	require.Len(t, msgs.Items, 1, "expected exactly one persisted message for the DM key")
	persisted := msgs.Items[0]
	assert.Equal(t, user.ID, persisted.SenderID)

	// The v2 DM watermark must already point at this message...
	dms, err := wcs.ListDMs(ctx, user.ID)
	require.NoError(t, err)
	require.Len(t, dms, 1, "expected the DM registry row to exist for the sender")
	assert.Equal(t, persisted.ID, dms[0].LastMessageID,
		"DM last-message watermark was not advanced for the cross-channel sender")

	// ...and the sender's own v2 read state must already be caught up to it,
	// so native web chat never renders this DM as unread for the person who
	// just sent it (albeit from Discord).
	rs, err := wcs.GetReadState(ctx, user.ID, dmKey)
	require.NoError(t, err)
	require.NotNil(t, rs, "sender read state was not set for the cross-channel message")
	assert.Equal(t, persisted.ID, rs.LastReadMessageID,
		"sender read watermark was not advanced for the cross-channel message — would show unread in web chat")

	// The load-bearing assertion: both watermarks were already current at
	// the moment of publish, not merely by the time the test got around to
	// checking after the handler returned.
	require.True(t, spy.called, "PublishUserMessage was never called")
	assert.Equal(t, persisted.ID, spy.lastMessageIDAtPublish,
		"DM last-message watermark at publish time = %q, want %q", spy.lastMessageIDAtPublish, persisted.ID)
	assert.False(t, spy.hasUnreadAtPublish,
		"computed hasUnread at publish time = true, want false — cross-channel self-send would flash/stick unread")
}

// TestHandleBrokerInbound_MentionCoAddressees verifies that when a broker
// plugin sends mention_co_addressees metadata, the hub reads it and applies
// CoAddressees/IsMention to the delivery envelope (additive mention routing).
func TestHandleBrokerInbound_MentionCoAddressees(t *testing.T) {
	t.Parallel()
	srv, s := testServer(t)
	ctx := context.Background()

	// Wire a recording dispatcher so the handler can complete the full
	// dispatch→persist→200 path instead of returning 503.
	dispatcher := &recordingDispatcher{}
	srv.SetDispatcher(dispatcher)

	// Enable the conversation envelope switch so the handler enters the
	// RenderDeliveryText path and populates DeliveryText with co-addressee
	// attribution (writeDenyEnabled() must return true).
	enableWriteDenySwitch(t, srv)

	// Separate owner and sender to avoid built-in membership conflicts.
	owner := &store.User{
		ID:          tid("owner-co-addr"),
		Email:       "co-addr-owner@example.com",
		DisplayName: "CoAddr Owner",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, owner))
	ensureHubMembership(ctx, s, owner.ID)

	user := &store.User{
		ID:          tid("user-co-addr"),
		Email:       "co-addr@example.com",
		DisplayName: "CoAddr User",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, user))
	ensureHubMembership(ctx, s, user.ID)

	project := &store.Project{
		ID:        tid("proj-co-addr"),
		Slug:      "co-addr-proj",
		Name:      "CoAddr Test Project",
		OwnerID:   owner.ID,
		CreatedBy: owner.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))
	srv.seedProjectCreatorMembership(ctx, project)
	msgAuthzAddProjectMember(t, s, user.ID, project.ID, project.Slug, store.GroupMemberRoleMember)
	// project-member role does not include agent.message (removed in R3);
	// grant it explicitly so the co-addressee sender is authorized.
	msgAuthzGrantAgentMessage(t, s, user.ID, project.ID)

	// Create two agents: primary (coder) and secondary (reviewer).
	primaryAgent := &store.Agent{
		ID:           tid("agent-co-primary"),
		Slug:         "coder",
		Name:         "Coder Agent",
		ProjectID:    project.ID,
		Phase:        string(state.PhaseRunning),
		MessageMode:  store.MessageModeProject,
		StateVersion: 1,
		Created:      time.Now(),
		Updated:      time.Now(),
	}
	require.NoError(t, s.CreateAgent(ctx, primaryAgent))

	secondaryAgent := &store.Agent{
		ID:           tid("agent-co-secondary"),
		Slug:         "reviewer",
		Name:         "Reviewer Agent",
		ProjectID:    project.ID,
		Phase:        string(state.PhaseRunning),
		MessageMode:  store.MessageModeProject,
		StateVersion: 1,
		Created:      time.Now(),
		Updated:      time.Now(),
	}
	require.NoError(t, s.CreateAgent(ctx, secondaryAgent))

	senderRef := "user:" + user.Email

	t.Run("primary with co-addressees accepted", func(t *testing.T) {
		topic := "scion.project." + project.ID + ".agent." + primaryAgent.Slug + ".messages"
		coAddressees, _ := json.Marshal([]string{"coder", "reviewer"})

		payload := inboundMessageRequest{
			Topic: topic,
			Message: &messages.StructuredMessage{
				Version:   messages.Version,
				Timestamp: time.Now().UTC().Format(time.RFC3339),
				Channel:   "discord",
				Sender:    senderRef,
				Recipient: "agent:" + primaryAgent.Slug,
				Msg:       "check this PR",
				Type:      messages.TypeInstruction,
				Metadata: map[string]string{
					"discord_channel_id":    "ch-123",
					"project_id":            project.ID,
					"mention_co_addressees": string(coAddressees),
				},
			},
		}
		body, err := json.Marshal(payload)
		require.NoError(t, err)

		req := httptest.NewRequest(http.MethodPost, "/api/v1/broker/inbound", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req = req.WithContext(contextWithBrokerIdentity(req.Context(), NewBrokerIdentity("test-broker")))

		rec := httptest.NewRecorder()
		srv.mux.ServeHTTP(rec, req)

		// ── Status: full 200 success (dispatch + persist completed). ──
		require.Equal(t, http.StatusOK, rec.Code,
			"primary with co-addressees should succeed; got %d: %s", rec.Code, rec.Body.String())

		// Verify response body confirms delivery to the primary agent.
		var resp map[string]interface{}
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
		assert.Equal(t, true, resp["delivered"], "response should confirm delivery")
		assert.Equal(t, primaryAgent.ID, resp["agentId"], "response should reference primary agent")

		// ── Dispatch assertions: the dispatcher received the message. ──
		calls := dispatcher.getCalls()
		require.NotEmpty(t, calls, "dispatcher should have recorded at least one call")
		lastCall := calls[len(calls)-1]
		assert.Equal(t, primaryAgent.Slug, lastCall.Agent.Slug,
			"dispatch target should be the primary agent (coder)")
		assert.Equal(t, "check this PR", lastCall.Message,
			"dispatched message body should match")

		// The structured message should carry co-addressee rendering in
		// DeliveryText (rendered by RenderDeliveryText when the envelope
		// switch is ON — enabled above via enableWriteDenySwitch).
		require.NotNil(t, lastCall.StructuredMessage,
			"dispatcher should receive a non-nil StructuredMessage")
		require.NotEmpty(t, lastCall.StructuredMessage.DeliveryText,
			"DeliveryText must be populated — envelope switch is ON")

		// Structurally parse the delivery envelope JSON to verify
		// co-addressee attribution — not substring matching.
		primaryEnv := parseCoAddrEnvelope(t, lastCall.StructuredMessage.DeliveryText)
		assert.ElementsMatch(t, []string{"agent:coder", "agent:reviewer"}, primaryEnv.To,
			"envelope To should list exactly the two co-addressees")
		assert.Equal(t, "message", primaryEnv.Type,
			"primary (instruction) envelope type should be 'message'")

		// ── Persistence: the message was stored. ──
		msgs, err := s.ListMessages(ctx, store.MessageFilter{
			AgentID: primaryAgent.ID,
		}, store.ListOptions{Limit: 10})
		require.NoError(t, err)
		require.NotEmpty(t, msgs.Items, "message should be persisted in the store")
		persisted := msgs.Items[0]
		assert.Equal(t, senderRef, persisted.Sender, "persisted sender should match")
		assert.Equal(t, "agent:"+primaryAgent.Slug, persisted.Recipient, "persisted recipient should match")
		assert.Equal(t, messages.TypeInstruction, persisted.Type, "persisted type should be instruction")
		assert.Equal(t, "discord", persisted.Channel, "persisted channel should match")
	})

	t.Run("mention with co-addressees accepted", func(t *testing.T) {
		topic := "scion.project." + project.ID + ".agent." + secondaryAgent.Slug + ".messages"
		coAddressees, _ := json.Marshal([]string{"coder", "reviewer"})

		// Record dispatcher call count before this subtest.
		callsBefore := len(dispatcher.getCalls())

		payload := inboundMessageRequest{
			Topic: topic,
			Message: &messages.StructuredMessage{
				Version:   messages.Version,
				Timestamp: time.Now().UTC().Format(time.RFC3339),
				Channel:   "discord",
				Sender:    senderRef,
				Recipient: "agent:" + secondaryAgent.Slug,
				Msg:       "check this PR",
				Type:      messages.TypeMention,
				Metadata: map[string]string{
					"discord_channel_id":    "ch-123",
					"project_id":            project.ID,
					"mention_co_addressees": string(coAddressees),
					"mention_source":        "agent:coder",
					"mention_position":      "body",
				},
			},
		}
		body, err := json.Marshal(payload)
		require.NoError(t, err)

		req := httptest.NewRequest(http.MethodPost, "/api/v1/broker/inbound", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req = req.WithContext(contextWithBrokerIdentity(req.Context(), NewBrokerIdentity("test-broker")))

		rec := httptest.NewRecorder()
		srv.mux.ServeHTTP(rec, req)

		// ── Status: full 200 success. ──
		require.Equal(t, http.StatusOK, rec.Code,
			"mention with co-addressees should succeed; got %d: %s", rec.Code, rec.Body.String())

		// Verify response body confirms delivery to the secondary agent.
		var resp map[string]interface{}
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
		assert.Equal(t, true, resp["delivered"], "response should confirm delivery")
		assert.Equal(t, secondaryAgent.ID, resp["agentId"], "response should reference secondary agent (reviewer)")

		// ── Dispatch assertions: the dispatcher received the mention. ──
		callsAfter := dispatcher.getCalls()
		require.Greater(t, len(callsAfter), callsBefore,
			"dispatcher should have recorded a new call for the mention")
		mentionCall := callsAfter[len(callsAfter)-1]
		assert.Equal(t, secondaryAgent.Slug, mentionCall.Agent.Slug,
			"dispatch target should be the secondary agent (reviewer)")

		// The structured message should carry mention-type co-addressee
		// rendering with both agents listed in the delivery envelope.
		require.NotNil(t, mentionCall.StructuredMessage,
			"dispatcher should receive a non-nil StructuredMessage")
		require.NotEmpty(t, mentionCall.StructuredMessage.DeliveryText,
			"DeliveryText must be populated — envelope switch is ON")

		// Structurally parse the delivery envelope JSON.
		mentionEnv := parseCoAddrEnvelope(t, mentionCall.StructuredMessage.DeliveryText)
		assert.ElementsMatch(t, []string{"agent:coder", "agent:reviewer"}, mentionEnv.To,
			"mention envelope To should list exactly the two co-addressees")
		// Mention-type messages set IsMention=true in the render input,
		// which changes the envelope type to "mention".
		assert.Equal(t, "mention", mentionEnv.Type,
			"mention envelope type should be 'mention', not 'message'")

		// ── Persistence: the mention message was stored. ──
		msgs, err := s.ListMessages(ctx, store.MessageFilter{
			AgentID: secondaryAgent.ID,
		}, store.ListOptions{Limit: 10})
		require.NoError(t, err)
		require.NotEmpty(t, msgs.Items, "mention message should be persisted in the store")
		persisted := msgs.Items[0]
		assert.Equal(t, senderRef, persisted.Sender, "persisted sender should match")
		assert.Equal(t, "agent:"+secondaryAgent.Slug, persisted.Recipient, "persisted recipient should match")
		assert.Equal(t, messages.TypeMention, persisted.Type, "persisted type should be mention")
		assert.Equal(t, "discord", persisted.Channel, "persisted channel should match")
	})
}

// coAddrEnvelope is a minimal struct for structurally verifying co-addressee
// attribution in DeliveryText. Mirrors the json tags of
// messaging.DeliveryEnvelope without importing the full type.
// Reuses extractEnvelopeJSON from handlers_outbound_def171_test.go.
type coAddrEnvelope struct {
	To   []string `json:"to"`
	Type string   `json:"type"`
}

// parseCoAddrEnvelope extracts and parses the JSON body from a rendered
// DeliveryText using the shared extractEnvelopeJSON helper.
func parseCoAddrEnvelope(t *testing.T, deliveryText string) coAddrEnvelope {
	t.Helper()
	jsonStr := extractEnvelopeJSON(t, deliveryText)
	var env coAddrEnvelope
	require.NoError(t, json.Unmarshal([]byte(jsonStr), &env),
		"failed to unmarshal envelope JSON: %s", jsonStr)
	return env
}

func TestHandleBrokerInbound_SurfaceExternalRefWithoutLegacyFields(t *testing.T) {
	t.Parallel()
	srv, s := testServer(t)
	ctx := context.Background()

	dispatcher := &recordingDispatcher{}
	srv.SetDispatcher(dispatcher)

	user := &store.User{
		ID:          tid("user-p4-surface"),
		Email:       "p4-surface@example.com",
		DisplayName: "P4 Surface User",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, user))
	ensureHubMembership(ctx, s, user.ID)

	project := &store.Project{
		ID:        tid("proj-p4-surface"),
		Slug:      "p4-surface",
		Name:      "P4 Surface",
		OwnerID:   user.ID,
		CreatedBy: user.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))
	srv.seedProjectCreatorMembership(ctx, project)
	msgAuthzAddProjectMember(t, s, user.ID, project.ID, project.Slug, store.GroupMemberRoleMember)

	agent := &store.Agent{
		ID:           tid("agent-p4-surface"),
		Slug:         "coder",
		Name:         "Coder",
		ProjectID:    project.ID,
		Phase:        string(state.PhaseRunning),
		MessageMode:  store.MessageModeProject,
		StateVersion: 1,
		Created:      time.Now(),
		Updated:      time.Now(),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	// Cut-over plugin sends top-level surface/external_ref/parent_ref/display_name
	// with NO legacy Channel or ThreadID on Message.
	payload := inboundMessageRequest{
		Topic:       "scion.project." + project.ID + ".agent." + agent.Slug + ".messages",
		Surface:     "slack",
		ExternalRef: "C0123ABC:1700000000.000100",
		ParentRef:   "C0123ABC",
		DisplayName: "#engineering",
		Message: &messages.StructuredMessage{
			Version:   messages.Version,
			Timestamp: time.Now().UTC().Format(time.RFC3339),
			Sender:    "user:" + user.Email,
			Recipient: "agent:" + agent.Slug,
			Msg:       "hello from cut-over slack plugin",
			Type:      messages.TypeInstruction,
		},
	}
	body, err := json.Marshal(payload)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/broker/inbound", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithBrokerIdentity(req.Context(), NewBrokerIdentity("test-broker")))

	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	// Conversation was created under the canonical thread:<project>:<external_ref>
	// key with surface, parent_ref, and display_name.
	wantExtRef, err := messaging.ThreadConversationExternalRef(project.ID, "C0123ABC:1700000000.000100")
	require.NoError(t, err)
	conv, err := s.GetConversationByExternalRef(ctx, "slack", wantExtRef)
	require.NoError(t, err)
	assert.Equal(t, "C0123ABC", conv.ParentRef)
	assert.Equal(t, "#engineering", conv.DisplayName)
	assert.Equal(t, "group", conv.Kind)

	// No spurious Phase 5 DM conversation was minted.
	allConvs, err := s.ListConversations(ctx, store.ConversationFilter{}, store.ListOptions{Limit: 20})
	require.NoError(t, err)
	assert.Len(t, allConvs.Items, 1, "must not create a spurious DM conversation when Phase 11 resolves")

	// Persisted message carries the resolved ConversationID and derived Channel.
	msgs, err := s.ListMessages(ctx, store.MessageFilter{AgentID: agent.ID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	require.Len(t, msgs.Items, 1)
	assert.Equal(t, conv.ID, msgs.Items[0].ConversationID)
	assert.Equal(t, "slack", msgs.Items[0].Channel)
}

func TestHandleBrokerInbound_LegacyChannelThreadIDOnly(t *testing.T) {
	t.Parallel()
	srv, s := testServer(t)
	ctx := context.Background()

	dispatcher := &recordingDispatcher{}
	srv.SetDispatcher(dispatcher)

	user := &store.User{
		ID:          tid("user-p4-legacy"),
		Email:       "p4-legacy@example.com",
		DisplayName: "P4 Legacy User",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, user))
	ensureHubMembership(ctx, s, user.ID)

	project := &store.Project{
		ID:        tid("proj-p4-legacy"),
		Slug:      "p4-legacy",
		Name:      "P4 Legacy",
		OwnerID:   user.ID,
		CreatedBy: user.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))
	srv.seedProjectCreatorMembership(ctx, project)
	msgAuthzAddProjectMember(t, s, user.ID, project.ID, project.Slug, store.GroupMemberRoleMember)

	agent := &store.Agent{
		ID:           tid("agent-p4-legacy"),
		Slug:         "coder",
		Name:         "Coder",
		ProjectID:    project.ID,
		Phase:        string(state.PhaseRunning),
		MessageMode:  store.MessageModeProject,
		StateVersion: 1,
		Created:      time.Now(),
		Updated:      time.Now(),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	// Old plugin binary sends only legacy Channel + ThreadID (no Surface/ExternalRef).
	payload := inboundMessageRequest{
		Topic: "scion.project." + project.ID + ".agent." + agent.Slug + ".messages",
		Message: &messages.StructuredMessage{
			Version:   messages.Version,
			Timestamp: time.Now().UTC().Format(time.RFC3339),
			Channel:   "slack",
			ThreadID:  "C9999:1700000001.000200",
			Sender:    "user:" + user.Email,
			Recipient: "agent:" + agent.Slug,
			Msg:       "hello from legacy plugin binary",
			Type:      messages.TypeInstruction,
		},
	}
	body, err := json.Marshal(payload)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/broker/inbound", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithBrokerIdentity(req.Context(), NewBrokerIdentity("test-broker")))

	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	msgs, err := s.ListMessages(ctx, store.MessageFilter{AgentID: agent.ID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	require.Len(t, msgs.Items, 1)
	assert.NotEmpty(t, msgs.Items[0].ConversationID)
	assert.Equal(t, "slack", msgs.Items[0].Channel)
	assert.Equal(t, "C9999:1700000001.000200", msgs.Items[0].ThreadID)
}

func TestHandleBrokerInbound_ParentRefWithoutExternalRef(t *testing.T) {
	t.Parallel()
	srv, s := testServer(t)
	ctx := context.Background()

	user := &store.User{
		ID:          tid("user-p4-parent"),
		Email:       "p4-parent@example.com",
		DisplayName: "P4 Parent User",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, user))
	ensureHubMembership(ctx, s, user.ID)

	project := &store.Project{
		ID:        tid("proj-p4-parent"),
		Slug:      "p4-parent",
		Name:      "P4 Parent",
		OwnerID:   user.ID,
		CreatedBy: user.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))
	srv.seedProjectCreatorMembership(ctx, project)
	msgAuthzAddProjectMember(t, s, user.ID, project.ID, project.Slug, store.GroupMemberRoleMember)

	agent := &store.Agent{
		ID:           tid("agent-p4-parent"),
		Slug:         "coder",
		Name:         "Coder",
		ProjectID:    project.ID,
		Phase:        string(state.PhaseRunning),
		MessageMode:  store.MessageModeProject,
		StateVersion: 1,
		Created:      time.Now(),
		Updated:      time.Now(),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	payload := inboundMessageRequest{
		Topic:     "scion.project." + project.ID + ".agent." + agent.Slug + ".messages",
		Surface:   "slack",
		ParentRef: "C0123ABC",
		Message: &messages.StructuredMessage{
			Version:   messages.Version,
			Timestamp: time.Now().UTC().Format(time.RFC3339),
			Sender:    "user:" + user.Email,
			Recipient: "agent:" + agent.Slug,
			Msg:       "missing external_ref",
			Type:      messages.TypeInstruction,
		},
	}
	body, err := json.Marshal(payload)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/broker/inbound", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithBrokerIdentity(req.Context(), NewBrokerIdentity("test-broker")))

	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestHandleAgentMessage_StripsClientSuppliedConversation(t *testing.T) {
	t.Parallel()
	srv, s := testServer(t)
	ctx := context.Background()

	dispatcher := &recordingDispatcher{}
	srv.SetDispatcher(dispatcher)

	user := &store.User{
		ID:          tid("user-p4-strip-conv"),
		Email:       "p4-strip-conv@example.com",
		DisplayName: "P4 Strip Conv User",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, user))
	ensureHubMembership(ctx, s, user.ID)

	project := &store.Project{
		ID:        tid("proj-p4-strip-conv"),
		Slug:      "p4-strip-conv",
		Name:      "P4 Strip Conv",
		OwnerID:   user.ID,
		CreatedBy: user.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))
	srv.seedProjectCreatorMembership(ctx, project)
	msgAuthzAddProjectMember(t, s, user.ID, project.ID, project.Slug, store.GroupMemberRoleMember)

	brokerID := tid("broker-p4-strip-conv")
	require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID: brokerID, Name: "p4-strip-conv-broker", Slug: "p4-strip-conv-broker",
		Status: store.BrokerStatusOnline,
	}))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: project.ID, BrokerID: brokerID,
		BrokerName: "p4-strip-conv-broker", Status: store.BrokerStatusOnline,
	}))

	agent := &store.Agent{
		ID:              tid("agent-p4-strip-conv"),
		Slug:            "coder",
		Name:            "Coder",
		ProjectID:       project.ID,
		RuntimeBrokerID: brokerID,
		Phase:           string(state.PhaseRunning),
		MessageMode:     store.MessageModeProject,
		StateVersion:    1,
		Created:         time.Now(),
		Updated:         time.Now(),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	// A user caller attempts to spoof structured_message.conversation on
	// POST /api/v1/agents/{id}/message. The hub must strip it before dispatch.
	reqBody := MessageRequest{
		StructuredMessage: &messages.StructuredMessage{
			Version:   messages.Version,
			Timestamp: time.Now().UTC().Format(time.RFC3339),
			Sender:    "user:" + user.Email,
			Recipient: "agent:" + agent.Slug,
			Msg:       "attempt spoofed conversation",
			Type:      messages.TypeInstruction,
			Conversation: &messages.Conversation{
				Surface:     "slack",
				ExternalRef: "C_SPOOFED:999",
				ParentRef:   "C_SPOOFED",
			},
		},
	}
	body, err := json.Marshal(reqBody)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+agent.ID+"/message", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(), NewAuthenticatedUser(user.ID, user.Email, user.DisplayName, user.Role, "test")))

	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	calls := dispatcher.getCalls()
	require.Len(t, calls, 1)
	require.NotNil(t, calls[0].StructuredMessage)
	assert.Nil(t, calls[0].StructuredMessage.Conversation, "client-supplied Conversation must be stripped on HTTP ingress")
}

// TestHandleBrokerInbound_R1_InboundAndOutboundShareConversation verifies that
// inbound broker messages and real agent outbound replies (via
// handleAgentOutboundMessage / POST /api/v1/agents/{id}/outbound-message)
// converge on the same conversation_id:
//  1. A new inbound Discord thread message followed by the agent's outbound
//     `--channel discord --thread-id <snowflake>` reply AND a `conv:<id>`
//     conversation-ref reply all resolve to the same conversation_id.
//  2. An inbound Telegram private-chat message (no thread, omitting
//     surface/external_ref) followed by the agent's outbound `user:<email>`
//     reply resolves to the same direct dm: conversation_id.
func TestHandleBrokerInbound_R1_InboundAndOutboundShareConversation(t *testing.T) {
	t.Parallel()

	postInbound := func(t *testing.T, srv *Server, payload inboundMessageRequest) {
		t.Helper()
		body, err := json.Marshal(payload)
		require.NoError(t, err)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/broker/inbound", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req = req.WithContext(contextWithBrokerIdentity(req.Context(), NewBrokerIdentity("test-broker")))
		rec := httptest.NewRecorder()
		srv.mux.ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	}

	setupEnv := func(t *testing.T, suffix string, channels ...string) (*Server, store.Store, *store.User, *store.Project, *store.Agent) {
		t.Helper()
		srv, s := testServer(t)
		ctx := context.Background()
		srv.SetDispatcher(&recordingDispatcher{})

		user := &store.User{
			ID:          tid("user-r1-" + suffix),
			Email:       "r1-" + suffix + "@example.com",
			DisplayName: "R1 User",
			Role:        store.UserRoleMember,
			Status:      "active",
			Created:     time.Now(),
		}
		require.NoError(t, s.CreateUser(ctx, user))
		ensureHubMembership(ctx, s, user.ID)

		project := &store.Project{
			ID:        tid("proj-r1-" + suffix),
			Slug:      "r1-" + suffix,
			Name:      "R1 " + suffix,
			OwnerID:   user.ID,
			CreatedBy: user.ID,
			Created:   time.Now(),
			Updated:   time.Now(),
		}
		require.NoError(t, s.CreateProject(ctx, project))
		srv.seedProjectCreatorMembership(ctx, project)
		msgAuthzAddProjectMember(t, s, user.ID, project.ID, project.Slug, store.GroupMemberRoleMember)

		agent := &store.Agent{
			ID:           tid("agent-r1-" + suffix),
			Slug:         "coder",
			Name:         "Coder",
			ProjectID:    project.ID,
			Phase:        string(state.PhaseRunning),
			MessageMode:  store.MessageModeProject,
			StateVersion: 1,
			Created:      time.Now(),
			Updated:      time.Now(),
		}
		require.NoError(t, s.CreateAgent(ctx, agent))
		if len(channels) > 0 {
			setupThreadTestChannels(t, srv, s, project, channels...)
		}
		return srv, s, user, project, agent
	}

	t.Run("new_discord_thread_shares_conversation_with_real_outbound_replies", func(t *testing.T) {
		srv, s, user, project, agent := setupEnv(t, "discord-new", "discord")
		ctx := context.Background()

		// 1. Inbound Discord thread message with raw snowflake external_ref.
		postInbound(t, srv, inboundMessageRequest{
			Topic:       "scion.project." + project.ID + ".agent." + agent.Slug + ".messages",
			Surface:     "discord",
			ExternalRef: "777777777777777777",
			ParentRef:   "200000000000000001",
			Message: &messages.StructuredMessage{
				Version:   messages.Version,
				Timestamp: time.Now().UTC().Format(time.RFC3339),
				Channel:   "discord",
				ThreadID:  "777777777777777777",
				Sender:    "user:" + user.Email,
				Recipient: "agent:" + agent.Slug,
				Msg:       "hello from discord thread",
				Type:      messages.TypeInstruction,
			},
		})

		msgs, err := s.ListMessages(ctx, store.MessageFilter{AgentID: agent.ID}, store.ListOptions{Limit: 10})
		require.NoError(t, err)
		require.Len(t, msgs.Items, 1)
		inboundConvID := msgs.Items[0].ConversationID
		require.NotEmpty(t, inboundConvID)

		// 2. Real agent outbound reply via `--channel discord --thread-id 777777777777777777`.
		rrThread := postOutboundRequest(t, srv, project.ID, agent.ID, OutboundMessageRequest{
			Recipient: "user:" + user.Email,
			Msg:       "reply via --channel/--thread-id",
			Channel:   "discord",
			ThreadID:  "777777777777777777",
		})
		require.Equal(t, http.StatusOK, rrThread.Code, "body: %s", rrThread.Body.String())
		var threadResp map[string]any
		require.NoError(t, json.Unmarshal(rrThread.Body.Bytes(), &threadResp))
		assert.Equal(t, inboundConvID, threadResp["conversation_id"],
			"inbound Discord thread and real outbound --thread-id reply must share one conversation")

		// 3. Real agent outbound reply via conversation ref `conv:<id>`.
		rrConv := postOutboundRequest(t, srv, project.ID, agent.ID, OutboundMessageRequest{
			ConversationRef: "conv:" + inboundConvID,
			Msg:             "reply via conv:<id>",
		})
		require.Equal(t, http.StatusOK, rrConv.Code, "body: %s", rrConv.Body.String())
		var convResp map[string]any
		require.NoError(t, json.Unmarshal(rrConv.Body.Bytes(), &convResp))
		assert.Equal(t, inboundConvID, convResp["conversation_id"],
			"conversation-ref reply must land in the same conversation")
	})

	t.Run("telegram_private_chat_dm_shares_direct_conversation_with_real_outbound_dm", func(t *testing.T) {
		srv, s, user, project, agent := setupEnv(t, "tg-dm", "telegram")
		ctx := context.Background()

		// 1. Inbound Telegram private-chat message (no forum topic, so
		// telegramInboundConversation omits surface/external_ref and ThreadID is empty).
		postInbound(t, srv, inboundMessageRequest{
			Topic: "scion.project." + project.ID + ".agent." + agent.Slug + ".messages",
			Message: &messages.StructuredMessage{
				Version:   messages.Version,
				Timestamp: time.Now().UTC().Format(time.RFC3339),
				Channel:   "telegram",
				Sender:    "user:" + user.Email,
				SenderID:  user.ID,
				Recipient: "agent:" + agent.Slug,
				Msg:       "hello from telegram private chat",
				Type:      messages.TypeInstruction,
				Metadata: map[string]string{
					"telegram_chat_id": "998877",
				},
			},
		})

		msgs, err := s.ListMessages(ctx, store.MessageFilter{AgentID: agent.ID}, store.ListOptions{Limit: 10})
		require.NoError(t, err)
		require.Len(t, msgs.Items, 1)
		inboundConvID := msgs.Items[0].ConversationID
		require.NotEmpty(t, inboundConvID)

		conv, err := s.GetConversation(ctx, inboundConvID)
		require.NoError(t, err)
		assert.Equal(t, "direct", conv.Kind)

		// 2. Real agent outbound `scion message user:<x>` (no ThreadID -> direct dm:).
		rrDM := postOutboundRequest(t, srv, project.ID, agent.ID, OutboundMessageRequest{
			Recipient: "user:" + user.Email,
			Msg:       "direct reply to telegram user",
			Channel:   "telegram",
		})
		require.Equal(t, http.StatusOK, rrDM.Code, "body: %s", rrDM.Body.String())
		var dmResp map[string]any
		require.NoError(t, json.Unmarshal(rrDM.Body.Bytes(), &dmResp))
		assert.Equal(t, inboundConvID, dmResp["conversation_id"],
			"inbound Telegram private chat and real outbound user DM must share one direct conversation")
	})

	t.Run("telegram_forum_topic_shares_conversation_with_real_outbound_thread_reply", func(t *testing.T) {
		srv, s, user, project, agent := setupEnv(t, "tg-forum", "telegram")
		ctx := context.Background()

		// 1. Inbound Telegram forum topic (external_ref == "42", parent_ref == "-100123456").
		postInbound(t, srv, inboundMessageRequest{
			Topic:       "scion.project." + project.ID + ".agent." + agent.Slug + ".messages",
			Surface:     "telegram",
			ExternalRef: "42",
			ParentRef:   "-100123456",
			Message: &messages.StructuredMessage{
				Version:   messages.Version,
				Timestamp: time.Now().UTC().Format(time.RFC3339),
				Channel:   "telegram",
				ThreadID:  "42",
				Sender:    "user:" + user.Email,
				Recipient: "agent:" + agent.Slug,
				Msg:       "hello from telegram forum topic 42",
				Type:      messages.TypeInstruction,
				Metadata: map[string]string{
					"telegram_chat_id": "-100123456",
				},
			},
		})

		msgs, err := s.ListMessages(ctx, store.MessageFilter{AgentID: agent.ID}, store.ListOptions{Limit: 10})
		require.NoError(t, err)
		require.Len(t, msgs.Items, 1)
		inboundConvID := msgs.Items[0].ConversationID
		require.NotEmpty(t, inboundConvID)

		// 2. Real agent outbound reply via `--channel telegram --thread-id 42`.
		rrTopic := postOutboundRequest(t, srv, project.ID, agent.ID, OutboundMessageRequest{
			Recipient: "user:" + user.Email,
			Msg:       "reply on telegram forum topic 42",
			Channel:   "telegram",
			ThreadID:  "42",
		})
		require.Equal(t, http.StatusOK, rrTopic.Code, "body: %s", rrTopic.Body.String())
		var topicResp map[string]any
		require.NoError(t, json.Unmarshal(rrTopic.Body.Bytes(), &topicResp))
		assert.Equal(t, inboundConvID, topicResp["conversation_id"],
			"inbound Telegram forum topic and real outbound --thread-id 42 reply must share one conversation")
	})
}

// TestHandleBrokerInbound_R2_PreP4LegacyFallback verifies read-only legacy
// Phase-5 fallback against REAL pre-P4 stored row shapes for Slack, Teams
// (replyToId and stripped channel ID), and Telegram forum topics, plus:
//   - when a row already exists under the new P4 key, the new key wins;
//   - when neither exists, only the new P4 key is created (legacy is never created).
func TestHandleBrokerInbound_R2_PreP4LegacyFallback(t *testing.T) {
	t.Parallel()

	setupEnv := func(t *testing.T, suffix string) (*Server, store.Store, *store.User, *store.Project, *store.Agent) {
		t.Helper()
		srv, s := testServer(t)
		ctx := context.Background()
		srv.SetDispatcher(&recordingDispatcher{})

		user := &store.User{
			ID:          tid("user-r2-" + suffix),
			Email:       "r2-" + suffix + "@example.com",
			DisplayName: "R2 User",
			Role:        store.UserRoleMember,
			Status:      "active",
			Created:     time.Now(),
		}
		require.NoError(t, s.CreateUser(ctx, user))
		ensureHubMembership(ctx, s, user.ID)

		project := &store.Project{
			ID:        tid("proj-r2-" + suffix),
			Slug:      "r2-" + suffix,
			Name:      "R2 " + suffix,
			OwnerID:   user.ID,
			CreatedBy: user.ID,
			Created:   time.Now(),
			Updated:   time.Now(),
		}
		require.NoError(t, s.CreateProject(ctx, project))
		srv.seedProjectCreatorMembership(ctx, project)
		msgAuthzAddProjectMember(t, s, user.ID, project.ID, project.Slug, store.GroupMemberRoleMember)

		agent := &store.Agent{
			ID:           tid("agent-r2-" + suffix),
			Slug:         "coder",
			Name:         "Coder",
			ProjectID:    project.ID,
			Phase:        string(state.PhaseRunning),
			MessageMode:  store.MessageModeProject,
			StateVersion: 1,
			Created:      time.Now(),
			Updated:      time.Now(),
		}
		require.NoError(t, s.CreateAgent(ctx, agent))
		return srv, s, user, project, agent
	}

	postInbound := func(t *testing.T, srv *Server, payload inboundMessageRequest) {
		t.Helper()
		body, err := json.Marshal(payload)
		require.NoError(t, err)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/broker/inbound", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req = req.WithContext(contextWithBrokerIdentity(req.Context(), NewBrokerIdentity("test-broker")))
		rec := httptest.NewRecorder()
		srv.mux.ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	}

	cases := []struct {
		name           string
		surface        string
		legacyThreadID string // real pre-P4 Phase-5 thread ID seeded in DB
		p4ExternalRef  string // real P4 plugin external_ref
		p4ParentRef    string // real P4 plugin parent_ref
		msgChannel     string // real P4 plugin Message.Channel
		msgThreadID    string // real P4 plugin Message.ThreadID
	}{
		{
			name:           "slack_pre_p4_ts_thread_reused",
			surface:        "slack",
			legacyThreadID: "1700000000.000100",
			p4ExternalRef:  "C0123ABC:1700000000.000100",
			p4ParentRef:    "C0123ABC",
			msgChannel:     "slack",
			msgThreadID:    "1700000000.000100",
		},
		{
			name:           "teams_pre_p4_replyToId_reused",
			surface:        "teams",
			legacyThreadID: "1700000000100",
			p4ExternalRef:  "19:chan-1@thread.tacv2;messageid=1700000000100",
			p4ParentRef:    "19:chan-1@thread.tacv2",
			msgChannel:     "teams",
			msgThreadID:    "1700000000100",
		},
		{
			name:           "teams_pre_p4_stripped_channel_id_reused",
			surface:        "teams",
			legacyThreadID: "19:chan-2@thread.tacv2",
			p4ExternalRef:  "19:chan-2@thread.tacv2;messageid=1700000000200",
			p4ParentRef:    "19:chan-2@thread.tacv2",
			msgChannel:     "teams",
			msgThreadID:    "",
		},
		{
			name:           "telegram_pre_p4_forum_topic_reused",
			surface:        "telegram",
			legacyThreadID: "42",
			p4ExternalRef:  "-100123456:42",
			p4ParentRef:    "-100123456",
			msgChannel:     "telegram",
			msgThreadID:    "42",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, s, user, project, agent := setupEnv(t, tc.name)
			ctx := context.Background()

			// Seed REAL pre-P4 Phase-5 row.
			legacyKey, err := messaging.ThreadConversationExternalRef(project.ID, tc.legacyThreadID)
			require.NoError(t, err)
			seeded, err := messaging.ResolveOrCreateConversationByKey(
				ctx, s, slog.Default(), legacyKey, "group", &project.ID,
				messaging.WithSurface(tc.surface),
			)
			require.NoError(t, err)

			// Send REAL P4 plugin inbound payload.
			postInbound(t, srv, inboundMessageRequest{
				Topic:       "scion.project." + project.ID + ".agent." + agent.Slug + ".messages",
				Surface:     tc.surface,
				ExternalRef: tc.p4ExternalRef,
				ParentRef:   tc.p4ParentRef,
				Message: &messages.StructuredMessage{
					Version:   messages.Version,
					Timestamp: time.Now().UTC().Format(time.RFC3339),
					Channel:   tc.msgChannel,
					ThreadID:  tc.msgThreadID,
					Sender:    "user:" + user.Email,
					Recipient: "agent:" + agent.Slug,
					Msg:       "follow-up in pre-p4 thread",
					Type:      messages.TypeInstruction,
				},
			})

			msgs, err := s.ListMessages(ctx, store.MessageFilter{AgentID: agent.ID}, store.ListOptions{Limit: 10})
			require.NoError(t, err)
			require.Len(t, msgs.Items, 1)
			assert.Equal(t, seeded.ConversationID, msgs.Items[0].ConversationID,
				"must reuse pre-P4 conversation row rather than splitting")

			allConvs, err := s.ListConversations(ctx, store.ConversationFilter{Surface: tc.surface}, store.ListOptions{Limit: 10})
			require.NoError(t, err)
			assert.Len(t, allConvs.Items, 1, "must not create a duplicate conversation row")
		})
	}

	t.Run("new_key_wins_when_both_new_and_legacy_exist", func(t *testing.T) {
		srv, s, user, project, agent := setupEnv(t, "new-wins")
		ctx := context.Background()

		legacyKey, err := messaging.ThreadConversationExternalRef(project.ID, "1700000000.000100")
		require.NoError(t, err)
		_, err = messaging.ResolveOrCreateConversationByKey(
			ctx, s, slog.Default(), legacyKey, "group", &project.ID,
			messaging.WithSurface("slack"),
		)
		require.NoError(t, err)

		newKey, err := messaging.ThreadConversationExternalRef(project.ID, "C0123ABC:1700000000.000100")
		require.NoError(t, err)
		newConv, err := messaging.ResolveOrCreateConversationByKey(
			ctx, s, slog.Default(), newKey, "group", &project.ID,
			messaging.WithSurface("slack"),
		)
		require.NoError(t, err)

		postInbound(t, srv, inboundMessageRequest{
			Topic:       "scion.project." + project.ID + ".agent." + agent.Slug + ".messages",
			Surface:     "slack",
			ExternalRef: "C0123ABC:1700000000.000100",
			ParentRef:   "C0123ABC",
			Message: &messages.StructuredMessage{
				Version:   messages.Version,
				Timestamp: time.Now().UTC().Format(time.RFC3339),
				Channel:   "slack",
				ThreadID:  "1700000000.000100",
				Sender:    "user:" + user.Email,
				Recipient: "agent:" + agent.Slug,
				Msg:       "new key should win",
				Type:      messages.TypeInstruction,
			},
		})

		msgs, err := s.ListMessages(ctx, store.MessageFilter{AgentID: agent.ID}, store.ListOptions{Limit: 10})
		require.NoError(t, err)
		require.Len(t, msgs.Items, 1)
		assert.Equal(t, newConv.ConversationID, msgs.Items[0].ConversationID,
			"when both new and legacy rows exist, the new key must win")
	})

	t.Run("neither_exists_creates_only_new_key_never_legacy", func(t *testing.T) {
		srv, s, user, project, agent := setupEnv(t, "new-only")
		ctx := context.Background()

		postInbound(t, srv, inboundMessageRequest{
			Topic:       "scion.project." + project.ID + ".agent." + agent.Slug + ".messages",
			Surface:     "slack",
			ExternalRef: "C0123ABC:1700000000.000999",
			ParentRef:   "C0123ABC",
			Message: &messages.StructuredMessage{
				Version:   messages.Version,
				Timestamp: time.Now().UTC().Format(time.RFC3339),
				Channel:   "slack",
				ThreadID:  "1700000000.000999",
				Sender:    "user:" + user.Email,
				Recipient: "agent:" + agent.Slug,
				Msg:       "brand new thread",
				Type:      messages.TypeInstruction,
			},
		})

		newKey, err := messaging.ThreadConversationExternalRef(project.ID, "C0123ABC:1700000000.000999")
		require.NoError(t, err)
		newConv, err := s.GetConversationByExternalRef(ctx, "slack", newKey)
		require.NoError(t, err)
		assert.Equal(t, "C0123ABC", newConv.ParentRef)

		legacyKey, err := messaging.ThreadConversationExternalRef(project.ID, "1700000000.000999")
		require.NoError(t, err)
		_, legacyErr := s.GetConversationByExternalRef(ctx, "slack", legacyKey)
		assert.ErrorIs(t, legacyErr, store.ErrNotFound, "legacy row must never be created")
	})
}

type inboundTestCaptureBus struct {
	mu        sync.Mutex
	published []*messages.StructuredMessage
}

func (b *inboundTestCaptureBus) Publish(_ context.Context, _ string, msg *messages.StructuredMessage) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.published = append(b.published, msg)
	return nil
}

func (b *inboundTestCaptureBus) Subscribe(string, eventbus.EventHandler) (eventbus.Subscription, error) {
	return &inboundTestNoopSub{}, nil
}

func (b *inboundTestCaptureBus) Close() error { return nil }

type inboundTestNoopSub struct{}

func (*inboundTestNoopSub) Unsubscribe() error { return nil }

// TestHandleBroadcast_StripsClientSuppliedConversation (O1) verifies that a
// client-supplied structured_message.conversation on POST /projects/{id}/broadcast
// is stripped before reaching the event bus (where enrichConversation is a no-op
// because broadcasts have no ConversationID).
func TestHandleBroadcast_StripsClientSuppliedConversation(t *testing.T) {
	t.Parallel()
	srv, s := testServer(t)
	ctx := context.Background()

	capBus := &inboundTestCaptureBus{}
	events := NewChannelEventPublisher()
	t.Cleanup(func() { events.Close() })
	proxy := NewMessageBrokerProxy(capBus, s, events, func() AgentDispatcher { return &recordingDispatcher{} }, slog.Default())
	srv.SetMessageBrokerProxy(proxy)

	user := &store.User{
		ID:          tid("user-bcast-strip"),
		Email:       "bcast-strip@example.com",
		DisplayName: "Broadcast Strip User",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, user))
	ensureHubMembership(ctx, s, user.ID)

	project := &store.Project{
		ID:        tid("proj-bcast-strip"),
		Slug:      "bcast-strip",
		Name:      "Broadcast Strip",
		OwnerID:   user.ID,
		CreatedBy: user.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))
	srv.seedProjectCreatorMembership(ctx, project)
	msgAuthzAddProjectMember(t, s, user.ID, project.ID, project.Slug, store.GroupMemberRoleMember)

	agent := &store.Agent{
		ID:           tid("agent-bcast-strip"),
		Slug:         "coder",
		Name:         "Coder",
		ProjectID:    project.ID,
		Phase:        string(state.PhaseRunning),
		MessageMode:  store.MessageModeProject,
		StateVersion: 1,
		Created:      time.Now(),
		Updated:      time.Now(),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	reqBody := BroadcastMessageRequest{
		StructuredMessage: &messages.StructuredMessage{
			Version: messages.Version,
			Msg:     "broadcast attempting spoofed conversation",
			Type:    messages.TypeInstruction,
			Conversation: &messages.Conversation{
				Surface:     "slack",
				ExternalRef: "C_OTHER:1",
			},
		},
	}
	body, err := json.Marshal(reqBody)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/"+project.ID+"/broadcast", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(), NewAuthenticatedUser(user.ID, user.Email, user.DisplayName, user.Role, "test")))

	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)
	require.Equal(t, http.StatusAccepted, rec.Code, "body: %s", rec.Body.String())

	capBus.mu.Lock()
	defer capBus.mu.Unlock()
	require.Len(t, capBus.published, 1)
	assert.Nil(t, capBus.published[0].Conversation,
		"client-supplied Conversation on broadcast must be stripped before reaching the bus")
}

// TestHandleBrokerInbound_StripsClientSuppliedConversation (O1) verifies that
// structured_message.conversation on POST /api/v1/broker/inbound is stripped.
func TestHandleBrokerInbound_StripsClientSuppliedConversation(t *testing.T) {
	t.Parallel()
	srv, s := testServer(t)
	ctx := context.Background()

	dispatcher := &recordingDispatcher{}
	srv.SetDispatcher(dispatcher)

	user := &store.User{
		ID:          tid("user-inb-strip"),
		Email:       "inb-strip@example.com",
		DisplayName: "Inbound Strip User",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, user))
	ensureHubMembership(ctx, s, user.ID)

	project := &store.Project{
		ID:        tid("proj-inb-strip"),
		Slug:      "inb-strip",
		Name:      "Inbound Strip",
		OwnerID:   user.ID,
		CreatedBy: user.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))
	srv.seedProjectCreatorMembership(ctx, project)
	msgAuthzAddProjectMember(t, s, user.ID, project.ID, project.Slug, store.GroupMemberRoleMember)

	agent := &store.Agent{
		ID:           tid("agent-inb-strip"),
		Slug:         "coder",
		Name:         "Coder",
		ProjectID:    project.ID,
		Phase:        string(state.PhaseRunning),
		MessageMode:  store.MessageModeProject,
		StateVersion: 1,
		Created:      time.Now(),
		Updated:      time.Now(),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	payload := inboundMessageRequest{
		Topic: "scion.project." + project.ID + ".agent." + agent.Slug + ".messages",
		Message: &messages.StructuredMessage{
			Version:   messages.Version,
			Timestamp: time.Now().UTC().Format(time.RFC3339),
			Channel:   "slack",
			Sender:    "user:" + user.Email,
			Recipient: "agent:" + agent.Slug,
			Msg:       "inbound attempting spoofed conversation",
			Type:      messages.TypeInstruction,
			Conversation: &messages.Conversation{
				Surface:     "slack",
				ExternalRef: "C_OTHER:1",
			},
		},
	}
	body, err := json.Marshal(payload)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/broker/inbound", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithBrokerIdentity(req.Context(), NewBrokerIdentity("test-broker")))

	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	calls := dispatcher.getCalls()
	require.Len(t, calls, 1)
	require.NotNil(t, calls[0].StructuredMessage)
	assert.Nil(t, calls[0].StructuredMessage.Conversation,
		"client-supplied Conversation must be stripped on /api/v1/broker/inbound")
}

// TestHandleAgentMessage_Phase11_ThreadKeyAndLegacyFallback (O2) verifies that
// the broker-authenticated Phase 11 block in handleAgentMessage routes through
// deriveBrokerInboundThreadKey plus the read-only legacy Phase-5 fallback.
func TestHandleAgentMessage_Phase11_ThreadKeyAndLegacyFallback(t *testing.T) {
	t.Parallel()
	srv, s := testServer(t)
	ctx := context.Background()

	dispatcher := &recordingDispatcher{}
	srv.SetDispatcher(dispatcher)

	broker := &store.RuntimeBroker{
		ID:      tid("broker-o2"),
		Slug:    "o2-broker",
		Name:    "O2 Broker",
		Status:  store.BrokerStatusOnline,
		Created: time.Now(),
		Updated: time.Now(),
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))

	user := &store.User{
		ID:          tid("user-o2"),
		Email:       "o2@example.com",
		DisplayName: "O2 User",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, user))
	ensureHubMembership(ctx, s, user.ID)

	project := &store.Project{
		ID:        tid("proj-o2"),
		Slug:      "o2",
		Name:      "O2",
		OwnerID:   user.ID,
		CreatedBy: user.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))
	srv.seedProjectCreatorMembership(ctx, project)
	msgAuthzAddProjectMember(t, s, user.ID, project.ID, project.Slug, store.GroupMemberRoleMember)
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID:  project.ID,
		BrokerID:   broker.ID,
		BrokerName: broker.Name,
		Status:     store.BrokerStatusOnline,
	}))

	agent := &store.Agent{
		ID:              tid("agent-o2"),
		Slug:            "coder",
		Name:            "Coder",
		ProjectID:       project.ID,
		RuntimeBrokerID: broker.ID,
		Phase:           string(state.PhaseRunning),
		MessageMode:     store.MessageModeProject,
		StateVersion:    1,
		Created:         time.Now(),
		Updated:         time.Now(),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	// 1. Seed a pre-P4 Slack thread row (thread:<proj>:<ts>).
	legacyKey, err := messaging.ThreadConversationExternalRef(project.ID, "1700000000.000500")
	require.NoError(t, err)
	seeded, err := messaging.ResolveOrCreateConversationByKey(
		ctx, s, slog.Default(), legacyKey, "group", &project.ID,
		messaging.WithSurface("slack"),
	)
	require.NoError(t, err)

	// Broker-authenticated POST /api/v1/agents/{id}/message with P4 surface+ref
	// and legacy ThreadID reuses the pre-P4 row.
	reqBody1 := MessageRequest{
		Surface:     "slack",
		ExternalRef: "C0123ABC:1700000000.000500",
		ParentRef:   "C0123ABC",
		StructuredMessage: &messages.StructuredMessage{
			Version:   messages.Version,
			Timestamp: time.Now().UTC().Format(time.RFC3339),
			Channel:   "slack",
			ThreadID:  "1700000000.000500",
			Sender:    "user:" + user.Email,
			Recipient: "agent:" + agent.Slug,
			Msg:       "broker agent-message hitting legacy fallback",
			Type:      messages.TypeInstruction,
		},
	}
	b1, err := json.Marshal(reqBody1)
	require.NoError(t, err)
	r1 := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+agent.ID+"/message", bytes.NewReader(b1))
	r1.Header.Set("Content-Type", "application/json")
	r1 = r1.WithContext(contextWithBrokerIdentity(
		contextWithIdentity(r1.Context(), NewAuthenticatedUser(user.ID, user.Email, user.DisplayName, user.Role, "test")),
		NewBrokerIdentity("test-broker"),
	))
	rec1 := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec1, r1)
	require.Equal(t, http.StatusOK, rec1.Code, "body: %s", rec1.Body.String())

	calls := dispatcher.getCalls()
	require.Len(t, calls, 1)
	assert.Equal(t, seeded.ConversationID, calls[0].StructuredMessage.Metadata["conversation_id"],
		"broker agent-message Phase 11 path must reuse pre-P4 legacy row")

	// 2. Brand-new thread creates under canonical thread:<proj>:<external_ref>.
	reqBody2 := MessageRequest{
		Surface:     "slack",
		ExternalRef: "C0123ABC:1700000000.000600",
		ParentRef:   "C0123ABC",
		StructuredMessage: &messages.StructuredMessage{
			Version:   messages.Version,
			Timestamp: time.Now().UTC().Format(time.RFC3339),
			Channel:   "slack",
			ThreadID:  "1700000000.000600",
			Sender:    "user:" + user.Email,
			Recipient: "agent:" + agent.Slug,
			Msg:       "broker agent-message creating new canonical thread",
			Type:      messages.TypeInstruction,
		},
	}
	b2, err := json.Marshal(reqBody2)
	require.NoError(t, err)
	r2 := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+agent.ID+"/message", bytes.NewReader(b2))
	r2.Header.Set("Content-Type", "application/json")
	r2 = r2.WithContext(contextWithBrokerIdentity(
		contextWithIdentity(r2.Context(), NewAuthenticatedUser(user.ID, user.Email, user.DisplayName, user.Role, "test")),
		NewBrokerIdentity("test-broker"),
	))
	rec2 := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec2, r2)
	require.Equal(t, http.StatusOK, rec2.Code, "body: %s", rec2.Body.String())

	wantNewKey, err := messaging.ThreadConversationExternalRef(project.ID, "C0123ABC:1700000000.000600")
	require.NoError(t, err)
	newConv, err := s.GetConversationByExternalRef(ctx, "slack", wantNewKey)
	require.NoError(t, err)
	assert.Equal(t, "C0123ABC", newConv.ParentRef)
}
