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
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/knadh/koanf/providers/confmap"
	"github.com/knadh/koanf/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
)

func setTrajectoryCaptureExperiment(t *testing.T, srv *Server, s store.Store, on bool) {
	t.Helper()
	ctx := t.Context()
	ops := ensureTestOps(t, srv, s)
	expDoc, err := json.Marshal(map[string]any{
		"overrides": map[string]bool{
			experiments.TokenomicsTrajectoryCapture: on,
		},
	})
	require.NoError(t, err)
	_, err = s.UpsertHubSetting(ctx, "experiments", json.RawMessage(expDoc), "test", -1, "managed")
	require.NoError(t, err)
	_, err = ops.Refresh(ctx)
	require.NoError(t, err)
	ApplySnapshot(srv, ops.Snapshot())
}

func TestTrajectoryCapturePolicy_AdminSettingsDB(t *testing.T) {
	srv, s := testServer(t)
	ops := ensureTestOps(t, srv, s)

	putConfig := func(body string) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		srv.handlePutServerConfigDB(rr, adminRequest(http.MethodPut, "/api/v1/admin/server-config", body), ops)
		return rr
	}

	// Default: allow_project_opt_out is true.
	assert.True(t, srv.trajectoryAllowProjectOptOut())

	// Experiment off: setting allow_project_opt_out=false is refused with 412.
	rr := putConfig(`{"telemetry":{"trajectory":{"allow_project_opt_out":false}}}`)
	require.Equal(t, http.StatusPreconditionFailed, rr.Code, rr.Body.String())
	assert.Contains(t, rr.Body.String(), ErrCodeExperimentDisabled)
	assert.True(t, srv.trajectoryAllowProjectOptOut())

	// Turn experiment on.
	setTrajectoryCaptureExperiment(t, srv, s, true)
	assert.True(t, srv.trajectoryAllowProjectOptOut())

	// Setting allow_project_opt_out=false succeeds and live-applies.
	rr = putConfig(`{"telemetry":{"trajectory":{"allow_project_opt_out":false}}}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.False(t, srv.trajectoryAllowProjectOptOut())
	require.NotNil(t, srv.config.TrajectoryAllowProjectOptOut)
	assert.False(t, *srv.config.TrajectoryAllowProjectOptOut)

	got := getServerConfigDB(t, srv, ops)
	require.NotNil(t, got.Telemetry)
	require.NotNil(t, got.Telemetry.Trajectory)
	require.NotNil(t, got.Telemetry.Trajectory.AllowProjectOptOut)
	assert.False(t, *got.Telemetry.Trajectory.AllowProjectOptOut)

	// Flipping back to true succeeds and unlocks.
	rr = putConfig(`{"telemetry":{"trajectory":{"allow_project_opt_out":true}}}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.True(t, srv.trajectoryAllowProjectOptOut())

	got = getServerConfigDB(t, srv, ops)
	require.NotNil(t, got.Telemetry)
	require.NotNil(t, got.Telemetry.Trajectory)
	require.NotNil(t, got.Telemetry.Trajectory.AllowProjectOptOut)
	assert.True(t, *got.Telemetry.Trajectory.AllowProjectOptOut)
}

func TestTrajectoryCapturePolicy_DBOnlyBootstrapIgnored(t *testing.T) {
	srv, s := testServer(t)
	bootstrap := koanf.New(".")
	require.NoError(t, bootstrap.Load(confmap.Provider(map[string]any{
		"telemetry.trajectory.allow_project_opt_out": false,
	}, "."), nil))
	ops := NewOperationalSettings(s, bootstrap, emptyKoanf())
	_, err := ops.Refresh(t.Context())
	require.NoError(t, err)
	ops.server = srv
	srv.SetOperationalSettings(ops)
	ApplySnapshot(srv, ops.Snapshot())

	setTrajectoryCaptureExperiment(t, srv, s, true)
	assert.Nil(t, ops.Snapshot().TrajectoryAllowProjectOptOut, "bootstrap must not populate DB-only trajectory lock")
	assert.True(t, srv.trajectoryAllowProjectOptOut(), "bootstrap allow_project_opt_out=false must not lock opt-outs")
}

func TestTrajectoryCapturePolicy_ProjectSettingsAndAgentReadSurface(t *testing.T) {
	srv, s := testServer(t)
	ops := ensureTestOps(t, srv, s)
	ctx := t.Context()

	proj := &store.Project{
		ID:      api.NewUUID(),
		Name:    "traj-policy-proj",
		Slug:    "traj-policy-proj",
		OwnerID: DevUserID,
	}
	require.NoError(t, s.CreateProject(ctx, proj))

	agent := &store.Agent{
		ID:        api.NewUUID(),
		Slug:      "traj-policy-agent",
		Name:      "traj-policy-agent",
		ProjectID: proj.ID,
		RunID:     "run-" + api.NewUUID(),
		OwnerID:   DevUserID,
		CreatedBy: DevUserID,
		Template:  "developer",
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	getProjectSettingsRaw := func() (int, map[string]any) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/"+proj.ID+"/settings", nil)
		req.Header.Set("Authorization", "Bearer "+testDevToken)
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, req)
		var out map[string]any
		_ = json.Unmarshal(rr.Body.Bytes(), &out)
		return rr.Code, out
	}

	putProjectSettings := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPut, "/api/v1/projects/"+proj.ID+"/settings", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+testDevToken)
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, req)
		return rr
	}

	getResolvedRaw := func() (int, map[string]any) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/"+proj.ID+"/settings/resolved", nil)
		req.Header.Set("Authorization", "Bearer "+testDevToken)
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, req)
		var out map[string]any
		_ = json.Unmarshal(rr.Body.Bytes(), &out)
		return rr.Code, out
	}

	getAgentRaw := func() (int, map[string]any) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/"+agent.ID, nil)
		req.Header.Set("Authorization", "Bearer "+testDevToken)
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, req)
		var out map[string]any
		_ = json.Unmarshal(rr.Body.Bytes(), &out)
		return rr.Code, out
	}

	putAdminLock := func(allow bool) {
		body, err := json.Marshal(map[string]any{
			"telemetry": map[string]any{
				"trajectory": map[string]any{
					"allow_project_opt_out": allow,
				},
			},
		})
		require.NoError(t, err)
		rr := httptest.NewRecorder()
		srv.handlePutServerConfigDB(rr, adminRequest(http.MethodPut, "/api/v1/admin/server-config", string(body)), ops)
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	}

	// 1. Experiment OFF: PUT trajectoryCapture rejected with 412; GET hides trajectory_capture.
	rr := putProjectSettings(`{"trajectoryCapture":"off"}`)
	require.Equal(t, http.StatusPreconditionFailed, rr.Code, rr.Body.String())
	assert.Contains(t, rr.Body.String(), ErrCodeExperimentDisabled)

	code, settingsResp := getProjectSettingsRaw()
	require.Equal(t, http.StatusOK, code)
	assert.NotContains(t, settingsResp, "trajectory_capture")
	assert.NotContains(t, settingsResp, "trajectoryCapture")

	code, resolvedResp := getResolvedRaw()
	require.Equal(t, http.StatusOK, code)
	assert.NotContains(t, resolvedResp, "trajectory_capture")

	code, agentResp := getAgentRaw()
	require.Equal(t, http.StatusOK, code)
	assert.NotContains(t, agentResp, "trajectory_capture")

	// 2. Turn experiment ON: default policy is "full", enforced_by_hub=false.
	setTrajectoryCaptureExperiment(t, srv, s, true)

	code, settingsResp = getProjectSettingsRaw()
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, map[string]any{"policy": "full", "enforced_by_hub": false}, settingsResp["trajectory_capture"])

	code, resolvedResp = getResolvedRaw()
	require.Equal(t, http.StatusOK, code)
	assert.NotContains(t, resolvedResp, "trajectory_capture")

	code, agentResp = getAgentRaw()
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, map[string]any{"policy": "full", "enforced_by_hub": false}, agentResp["trajectory_capture"])

	// 3. Invalid value rejected with 400.
	rr = putProjectSettings(`{"trajectoryCapture":"bogus"}`)
	require.Equal(t, http.StatusBadRequest, rr.Code, rr.Body.String())

	// 4. Opt-out to metadata_only accepted while lock is true.
	rr = putProjectSettings(`{"trajectoryCapture":"metadata_only"}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var putResp map[string]any
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &putResp))
	assert.Equal(t, "metadata_only", putResp["trajectoryCapture"])
	assert.Equal(t, map[string]any{"policy": "metadata_only", "enforced_by_hub": false}, putResp["trajectory_capture"])

	code, agentResp = getAgentRaw()
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, map[string]any{"policy": "metadata_only", "enforced_by_hub": false}, agentResp["trajectory_capture"])

	// Opt-out to off accepted while lock is true.
	rr = putProjectSettings(`{"trajectoryCapture":"off"}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	// 5. Hub flips allow_project_opt_out to false:
	//    - stored opt-out ("off") becomes effective "full" with enforced_by_hub=true everywhere;
	//    - echoing the already-stored opt-out ("off") with an unrelated field passes (O1);
	//    - a different or new opt-out write gets 403 capture_opt_out_disallowed;
	//    - unrelated project settings PUT still succeeds;
	//    - setting "full" or "" still succeeds.
	putAdminLock(false)

	code, settingsResp = getProjectSettingsRaw()
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, map[string]any{"policy": "full", "enforced_by_hub": true}, settingsResp["trajectory_capture"])

	code, resolvedResp = getResolvedRaw()
	require.Equal(t, http.StatusOK, code)
	assert.NotContains(t, resolvedResp, "trajectory_capture")
	resolvedMap := resolvedResp["settings"].(map[string]any)
	trajEntry := resolvedMap[projectSettingTrajectoryCapture].(map[string]any)
	assert.Equal(t, "hub", trajEntry["lockedBy"])
	assert.Equal(t, "full", trajEntry["effective"])
	assert.Equal(t, "off", trajEntry["storedValue"])

	code, agentResp = getAgentRaw()
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, map[string]any{"policy": "full", "enforced_by_hub": true}, agentResp["trajectory_capture"])

	// Echoing the already-stored "off" value alongside another field passes (O1),
	// while the effective policy remains "full" (enforced_by_hub=true).
	rr = putProjectSettings(`{"trajectoryCapture":"off","defaultTemplate":"x"}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var echoResp map[string]any
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &echoResp))
	assert.Equal(t, "x", echoResp["defaultTemplate"])
	assert.Equal(t, map[string]any{"policy": "full", "enforced_by_hub": true}, echoResp["trajectory_capture"])

	// Changing to a different opt-out while locked returns 403 capture_opt_out_disallowed.
	rr = putProjectSettings(`{"trajectoryCapture":"metadata_only"}`)
	require.Equal(t, http.StatusForbidden, rr.Code, rr.Body.String())
	var errResp ErrorResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &errResp))
	assert.Equal(t, telemetrycontract.TrajectoryErrCaptureOptOutDisallowed, errResp.Error.Code)

	// Unrelated partial PUT passes while locked.
	rr = putProjectSettings(`{"defaultTemplate":"developer"}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	// Explicit "full" passes while locked, and a subsequent new opt-out (full -> off/metadata_only) gets 403.
	rr = putProjectSettings(`{"trajectoryCapture":"full"}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	for _, optOut := range []string{"off", "metadata_only"} {
		rr = putProjectSettings(`{"trajectoryCapture":"` + optOut + `"}`)
		require.Equal(t, http.StatusForbidden, rr.Code, "optOut=%s body=%s", optOut, rr.Body.String())
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &errResp))
		assert.Equal(t, telemetrycontract.TrajectoryErrCaptureOptOutDisallowed, errResp.Error.Code)
	}

	// 6. Experiment turned back OFF: stored annotation is hidden in both /settings and /settings/resolved (O2).
	setTrajectoryCaptureExperiment(t, srv, s, false)
	code, settingsResp = getProjectSettingsRaw()
	require.Equal(t, http.StatusOK, code)
	assert.NotContains(t, settingsResp, "trajectory_capture")
	assert.NotContains(t, settingsResp, "trajectoryCapture")

	code, resolvedResp = getResolvedRaw()
	require.Equal(t, http.StatusOK, code)
	assert.NotContains(t, resolvedResp, "trajectory_capture")
	resolvedProject := resolvedResp["project"].(map[string]any)
	assert.NotContains(t, resolvedProject, "trajectoryCapture")
	resolvedMap = resolvedResp["settings"].(map[string]any)
	trajEntry = resolvedMap[projectSettingTrajectoryCapture].(map[string]any)
	assert.Equal(t, false, trajEntry["projectSet"])
	assert.Nil(t, trajEntry["projectValue"])
	assert.Nil(t, trajEntry["lockedBy"])
}

func TestTrajectoryCapturePolicy_PrepareEnforcement(t *testing.T) {
	srv, s := testServer(t)
	ops := ensureTestOps(t, srv, s)
	ctx := context.Background()

	loc, err := storage.NewLocal(storage.Config{
		Provider:  storage.ProviderLocal,
		Bucket:    "local",
		LocalPath: t.TempDir(),
	})
	require.NoError(t, err)
	srv.SetStorage(loc)

	proj := &store.Project{
		ID:      api.NewUUID(),
		Name:    "traj-prep-proj",
		Slug:    "traj-prep-proj",
		OwnerID: DevUserID,
	}
	require.NoError(t, s.CreateProject(ctx, proj))

	runID := "run-" + api.NewUUID()
	agent := &store.Agent{
		ID:        api.NewUUID(),
		Slug:      "traj-prep-agent",
		Name:      "traj-prep-agent",
		ProjectID: proj.ID,
		RunID:     runID,
		OwnerID:   DevUserID,
		CreatedBy: DevUserID,
		AppliedConfig: &store.AgentAppliedConfig{
			Harness:       "claude",
			HarnessConfig: "claude-default",
		},
		Template: "developer",
	}
	require.NoError(t, s.CreateAgent(ctx, agent))
	_, err = s.EnsureRunAttribution(ctx, &store.RunAttribution{
		RunID:         runID,
		AgentID:       agent.ID,
		AgentSlug:     agent.Slug,
		ProjectID:     proj.ID,
		HubID:         srv.usageLedgerHubID(),
		Harness:       "claude",
		HarnessConfig: "claude-default",
		Template:      "developer",
	})
	require.NoError(t, err)

	agentToken, cred, err := srv.agentTokenService.SignAgentToken(AgentTokenGrant{
		AgentID:   agent.ID,
		ProjectID: proj.ID,
		Scopes:    []AgentTokenScope{ScopeAgentStatusUpdate},
	}, runID)
	require.NoError(t, err)
	require.NoError(t, s.CreateAgentCredential(ctx, cred))

	doPrepare := func(byteStart int64) *httptest.ResponseRecorder {
		raw := []byte("{\"type\":\"user\",\"text\":\"hello\"}\n")
		gz, rawHex, gzHex := gzipAndHash(t, raw)
		payload, err := json.Marshal(telemetrycontract.TrajectorySegmentPrepareRequest{
			Harness:            "claude",
			NativeRelativePath: ".claude/projects/-workspace/policy-test.jsonl",
			ArtifactKind:       telemetrycontract.TrajectoryArtifactKindJSONLAppend,
			Generation:         0,
			ByteStart:          byteStart,
			ByteEnd:            byteStart + int64(len(raw)),
			RawSHA256:          rawHex,
			CompressedSize:     int64(len(gz)),
			CompressedSHA256:   gzHex,
			ContentEncoding:    telemetrycontract.TrajectoryContentEncodingGzip,
		})
		require.NoError(t, err)
		req := httptest.NewRequest(http.MethodPost, telemetrycontract.TrajectorySegmentsPrepareAgentPath(agent.ID), bytes.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Scion-Agent-Token", agentToken)
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, req)
		return rr
	}

	setProjectPolicy := func(policy string) {
		req := httptest.NewRequest(http.MethodPut, "/api/v1/projects/"+proj.ID+"/settings", strings.NewReader(`{"trajectoryCapture":"`+policy+`"}`))
		req.Header.Set("Authorization", "Bearer "+testDevToken)
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, req)
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	}

	setAdminLock := func(allow bool) {
		body, err := json.Marshal(map[string]any{
			"telemetry": map[string]any{
				"trajectory": map[string]any{
					"allow_project_opt_out": allow,
				},
			},
		})
		require.NoError(t, err)
		rr := httptest.NewRecorder()
		srv.handlePutServerConfigDB(rr, adminRequest(http.MethodPut, "/api/v1/admin/server-config", string(body)), ops)
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	}

	// Experiment off: prepare returns 404.
	rr := doPrepare(0)
	require.Equal(t, http.StatusNotFound, rr.Code, rr.Body.String())

	// Experiment on, default policy (full): prepare returns 200.
	setTrajectoryCaptureExperiment(t, srv, s, true)
	rr = doPrepare(0)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	// Project sets off -> prepare returns 403 capture_disabled.
	setProjectPolicy(telemetrycontract.TrajectoryCapturePolicyOff)
	rr = doPrepare(100)
	require.Equal(t, http.StatusForbidden, rr.Code, rr.Body.String())
	var errResp ErrorResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &errResp))
	assert.Equal(t, telemetrycontract.TrajectoryErrCaptureDisabled, errResp.Error.Code)

	// Project sets metadata_only -> prepare returns 403 capture_metadata_only.
	setProjectPolicy(telemetrycontract.TrajectoryCapturePolicyMetadataOnly)
	rr = doPrepare(200)
	require.Equal(t, http.StatusForbidden, rr.Code, rr.Body.String())
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &errResp))
	assert.Equal(t, telemetrycontract.TrajectoryErrCaptureMetadataOnly, errResp.Error.Code)

	// Hub flips allow_project_opt_out=false -> stored metadata_only becomes effective full -> prepare returns 200.
	setAdminLock(false)
	rr = doPrepare(300)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
}

type failingGetProjectStore struct {
	store.Store
	fault *storeFaultSwitch
}

func (f *failingGetProjectStore) GetProject(ctx context.Context, id string) (*store.Project, error) {
	if f.fault.Active() {
		return nil, errors.New("injected GetProject failure")
	}
	return f.Store.GetProject(ctx, id)
}

func TestTrajectoryCapturePolicy_AgentGetOmitsOnGetProjectError(t *testing.T) {
	srv, s, _, fault := testServerWithStoreFault(t, func(inner store.Store, f *storeFaultSwitch) *failingGetProjectStore {
		return &failingGetProjectStore{Store: inner, fault: f}
	})
	setTrajectoryCaptureExperiment(t, srv, s, true)
	ctx := t.Context()

	proj := &store.Project{
		ID:      api.NewUUID(),
		Name:    "traj-fault-proj",
		Slug:    "traj-fault-proj",
		OwnerID: DevUserID,
		Annotations: map[string]string{
			projectSettingTrajectoryCapture: telemetrycontract.TrajectoryCapturePolicyOff,
		},
	}
	require.NoError(t, s.CreateProject(ctx, proj))

	agent := &store.Agent{
		ID:        api.NewUUID(),
		Slug:      "traj-fault-agent",
		Name:      "traj-fault-agent",
		ProjectID: proj.ID,
		RunID:     "run-" + api.NewUUID(),
		OwnerID:   DevUserID,
		CreatedBy: DevUserID,
		Template:  "developer",
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	// Arm fault so GetProject fails on single-agent GET: trajectory_capture must be omitted.
	fault.Arm()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/"+agent.ID, nil)
	req.Header.Set("Authorization", "Bearer "+testDevToken)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	var agentResp map[string]any
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &agentResp))
	assert.NotContains(t, agentResp, "trajectory_capture")
}
