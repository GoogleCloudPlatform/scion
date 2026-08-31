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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func TestParseAgentMessageTopic(t *testing.T) {
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
			name:      "legacy grove topic",
			topic:     "scion.grove.my-project-123.agent.coder.messages",
			projectID: "my-project-123",
			agentSlug: "coder",
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
			srv.createProjectMembersGroup(ctx, project)
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
	srv.createProjectMembersGroup(ctx, project)
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
				// Verify conversation was created in the store.
				convs, err := s.ListConversations(ctx, store.ConversationFilter{
					Surface: tt.surface,
				}, store.ListOptions{Limit: 10})
				require.NoError(t, err)

				found := false
				for _, c := range convs.Items {
					if c.ExternalRef == tt.externalRef && c.Surface == tt.surface {
						found = true
						assert.Equal(t, tt.parentRef, c.ParentRef)
						assert.Equal(t, "group", c.Kind)
						assert.Equal(t, "active", c.DriftState)
						assert.Equal(t, project.ID, *c.ProjectID)
					}
				}
				assert.True(t, found, "expected conversation with surface=%s external_ref=%s to exist", tt.surface, tt.externalRef)
			}
		})
	}
}

// TestHandleBrokerInbound_ConversationResolution_PerPlugin_Regression tests the
// AC-8 regression case for each platform surface: external_ref present with
// empty surface must be rejected at the broker edge.
func TestHandleBrokerInbound_ConversationResolution_PerPlugin_Regression(t *testing.T) {
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
			srv.createProjectMembersGroup(ctx, project)
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
	srv.createProjectMembersGroup(ctx, project)
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

// TestHandleBrokerInbound_UnmappedExternalSenderDenied verifies that an
// unmapped external-channel sender (e.g. "discord:someuser") is denied.
func TestHandleBrokerInbound_UnmappedExternalSenderDenied(t *testing.T) {
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
