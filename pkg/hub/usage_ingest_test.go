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
	"strings"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/usageevent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/usagerollupdaily"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
)

func usageInt64p(v int64) *int64 { return &v }

func makeWireUsageEvent(hubID, agentID, projectID, runID, eventID string, seq int64, ts time.Time, in, out int64) telemetrycontract.UsageEvent {
	env := telemetrycontract.NewEnvelope(
		telemetrycontract.EventTypeUsage,
		eventID,
		"sciontool/agent/"+agentID,
		ts,
		ts,
	)
	env.HubID = hubID
	env.ProjectID = projectID
	env.AgentID = agentID
	return telemetrycontract.UsageEvent{
		Envelope:            env,
		UsageSource:         telemetrycontract.UsageSourceNative,
		SourceKind:          "claude.api_request",
		SourceSchemaVersion: "1",
		SourceEventID:       "rid1:" + eventID,
		SourceIdentity:      telemetrycontract.SourceIdentityRequestID,
		SourceOccurrence:    1,
		ProducerEpoch:       "ep-1",
		ProducerSeq:         seq,
		RunID:               runID,
		Harness:             "claude",
		Model:               "claude-sonnet-4-6",
		ModelSource:         telemetrycontract.ModelSourceEvent,
		Status:              telemetrycontract.StatusSuccess,
		Calls:               1,
		Input:               usageInt64p(in),
		Output:              usageInt64p(out),
		InputDerivation:     telemetrycontract.InputDerivationReported,
		TokenStatus:         telemetrycontract.TokenStatusReported,
		MalformedFields:     []string{},
	}
}

func doAgentUsageRequest(t *testing.T, srv *Server, method, path, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var bodyBytes []byte
	if body != nil {
		var err error
		bodyBytes, err = json.Marshal(body)
		require.NoError(t, err)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(bodyBytes))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("X-Scion-Agent-Token", token)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func TestUsageIngest_ExperimentOffReturns404(t *testing.T) {
	ctx := context.Background()
	srv, s := testServer(t)
	srv.SetHubID("hub-ingest-test")
	setUsageLedgerExperimentForTest(t, srv, false)

	proj := &store.Project{ID: tid("usage-off-proj"), Name: "usage-off", Slug: "usage-off"}
	require.NoError(t, s.CreateProject(ctx, proj))
	ag := &store.Agent{
		ID:        tid("usage-off-agent"),
		Slug:      "usage-off-agent",
		Name:      "usage-off-agent",
		ProjectID: proj.ID,
		Phase:     string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, ag))
	tok := srv.generateRecordedAgentToken(t, ag.ID, proj.ID, []AgentTokenScope{ScopeAgentStatusUpdate}, nil)

	// Agent ingest -> 404
	rec := doAgentUsageRequest(t, srv, http.MethodPost, telemetrycontract.UsageIngestAgentPath(ag.ID), tok, telemetrycontract.UsageIngestRequest{})
	assert.Equal(t, http.StatusNotFound, rec.Code)

	// Capability probe -> 404
	rec = doAgentUsageRequest(t, srv, http.MethodGet, telemetrycontract.UsageCapabilitiesAgentPath(ag.ID), tok, nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)

	// Broker harvest (with valid broker HMAC) -> 404 when experiment is off.
	brk, brkKey := newOnboardingSigningBroker(t, s, "usage-off-broker")
	rec = doBrokerSignedRequest(t, srv, brk.ID, brkKey, "", http.MethodPost, telemetrycontract.UsageIngestBrokerPath(brk.ID, ag.ID), telemetrycontract.UsageIngestRequest{})
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestUsageIngest_AuthAndCapabilityProbe(t *testing.T) {
	ctx := context.Background()
	srv, s := testServer(t)
	srv.SetHubID("hub-cap-test")
	setUsageLedgerExperimentForTest(t, srv, true)
	srv.reconcileUsageLedger(ctx)

	proj := &store.Project{ID: tid("usage-auth-proj"), Name: "usage-auth", Slug: "usage-auth"}
	require.NoError(t, s.CreateProject(ctx, proj))
	ag1 := &store.Agent{ID: tid("usage-auth-a1"), Slug: "a1", Name: "a1", ProjectID: proj.ID, Phase: string(state.PhaseRunning)}
	ag2 := &store.Agent{ID: tid("usage-auth-a2"), Slug: "a2", Name: "a2", ProjectID: proj.ID, Phase: string(state.PhaseRunning)}
	require.NoError(t, s.CreateAgent(ctx, ag1))
	require.NoError(t, s.CreateAgent(ctx, ag2))
	_, err := s.SetAgentRunID(ctx, ag1.ID, "run-a1", nil)
	require.NoError(t, err)

	tok1 := srv.generateRecordedAgentToken(t, ag1.ID, proj.ID, []AgentTokenScope{ScopeAgentStatusUpdate}, nil)
	tok2 := srv.generateRecordedAgentToken(t, ag2.ID, proj.ID, []AgentTokenScope{ScopeAgentStatusUpdate}, nil)

	// No token -> 401
	rec := doAgentUsageRequest(t, srv, http.MethodPost, telemetrycontract.UsageIngestAgentPath(ag1.ID), "", telemetrycontract.UsageIngestRequest{})
	assert.Equal(t, http.StatusUnauthorized, rec.Code)

	// Wrong agent token -> 403 on ingest and capabilities
	rec = doAgentUsageRequest(t, srv, http.MethodPost, telemetrycontract.UsageIngestAgentPath(ag1.ID), tok2, telemetrycontract.UsageIngestRequest{})
	assert.Equal(t, http.StatusForbidden, rec.Code)
	rec = doAgentUsageRequest(t, srv, http.MethodGet, telemetrycontract.UsageCapabilitiesAgentPath(ag1.ID), tok2, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code)

	// Self token -> 200 on capability probe
	rec = doAgentUsageRequest(t, srv, http.MethodGet, telemetrycontract.UsageCapabilitiesAgentPath(ag1.ID), tok1, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var capResp telemetrycontract.UsageCapabilitiesResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &capResp))
	assert.True(t, capResp.UsageIngestEnabled)
	assert.True(t, capResp.SuppressDirectOTel)
	assert.Equal(t, telemetrycontract.UsageIngestMaxBatchEvents, capResp.MaxBatchEvents)
	assert.Equal(t, telemetrycontract.UsageIngestMaxBatchBytes, capResp.MaxBatchBytes)
	assert.Equal(t, "hub-cap-test", capResp.HubID)
	assert.Equal(t, "run-a1", capResp.RunID)
}

func TestUsageIngest_EndToEndIdempotentAntiSpoofingAndAnomalous(t *testing.T) {
	ctx := context.Background()
	srv, s := testServer(t)
	srv.SetHubID("hub-e2e")
	setUsageLedgerExperimentForTest(t, srv, true)
	srv.reconcileUsageLedger(ctx)

	owner := &store.User{ID: uuid.NewString(), Email: "e2e-owner@example.com", DisplayName: "Owner", Role: "member", Status: "active"}
	require.NoError(t, s.CreateUser(ctx, owner))
	proj := &store.Project{ID: tid("usage-e2e-proj"), Name: "usage-e2e", Slug: "usage-e2e"}
	require.NoError(t, s.CreateProject(ctx, proj))
	ag := &store.Agent{
		ID:            tid("usage-e2e-agent"),
		Slug:          "usage-e2e-agent",
		Name:          "usage-e2e-agent",
		ProjectID:     proj.ID,
		OwnerID:       owner.ID,
		CreatedBy:     owner.ID,
		AppliedConfig: &store.AgentAppliedConfig{HarnessConfig: "claude"},
		Template:      "default",
		Phase:         string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, ag))
	_, err := s.SetAgentRunID(ctx, ag.ID, "run-e2e-1", nil)
	require.NoError(t, err)
	tok := srv.generateRecordedAgentToken(t, ag.ID, proj.ID, []AgentTokenScope{ScopeAgentStatusUpdate}, nil)

	now := time.Now().UTC().Truncate(time.Microsecond)

	// 1. Anti-spoofing: event claims forged project_id and forged harness; also test future clock-skew clamp & anomalous >50M spike.
	evNormal := makeWireUsageEvent("hub-e2e", ag.ID, "forged-project-id", "run-e2e-1", "evt-e2e-1", 1, now.Add(2*time.Hour), 100, 50)
	evNormal.Harness = "forged-harness"

	evSpike := makeWireUsageEvent("hub-e2e", ag.ID, "forged-project-id", "run-e2e-1", "evt-e2e-2", 2, now, 55_000_000, 0)

	// Calls-only event with NULL token fields.
	evCallsOnly := makeWireUsageEvent("hub-e2e", ag.ID, proj.ID, "run-e2e-1", "evt-e2e-3", 3, now, 0, 0)
	evCallsOnly.Input, evCallsOnly.Output, evCallsOnly.InputDerivation = nil, nil, ""
	evCallsOnly.TokenStatus = telemetrycontract.TokenStatusNotReported

	// Bad event: wrong agent_id in payload -> per-event reject.
	evWrongAgent := makeWireUsageEvent("hub-e2e", "other-agent-id", proj.ID, "run-e2e-1", "evt-e2e-bad", 4, now, 10, 5)

	reqBody := telemetrycontract.UsageIngestRequest{
		Events:               []telemetrycontract.UsageEvent{evNormal, evSpike, evCallsOnly, evWrongAgent},
		RunID:                "run-e2e-1",
		ProducerEpoch:        "ep-1",
		ProducerSeq:          3,
		LastSourceActivityAt: telemetrycontract.FormatTS(now),
		SupportedSignals:     []string{"calls", "input", "output"},
	}
	beforePost := time.Now().UTC().Truncate(time.Microsecond)
	rec := doAgentUsageRequest(t, srv, http.MethodPost, telemetrycontract.UsageIngestAgentPath(ag.ID), tok, reqBody)
	afterPost := time.Now().UTC().Truncate(time.Microsecond)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp telemetrycontract.UsageIngestResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, 3, resp.Accepted)
	assert.Equal(t, 0, resp.Duplicates)
	require.Len(t, resp.Rejected, 1)
	assert.Equal(t, 3, resp.Rejected[0].Index)
	assert.Equal(t, telemetrycontract.UsageRejectAgentMismatch, resp.Rejected[0].Code)
	require.NotNil(t, resp.Enforcement)
	assert.Equal(t, telemetrycontract.UsageEnforcementActionNone, resp.Enforcement.Action)

	// R3: Verify stored usage_events row and usage_rollups_daily directly in DB:
	// - project_id and harness were stamped from run_attribution (forged payload ignored),
	// - future event_ts (+2h) was clamped to received_at within [beforePost, afterPost],
	// - rollup exists for real proj.ID and NO rollup exists for forged-project-id.
	cs, ok := s.(*entadapter.CompositeStore)
	require.True(t, ok, "test store must be CompositeStore")
	entClient := ent.NewClient(ent.Driver(entsql.OpenDB(dialect.SQLite, cs.DB())))

	storedEv, err := entClient.UsageEvent.Query().
		Where(usageevent.HubIDEQ("hub-e2e"), usageevent.EventIDEQ("evt-e2e-1")).
		Only(ctx)
	require.NoError(t, err)
	assert.Equal(t, proj.ID, storedEv.ProjectID, "stored usage_events.project_id must come from run_attribution, not payload")
	assert.Equal(t, "claude", storedEv.Harness, "stored usage_events.harness must come from run_attribution, not payload")
	assert.Equal(t, storedEv.ReceivedAt.UTC(), storedEv.EventTs.UTC(), "future event_ts must be clamped to received_at")
	assert.False(t, storedEv.EventTs.UTC().Before(beforePost), "clamped event_ts %v before request start %v", storedEv.EventTs, beforePost)
	assert.False(t, storedEv.EventTs.UTC().After(afterPost), "clamped event_ts %v after request end %v", storedEv.EventTs, afterPost)

	realRollups, err := entClient.UsageRollupDaily.Query().
		Where(usagerollupdaily.ProjectIDEQ(proj.ID), usagerollupdaily.AgentIDEQ(ag.ID)).
		All(ctx)
	require.NoError(t, err)
	require.Len(t, realRollups, 1)
	assert.Equal(t, "claude", realRollups[0].Harness)
	assert.Equal(t, int64(3), realRollups[0].Events)

	forgedCount, err := entClient.UsageRollupDaily.Query().
		Where(usagerollupdaily.ProjectIDEQ("forged-project-id")).
		Count(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, forgedCount, "no rollup row may exist for forged-project-id")

	// 2. Duplicate replay of same batch -> duplicates=3, accepted=0.
	recDup := doAgentUsageRequest(t, srv, http.MethodPost, telemetrycontract.UsageIngestAgentPath(ag.ID), tok, reqBody)
	require.Equal(t, http.StatusOK, recDup.Code)
	var respDup telemetrycontract.UsageIngestResponse
	require.NoError(t, json.Unmarshal(recDup.Body.Bytes(), &respDup))
	assert.Equal(t, 0, respDup.Accepted)
	assert.Equal(t, 3, respDup.Duplicates)

	// 3. Heartbeat-only batch updates meter_health (last_source_activity_at, supported_signals, final_state).
	later := now.Add(30 * time.Second)
	hbReq := telemetrycontract.UsageIngestRequest{
		Events:               []telemetrycontract.UsageEvent{},
		RunID:                "run-e2e-1",
		ProducerEpoch:        "ep-1",
		ProducerSeq:          3,
		LastSourceActivityAt: telemetrycontract.FormatTS(later),
		SupportedSignals:     []string{"calls", "input", "output", "cache_read"},
		FinalState:           telemetrycontract.MeterFinalStateFinal,
	}
	recHB := doAgentUsageRequest(t, srv, http.MethodPost, telemetrycontract.UsageIngestAgentPath(ag.ID), tok, hbReq)
	require.Equal(t, http.StatusOK, recHB.Code, recHB.Body.String())

	mhs, err := s.GetMeterHealth(ctx, ag.ID, "run-e2e-1")
	require.NoError(t, err)
	require.Len(t, mhs, 1)
	assert.Equal(t, int64(1), mhs[0].AnomalousCount)
	assert.Equal(t, int64(3), mhs[0].LastContiguousSeq)
	assert.Equal(t, store.MeterHealthFinalStateFinal, mhs[0].FinalState)
	assert.Equal(t, []string{"calls", "input", "output", "cache_read"}, mhs[0].SupportedSignals)

	// 4. Verify run_attribution dimensions were authoritative (not forged-project-id).
	attr, err := s.GetRunAttribution(ctx, "run-e2e-1")
	require.NoError(t, err)
	assert.Equal(t, proj.ID, attr.ProjectID)
	assert.Equal(t, "claude", attr.Harness)

	// 5. Verify body size > 256 KiB returns 413.
	oversized := bytes.NewReader([]byte(`{"events":[],"run_id":"` + strings.Repeat("a", telemetrycontract.UsageIngestMaxBatchBytes) + `"}`))
	reqOver := httptest.NewRequest(http.MethodPost, telemetrycontract.UsageIngestAgentPath(ag.ID), oversized)
	reqOver.Header.Set("Content-Type", "application/json")
	reqOver.Header.Set("X-Scion-Agent-Token", tok)
	recOver := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recOver, reqOver)
	assert.Equal(t, http.StatusRequestEntityTooLarge, recOver.Code)
}

func TestUsageIngest_LazyAttributionAndUnknownRunRejection(t *testing.T) {
	ctx := context.Background()
	srv, s := testServer(t)
	srv.SetHubID("hub-lazy")

	// Start with experiment OFF so SetAgentRunID writes NO run_attribution row (simulates OFF->ON race).
	setUsageLedgerExperimentForTest(t, srv, false)
	srv.reconcileUsageLedger(ctx)

	proj := &store.Project{ID: tid("usage-lazy-proj"), Name: "usage-lazy", Slug: "usage-lazy"}
	require.NoError(t, s.CreateProject(ctx, proj))
	ag1 := &store.Agent{ID: tid("usage-lazy-a1"), Slug: "lazy-a1", Name: "lazy-a1", ProjectID: proj.ID, Phase: string(state.PhaseRunning)}
	ag2 := &store.Agent{ID: tid("usage-lazy-a2"), Slug: "lazy-a2", Name: "lazy-a2", ProjectID: proj.ID, Phase: string(state.PhaseRunning)}
	require.NoError(t, s.CreateAgent(ctx, ag1))
	require.NoError(t, s.CreateAgent(ctx, ag2))

	_, err := s.SetAgentRunID(ctx, ag1.ID, "run-lazy-current", nil)
	require.NoError(t, err)
	_, err = s.SetAgentRunID(ctx, ag2.ID, "run-other-agent", nil)
	require.NoError(t, err)

	// Flip experiment ON without running backfill so run-lazy-current has no attribution yet.
	setUsageLedgerExperimentForTest(t, srv, true)
	// Ensure ag2's run DOES have attribution so we can test cross-agent rejection.
	_, err = s.EnsureRunAttributionForAgent(ctx, ag2.ID, "run-other-agent", "hub-lazy")
	require.NoError(t, err)

	_, err = s.GetRunAttribution(ctx, "run-lazy-current")
	require.ErrorIs(t, err, store.ErrNotFound)

	tok1 := srv.generateRecordedAgentToken(t, ag1.ID, proj.ID, []AgentTokenScope{ScopeAgentStatusUpdate}, nil)
	now := time.Now().UTC().Truncate(time.Microsecond)

	evCurrent := makeWireUsageEvent("hub-lazy", ag1.ID, "forged-proj", "run-lazy-current", "evt-lazy-1", 1, now, 100, 20)
	evUnknown := makeWireUsageEvent("hub-lazy", ag1.ID, proj.ID, "run-does-not-exist", "evt-lazy-2", 2, now, 100, 20)
	evOtherAgent := makeWireUsageEvent("hub-lazy", ag1.ID, proj.ID, "run-other-agent", "evt-lazy-3", 3, now, 100, 20)

	rec := doAgentUsageRequest(t, srv, http.MethodPost, telemetrycontract.UsageIngestAgentPath(ag1.ID), tok1, telemetrycontract.UsageIngestRequest{
		Events: []telemetrycontract.UsageEvent{evCurrent, evUnknown, evOtherAgent},
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp telemetrycontract.UsageIngestResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, 1, resp.Accepted)
	require.Len(t, resp.Rejected, 2)
	assert.Equal(t, telemetrycontract.UsageRejectUnknownRun, resp.Rejected[0].Code)
	assert.Equal(t, telemetrycontract.UsageRejectRunAgentMismatch, resp.Rejected[1].Code)

	// Verify run-lazy-current was lazily attributed from Hub agent state (NOT forged-proj).
	attr, err := s.GetRunAttribution(ctx, "run-lazy-current")
	require.NoError(t, err)
	assert.Equal(t, proj.ID, attr.ProjectID)
	assert.Equal(t, ag1.ID, attr.AgentID)
}

func TestUsageIngest_BrokerHarvestRoute(t *testing.T) {
	ctx := context.Background()
	srv, s := testServer(t)
	srv.SetHubID("hub-broker-harvest")
	setUsageLedgerExperimentForTest(t, srv, true)
	srv.reconcileUsageLedger(ctx)

	proj := &store.Project{ID: tid("usage-brk-proj"), Name: "usage-brk", Slug: "usage-brk"}
	require.NoError(t, s.CreateProject(ctx, proj))

	broker1, key1 := newOnboardingSigningBroker(t, s, "usage-broker-1")
	broker2, key2 := newOnboardingSigningBroker(t, s, "usage-broker-2")

	ag := &store.Agent{
		ID:              tid("usage-brk-agent"),
		Slug:            "usage-brk-agent",
		Name:            "usage-brk-agent",
		ProjectID:       proj.ID,
		RuntimeBrokerID: broker1.ID,
		Phase:           string(state.PhaseStopped),
	}
	require.NoError(t, s.CreateAgent(ctx, ag))
	_, err := s.SetAgentRunID(ctx, ag.ID, "run-brk-1", nil)
	require.NoError(t, err)

	now := time.Now().UTC().Truncate(time.Microsecond)
	ev := makeWireUsageEvent("hub-broker-harvest", ag.ID, proj.ID, "run-brk-1", "evt-brk-1", 1, now, 300, 120)
	reqBody := telemetrycontract.UsageIngestRequest{
		Events:        []telemetrycontract.UsageEvent{ev},
		RunID:         "run-brk-1",
		ProducerEpoch: "ep-brk",
		ProducerSeq:   1,
		FinalState:    telemetrycontract.MeterFinalStateFinal,
	}

	// Non-owning broker2 -> 403.
	rec := doBrokerSignedRequest(t, srv, broker2.ID, key2, "", http.MethodPost, telemetrycontract.UsageIngestBrokerPath(broker2.ID, ag.ID), reqBody)
	assert.Equal(t, http.StatusForbidden, rec.Code)

	// Broker2 calling broker1's path -> 403.
	rec = doBrokerSignedRequest(t, srv, broker2.ID, key2, "", http.MethodPost, telemetrycontract.UsageIngestBrokerPath(broker1.ID, ag.ID), reqBody)
	assert.Equal(t, http.StatusForbidden, rec.Code)

	// Owning broker1 -> 200 OK.
	rec = doBrokerSignedRequest(t, srv, broker1.ID, key1, "", http.MethodPost, telemetrycontract.UsageIngestBrokerPath(broker1.ID, ag.ID), reqBody)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp telemetrycontract.UsageIngestResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, 1, resp.Accepted)
}
