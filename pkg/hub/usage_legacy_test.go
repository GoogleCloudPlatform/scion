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
	t.Cleanup(srv.stopUsageLedger)

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

	agentToken := srv.generateRecordedAgentToken(t, agent.ID, project.ID, []AgentTokenScope{ScopeAgentStatusUpdate, ScopeProjectRead}, nil)

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

	// 4. Unmarked legacy post: converts asynchronously to ledger + rollups with NULLs and legacy_client status.
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
	srv.usageLedger.liveInFlight.Wait()

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
	srv.usageLedger.liveInFlight.Wait()

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

	// 5. Track 1 event at now-2m:
	//    (a) An unmarked legacy segment whose window [now-3m, now-1m] overlaps
	//        the Track 1 event must NOT double count;
	//    (b) A later unmarked legacy segment with a new session_id and a window
	//        [now-30s, now] AFTER the Track 1 event (e.g. after sink revocation)
	//        MUST be converted and included in totals (R1).
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

	// (a) Overlapping window [now-3m, now-1m] -> skipped.
	code, _ = postMetrics(t, metricsPayloadRequest{
		Type:    "agent_metrics",
		AgentID: agent.ID,
		Session: metricsSession{
			ID:        "sess-overlapping-t1",
			StartedAt: now.Add(-3 * time.Minute).Format(time.RFC3339),
			EndedAt:   now.Add(-1 * time.Minute).Format(time.RFC3339),
			Status:    "completed",
			Model:     "claude-sonnet-4-6",
		},
		Tokens: metricsTokens{Input: 7777, Output: 6666},
	})
	require.Equal(t, http.StatusCreated, code)
	srv.usageLedger.liveInFlight.Wait()

	usageAfterOverlap := getAgentUsage(t)
	assert.Equal(t, MeasurementStatusMeasured, usageAfterOverlap.MeasurementStatus)
	require.NotNil(t, usageAfterOverlap.Tokens.Input)
	assert.Equal(t, int64(1700), *usageAfterOverlap.Tokens.Input, "overlapping window must not double count")
	require.NotNil(t, usageAfterOverlap.Tokens.Output)
	assert.Equal(t, int64(350), *usageAfterOverlap.Tokens.Output)

	// (b) Later non-overlapping window [now-30s, now] with new session_id -> converted (R1).
	code, _ = postMetrics(t, metricsPayloadRequest{
		Type:    "agent_metrics",
		AgentID: agent.ID,
		Session: metricsSession{
			ID:        "sess-after-revoke",
			StartedAt: now.Add(-30 * time.Second).Format(time.RFC3339),
			EndedAt:   now.Format(time.RFC3339),
			Status:    "completed",
			Model:     "claude-sonnet-4-6",
		},
		Tokens: metricsTokens{Input: 400, Output: 80},
	})
	require.Equal(t, http.StatusCreated, code)
	srv.usageLedger.liveInFlight.Wait()

	usageAfterRevoke := getAgentUsage(t)
	assert.Equal(t, MeasurementStatusMeasured, usageAfterRevoke.MeasurementStatus)
	require.NotNil(t, usageAfterRevoke.Tokens.Input)
	assert.Equal(t, int64(2100), *usageAfterRevoke.Tokens.Input, "later non-overlapping unmarked segment must be converted")
	require.NotNil(t, usageAfterRevoke.Tokens.Output)
	assert.Equal(t, int64(430), *usageAfterRevoke.Tokens.Output)
}

func TestHandleAgentMetrics_LiveConversionAsyncDoesNotBlockPost(t *testing.T) {
	// R3: With the store's convert call blocked, POST /metrics still returns
	// 201 Created promptly, and the row is converted once released.
	ctx := context.Background()
	srv, s := testServer(t)
	srv.SetHubID("hub-legacy-async-post")
	t.Cleanup(srv.stopUsageLedger)

	proj := &store.Project{
		ID:   uuid.NewString(),
		Name: "Async Post Proj",
		Slug: "async-post-" + uuid.NewString()[:8],
	}
	require.NoError(t, s.CreateProject(ctx, proj))
	ag := &store.Agent{
		ID:        uuid.NewString(),
		Slug:      "async-post-agent",
		Name:      "Async Post Agent",
		ProjectID: proj.ID,
		Phase:     string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, ag))
	agentToken := srv.generateRecordedAgentToken(t, ag.ID, proj.ID, []AgentTokenScope{ScopeAgentStatusUpdate}, nil)

	started := make(chan struct{}, 1)
	release := make(chan struct{})
	srv.usageLedger.liveConvertFn = func(c context.Context, hubID string, row *store.AgentSessionMetrics, preferredRunID string) (bool, error) {
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
		return s.ConvertLegacySessionMetricsRow(c, hubID, row, preferredRunID)
	}
	setUsageLedgerExperimentForTest(t, srv, true)

	now := time.Now().UTC().Truncate(time.Second)
	body, err := json.Marshal(metricsPayloadRequest{
		Type:    "agent_metrics",
		AgentID: ag.ID,
		Session: metricsSession{
			ID:        "sess-async-block",
			StartedAt: now.Add(-5 * time.Minute).Format(time.RFC3339),
			EndedAt:   now.Format(time.RFC3339),
			Status:    "completed",
			Model:     "claude-sonnet-4-6",
		},
		Tokens: metricsTokens{Input: 333, Output: 66},
	})
	require.NoError(t, err)

	reqCtx, cancelReq := context.WithCancel(ctx)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+ag.ID+"/metrics", bytes.NewReader(body)).WithContext(reqCtx)
	req.Header.Set("X-Scion-Agent-Token", agentToken)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	postDone := make(chan struct{})
	go func() {
		defer close(postDone)
		srv.Handler().ServeHTTP(rec, req)
	}()

	select {
	case <-postDone:
	case <-time.After(1 * time.Second):
		close(release)
		t.Fatal("POST /metrics blocked on live conversion")
	}
	require.Equal(t, http.StatusCreated, rec.Code)

	// Cancel the HTTP request context immediately after response; conversion
	// must still succeed once released because it runs on the server worker
	// context rather than the HTTP request context.
	cancelReq()

	var resp map[string]string
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	metricsID := resp["id"]
	require.NotEmpty(t, metricsID)

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("timed out waiting for async live converter to start")
	}

	// Row is not yet stamped while blocked.
	beforeRow, err := s.GetAgentSessionMetrics(ctx, metricsID)
	require.NoError(t, err)
	assert.Nil(t, beforeRow.ConvertedAt)

	close(release)
	srv.usageLedger.liveInFlight.Wait()

	afterRow, err := s.GetAgentSessionMetrics(ctx, metricsID)
	require.NoError(t, err)
	assert.NotNil(t, afterRow.ConvertedAt, "row must be converted after release even though request ctx was cancelled")
}

func TestUsageLedger_SyncReconcileDoesNotBlockOnLegacyBackfill(t *testing.T) {
	// R2: Sync startup reconcileUsageLedger must return promptly while a
	// stubbed legacyBackfillFn is blocked, and the legacy backfill must still
	// complete once unblocked.
	ctx := context.Background()
	srv, _ := testServer(t)
	srv.SetHubID("hub-legacy-sync-nonblock")
	t.Cleanup(srv.stopUsageLedger)

	started := make(chan struct{}, 1)
	release := make(chan struct{})
	finished := make(chan struct{})
	srv.usageLedger.backfillFn = func(context.Context, string) (int, error) {
		return 0, nil
	}
	srv.usageLedger.legacyBackfillFn = func(context.Context, string) (store.LegacySessionMetricsBackfillResult, error) {
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
		close(finished)
		return store.LegacySessionMetricsBackfillResult{Processed: 2, Converted: 2}, nil
	}

	setUsageLedgerExperimentForTest(t, srv, true)

	reconcileDone := make(chan struct{})
	go func() {
		defer close(reconcileDone)
		srv.reconcileUsageLedger(ctx)
	}()

	select {
	case <-reconcileDone:
	case <-time.After(1 * time.Second):
		close(release)
		t.Fatal("sync reconcileUsageLedger blocked on legacy backfill")
	}

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("legacy backfill goroutine did not start")
	}

	close(release)
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("legacy backfill did not complete after release")
	}
}

func TestUsageLedger_LegacyBackfillOnToggleAndOffOnGap(t *testing.T) {
	ctx := context.Background()
	srv, s := testServer(t)
	srv.SetHubID("hub-legacy-toggle")
	t.Cleanup(srv.stopUsageLedger)

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

	// 1. Toggle ON -> async legacy catch-up backfills row1 automatically.
	setUsageLedgerExperimentForTest(t, srv, true)
	srv.reconcileUsageLedger(ctx)

	require.Eventually(t, func() bool {
		got1, err := s.GetAgentSessionMetrics(ctx, row1.ID)
		return err == nil && got1.ConvertedAt != nil
	}, 2*time.Second, 20*time.Millisecond, "row1 should be backfilled asynchronously")

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

	require.Eventually(t, func() bool {
		got2, err := s.GetAgentSessionMetrics(ctx, row2.ID)
		return err == nil && got2.ConvertedAt != nil
	}, 2*time.Second, 20*time.Millisecond, "row2 posted while OFF should be caught up on next ON")

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

func TestUsageLedger_LegacyBackfillRetryAndPeriodicCatchup(t *testing.T) {
	ctx := context.Background()
	srv, _ := testServer(t)
	srv.SetHubID("hub-legacy-retry")
	srv.usageLedger.retryInterval = 15 * time.Millisecond
	srv.usageLedger.retryMax = 30 * time.Millisecond
	srv.usageLedger.legacyPeriodicInterval = 25 * time.Millisecond

	attempts := make(chan int, 8)
	call := 0
	srv.usageLedger.backfillFn = func(context.Context, string) (int, error) {
		return 0, nil
	}
	srv.usageLedger.legacyBackfillFn = func(context.Context, string) (store.LegacySessionMetricsBackfillResult, error) {
		call++
		select {
		case attempts <- call:
		default:
		}
		if call == 1 {
			return store.LegacySessionMetricsBackfillResult{}, errors.New("transient legacy backfill error")
		}
		return store.LegacySessionMetricsBackfillResult{Processed: 1, Converted: 1}, nil
	}

	setUsageLedgerExperimentForTest(t, srv, true)
	srv.reconcileUsageLedgerAsync(ctx)
	t.Cleanup(srv.stopUsageLedger)

	// Attempt 1 fails -> Attempt 2 retries and succeeds -> Attempt 3 fires via periodic timer (R3).
	for want := 1; want <= 3; want++ {
		select {
		case got := <-attempts:
			assert.Equal(t, want, got)
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for legacy backfill attempt %d", want)
		}
	}
}

func TestUsageLedger_OffPeriodRunBackfillReadsLegacyClient(t *testing.T) {
	// R1 end-to-end Hub test: run minted while experiment is OFF, segment
	// posted while OFF, then OFF -> ON reconcile: active-run attribution
	// backfill completes first, legacy catch-up attributes the segment to the
	// current run via agents.run_id_updated_at, and GET /agents/{id}/usage
	// reports measurement_status = "legacy_client".
	ctx := context.Background()
	srv, s := testServer(t)
	srv.SetHubID("hub-legacy-off-run")
	t.Cleanup(srv.stopUsageLedger)

	proj := &store.Project{
		ID:   uuid.NewString(),
		Name: "OffRun Proj",
		Slug: "offrun-" + uuid.NewString()[:8],
	}
	require.NoError(t, s.CreateProject(ctx, proj))
	ag := &store.Agent{
		ID:            uuid.NewString(),
		Slug:          "offrun-agent",
		Name:          "OffRun Agent",
		ProjectID:     proj.ID,
		AppliedConfig: &store.AgentAppliedConfig{Harness: "claude", HarnessConfig: "claude"},
		Phase:         string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, ag))

	// Experiment is OFF: mint the current run (sets agents.run_id_updated_at).
	setUsageLedgerExperimentForTest(t, srv, false)
	srv.reconcileUsageLedger(ctx)
	runID := "run-off-period-" + uuid.NewString()[:8]
	_, err := s.SetAgentRunID(ctx, ag.ID, runID, nil)
	require.NoError(t, err)

	agentToken := srv.generateRecordedAgentToken(t, ag.ID, proj.ID, []AgentTokenScope{ScopeAgentStatusUpdate, ScopeProjectRead}, nil)

	// Post an unmarked segment after the run was minted while experiment is OFF,
	// and ensure the segment has ended before the OFF -> ON active-run backfill
	// runs so run_attribution.created_at > segEnd > segStart >= run_id_updated_at.
	// That guarantees the old created_at boundary would fail this test.
	segStart := time.Now().UTC().Add(10 * time.Millisecond).Truncate(time.Millisecond)
	segEnd := segStart.Add(10 * time.Millisecond)
	body, err := json.Marshal(metricsPayloadRequest{
		Type:    "agent_metrics",
		AgentID: ag.ID,
		Session: metricsSession{
			ID:        "sess-off-curr-run",
			StartedAt: segStart.Format(time.RFC3339Nano),
			EndedAt:   segEnd.Format(time.RFC3339Nano),
			Status:    "completed",
			Model:     "claude-sonnet-4-6",
		},
		Tokens: metricsTokens{Input: 750, Output: 125},
	})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+ag.ID+"/metrics", bytes.NewReader(body))
	req.Header.Set("X-Scion-Agent-Token", agentToken)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusCreated, rec.Code)

	var postResp map[string]string
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &postResp))
	metricsID := postResp["id"]

	// Wait until wall clock is strictly past segEnd so BackfillActiveRunAttributions
	// stamps run_attribution.created_at after the segment ended.
	if wait := time.Until(segEnd.Add(15 * time.Millisecond)); wait > 0 {
		time.Sleep(wait)
	}

	// Toggle experiment ON and reconcile.
	setUsageLedgerExperimentForTest(t, srv, true)
	srv.reconcileUsageLedger(ctx)

	runAttr, err := s.GetRunAttribution(ctx, runID)
	require.NoError(t, err)
	require.NotNil(t, runAttr)
	require.True(t, runAttr.CreatedAt.After(segEnd),
		"precondition: run_attribution.created_at (%v) must be after segEnd (%v)", runAttr.CreatedAt, segEnd)

	require.Eventually(t, func() bool {
		row, rerr := s.GetAgentSessionMetrics(ctx, metricsID)
		return rerr == nil && row.ConvertedAt != nil
	}, 2*time.Second, 20*time.Millisecond, "OFF-period segment should be caught up")

	// GET /api/v1/agents/{id}/usage must report legacy_client (not unmeasured).
	getReq := httptest.NewRequest(http.MethodGet, "/api/v1/agents/"+ag.ID+"/usage", nil)
	getReq.Header.Set("X-Scion-Agent-Token", agentToken)
	getRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(getRec, getReq)
	require.Equal(t, http.StatusOK, getRec.Code, getRec.Body.String())

	var usageResp AgentUsageResponse
	require.NoError(t, json.Unmarshal(getRec.Body.Bytes(), &usageResp))
	assert.Equal(t, MeasurementStatusLegacyClient, usageResp.MeasurementStatus)
	require.NotNil(t, usageResp.Tokens.Input)
	assert.Equal(t, int64(750), *usageResp.Tokens.Input)
	require.NotNil(t, usageResp.Tokens.Output)
	assert.Equal(t, int64(125), *usageResp.Tokens.Output)
}

func TestHandleAgentMetrics_QueuedLiveConversionSkipsWhenExperimentDisabled(t *testing.T) {
	// O-c: If the experiment is turned OFF while a live conversion job is
	// queued behind an earlier job, the worker must skip it and leave
	// converted_at NULL for the next ON catch-up.
	ctx := context.Background()
	srv, s := testServer(t)
	srv.SetHubID("hub-legacy-oc")
	t.Cleanup(srv.stopUsageLedger)

	proj := &store.Project{
		ID:   uuid.NewString(),
		Name: "OC Proj",
		Slug: "oc-proj-" + uuid.NewString()[:8],
	}
	require.NoError(t, s.CreateProject(ctx, proj))
	ag := &store.Agent{
		ID:        uuid.NewString(),
		Slug:      "oc-agent",
		Name:      "OC Agent",
		ProjectID: proj.ID,
		Phase:     string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, ag))
	agentToken := srv.generateRecordedAgentToken(t, ag.ID, proj.ID, []AgentTokenScope{ScopeAgentStatusUpdate}, nil)

	firstStarted := make(chan struct{}, 1)
	releaseFirst := make(chan struct{})
	srv.usageLedger.liveConvertFn = func(c context.Context, hubID string, row *store.AgentSessionMetrics, preferredRunID string) (bool, error) {
		if row.SessionID == "oc-sess-1" {
			select {
			case firstStarted <- struct{}{}:
			default:
			}
			<-releaseFirst
		}
		return s.ConvertLegacySessionMetricsRow(c, hubID, row, preferredRunID)
	}

	setUsageLedgerExperimentForTest(t, srv, true)

	post := func(sess string, offset time.Duration) string {
		now := time.Now().UTC().Add(offset).Truncate(time.Second)
		body, _ := json.Marshal(metricsPayloadRequest{
			Type:    "agent_metrics",
			AgentID: ag.ID,
			Session: metricsSession{
				ID:        sess,
				StartedAt: now.Add(-2 * time.Minute).Format(time.RFC3339),
				EndedAt:   now.Format(time.RFC3339),
				Status:    "completed",
				Model:     "claude-sonnet-4-6",
			},
			Tokens: metricsTokens{Input: 100, Output: 20},
		})
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+ag.ID+"/metrics", bytes.NewReader(body))
		req.Header.Set("X-Scion-Agent-Token", agentToken)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		require.Equal(t, http.StatusCreated, rec.Code)
		var resp map[string]string
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		return resp["id"]
	}

	id1 := post("oc-sess-1", -5*time.Minute)
	<-firstStarted
	id2 := post("oc-sess-2", 0)

	// Turn experiment OFF before job 2 runs.
	setUsageLedgerExperimentForTest(t, srv, false)
	close(releaseFirst)
	srv.usageLedger.liveInFlight.Wait()

	got1, err := s.GetAgentSessionMetrics(ctx, id1)
	require.NoError(t, err)
	assert.NotNil(t, got1.ConvertedAt, "in-flight job 1 completed")

	got2, err := s.GetAgentSessionMetrics(ctx, id2)
	require.NoError(t, err)
	assert.Nil(t, got2.ConvertedAt, "queued job 2 must be skipped once experiment is OFF")
}
