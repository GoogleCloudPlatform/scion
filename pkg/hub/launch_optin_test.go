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

// This file covers a create request's acceptAsyncLaunch opt-in: it must be
// persisted as store.Agent.LaunchAsyncOptIn, and default to false when
// absent.
package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCreateAgent_AcceptAsyncLaunchOptInPersisted(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, project := setupCreateAgentServer(t, disp)
	ctx := context.Background()

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:              "optin-test",
		ProjectID:         project.ID,
		Task:              "do something",
		AcceptAsyncLaunch: true,
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.Agent)

	persisted, err := s.GetAgent(ctx, resp.Agent.ID)
	require.NoError(t, err)
	assert.True(t, persisted.LaunchAsyncOptIn, "acceptAsyncLaunch: true must persist as LaunchAsyncOptIn")
}

func TestCreateAgent_AcceptAsyncLaunchDefaultsFalse(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, project := setupCreateAgentServer(t, disp)
	ctx := context.Background()

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:      "optin-default-test",
		ProjectID: project.ID,
		Task:      "do something",
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.Agent)

	persisted, err := s.GetAgent(ctx, resp.Agent.ID)
	require.NoError(t, err)
	assert.False(t, persisted.LaunchAsyncOptIn, "acceptAsyncLaunch must default to false when absent")
}
