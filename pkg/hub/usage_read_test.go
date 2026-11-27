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
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
)

func bindProjectRoleForUsageReadTest(t *testing.T, s store.Store, userID, projectID, roleName string) {
	t.Helper()
	ctx := context.Background()
	role, err := s.GetRoleDefinitionByName(ctx, roleName, store.RoleScopeProject)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		ID:               uuid.NewString(),
		RoleDefinitionID: role.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      userID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)
}

func doUserUsageReadRequest(t *testing.T, srv *Server, path string, user *store.User) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if user != nil {
		token, _, _, err := srv.userTokenService.GenerateTokenPair(
			user.ID, user.Email, user.DisplayName, user.Role, ClientTypeWeb,
		)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func TestAgentUsageRead_ExperimentOffReturns404(t *testing.T) {
	ctx := context.Background()
	srv, s := testServer(t)
	srv.SetHubID("hub-usage-read-off")
	setUsageLedgerExperimentForTest(t, srv, false)

	owner := &store.User{ID: uuid.NewString(), Email: "usage-off@example.com", DisplayName: "Owner", Role: "member", Status: "active"}
	require.NoError(t, s.CreateUser(ctx, owner))
	proj := &store.Project{ID: tid("usage-read-off-proj"), Name: "usage-read-off", Slug: "usage-read-off"}
	require.NoError(t, s.CreateProject(ctx, proj))
	ag := &store.Agent{
		ID:        tid("usage-read-off-agent"),
		Slug:      "usage-read-off-agent",
		Name:      "usage-read-off-agent",
		ProjectID: proj.ID,
		OwnerID:   owner.ID,
		CreatedBy: owner.ID,
		Phase:     string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, ag))

	rec := doUserUsageReadRequest(t, srv, "/api/v1/agents/"+ag.ID+"/usage", owner)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestAgentUsageRead_Authz(t *testing.T) {
	ctx := context.Background()
	srv, s := testServer(t)
	srv.SetHubID("hub-usage-authz")
	setUsageLedgerExperimentForTest(t, srv, true)
	srv.reconcileUsageLedger(ctx)

	owner := &store.User{ID: uuid.NewString(), Email: "usage-owner@example.com", DisplayName: "Owner", Role: "member", Status: "active"}
	member := &store.User{ID: uuid.NewString(), Email: "usage-member@example.com", DisplayName: "Member", Role: "member", Status: "active"}
	outsider := &store.User{ID: uuid.NewString(), Email: "usage-outsider@example.com", DisplayName: "Outsider", Role: "member", Status: "active"}
	require.NoError(t, s.CreateUser(ctx, owner))
	require.NoError(t, s.CreateUser(ctx, member))
	require.NoError(t, s.CreateUser(ctx, outsider))

	proj := &store.Project{ID: tid("usage-authz-proj"), Name: "usage-authz", Slug: "usage-authz"}
	require.NoError(t, s.CreateProject(ctx, proj))

	bindProjectRoleForUsageReadTest(t, s, owner.ID, proj.ID, "project-owner")
	bindProjectRoleForUsageReadTest(t, s, member.ID, proj.ID, "project-member")

	ag1 := &store.Agent{
		ID:        tid("usage-authz-a1"),
		Slug:      "a1",
		Name:      "a1",
		ProjectID: proj.ID,
		OwnerID:   owner.ID,
		CreatedBy: owner.ID,
		Phase:     string(state.PhaseRunning),
	}
	ag2 := &store.Agent{
		ID:        tid("usage-authz-a2"),
		Slug:      "a2",
		Name:      "a2",
		ProjectID: proj.ID,
		OwnerID:   owner.ID,
		CreatedBy: owner.ID,
		Phase:     string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, ag1))
	require.NoError(t, s.CreateAgent(ctx, ag2))

	path := "/api/v1/agents/" + ag1.ID + "/usage"

	// Unauthenticated -> 401
	rec := doUserUsageReadRequest(t, srv, path, nil)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)

	// Owner -> 200
	rec = doUserUsageReadRequest(t, srv, path, owner)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// Project member -> 200
	rec = doUserUsageReadRequest(t, srv, path, member)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// Unrelated user -> 403
	rec = doUserUsageReadRequest(t, srv, path, outsider)
	assert.Equal(t, http.StatusForbidden, rec.Code)

	// Agent token for self (ag1) with project:read -> 200
	tok1 := srv.generateRecordedAgentToken(t, ag1.ID, proj.ID, []AgentTokenScope{ScopeProjectRead}, nil)
	rec = doAgentUsageRequest(t, srv, http.MethodGet, path, tok1, nil)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// Agent token for another agent (ag2 in the same project, even with project:read) -> 403
	tok2 := srv.generateRecordedAgentToken(t, ag2.ID, proj.ID, []AgentTokenScope{ScopeProjectRead}, nil)
	rec = doAgentUsageRequest(t, srv, http.MethodGet, path, tok2, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code)
}

func TestAgentUsageRead_NullVsZeroAndMixed(t *testing.T) {
	ctx := context.Background()
	srv, s := testServer(t)
	srv.SetHubID("hub-null-zero")
	setUsageLedgerExperimentForTest(t, srv, true)
	srv.reconcileUsageLedger(ctx)

	owner := &store.User{ID: uuid.NewString(), Email: "nz-owner@example.com", DisplayName: "Owner", Role: "member", Status: "active"}
	require.NoError(t, s.CreateUser(ctx, owner))
	proj := &store.Project{ID: tid("nz-proj"), Name: "nz", Slug: "nz"}
	require.NoError(t, s.CreateProject(ctx, proj))
	bindProjectRoleForUsageReadTest(t, s, owner.ID, proj.ID, "project-owner")

	// Agent 1: calls-only (all token buckets NULL) -> every token bucket must serialize as JSON null, never 0.
	agNull := &store.Agent{ID: tid("nz-agent-null"), Slug: "nz-null", Name: "nz-null", ProjectID: proj.ID, OwnerID: owner.ID, CreatedBy: owner.ID, Phase: string(state.PhaseRunning)}
	require.NoError(t, s.CreateAgent(ctx, agNull))
	_, err := s.SetAgentRunID(ctx, agNull.ID, "run-nz-null", nil)
	require.NoError(t, err)
	tokNull := srv.generateRecordedAgentToken(t, agNull.ID, proj.ID, []AgentTokenScope{ScopeAgentStatusUpdate}, nil)

	now := time.Now().UTC().Truncate(time.Microsecond)
	evCallsOnly := makeWireUsageEvent("hub-null-zero", agNull.ID, proj.ID, "run-nz-null", "evt-nz-1", 1, now, 0, 0)
	evCallsOnly.Input, evCallsOnly.Output, evCallsOnly.InputDerivation = nil, nil, ""
	evCallsOnly.TokenStatus = telemetrycontract.TokenStatusNotReported
	rec := doAgentUsageRequest(t, srv, http.MethodPost, telemetrycontract.UsageIngestAgentPath(agNull.ID), tokNull, telemetrycontract.UsageIngestRequest{
		Events:        []telemetrycontract.UsageEvent{evCallsOnly},
		RunID:         "run-nz-null",
		ProducerEpoch: "ep-1",
		ProducerSeq:   1,
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	readRec := doUserUsageReadRequest(t, srv, "/api/v1/agents/"+agNull.ID+"/usage", owner)
	require.Equal(t, http.StatusOK, readRec.Code, readRec.Body.String())
	var raw map[string]any
	require.NoError(t, json.Unmarshal(readRec.Body.Bytes(), &raw))
	tokMap := raw["tokens"].(map[string]any)
	for _, field := range []string{"input", "output", "cache_read", "cache_write", "cache_write_1h", "reasoning", "summable_tokens"} {
		v, exists := tokMap[field]
		assert.True(t, exists, "field %s must be present as explicit null", field)
		assert.Nil(t, v, "field %s must be null, not 0", field)
	}
	assert.Equal(t, float64(1), raw["calls"])
	assert.Equal(t, float64(1), raw["events"])
	assert.Equal(t, float64(1), raw["null_token_events"])

	// Agent 2: measured 0 (cache_read=0, reasoning=0) + NULL (cache_write=nil, cache_write_1h=nil)
	// plus a second all-NULL event (mixed known+unknown flagged via null_token_events=1).
	agMix := &store.Agent{ID: tid("nz-agent-mix"), Slug: "nz-mix", Name: "nz-mix", ProjectID: proj.ID, OwnerID: owner.ID, CreatedBy: owner.ID, Phase: string(state.PhaseRunning)}
	require.NoError(t, s.CreateAgent(ctx, agMix))
	_, err = s.SetAgentRunID(ctx, agMix.ID, "run-nz-mix", nil)
	require.NoError(t, err)
	tokMix := srv.generateRecordedAgentToken(t, agMix.ID, proj.ID, []AgentTokenScope{ScopeAgentStatusUpdate}, nil)

	evKnown := makeWireUsageEvent("hub-null-zero", agMix.ID, proj.ID, "run-nz-mix", "evt-mix-1", 1, now, 10, 5)
	evKnown.CacheRead = usageInt64p(0)
	evKnown.CacheWrite = nil
	evKnown.CacheWrite1h = nil
	evKnown.Reasoning = usageInt64p(0)

	evUnknown := makeWireUsageEvent("hub-null-zero", agMix.ID, proj.ID, "run-nz-mix", "evt-mix-2", 2, now, 0, 0)
	evUnknown.Input, evUnknown.Output, evUnknown.InputDerivation = nil, nil, ""
	evUnknown.TokenStatus = telemetrycontract.TokenStatusNotReported

	rec = doAgentUsageRequest(t, srv, http.MethodPost, telemetrycontract.UsageIngestAgentPath(agMix.ID), tokMix, telemetrycontract.UsageIngestRequest{
		Events:        []telemetrycontract.UsageEvent{evKnown, evUnknown},
		RunID:         "run-nz-mix",
		ProducerEpoch: "ep-1",
		ProducerSeq:   2,
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	readRec = doUserUsageReadRequest(t, srv, "/api/v1/agents/"+agMix.ID+"/usage", owner)
	require.Equal(t, http.StatusOK, readRec.Code, readRec.Body.String())
	var resp AgentUsageResponse
	require.NoError(t, json.Unmarshal(readRec.Body.Bytes(), &resp))
	require.NotNil(t, resp.Tokens.Input)
	assert.Equal(t, int64(10), *resp.Tokens.Input)
	require.NotNil(t, resp.Tokens.Output)
	assert.Equal(t, int64(5), *resp.Tokens.Output)
	require.NotNil(t, resp.Tokens.CacheRead, "measured 0 must stay 0")
	assert.Equal(t, int64(0), *resp.Tokens.CacheRead)
	assert.Nil(t, resp.Tokens.CacheWrite, "all-NULL cache_write must stay nil")
	assert.Nil(t, resp.Tokens.CacheWrite1h, "all-NULL cache_write_1h must stay nil")
	require.NotNil(t, resp.Tokens.Reasoning, "measured 0 reasoning must stay 0")
	assert.Equal(t, int64(0), *resp.Tokens.Reasoning)
	require.NotNil(t, resp.Tokens.SummableTokens)
	assert.Equal(t, int64(15), *resp.Tokens.SummableTokens)
	assert.Equal(t, int64(2), resp.Events)
	assert.Equal(t, int64(1), resp.NullTokenEvents, "mixed known+unknown flagged via null_token_events")
}

func TestAgentUsageRead_WindowFilteringAndMultiModel(t *testing.T) {
	ctx := context.Background()
	srv, s := testServer(t)
	srv.SetHubID("hub-window-mm")
	setUsageLedgerExperimentForTest(t, srv, true)
	srv.reconcileUsageLedger(ctx)

	owner := &store.User{ID: uuid.NewString(), Email: "wm-owner@example.com", DisplayName: "Owner", Role: "member", Status: "active"}
	require.NoError(t, s.CreateUser(ctx, owner))
	proj := &store.Project{ID: tid("wm-proj"), Name: "wm", Slug: "wm"}
	require.NoError(t, s.CreateProject(ctx, proj))
	bindProjectRoleForUsageReadTest(t, s, owner.ID, proj.ID, "project-owner")
	ag := &store.Agent{ID: tid("wm-agent"), Slug: "wm-agent", Name: "wm-agent", ProjectID: proj.ID, OwnerID: owner.ID, CreatedBy: owner.ID, Phase: string(state.PhaseRunning)}
	require.NoError(t, s.CreateAgent(ctx, ag))
	_, err := s.SetAgentRunID(ctx, ag.ID, "run-wm-1", nil)
	require.NoError(t, err)
	tok := srv.generateRecordedAgentToken(t, ag.ID, proj.ID, []AgentTokenScope{ScopeAgentStatusUpdate}, nil)

	now := time.Now().UTC().Truncate(time.Microsecond)
	tenDaysAgo := now.Add(-10 * 24 * time.Hour)

	evOld := makeWireUsageEvent("hub-window-mm", ag.ID, proj.ID, "run-wm-1", "evt-wm-old", 1, tenDaysAgo, 100, 40)
	evOld.Model = "claude-haiku-4-5"
	evNew1 := makeWireUsageEvent("hub-window-mm", ag.ID, proj.ID, "run-wm-1", "evt-wm-new1", 2, now, 200, 80)
	evNew1.Model = "claude-sonnet-4-6"
	evNew2 := makeWireUsageEvent("hub-window-mm", ag.ID, proj.ID, "run-wm-1", "evt-wm-new2", 3, now, 50, 20)
	evNew2.Model = "claude-haiku-4-5"
	evNew2.Status = telemetrycontract.StatusError

	rec := doAgentUsageRequest(t, srv, http.MethodPost, telemetrycontract.UsageIngestAgentPath(ag.ID), tok, telemetrycontract.UsageIngestRequest{
		Events:        []telemetrycontract.UsageEvent{evOld, evNew1, evNew2},
		RunID:         "run-wm-1",
		ProducerEpoch: "ep-1",
		ProducerSeq:   3,
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// Default 30d window includes both days and both models.
	readAll := doUserUsageReadRequest(t, srv, "/api/v1/agents/"+ag.ID+"/usage", owner)
	require.Equal(t, http.StatusOK, readAll.Code)
	var respAll AgentUsageResponse
	require.NoError(t, json.Unmarshal(readAll.Body.Bytes(), &respAll))
	assert.Equal(t, int64(3), respAll.Calls)
	assert.Equal(t, int64(1), respAll.ErrorCalls)
	require.NotNil(t, respAll.Tokens.SummableTokens)
	assert.Equal(t, int64(490), *respAll.Tokens.SummableTokens)
	require.Len(t, respAll.ByModel, 2)
	// Sorted by summable tokens DESC: sonnet (280) then haiku (210)
	assert.Equal(t, "claude-sonnet-4-6", respAll.ByModel[0].Model)
	assert.Equal(t, int64(280), *respAll.ByModel[0].Tokens.SummableTokens)
	assert.Equal(t, "claude-haiku-4-5", respAll.ByModel[1].Model)
	assert.Equal(t, int64(210), *respAll.ByModel[1].Tokens.SummableTokens)
	assert.Equal(t, int64(1), respAll.ByModel[1].ErrorCalls)

	// Narrow 2d window excludes tenDaysAgo event.
	readRecent := doUserUsageReadRequest(t, srv, "/api/v1/agents/"+ag.ID+"/usage?since=2d", owner)
	require.Equal(t, http.StatusOK, readRecent.Code)
	var respRecent AgentUsageResponse
	require.NoError(t, json.Unmarshal(readRecent.Body.Bytes(), &respRecent))
	assert.Equal(t, int64(2), respRecent.Calls)
	require.NotNil(t, respRecent.Tokens.SummableTokens)
	assert.Equal(t, int64(350), *respRecent.Tokens.SummableTokens)

	// Invalid query params -> 400
	badSince := doUserUsageReadRequest(t, srv, "/api/v1/agents/"+ag.ID+"/usage?since=not-a-date", owner)
	assert.Equal(t, http.StatusBadRequest, badSince.Code)

	inverted := doUserUsageReadRequest(t, srv, "/api/v1/agents/"+ag.ID+"/usage?since=2026-11-25&until=2026-11-20", owner)
	assert.Equal(t, http.StatusBadRequest, inverted.Code)

	// Overflow / over-cap relative day windows -> 400
	overCap := doUserUsageReadRequest(t, srv, "/api/v1/agents/"+ag.ID+"/usage?since=3651d", owner)
	assert.Equal(t, http.StatusBadRequest, overCap.Code)
	overflowDays := doUserUsageReadRequest(t, srv, "/api/v1/agents/"+ag.ID+"/usage?since=200000d", owner)
	assert.Equal(t, http.StatusBadRequest, overflowDays.Code)

	// since=7d reports exactly 7 inclusive UTC calendar days: [untilDay - 6d 00:00:00Z, untilDay 23:59:59.999999Z].
	read7d := doUserUsageReadRequest(t, srv, "/api/v1/agents/"+ag.ID+"/usage?since=7d&until=2026-11-25", owner)
	require.Equal(t, http.StatusOK, read7d.Code, read7d.Body.String())
	var resp7d AgentUsageResponse
	require.NoError(t, json.Unmarshal(read7d.Body.Bytes(), &resp7d))
	assert.Equal(t, time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC), resp7d.Window.Since)
	assert.Equal(t, time.Date(2026, 11, 25, 23, 59, 59, 999999000, time.UTC), resp7d.Window.Until)
}

func TestAgentUsageRead_MeasurementStatus(t *testing.T) {
	ctx := context.Background()
	srv, s := testServer(t)
	srv.SetHubID("hub-status-test")
	setUsageLedgerExperimentForTest(t, srv, true)
	srv.reconcileUsageLedger(ctx)

	owner := &store.User{ID: uuid.NewString(), Email: "st-owner@example.com", DisplayName: "Owner", Role: "member", Status: "active"}
	require.NoError(t, s.CreateUser(ctx, owner))
	proj := &store.Project{ID: tid("st-proj"), Name: "st", Slug: "st"}
	require.NoError(t, s.CreateProject(ctx, proj))
	bindProjectRoleForUsageReadTest(t, s, owner.ID, proj.ID, "project-owner")
	ag := &store.Agent{ID: tid("st-agent"), Slug: "st-agent", Name: "st-agent", ProjectID: proj.ID, OwnerID: owner.ID, CreatedBy: owner.ID, Phase: string(state.PhaseRunning)}
	require.NoError(t, s.CreateAgent(ctx, ag))

	// 1. No Track 1 data -> unmeasured.
	rec := doUserUsageReadRequest(t, srv, "/api/v1/agents/"+ag.ID+"/usage", owner)
	require.Equal(t, http.StatusOK, rec.Code)
	var resp AgentUsageResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, MeasurementStatusUnmeasured, resp.MeasurementStatus)
	assert.Nil(t, resp.LastReceiptAt)

	// 2. Unit test deriveMeasurementStatus directly for DB-clock staleness, startup grace, and final_state="final".
	dbNow, err := s.DBNow(ctx)
	require.NoError(t, err)

	// Last receipt 4m ago (> 3 * 60s = 180s) on an open meter -> stale.
	assert.Equal(t, MeasurementStatusStale, deriveMeasurementStatus(measurementStatusInput{
		MeterHealth: []*store.MeterHealth{{
			RunID:         "r1",
			LastReceiptAt: dbNow.Add(-4 * time.Minute),
			FinalState:    store.MeterHealthFinalStateOpen,
		}},
		HasRunUsageEvents: true,
		DBNow:             dbNow,
	}))

	// Fresh receipt 30s ago on an open meter with events -> measured.
	assert.Equal(t, MeasurementStatusMeasured, deriveMeasurementStatus(measurementStatusInput{
		MeterHealth: []*store.MeterHealth{{
			RunID:         "r1",
			LastReceiptAt: dbNow.Add(-30 * time.Second),
			FinalState:    store.MeterHealthFinalStateOpen,
		}},
		HasRunUsageEvents: true,
		DBNow:             dbNow,
	}))

	// Cleanly closed run (final_state="final") with old receipt -> stays measured, never stale.
	assert.Equal(t, MeasurementStatusMeasured, deriveMeasurementStatus(measurementStatusInput{
		MeterHealth: []*store.MeterHealth{{
			RunID:         "r1",
			LastReceiptAt: dbNow.Add(-10 * time.Minute),
			FinalState:    store.MeterHealthFinalStateFinal,
		}},
		HasRunUsageEvents: true,
		DBNow:             dbNow,
	}))

	// 3. R3: previous run ingested events, then current run switches to run-p2
	// with no meter_health and no events -> must read "unmeasured" (both inside
	// and past the 60s startup grace), never "measured".
	_, err = s.SetAgentRunID(ctx, ag.ID, "run-p1", nil)
	require.NoError(t, err)
	tok := srv.generateRecordedAgentToken(t, ag.ID, proj.ID, []AgentTokenScope{ScopeAgentStatusUpdate}, nil)
	evP1 := makeWireUsageEvent("hub-status-test", ag.ID, proj.ID, "run-p1", "evt-st-p1", 1, dbNow, 20, 10)
	ingRec := doAgentUsageRequest(t, srv, http.MethodPost, telemetrycontract.UsageIngestAgentPath(ag.ID), tok, telemetrycontract.UsageIngestRequest{
		Events:        []telemetrycontract.UsageEvent{evP1},
		RunID:         "run-p1",
		ProducerEpoch: "ep-p1",
		ProducerSeq:   1,
	})
	require.Equal(t, http.StatusOK, ingRec.Code, ingRec.Body.String())

	// Switch agent to a brand new run-p2 (inside grace, no meter row, no events).
	_, err = s.SetAgentRunID(ctx, ag.ID, "run-p2", nil)
	require.NoError(t, err)
	recP2 := doUserUsageReadRequest(t, srv, "/api/v1/agents/"+ag.ID+"/usage", owner)
	require.Equal(t, http.StatusOK, recP2.Code)
	var respP2 AgentUsageResponse
	require.NoError(t, json.Unmarshal(recP2.Body.Bytes(), &respP2))
	assert.Equal(t, MeasurementStatusUnmeasured, respP2.MeasurementStatus, "new run without events/meter_health must not inherit measured from prior run")

	// Also verify current run past 60s startup grace with no meter row/events -> unmeasured.
	oldRunID := "run-p3-old-unmeasured"
	_, err = s.EnsureRunAttribution(ctx, &store.RunAttribution{
		RunID:      oldRunID,
		Generation: 3,
		HubID:      "hub-status-test",
		ProjectID:  proj.ID,
		AgentID:    ag.ID,
		Harness:    "claude",
		CreatedAt:  dbNow.Add(-10 * time.Minute),
	})
	require.NoError(t, err)
	_, err = s.SetAgentRunID(ctx, ag.ID, oldRunID, nil)
	require.NoError(t, err)
	recP3 := doUserUsageReadRequest(t, srv, "/api/v1/agents/"+ag.ID+"/usage", owner)
	require.Equal(t, http.StatusOK, recP3.Code)
	var respP3 AgentUsageResponse
	require.NoError(t, json.Unmarshal(recP3.Body.Bytes(), &respP3))
	assert.Equal(t, MeasurementStatusUnmeasured, respP3.MeasurementStatus, "current run past grace with no meter row/events must be unmeasured")

	// 4. End-to-end HTTP stale check using DB-stored old run_attribution + old open meter_health.
	staleRunID := "run-stale-e2e"
	_, err = s.EnsureRunAttribution(ctx, &store.RunAttribution{
		RunID:      staleRunID,
		Generation: 4,
		HubID:      "hub-status-test",
		ProjectID:  proj.ID,
		AgentID:    ag.ID,
		Harness:    "claude",
		CreatedAt:  dbNow.Add(-10 * time.Minute),
	})
	require.NoError(t, err)
	_, err = s.SetAgentRunID(ctx, ag.ID, staleRunID, nil)
	require.NoError(t, err)
	_, err = s.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
		AgentID: ag.ID,
		MeterHealth: &store.UsageMeterHealthUpdate{
			AgentID:       ag.ID,
			RunID:         staleRunID,
			ProducerEpoch: "ep-stale",
			LastReceiptAt: dbNow.Add(-5 * time.Minute),
			FinalState:    store.MeterHealthFinalStateOpen,
		},
	})
	require.NoError(t, err)

	recStale := doUserUsageReadRequest(t, srv, "/api/v1/agents/"+ag.ID+"/usage", owner)
	require.Equal(t, http.StatusOK, recStale.Code)
	var respStale AgentUsageResponse
	require.NoError(t, json.Unmarshal(recStale.Body.Bytes(), &respStale))
	assert.Equal(t, MeasurementStatusStale, respStale.MeasurementStatus)
	require.NotNil(t, respStale.LastReceiptAt)
}

// TestAgentUsageRead_DayAlignedWindowAndNoZeroFabrication verifies R2 and O1:
//   - Sub-day since/until snaps to inclusive UTC day boundaries and reports the
//     snapped effective window in response.window;
//   - Events earlier on the first day of a sub-day since or default 30d window
//     are covered by both rollups and presence so NULL buckets stay null (never 0);
//   - Rollups with no presence coverage (past retention) return null for zero-sum
//     buckets, never fabricated 0;
//   - Mixed known+unknown per-bucket events surface in tokens.unknown_events (O1).
func TestAgentUsageRead_DayAlignedWindowAndNoZeroFabrication(t *testing.T) {
	ctx := context.Background()
	srv, s := testServer(t)
	srv.SetHubID("hub-r2-test")
	setUsageLedgerExperimentForTest(t, srv, true)
	srv.reconcileUsageLedger(ctx)

	owner := &store.User{ID: uuid.NewString(), Email: "r2-owner@example.com", DisplayName: "R2", Role: "member", Status: "active"}
	require.NoError(t, s.CreateUser(ctx, owner))
	proj := &store.Project{ID: tid("r2-proj"), Name: "r2", Slug: "r2"}
	require.NoError(t, s.CreateProject(ctx, proj))
	bindProjectRoleForUsageReadTest(t, s, owner.ID, proj.ID, "project-owner")

	ag := &store.Agent{ID: tid("r2-agent"), Slug: "r2-agent", Name: "r2-agent", ProjectID: proj.ID, OwnerID: owner.ID, CreatedBy: owner.ID, Phase: string(state.PhaseRunning)}
	require.NoError(t, s.CreateAgent(ctx, ag))
	_, err := s.SetAgentRunID(ctx, ag.ID, "run-r2-1", nil)
	require.NoError(t, err)
	tok := srv.generateRecordedAgentToken(t, ag.ID, proj.ID, []AgentTokenScope{ScopeAgentStatusUpdate}, nil)

	dbNow, err := s.DBNow(ctx)
	require.NoError(t, err)
	// Place event at 01:00Z on day D = 29 days ago (first day of default 30-calendar-day window).
	dayD := time.Date(dbNow.Year(), dbNow.Month(), dbNow.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, -(defaultUsageWindowDays - 1))
	evTime := dayD.Add(1 * time.Hour)

	ev1 := makeWireUsageEvent("hub-r2-test", ag.ID, proj.ID, "run-r2-1", "evt-r2-1", 1, evTime, 7, 3)
	ev1.CacheRead = nil
	ev1.CacheWrite = nil
	ev1.CacheWrite1h = nil
	ev1.Reasoning = nil

	rec := doAgentUsageRequest(t, srv, http.MethodPost, telemetrycontract.UsageIngestAgentPath(ag.ID), tok, telemetrycontract.UsageIngestRequest{
		Events:        []telemetrycontract.UsageEvent{ev1},
		RunID:         "run-r2-1",
		ProducerEpoch: "ep-r2",
		ProducerSeq:   1,
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// 1. Sub-day since = D T12:00:00Z -> snaps to D T00:00:00Z, NULL buckets stay null (never 0).
	subDaySince := dayD.Add(12 * time.Hour).Format(time.RFC3339)
	readSub := doUserUsageReadRequest(t, srv, "/api/v1/agents/"+ag.ID+"/usage?since="+subDaySince, owner)
	require.Equal(t, http.StatusOK, readSub.Code, readSub.Body.String())
	var respSub AgentUsageResponse
	require.NoError(t, json.Unmarshal(readSub.Body.Bytes(), &respSub))
	assert.Equal(t, dayD, respSub.Window.Since, "window.since must report snapped UTC day start")
	require.NotNil(t, respSub.Tokens.Input)
	assert.Equal(t, int64(7), *respSub.Tokens.Input)
	require.NotNil(t, respSub.Tokens.Output)
	assert.Equal(t, int64(3), *respSub.Tokens.Output)
	assert.Nil(t, respSub.Tokens.CacheRead, "cache_read must stay null, never 0")
	assert.Nil(t, respSub.Tokens.CacheWrite, "cache_write must stay null, never 0")
	assert.Nil(t, respSub.Tokens.CacheWrite1h, "cache_write_1h must stay null, never 0")
	assert.Nil(t, respSub.Tokens.Reasoning, "reasoning must stay null, never 0")
	require.NotNil(t, respSub.Tokens.SummableTokens)
	assert.Equal(t, int64(10), *respSub.Tokens.SummableTokens)

	// 2. Default 30d window -> also snaps to dayD 00:00Z and keeps NULL buckets null.
	readDef := doUserUsageReadRequest(t, srv, "/api/v1/agents/"+ag.ID+"/usage", owner)
	require.Equal(t, http.StatusOK, readDef.Code, readDef.Body.String())
	var respDef AgentUsageResponse
	require.NoError(t, json.Unmarshal(readDef.Body.Bytes(), &respDef))
	assert.Equal(t, dayD, respDef.Window.Since)
	assert.Nil(t, respDef.Tokens.CacheRead)
	assert.Nil(t, respDef.Tokens.CacheWrite)
	assert.Nil(t, respDef.Tokens.Reasoning)

	// 3. Unit check for past-retention fallback (presence.eventCount == 0):
	// zero-sum buckets MUST return nil (unknown), never 0.
	pastRet := resolveNullableTokens(5, 0, 100, 40, 0, 0, 0, 0, usageBucketPresence{})
	require.NotNil(t, pastRet.Input)
	assert.Equal(t, int64(100), *pastRet.Input)
	require.NotNil(t, pastRet.Output)
	assert.Equal(t, int64(40), *pastRet.Output)
	assert.Nil(t, pastRet.CacheRead, "past-retention zero-sum bucket must be nil, never 0")
	assert.Nil(t, pastRet.CacheWrite, "past-retention zero-sum bucket must be nil, never 0")
	assert.Nil(t, pastRet.CacheWrite1h, "past-retention zero-sum bucket must be nil, never 0")
	assert.Nil(t, pastRet.Reasoning, "past-retention zero-sum bucket must be nil, never 0")

	// 4. O1: Mixed known+unknown per-bucket (event 2 has cache_write=5, event 1 had cache_write=NULL).
	ev2 := makeWireUsageEvent("hub-r2-test", ag.ID, proj.ID, "run-r2-1", "evt-r2-2", 2, evTime.Add(time.Minute), 10, 4)
	ev2.CacheWrite = usageInt64p(5)
	rec2 := doAgentUsageRequest(t, srv, http.MethodPost, telemetrycontract.UsageIngestAgentPath(ag.ID), tok, telemetrycontract.UsageIngestRequest{
		Events:        []telemetrycontract.UsageEvent{ev2},
		RunID:         "run-r2-1",
		ProducerEpoch: "ep-r2",
		ProducerSeq:   2,
	})
	require.Equal(t, http.StatusOK, rec2.Code, rec2.Body.String())

	readMixed := doUserUsageReadRequest(t, srv, "/api/v1/agents/"+ag.ID+"/usage", owner)
	require.Equal(t, http.StatusOK, readMixed.Code, readMixed.Body.String())
	var respMixed AgentUsageResponse
	require.NoError(t, json.Unmarshal(readMixed.Body.Bytes(), &respMixed))
	require.NotNil(t, respMixed.Tokens.CacheWrite)
	assert.Equal(t, int64(5), *respMixed.Tokens.CacheWrite)
	require.NotNil(t, respMixed.Tokens.UnknownEvents, "mixed known+unknown cache_write must populate unknown_events")
	assert.Equal(t, int64(1), respMixed.Tokens.UnknownEvents.CacheWrite)
	assert.Equal(t, int64(0), respMixed.Tokens.UnknownEvents.Input)
}

// TestAgentUsage_VerticalSliceIngestReplayAndRead is the Gate 1 workstation-SQLite
// vertical-slice test: drives a canonical usage batch through POST /api/v1/agents/{id}/usage/events,
// replays the exact same batch to verify idempotency, then reads GET /api/v1/agents/{id}/usage
// and asserts exact §3.1 totals: (input=8, output=5, cache_read=3, cache_write=2, reasoning=2) -> summable 18.
func TestAgentUsage_VerticalSliceIngestReplayAndRead(t *testing.T) {
	ctx := context.Background()
	srv, s := testServer(t)
	srv.SetHubID("hub-gate1-slice")
	setUsageLedgerExperimentForTest(t, srv, true)
	srv.reconcileUsageLedger(ctx)

	owner := &store.User{ID: uuid.NewString(), Email: "gate1-owner@example.com", DisplayName: "Gate1", Role: "member", Status: "active"}
	require.NoError(t, s.CreateUser(ctx, owner))
	proj := &store.Project{ID: tid("gate1-proj"), Name: "gate1", Slug: "gate1"}
	require.NoError(t, s.CreateProject(ctx, proj))
	bindProjectRoleForUsageReadTest(t, s, owner.ID, proj.ID, "project-owner")
	ag := &store.Agent{
		ID:            tid("gate1-agent"),
		Slug:          "gate1-claude",
		Name:          "gate1-claude",
		ProjectID:     proj.ID,
		OwnerID:       owner.ID,
		CreatedBy:     owner.ID,
		AppliedConfig: &store.AgentAppliedConfig{Harness: "claude", HarnessConfig: "claude"},
		Template:      "default",
		Phase:         string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, ag))
	_, err := s.SetAgentRunID(ctx, ag.ID, "run-gate1-1", nil)
	require.NoError(t, err)
	agentTok := srv.generateRecordedAgentToken(t, ag.ID, proj.ID, []AgentTokenScope{ScopeAgentStatusUpdate}, nil)

	now := time.Now().UTC().Truncate(time.Microsecond)
	ev := makeWireUsageEvent("hub-gate1-slice", ag.ID, proj.ID, "run-gate1-1", "evt-gate1-1", 1, now, 8, 5)
	ev.CacheRead = usageInt64p(3)
	ev.CacheWrite = usageInt64p(2)
	ev.CacheWrite1h = nil
	ev.Reasoning = usageInt64p(2)

	batch := telemetrycontract.UsageIngestRequest{
		Events:               []telemetrycontract.UsageEvent{ev},
		RunID:                "run-gate1-1",
		ProducerEpoch:        "ep-gate1",
		ProducerSeq:          1,
		LastSourceActivityAt: telemetrycontract.FormatTS(now),
		SupportedSignals:     []string{"calls", "input", "output", "cache_read", "cache_write", "reasoning"},
	}

	// 1. Ingest initial batch.
	rec1 := doAgentUsageRequest(t, srv, http.MethodPost, telemetrycontract.UsageIngestAgentPath(ag.ID), agentTok, batch)
	require.Equal(t, http.StatusOK, rec1.Code, rec1.Body.String())
	var ingResp1 telemetrycontract.UsageIngestResponse
	require.NoError(t, json.Unmarshal(rec1.Body.Bytes(), &ingResp1))
	assert.Equal(t, 1, ingResp1.Accepted)
	assert.Equal(t, 0, ingResp1.Duplicates)

	// 2. Replay exact duplicate batch -> must not double count.
	rec2 := doAgentUsageRequest(t, srv, http.MethodPost, telemetrycontract.UsageIngestAgentPath(ag.ID), agentTok, batch)
	require.Equal(t, http.StatusOK, rec2.Code, rec2.Body.String())
	var ingResp2 telemetrycontract.UsageIngestResponse
	require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &ingResp2))
	assert.Equal(t, 0, ingResp2.Accepted)
	assert.Equal(t, 1, ingResp2.Duplicates)

	// 3. Read GET /api/v1/agents/{id}/usage as owner and assert exact §3.1 math.
	readRec := doUserUsageReadRequest(t, srv, "/api/v1/agents/"+ag.ID+"/usage", owner)
	require.Equal(t, http.StatusOK, readRec.Code, readRec.Body.String())

	var resp AgentUsageResponse
	require.NoError(t, json.Unmarshal(readRec.Body.Bytes(), &resp))
	assert.Equal(t, ag.ID, resp.AgentID)
	assert.Equal(t, proj.ID, resp.ProjectID)
	assert.Equal(t, int64(1), resp.Calls)
	assert.Equal(t, int64(0), resp.ErrorCalls)
	assert.Equal(t, int64(1), resp.Events)
	assert.Equal(t, int64(0), resp.NullTokenEvents)

	require.NotNil(t, resp.Tokens.Input)
	assert.Equal(t, int64(8), *resp.Tokens.Input)
	require.NotNil(t, resp.Tokens.Output)
	assert.Equal(t, int64(5), *resp.Tokens.Output)
	require.NotNil(t, resp.Tokens.CacheRead)
	assert.Equal(t, int64(3), *resp.Tokens.CacheRead)
	require.NotNil(t, resp.Tokens.CacheWrite)
	assert.Equal(t, int64(2), *resp.Tokens.CacheWrite)
	assert.Nil(t, resp.Tokens.CacheWrite1h)
	require.NotNil(t, resp.Tokens.Reasoning)
	assert.Equal(t, int64(2), *resp.Tokens.Reasoning)
	require.NotNil(t, resp.Tokens.SummableTokens)
	assert.Equal(t, int64(18), *resp.Tokens.SummableTokens, "8+5+3+2=18 (reasoning never added twice)")

	assert.Equal(t, MeasurementStatusMeasured, resp.MeasurementStatus)
	require.NotNil(t, resp.LastReceiptAt)
	assert.False(t, resp.AsOf.IsZero())

	require.Len(t, resp.ByModel, 1)
	assert.Equal(t, "claude-sonnet-4-6", resp.ByModel[0].Model)
	assert.Equal(t, "claude", resp.ByModel[0].Harness)
	require.NotNil(t, resp.ByModel[0].Tokens.SummableTokens)
	assert.Equal(t, int64(18), *resp.ByModel[0].Tokens.SummableTokens)
}

func TestAgentUsageRead_ResolveCurrentRunNoCrossRunFallbackAndModelTieBreak(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Microsecond)

	// (a) Regression test: when agent.RunID is set but has no matching
	// run_attribution row, resolveCurrentRunForUsage must NOT fall back to a
	// different run's attribution.
	olderAttr := &store.RunAttribution{
		RunID:     "run-old",
		AgentID:   "ag-1",
		ProjectID: "proj-1",
		Harness:   "claude",
		CreatedAt: now.Add(-1 * time.Hour),
	}
	ag := &store.Agent{
		ID:        "ag-1",
		ProjectID: "proj-1",
		RunID:     "run-current-missing-attr",
	}
	gotRunID, gotAttr := resolveCurrentRunForUsage(ag, []*store.RunAttribution{olderAttr})
	assert.Equal(t, "run-current-missing-attr", gotRunID)
	assert.Nil(t, gotAttr, "must not fall back to another run's attribution when agent.RunID is set")

	// When agent.RunID matches a row in runs, return that exact attribution.
	agMatch := &store.Agent{ID: "ag-1", ProjectID: "proj-1", RunID: "run-old"}
	gotRunID, gotAttr = resolveCurrentRunForUsage(agMatch, []*store.RunAttribution{olderAttr})
	assert.Equal(t, "run-old", gotRunID)
	assert.Equal(t, olderAttr, gotAttr)

	// (b) Sort tie-break test: equal summable tokens, equal calls, and equal
	// model must break ties deterministically on Harness ASC, then UsageSource ASC
	// (including "" for unknown harness per NOT NULL DEFAULT '' schema).
	rollups := []*store.UsageRollupDaily{
		{Model: "claude-sonnet-4-6", Harness: "codex", UsageSource: store.UsageSourceNative, Calls: 2, Input: 10, Output: 5, Events: 2},
		{Model: "claude-sonnet-4-6", Harness: "claude", UsageSource: store.UsageSourceNative, Calls: 2, Input: 10, Output: 5, Events: 2},
		{Model: "claude-sonnet-4-6", Harness: "claude", UsageSource: store.UsageSourceHooks, Calls: 2, Input: 10, Output: 5, Events: 2},
		{Model: "claude-sonnet-4-6", Harness: "", UsageSource: store.UsageSourceNative, Calls: 2, Input: 10, Output: 5, Events: 2},
	}
	resp := buildAgentUsageResponse(ag, now.Add(-24*time.Hour), now, now, rollups, nil, ag.RunID, nil, false)
	require.Len(t, resp.ByModel, 4)
	assert.Equal(t, "", resp.ByModel[0].Harness)
	assert.Equal(t, store.UsageSourceNative, resp.ByModel[0].UsageSource)
	assert.Equal(t, "claude", resp.ByModel[1].Harness)
	assert.Equal(t, store.UsageSourceHooks, resp.ByModel[1].UsageSource)
	assert.Equal(t, "claude", resp.ByModel[2].Harness)
	assert.Equal(t, store.UsageSourceNative, resp.ByModel[2].UsageSource)
	assert.Equal(t, "codex", resp.ByModel[3].Harness)
	assert.Equal(t, store.UsageSourceNative, resp.ByModel[3].UsageSource)

	// (c) Pre-migration legacy daily rows (presence_tracked_events == 0):
	// sum == 0 must surface as nil (never fabricated 0), while sum > 0 surfaces
	// &sum without falsely flagging all legacy events as unknown.
	legacyRollups := []*store.UsageRollupDaily{
		{
			Model:                 "claude-sonnet-4-6",
			Harness:               "claude",
			UsageSource:           store.UsageSourceNative,
			Calls:                 3,
			Input:                 100,
			Output:                50,
			CacheRead:             0, // legacy 0 without presence counter -> must read as nil
			CacheWrite:            0,
			Events:                3,
			PresenceTrackedEvents: 0,
		},
	}
	legacyResp := buildAgentUsageResponse(ag, now.Add(-24*time.Hour), now, now, legacyRollups, nil, ag.RunID, nil, false)
	require.NotNil(t, legacyResp.Tokens.Input)
	assert.Equal(t, int64(100), *legacyResp.Tokens.Input)
	require.NotNil(t, legacyResp.Tokens.Output)
	assert.Equal(t, int64(50), *legacyResp.Tokens.Output)
	assert.Nil(t, legacyResp.Tokens.CacheRead, "legacy row with sum=0 and presence_tracked_events=0 must read as null, never 0")
	assert.Nil(t, legacyResp.Tokens.CacheWrite)
	assert.Nil(t, legacyResp.Tokens.UnknownEvents, "pure legacy row must not fabricate unknown_events counts")
}

func TestAgentUsageRead_SaturatesOnOverflow(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Microsecond)
	ag := &store.Agent{ID: "ag-sat", ProjectID: "proj-sat", RunID: "run-sat", Phase: string(state.PhaseRunning)}
	rollups := []*store.UsageRollupDaily{
		{
			Model:                 "claude-sonnet-4-6",
			Harness:               "claude",
			UsageSource:           store.UsageSourceNative,
			Calls:                 1,
			Input:                 math.MaxInt64 - 10,
			Output:                50,
			Events:                1,
			InputKnownEvents:      1,
			OutputKnownEvents:     1,
			PresenceTrackedEvents: 1,
		},
		{
			Model:                 "claude-sonnet-4-6",
			Harness:               "claude",
			UsageSource:           store.UsageSourceNative,
			Calls:                 1,
			Input:                 100,
			Output:                25,
			Events:                1,
			InputKnownEvents:      1,
			OutputKnownEvents:     1,
			PresenceTrackedEvents: 1,
		},
	}

	resp := buildAgentUsageResponse(ag, now.Add(-24*time.Hour), now, now, rollups, nil, ag.RunID, nil, true)
	require.NotNil(t, resp.Tokens.Input)
	assert.Equal(t, int64(math.MaxInt64), *resp.Tokens.Input)
	require.NotNil(t, resp.Tokens.Output)
	assert.Equal(t, int64(75), *resp.Tokens.Output)
	require.NotNil(t, resp.Tokens.SummableTokens)
	assert.Equal(t, int64(math.MaxInt64), *resp.Tokens.SummableTokens)
	assert.True(t, resp.Tokens.Saturated)

	require.Len(t, resp.ByModel, 1)
	require.NotNil(t, resp.ByModel[0].Tokens.Input)
	assert.Equal(t, int64(math.MaxInt64), *resp.ByModel[0].Tokens.Input)
	assert.True(t, resp.ByModel[0].Tokens.Saturated)
}
