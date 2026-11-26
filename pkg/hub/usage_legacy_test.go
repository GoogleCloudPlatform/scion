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
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
)

func TestHandleAgentMetrics_LegacyConverter(t *testing.T) {
	ctx := context.Background()
	srv, s := testServer(t)
	srv.SetHubID("hub-legacy-http")

	user := &store.User{
		ID:          uuid.NewString(),
		Email:       "legacy-http@example.com",
		DisplayName: "Legacy HTTP User",
		Role:        "admin",
		Status:      "active",
	}
	require.NoError(t, s.CreateUser(ctx, user))

	project := &store.Project{
		ID:      uuid.NewString(),
		Name:    "Legacy HTTP Project",
		Slug:    "legacy-http-" + uuid.NewString()[:8],
		OwnerID: user.ID,
	}
	require.NoError(t, s.CreateProject(ctx, project))

	agent := &store.Agent{
		ID:            uuid.NewString(),
		Slug:          "legacy-http-agent",
		Name:          "Legacy HTTP Agent",
		ProjectID:     project.ID,
		OwnerID:       user.ID,
		CreatedBy:     user.ID,
		AppliedConfig: &store.AgentAppliedConfig{Harness: "claude", HarnessConfig: "claude"},
		Phase:         string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))
	runID := "run-leg-http-" + uuid.NewString()[:8]
	_, err := s.SetAgentRunID(ctx, agent.ID, runID, nil)
	require.NoError(t, err)

	agentToken := srv.generateRecordedAgentToken(t, agent.ID, project.ID, []AgentTokenScope{ScopeAgentStatusUpdate}, nil)

	postMetrics := func(t *testing.T, payload metricsPayloadRequest) (int, string) {
		t.Helper()
		body, err := json.Marshal(payload)
		require.NoError(t, err)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+agent.ID+"/metrics", bytes.NewReader(body))
		req.Header.Set("X-Scion-Agent-Token", agentToken)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		var resp map[string]string
		_ = json.Unmarshal(rec.Body.Bytes(), &resp)
		return rec.Code, resp["id"]
	}

	getAgentUsage := func(t *testing.T) AgentUsageResponse {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/"+agent.ID+"/usage", nil)
		req.Header.Set("X-Scion-Agent-Token", agentToken)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var resp AgentUsageResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		return resp
	}

	now := time.Now().UTC().Truncate(time.Second)

	// 1. Experiment OFF: unmarked post does nothing in usage ledger and leaves converted_at nil.
	setUsageLedgerExperimentForTest(t, srv, false)
	code, offID := postMetrics(t, metricsPayloadRequest{
		Type:    "agent_metrics",
		AgentID: agent.ID,
		Session: metricsSession{
			ID:        "sess-off",
			StartedAt: now.Add(-20 * time.Minute).Format(time.RFC3339),
			EndedAt:   now.Add(-15 * time.Minute).Format(time.RFC3339),
			Status:    "completed",
			Model:     "claude-sonnet-4-6",
		},
		Tokens: metricsTokens{Input: 500, Output: 100},
	})
	require.Equal(t, http.StatusCreated, code)
	offRow, err := s.GetAgentSessionMetrics(ctx, offID)
	require.NoError(t, err)
	assert.Nil(t, offRow.ConvertedAt, "experiment OFF must leave converted_at nil")

	rollupsOff, err := s.ListUsageRollupsDailyByAgent(ctx, agent.ID, now.Add(-24*time.Hour), now.Add(24*time.Hour))
	require.NoError(t, err)
	assert.Empty(t, rollupsOff)

	// 2. Enable experiment (without running reconcile yet so we isolate live path first).
	setUsageLedgerExperimentForTest(t, srv, true)

	// 3. Marked post (usage_events_enabled: true) is ignored by converter and stamped converted_at.
	code, markedID := postMetrics(t, metricsPayloadRequest{
		Type:               "agent_metrics",
		AgentID:            agent.ID,
		UsageEventsEnabled: true,
		Session: metricsSession{
			ID:        "sess-marked",
			StartedAt: now.Add(-14 * time.Minute).Format(time.RFC3339),
			EndedAt:   now.Add(-12 * time.Minute).Format(time.RFC3339),
			Status:    "completed",
			Model:     "claude-sonnet-4-6",
		},
		Tokens: metricsTokens{Input: 9999, Output: 8888},
	})
	require.Equal(t, http.StatusCreated, code)
	markedRow, err := s.GetAgentSessionMetrics(ctx, markedID)
	require.NoError(t, err)
	assert.NotNil(t, markedRow.ConvertedAt, "marked post must set converted_at so catch-up skips it")

	rollupsAfterMarked, err := s.ListUsageRollupsDailyByAgent(ctx, agent.ID, now.Add(-24*time.Hour), now.Add(24*time.Hour))
	require.NoError(t, err)
	assert.Empty(t, rollupsAfterMarked, "marked post must not be converted into ledger/rollups")

	// 4. Unmarked legacy post: converts to ledger + rollups with NULLs and legacy_client status.
	seg1Payload := metricsPayloadRequest{
		Type:    "agent_metrics",
		AgentID: agent.ID,
		Session: metricsSession{
			ID:        "sess-legacy-1",
			StartedAt: now.Add(-10 * time.Minute).Format(time.RFC3339),
			EndedAt:   now.Add(-8 * time.Minute).Format(time.RFC3339),
			Status:    "completed",
			TurnCount: 3,
			Model:     "claude-sonnet-4-6",
		},
		Tokens: metricsTokens{Input: 1000, Output: 200, Cached: 300, Reasoning: 0},
	}
	code, leg1ID := postMetrics(t, seg1Payload)
	require.Equal(t, http.StatusCreated, code)
	leg1Row, err := s.GetAgentSessionMetrics(ctx, leg1ID)
	require.NoError(t, err)
	assert.NotNil(t, leg1Row.ConvertedAt)

	// Re-post of the same (agent_id, session_id, started_at) must be idempotent.
	code, leg1DupID := postMetrics(t, seg1Payload)
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, leg1ID, leg1DupID)

	// Second segment with new started_at increments totals cleanly.
	code, _ = postMetrics(t, metricsPayloadRequest{
		Type:    "agent_metrics",
		AgentID: agent.ID,
		Session: metricsSession{
			ID:        "sess-legacy-1",
			StartedAt: now.Add(-6 * time.Minute).Format(time.RFC3339),
			EndedAt:   now.Add(-4 * time.Minute).Format(time.RFC3339),
			Status:    "completed",
			Model:     "claude-sonnet-4-6",
		},
		Tokens: metricsTokens{Input: 500, Output: 100, Cached: 0, Reasoning: 0},
	})
	require.Equal(t, http.StatusCreated, code)

	usageResp := getAgentUsage(t)
	assert.Equal(t, MeasurementStatusLegacyClient, usageResp.MeasurementStatus)
	require.NotNil(t, usageResp.Tokens.Input)
	assert.Equal(t, int64(1500), *usageResp.Tokens.Input)
	require.NotNil(t, usageResp.Tokens.Output)
	assert.Equal(t, int64(300), *usageResp.Tokens.Output)
	require.NotNil(t, usageResp.Tokens.CacheRead)
	assert.Equal(t, int64(300), *usageResp.Tokens.CacheRead)
	assert.Nil(t, usageResp.Tokens.CacheWrite, "cache_write must be JSON null, never 0")
	assert.Nil(t, usageResp.Tokens.CacheWrite1h)
	assert.Nil(t, usageResp.Tokens.Reasoning)
	require.NotNil(t, usageResp.Tokens.SummableTokens)
	assert.Equal(t, int64(2100), *usageResp.Tokens.SummableTokens)
	assert.Equal(t, int64(0), usageResp.Calls)

	// 5. Once a Track 1 event is ingested for the same run, measurement_status
	//    becomes "measured", and a subsequent unmarked legacy post does not double count.
	in200 := int64(200)
	out50 := int64(50)
	_, err = s.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
		AgentID: agent.ID,
		Events: []*store.UsageEvent{{
			HubID:          srv.usageLedgerHubID(),
			EventID:        "t1-ev-1",
			RunID:          runID,
			ProjectID:      project.ID,
			AgentID:        agent.ID,
			BillingUserID:  &user.ID,
			Harness:        "claude",
			Model:          "claude-sonnet-4-6",
			ModelSource:    telemetrycontract.ModelSourceEvent,
			UsageSource:    store.UsageSourceNative,
			SourceKind:     "claude.api_request",
			SourceIdentity: telemetrycontract.SourceIdentityRequestID,
			Status:         telemetrycontract.StatusSuccess,
			Calls:          1,
			Input:          &in200,
			Output:         &out50,
			TokenStatus:    telemetrycontract.TokenStatusReported,
			EventTS:        now.Add(-2 * time.Minute),
			ReceivedAt:     now.Add(-2 * time.Minute),
			ProducerEpoch:  "ep-1",
			ProducerSeq:    1,
		}},
	})
	require.NoError(t, err)

	code, _ = postMetrics(t, metricsPayloadRequest{
		Type:    "agent_metrics",
		AgentID: agent.ID,
		Session: metricsSession{
			ID:        "sess-after-t1",
			StartedAt: now.Add(-1 * time.Minute).Format(time.RFC3339),
			EndedAt:   now.Format(time.RFC3339),
			Status:    "completed",
			Model:     "claude-sonnet-4-6",
		},
		Tokens: metricsTokens{Input: 7777, Output: 6666},
	})
	require.Equal(t, http.StatusCreated, code)

	usageAfterT1 := getAgentUsage(t)
	assert.Equal(t, MeasurementStatusMeasured, usageAfterT1.MeasurementStatus)
	require.NotNil(t, usageAfterT1.Tokens.Input)
	assert.Equal(t, int64(1700), *usageAfterT1.Tokens.Input, "unmarked post on a Track 1 run must not double count")
	require.NotNil(t, usageAfterT1.Tokens.Output)
	assert.Equal(t, int64(350), *usageAfterT1.Tokens.Output)
}

func TestUsageLedger_LegacyBackfillOnToggleAndOffOnGap(t *testing.T) {
	ctx := context.Background()
	srv, s := testServer(t)
	srv.SetHubID("hub-legacy-toggle")

	proj := &store.Project{
		ID:   uuid.NewString(),
		Name: "Toggle Proj",
		Slug: "toggle-proj-" + uuid.NewString()[:8],
	}
	require.NoError(t, s.CreateProject(ctx, proj))
	ag := &store.Agent{
		ID:        uuid.NewString(),
		Slug:      "toggle-agent",
		Name:      "Toggle Agent",
		ProjectID: proj.ID,
		Phase:     string(state.PhaseStopped),
	}
	require.NoError(t, s.CreateAgent(ctx, ag))

	t1 := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	e1 := t1.Add(5 * time.Minute)
	row1 := &store.AgentSessionMetrics{
		ID:           uuid.NewString(),
		AgentID:      ag.ID,
		ProjectID:    proj.ID,
		SessionID:    "toggle-s1",
		StartedAt:    t1,
		EndedAt:      &e1,
		Status:       "completed",
		Model:        "claude-sonnet-4-6",
		TokensInput:  100,
		TokensOutput: 40,
	}
	require.NoError(t, s.CreateAgentSessionMetrics(ctx, row1))

	// 1. Toggle ON -> reconcile backfills row1 automatically.
	setUsageLedgerExperimentForTest(t, srv, true)
	srv.reconcileUsageLedger(ctx)

	got1, err := s.GetAgentSessionMetrics(ctx, row1.ID)
	require.NoError(t, err)
	require.NotNil(t, got1.ConvertedAt)

	// 2. Toggle OFF, insert row2 while OFF, toggle ON -> row2 is caught up
	//    without double counting row1.
	setUsageLedgerExperimentForTest(t, srv, false)
	srv.reconcileUsageLedger(ctx)

	t2 := t1.Add(2 * time.Hour)
	e2 := t2.Add(5 * time.Minute)
	row2 := &store.AgentSessionMetrics{
		ID:           uuid.NewString(),
		AgentID:      ag.ID,
		ProjectID:    proj.ID,
		SessionID:    "toggle-s2",
		StartedAt:    t2,
		EndedAt:      &e2,
		Status:       "completed",
		Model:        "claude-sonnet-4-6",
		TokensInput:  200,
		TokensOutput: 60,
	}
	require.NoError(t, s.CreateAgentSessionMetrics(ctx, row2))

	setUsageLedgerExperimentForTest(t, srv, true)
	srv.reconcileUsageLedger(ctx)

	got2, err := s.GetAgentSessionMetrics(ctx, row2.ID)
	require.NoError(t, err)
	require.NotNil(t, got2.ConvertedAt)

	day := time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)
	rollups, err := s.ListUsageRollupsDailyByAgent(ctx, ag.ID, day, day)
	require.NoError(t, err)
	require.Len(t, rollups, 1)
	assert.Equal(t, int64(300), rollups[0].Input)
	assert.Equal(t, int64(100), rollups[0].Output)
	assert.Equal(t, int64(2), rollups[0].Events)
}

func TestDeriveMeasurementStatus_LegacyClient(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

	assert.Equal(t, MeasurementStatusLegacyClient, deriveMeasurementStatus(measurementStatusInput{
		HasRunUsageEvents:  true,
		HasLegacyUsageOnly: true,
		DBNow:              now,
	}))

	// If Track 1 meter_health exists (even fresh), status is measured, not legacy_client.
	assert.Equal(t, MeasurementStatusMeasured, deriveMeasurementStatus(measurementStatusInput{
		MeterHealth: []*store.MeterHealth{{
			LastReceiptAt: now.Add(-10 * time.Second),
			FinalState:    store.MeterHealthFinalStateOpen,
		}},
		HasRunUsageEvents:  true,
		HasLegacyUsageOnly: true,
		DBNow:              now,
	}))

	// Mixed Track 1 + legacy events -> measured.
	assert.Equal(t, MeasurementStatusMeasured, deriveMeasurementStatus(measurementStatusInput{
		HasRunUsageEvents:  true,
		HasLegacyUsageOnly: false,
		DBNow:              now,
	}))
}

func TestUsageLedger_LegacyBackfillRetryOnFailure(t *testing.T) {
	ctx := context.Background()
	srv, _ := testServer(t)
	srv.SetHubID("hub-legacy-retry")
	srv.usageLedger.retryInterval = 15 * time.Millisecond
	srv.usageLedger.retryMax = 30 * time.Millisecond

	attempts := make(chan int, 4)
	call := 0
	srv.usageLedger.backfillFn = func(context.Context, string) (int, error) {
		return 0, nil
	}
	srv.usageLedger.legacyBackfillFn = func(context.Context, string) (store.LegacySessionMetricsBackfillResult, error) {
		call++
		attempts <- call
		if call == 1 {
			return store.LegacySessionMetricsBackfillResult{}, errors.New("transient legacy backfill error")
		}
		return store.LegacySessionMetricsBackfillResult{Processed: 1, Converted: 1}, nil
	}

	setUsageLedgerExperimentForTest(t, srv, true)
	srv.reconcileUsageLedgerAsync(ctx)
	t.Cleanup(srv.stopUsageLedger)

	select {
	case n := <-attempts:
		assert.Equal(t, 1, n)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for first legacy backfill attempt")
	}
	select {
	case n := <-attempts:
		assert.Equal(t, 2, n)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for retry legacy backfill attempt")
	}
}
