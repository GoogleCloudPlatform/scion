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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
)

func setNoticeTestExperiments(t *testing.T, srv *Server, s store.Store, overrides map[string]bool) {
	t.Helper()
	ctx := t.Context()
	ops := ensureTestOps(t, srv, s)
	expDoc, err := json.Marshal(map[string]any{
		"overrides": overrides,
	})
	require.NoError(t, err)
	_, err = s.UpsertHubSetting(ctx, "experiments", json.RawMessage(expDoc), "test", -1, "managed")
	require.NoError(t, err)
	_, err = ops.Refresh(ctx)
	require.NoError(t, err)
	ApplySnapshot(srv, ops.Snapshot())
}

func TestTrajectoryCaptureNotice_RunAttributionAndUsageRead(t *testing.T) {
	assert.Equal(t, telemetrycontract.TrajectoryCaptureProjectAnnotation, projectSettingTrajectoryCapture)
	srv, s := testServer(t)
	ctx := t.Context()

	setNoticeTestExperiments(t, srv, s, map[string]bool{
		experiments.TokenomicsUsageLedger:       true,
		experiments.TokenomicsTrajectoryCapture: true,
	})
	srv.reconcileUsageLedger(ctx)

	proj := &store.Project{
		ID:   api.NewUUID(),
		Name: "notice-proj",
		Slug: "notice-proj",
	}
	require.NoError(t, s.CreateProject(ctx, proj))

	agent := &store.Agent{
		ID:        api.NewUUID(),
		ProjectID: proj.ID,
		Name:      "notice-agent",
		Slug:      "notice-agent",
		Template:  "claude",
		Phase:     "running",
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	// Atomic run-ID minting via Hub-wired store stamps capture_policy="full" and notice_version="v1".
	_, err := s.SetAgentRunID(ctx, agent.ID, "run-notice-1", nil)
	require.NoError(t, err)
	attr, err := s.GetRunAttribution(ctx, "run-notice-1")
	require.NoError(t, err)
	assert.Equal(t, telemetrycontract.TrajectoryCapturePolicyFull, attr.CapturePolicy)
	assert.Equal(t, telemetrycontract.TrajectoryNoticeVersion, attr.NoticeVersion)

	// GET /api/v1/agents/{id}/usage exposes capture_policy and notice_version on runs[].
	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/"+agent.ID+"/usage", nil)
	req.Header.Set("Authorization", "Bearer "+testDevToken)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var usageResp AgentUsageResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &usageResp))
	require.Len(t, usageResp.Runs, 1)
	assert.Equal(t, "run-notice-1", usageResp.Runs[0].RunID)
	assert.Equal(t, telemetrycontract.TrajectoryCapturePolicyFull, usageResp.Runs[0].CapturePolicy)
	assert.Equal(t, telemetrycontract.TrajectoryNoticeVersion, usageResp.Runs[0].NoticeVersion)
}

func TestCreateAgentResponse_TrajectoryCapture(t *testing.T) {
	ctx := t.Context()

	t.Run("sync 201 create includes trajectory_capture when experiment on and omits when off", func(t *testing.T) {
		disp := &createAgentDispatcher{}
		srv, s, project := setupCreateAgentServer(t, disp)
		project.Annotations = map[string]string{
			projectSettingTrajectoryCapture: telemetrycontract.TrajectoryCapturePolicyMetadataOnly,
		}
		require.NoError(t, s.UpdateProject(ctx, project))

		// Experiment OFF -> key must be absent from raw JSON.
		setNoticeTestExperiments(t, srv, s, map[string]bool{
			experiments.TokenomicsTrajectoryCapture: false,
		})
		recOff := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
			Name:      "sync-exp-off",
			ProjectID: project.ID,
			Task:      "hello",
		})
		require.Equal(t, http.StatusCreated, recOff.Code, recOff.Body.String())
		var rawOff map[string]any
		require.NoError(t, json.Unmarshal(recOff.Body.Bytes(), &rawOff))
		_, hasOff := rawOff["trajectory_capture"]
		assert.False(t, hasOff, "trajectory_capture must be omitted when experiment is off")

		// Experiment ON -> key present with resolved project policy.
		setNoticeTestExperiments(t, srv, s, map[string]bool{
			experiments.TokenomicsTrajectoryCapture: true,
		})
		recOn := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
			Name:      "sync-exp-on",
			ProjectID: project.ID,
			Task:      "hello",
		})
		require.Equal(t, http.StatusCreated, recOn.Code, recOn.Body.String())
		var respOn CreateAgentResponse
		require.NoError(t, json.Unmarshal(recOn.Body.Bytes(), &respOn))
		require.NotNil(t, respOn.TrajectoryCapture)
		assert.Equal(t, telemetrycontract.TrajectoryCapturePolicyMetadataOnly, respOn.TrajectoryCapture.Policy)
		assert.False(t, respOn.TrajectoryCapture.EnforcedByHub)
	})

	t.Run("202 env-gather includes trajectory_capture when experiment on and omits when off", func(t *testing.T) {
		disp := &createAgentDispatcher{
			envReqs: &RemoteEnvRequirementsResponse{Needs: []string{"SOME_REQUIRED_KEY"}},
		}
		srv, s, project := setupCreateAgentServer(t, disp)
		project.Annotations = map[string]string{
			projectSettingTrajectoryCapture: telemetrycontract.TrajectoryCapturePolicyMetadataOnly,
		}
		require.NoError(t, s.UpdateProject(ctx, project))

		// Experiment OFF -> key absent.
		setNoticeTestExperiments(t, srv, s, map[string]bool{
			experiments.TokenomicsTrajectoryCapture: false,
		})
		recOff := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
			Name:      "gather-exp-off",
			ProjectID: project.ID,
			Task:      "hello",
			GatherEnv: true,
		})
		require.Equal(t, http.StatusAccepted, recOff.Code, recOff.Body.String())
		var rawOff map[string]any
		require.NoError(t, json.Unmarshal(recOff.Body.Bytes(), &rawOff))
		_, hasOff := rawOff["trajectory_capture"]
		assert.False(t, hasOff, "trajectory_capture must be omitted on 202 env-gather when experiment is off")

		// Experiment ON -> key present with resolved policy.
		setNoticeTestExperiments(t, srv, s, map[string]bool{
			experiments.TokenomicsTrajectoryCapture: true,
		})
		recOn := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
			Name:      "gather-exp-on",
			ProjectID: project.ID,
			Task:      "hello",
			GatherEnv: true,
		})
		require.Equal(t, http.StatusAccepted, recOn.Code, recOn.Body.String())
		var respOn CreateAgentResponse
		require.NoError(t, json.Unmarshal(recOn.Body.Bytes(), &respOn))
		require.NotNil(t, respOn.TrajectoryCapture)
		assert.Equal(t, telemetrycontract.TrajectoryCapturePolicyMetadataOnly, respOn.TrajectoryCapture.Policy)
		assert.False(t, respOn.TrajectoryCapture.EnforcedByHub)
	})
}
