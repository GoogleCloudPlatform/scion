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

//go:build !no_sqlite && (!hubshard || hubshard_1)

// This file covers the failure cause a failed launch report carries
// (ptone/scion#4666): which causes the hub stores, and that the agent API's
// launch view returns the stored one.
package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAgentLaunchReport_FailedCause(t *testing.T) {
	cases := []struct {
		name      string
		errorCode string
		cause     string
		want      string
	}{
		{"runtime_error with a known cause is stored", ErrCodeRuntimeError, api.BrokerRuntimeCauseForbidden, api.BrokerRuntimeCauseForbidden},
		{"unknown cause is dropped", ErrCodeRuntimeError, "pods is forbidden: User system:serviceaccount:team-ns:scion-sa", ""},
		{"cause on another code is dropped", "template_not_found", api.BrokerRuntimeCauseForbidden, ""},
		// An older broker sends no cause.
		{"no cause", ErrCodeRuntimeError, "", ""},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, s := testServer(t)
			ctx := context.Background()

			project := &store.Project{ID: tid("lr-cause-project"), Slug: "lr-cause-project", Name: "LR Cause Project", Created: time.Now(), Updated: time.Now()}
			require.NoError(t, s.CreateProject(ctx, project))
			agent := &store.Agent{
				ID: tid("lr-cause-agent"), Slug: "lr-cause-agent", Name: "LR Cause Agent", ProjectID: project.ID,
				Phase: string(state.PhaseCreated), RuntimeBrokerID: "broker-1", StateVersion: 1,
				Created: time.Now(), Updated: time.Now(),
			}
			require.NoError(t, s.CreateAgent(ctx, agent))
			launchID, err := s.BeginLaunch(ctx, agent.ID, store.LaunchKindCreate, 5*time.Minute)
			require.NoError(t, err)

			rec := postLaunchReport(t, srv, "broker-1", agent.ID, "broker-1", AgentLaunchReport{
				LaunchID: launchID, InstanceID: "i1", State: "failed", Step: "launching",
				ErrorCode: tc.errorCode, Message: "Failed to create agent", Cause: tc.cause,
			})
			require.Equal(t, http.StatusOK, rec.Code, "case %d: %s", i, rec.Body.String())

			after, err := s.GetAgent(ctx, agent.ID)
			require.NoError(t, err)
			assert.Equal(t, tc.errorCode, after.LaunchError)
			assert.Equal(t, tc.want, after.LaunchCause, "stored launch cause")

			get := doRequest(t, srv, http.MethodGet, "/api/v1/agents/"+agent.ID, nil)
			require.Equal(t, http.StatusOK, get.Code, get.Body.String())
			var body struct {
				Launch map[string]json.RawMessage `json:"launch"`
			}
			require.NoError(t, json.Unmarshal(get.Body.Bytes(), &body))
			require.NotNil(t, body.Launch, "expected a launch view: %s", get.Body.String())
			causeRaw, ok := body.Launch["cause"]
			if tc.want == "" {
				assert.False(t, ok, "expected no launch.cause, got %s", causeRaw)
				return
			}
			require.True(t, ok, "expected launch.cause: %s", get.Body.String())
			var cause string
			require.NoError(t, json.Unmarshal(causeRaw, &cause))
			assert.Equal(t, tc.want, cause)
		})
	}
}
