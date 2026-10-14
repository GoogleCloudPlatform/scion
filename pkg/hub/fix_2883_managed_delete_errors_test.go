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
	"errors"
	"net/http"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/managedagent"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for ptone/scion#2883: a managed-agent delete whose cloud cleanup
// fails is a failed delete (502 deletion{failed, runtime_error}, row kept,
// prior phase restored); force=true removes the record anyway.

// setupManagedAgentForDelete creates a running managed agent with a recorded
// interaction and activity "working".
func setupManagedAgentForDelete(t *testing.T, s store.Store, suffix string) *store.Agent {
	t.Helper()
	ctx := context.Background()
	agent := setupBrokerAgentInPhase(t, s, suffix, state.PhaseRunning)
	agent.Runtime = ManagedRuntimePrefix + "test"
	agent.Annotations = map[string]string{annotationInteractionID: "interaction-" + suffix}
	require.NoError(t, s.UpdateAgent(ctx, agent))
	require.NoError(t, s.UpdateAgentStatus(ctx, agent.ID, store.AgentStatusUpdate{Activity: "working"}))
	return agent
}

// managedDeleteFailureCases are the cloud cleanup failures a managed delete
// can meet. backend nil means no managed backend is configured on the hub.
func managedDeleteFailureCases() []struct {
	name    string
	backend func() managedagent.ManagedAgentBackend
	want    string
} {
	return []struct {
		name    string
		backend func() managedagent.ManagedAgentBackend
		want    string
	}{
		{"read fails", func() managedagent.ManagedAgentBackend {
			return &recordingManagedBackend{getErr: errors.New("backend unavailable")}
		}, "backend unavailable"},
		{"cancel fails", func() managedagent.ManagedAgentBackend {
			return &recordingManagedBackend{cancelErr: errors.New("cancel refused")}
		}, "cancel refused"},
		{"no backend configured", nil, "managed agent backend"},
	}
}

// installManagedDeleteBackend installs the case's backend. With no backend,
// the lazy loader reads settings from an empty home directory and fails.
func installManagedDeleteBackend(t *testing.T, mk func() managedagent.ManagedAgentBackend) {
	t.Helper()
	if mk == nil {
		t.Setenv("HOME", t.TempDir())
		useManagedBackend(t, nil)
		return
	}
	useManagedBackend(t, mk())
}

func TestManagedDelete_CloudCleanupError_RollsBack(t *testing.T) {
	for i, tc := range managedDeleteFailureCases() {
		t.Run(tc.name, func(t *testing.T) {
			srv, s, pub, disp := engineTestServer(t)
			installManagedDeleteBackend(t, tc.backend)
			agent := setupManagedAgentForDelete(t, s, "mdel-fail-"+string(rune('a'+i)))

			rec := doRequest(t, srv, http.MethodDelete, "/api/v1/agents/"+agent.ID, nil)
			require.Equal(t, http.StatusBadGateway, rec.Code, rec.Body.String())
			code, details := errorBody(t, rec)
			assert.Equal(t, ErrCodeRuntimeError, code)
			assert.Equal(t, store.DeletionCodeRuntimeError, details["deletionCode"])
			assert.Contains(t, rec.Body.String(), tc.want)
			assert.Zero(t, disp.callCount(), "a managed delete never dispatches to a broker")
			assert.Zero(t, pub.count("deleted"))

			got := mustGetAgent(t, s, agent.ID)
			assert.True(t, got.DeletedAt.IsZero(), "the row is kept")
			assert.Equal(t, string(state.PhaseRunning), got.Phase, "prior phase restored")
			assert.Equal(t, "working", got.Activity, "prior activity restored")
			assert.Equal(t, store.DeletionStateFailed, got.DeletionState)
			assert.Equal(t, store.DeletionCodeRuntimeError, got.DeletionCode)
			assert.Contains(t, got.DeletionError, managedDeleteFailedPrefix)
		})
	}
}

func TestManagedDelete_CloudCleanupError_ForceDeletes(t *testing.T) {
	for i, tc := range managedDeleteFailureCases() {
		t.Run(tc.name, func(t *testing.T) {
			srv, s, pub, _ := engineTestServer(t)
			installManagedDeleteBackend(t, tc.backend)
			agent := setupManagedAgentForDelete(t, s, "mdel-force-"+string(rune('a'+i)))

			rec := doRequest(t, srv, http.MethodDelete, "/api/v1/agents/"+agent.ID+"?force=true", nil)
			require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
			assert.True(t, agentGone(t, s, agent.ID))
			assert.Equal(t, 1, pub.count("deleted"))
		})
	}
}

// Regression: a cleanup that succeeds deletes the agent and cancels its
// interaction once.
func TestManagedDelete_CloudCleanupSucceeds(t *testing.T) {
	srv, s, pub, _ := engineTestServer(t)
	backend := &recordingManagedBackend{}
	useManagedBackend(t, backend)
	agent := setupManagedAgentForDelete(t, s, "mdel-ok")

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/agents/"+agent.ID, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	assert.True(t, agentGone(t, s, agent.ID))
	assert.Equal(t, 1, pub.count("deleted"))
	assert.Equal(t, []string{"interaction-mdel-ok"}, backend.cancels())
}
