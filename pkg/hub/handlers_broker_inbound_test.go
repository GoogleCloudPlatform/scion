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

			// Create project.
			project := &store.Project{
				ID:      tid("proj-broker-inbound"),
				Slug:    "broker-inbound-proj",
				Name:    "Broker Inbound Test Project",
				Created: time.Now(),
				Updated: time.Now(),
			}
			require.NoError(t, s.CreateProject(ctx, project))

			// Create agent with the specified phase.
			agent := &store.Agent{
				ID:           tid("agent-errstate"),
				Slug:         "errstate-agent",
				Name:         "Error State Agent",
				ProjectID:    project.ID,
				Phase:        string(tt.phase),
				StateVersion: 1,
				Created:      time.Now(),
				Updated:      time.Now(),
			}
			require.NoError(t, s.CreateAgent(ctx, agent))

			// Build broker inbound request.
			topic := "scion.project." + project.ID + ".agent." + agent.Slug + ".messages"
			payload := inboundMessageRequest{
				Topic: topic,
				Message: &messages.StructuredMessage{
					Version:   messages.Version,
					Timestamp: time.Now().UTC().Format(time.RFC3339),
					Channel:   "discord",
					Sender:    "agent:other-agent",
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

func TestHandleBrokerInbound_AllowsRunningAgent(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create project.
	project := &store.Project{
		ID:      tid("proj-broker-running"),
		Slug:    "broker-running-proj",
		Name:    "Broker Running Test Project",
		Created: time.Now(),
		Updated: time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))

	// Create a running agent.
	agent := &store.Agent{
		ID:           tid("agent-running"),
		Slug:         "running-agent",
		Name:         "Running Agent",
		ProjectID:    project.ID,
		Phase:        string(state.PhaseRunning),
		StateVersion: 1,
		Created:      time.Now(),
		Updated:      time.Now(),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	// Build broker inbound request.
	topic := "scion.project." + project.ID + ".agent." + agent.Slug + ".messages"
	payload := inboundMessageRequest{
		Topic: topic,
		Message: &messages.StructuredMessage{
			Version:   messages.Version,
			Timestamp: time.Now().UTC().Format(time.RFC3339),
			Channel:   "discord",
			Sender:    "agent:other-agent",
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
