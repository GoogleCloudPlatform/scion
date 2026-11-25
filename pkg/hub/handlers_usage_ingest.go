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

package hub

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
)

// handleAgentUsageCapabilities serves GET /api/v1/agents/{id}/usage/capabilities.
// Gated behind tokenomics.usage_ledger (returns 404 when off so sciontool
// falls back to legacy direct-export behavior). Only the agent itself
// (X-Scion-Agent-Token) may query its capabilities.
func (s *Server) handleAgentUsageCapabilities(w http.ResponseWriter, r *http.Request, id string) {
	if !s.experimentEnabled(experiments.TokenomicsUsageLedger) {
		NotFound(w, "route")
		return
	}
	if r.Method != http.MethodGet {
		MethodNotAllowed(w, http.MethodGet)
		return
	}

	ctx := r.Context()
	agentIdent := GetAgentIdentityFromContext(ctx)
	if agentIdent == nil {
		Unauthorized(w)
		return
	}
	if agentIdent.ID() != id {
		writeError(w, http.StatusForbidden, ErrCodeForbidden,
			"Agents can only query their own usage capabilities", nil)
		return
	}

	agent, err := s.store.GetAgent(ctx, id)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	if agent == nil {
		NotFound(w, "Agent")
		return
	}

	writeJSON(w, http.StatusOK, telemetrycontract.UsageCapabilitiesResponse{
		UsageIngestEnabled: true,
		// Keep container-direct OTel export active until Hub OTel re-emission
		// lands in P2; advertising true before then would cause a P2-aware
		// sciontool to suppress gen_ai.api.calls / scion.usage.tokens while
		// this Hub does not yet re-emit them.
		SuppressDirectOTel:   false,
		MaxBatchEvents:       telemetrycontract.UsageIngestMaxBatchEvents,
		MaxBatchBytes:        telemetrycontract.UsageIngestMaxBatchBytes,
		HeartbeatIntervalSec: telemetrycontract.DefaultUsageHeartbeatIntervalSec,
		DrainIntervalSec:     telemetrycontract.DefaultUsageDrainIntervalSec,
		HubID:                s.HubID(),
		RunID:                agent.RunID,
	})
}

// handleAgentUsageEvents serves POST /api/v1/agents/{id}/usage/events.
// Gated behind tokenomics.usage_ledger. Only the agent itself
// (X-Scion-Agent-Token) may ingest its own usage events and heartbeats.
func (s *Server) handleAgentUsageEvents(w http.ResponseWriter, r *http.Request, id string) {
	if !s.experimentEnabled(experiments.TokenomicsUsageLedger) {
		NotFound(w, "route")
		return
	}
	if r.Method != http.MethodPost {
		MethodNotAllowed(w, http.MethodPost)
		return
	}

	ctx := r.Context()
	agentIdent := GetAgentIdentityFromContext(ctx)
	if agentIdent == nil {
		Unauthorized(w)
		return
	}
	if agentIdent.ID() != id {
		writeError(w, http.StatusForbidden, ErrCodeForbidden,
			"Agents can only report their own usage", nil)
		return
	}

	tokenRunID := presentedAgentTokenRunID(ctx)
	s.ingestUsageEvents(w, r, id, "", tokenRunID)
}

// handleBrokerAgentUsageEvents serves POST /api/v1/brokers/{bid}/agents/{id}/usage/events
// for broker post-exit / pre-delete outbox harvest drains.
// Gated behind tokenomics.usage_ledger and authenticated via broker HMAC;
// the calling broker must match {bid} and be the owning broker for {id}.
func (s *Server) handleBrokerAgentUsageEvents(w http.ResponseWriter, r *http.Request, brokerID, agentID string) {
	if !s.experimentEnabled(experiments.TokenomicsUsageLedger) {
		NotFound(w, "route")
		return
	}
	if r.Method != http.MethodPost {
		MethodNotAllowed(w, http.MethodPost)
		return
	}

	ctx := r.Context()
	brokerIdent := GetBrokerIdentityFromContext(ctx)
	if brokerIdent == nil {
		Unauthorized(w)
		return
	}
	if brokerIdent.BrokerID() != brokerID {
		Forbidden(w)
		return
	}

	agent, err := s.store.GetAgent(ctx, agentID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	if agent == nil {
		NotFound(w, "Agent")
		return
	}
	if agent.RuntimeBrokerID != brokerID {
		writeError(w, http.StatusForbidden, ErrCodeForbidden,
			"Broker does not own target agent", nil)
		return
	}

	s.ingestUsageEvents(w, r, agentID, brokerID, "")
}

func (s *Server) ingestUsageEvents(w http.ResponseWriter, r *http.Request, agentID, brokerID, tokenRunID string) {
	ctx := r.Context()

	// Enforce 256 KiB batch byte limit (§4.1).
	body, err := io.ReadAll(io.LimitReader(r.Body, int64(telemetrycontract.UsageIngestMaxBatchBytes)+1))
	if err != nil {
		BadRequest(w, "Failed to read request body: "+err.Error())
		return
	}
	if len(body) > telemetrycontract.UsageIngestMaxBatchBytes {
		writeError(w, http.StatusRequestEntityTooLarge, ErrCodeValidationError,
			fmt.Sprintf("request body exceeds maximum size of %d bytes", telemetrycontract.UsageIngestMaxBatchBytes), nil)
		return
	}

	var req telemetrycontract.UsageIngestRequest
	if err := json.Unmarshal(body, &req); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	if len(req.Events) > telemetrycontract.UsageIngestMaxBatchEvents {
		ValidationError(w, fmt.Sprintf("batch contains %d events, maximum is %d",
			len(req.Events), telemetrycontract.UsageIngestMaxBatchEvents), nil)
		return
	}
	if !telemetrycontract.ValidMeterFinalState(req.FinalState) {
		ValidationError(w, fmt.Sprintf("invalid final_state %q", req.FinalState), nil)
		return
	}

	receivedAt := time.Now().UTC().Truncate(time.Microsecond)
	maxFuture := receivedAt.Add(telemetrycontract.UsageIngestMaxFutureSkew)

	var lastSourceActivity *time.Time
	if req.LastSourceActivityAt != "" {
		t, err := telemetrycontract.ParseTS(req.LastSourceActivityAt)
		if err != nil {
			ValidationError(w, "invalid last_source_activity_at: "+err.Error(), nil)
			return
		}
		if t.After(maxFuture) {
			t = receivedAt
		}
		tUTC := t.UTC().Truncate(time.Microsecond)
		lastSourceActivity = &tUTC
	}

	agent, err := s.store.GetAgent(ctx, agentID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	if agent == nil {
		NotFound(w, "Agent")
		return
	}

	selfHubID := s.HubID()
	effectiveHubID := selfHubID
	if effectiveHubID == "" {
		effectiveHubID = "hub-local"
	}

	attrCache := make(map[string]*store.RunAttribution)
	resolveAttribution := func(runID string) (*store.RunAttribution, string, string, error) {
		if runID == "" {
			return nil, telemetrycontract.UsageRejectUnknownRun, "run_id is required", nil
		}
		if cached, ok := attrCache[runID]; ok {
			if cached == nil {
				return nil, telemetrycontract.UsageRejectUnknownRun, "run_attribution not found for run_id", nil
			}
			if cached.AgentID != agentID {
				return nil, telemetrycontract.UsageRejectRunAgentMismatch, "run_id belongs to a different agent", nil
			}
			return cached, "", "", nil
		}

		attr, err := s.store.GetRunAttribution(ctx, runID)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return nil, "", "", err
		}
		if attr == nil {
			// Lazy attribution fallback (P1.1 review O2): handle the narrow
			// experiment OFF->ON race when an event names the live agent's
			// current run_id. Never create attribution from payload, and never
			// lazily attribute soft-deleted agents (SoftFinishRunID).
			if agent.DeletedAt.IsZero() && agent.RunID != "" && runID == agent.RunID {
				attr, err = s.store.EnsureRunAttributionForAgent(ctx, agentID, runID, effectiveHubID)
				if err != nil {
					return nil, "", "", err
				}
			}
		}
		attrCache[runID] = attr
		if attr == nil {
			return nil, telemetrycontract.UsageRejectUnknownRun, "run_attribution not found for run_id", nil
		}
		if attr.AgentID != agentID {
			return nil, telemetrycontract.UsageRejectRunAgentMismatch, "run_id belongs to a different agent", nil
		}
		return attr, "", "", nil
	}

	rejected := make([]telemetrycontract.UsageIngestRejection, 0)
	validEvents := make([]*store.UsageEvent, 0, len(req.Events))

	for i := range req.Events {
		ev := &req.Events[i]

		if err := ev.Validate(); err != nil {
			rejected = append(rejected, telemetrycontract.UsageIngestRejection{
				Index:   i,
				EventID: ev.EventID,
				Code:    telemetrycontract.UsageRejectInvalidEvent,
				Reason:  err.Error(),
			})
			continue
		}

		// Agent path match (§4.1 item 3).
		if ev.AgentID != "" && ev.AgentID != agentID {
			rejected = append(rejected, telemetrycontract.UsageIngestRejection{
				Index:   i,
				EventID: ev.EventID,
				Code:    telemetrycontract.UsageRejectAgentMismatch,
				Reason:  "event agent_id does not match request path",
			})
			continue
		}

		// Hub ID match (§4.1 item 3).
		if ev.HubID != "" && selfHubID != "" && ev.HubID != selfHubID {
			rejected = append(rejected, telemetrycontract.UsageIngestRejection{
				Index:   i,
				EventID: ev.EventID,
				Code:    telemetrycontract.UsageRejectHubMismatch,
				Reason:  "event hub_id does not match this hub",
			})
			continue
		}

		// Safe integer bounds (§3.1, §4.1 item 3).
		if err := checkUsageEventSafeIntegers(ev); err != nil {
			rejected = append(rejected, telemetrycontract.UsageIngestRejection{
				Index:   i,
				EventID: ev.EventID,
				Code:    telemetrycontract.UsageRejectValueOutOfRange,
				Reason:  err.Error(),
			})
			continue
		}

		// Timestamp parsing & bounded clock-skew clamp (§4.1 item 3).
		eventTS, err := telemetrycontract.ParseTS(ev.EventTS)
		if err != nil {
			rejected = append(rejected, telemetrycontract.UsageIngestRejection{
				Index:   i,
				EventID: ev.EventID,
				Code:    telemetrycontract.UsageRejectInvalidTimestamp,
				Reason:  err.Error(),
			})
			continue
		}
		if eventTS.After(maxFuture) {
			eventTS = receivedAt
		}
		eventTS = eventTS.UTC().Truncate(time.Microsecond)

		// Run ID resolution: event -> batch -> agent token claim.
		runID := strings.TrimSpace(ev.RunID)
		if runID == "" {
			runID = strings.TrimSpace(req.RunID)
		}
		if runID == "" {
			runID = strings.TrimSpace(tokenRunID)
		}

		attr, rejectCode, rejectReason, err := resolveAttribution(runID)
		if err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
		if rejectCode != "" {
			rejected = append(rejected, telemetrycontract.UsageIngestRejection{
				Index:   i,
				EventID: ev.EventID,
				Code:    rejectCode,
				Reason:  rejectReason,
			})
			continue
		}

		// Anomalous spike guard (§4.1 item 3): > 50,000,000 summable tokens.
		summable, overflow := ev.SummableTokens()
		anomalous := overflow || summable > telemetrycontract.UsageIngestAnomalousTokenThreshold
		if anomalous {
			slog.Warn("usage ingest: anomalous token spike flagged",
				"agent_id", agentID, "run_id", attr.RunID, "event_id", ev.EventID,
				"summable_tokens", summable, "overflow", overflow)
		}

		// Anti-spoofing (§3.2): stamp ALL dimensions from Hub-frozen
		// run_attribution, ignoring payload project_id/user_id/harness/labels.
		// An empty harness string on run_attribution means unknown and must
		// NOT fall back to payload values.
		harness := attr.Harness
		var nativeRequestID *string
		if ev.SourceIdentity == telemetrycontract.SourceIdentityRequestID && ev.SourceEventID != "" {
			idCopy := ev.SourceEventID
			nativeRequestID = &idCopy
		}
		var sessionIDHash *string
		if ev.SessionID != "" {
			sum := sha256.Sum256([]byte(ev.SessionID))
			h := hex.EncodeToString(sum[:])
			sessionIDHash = &h
		}
		var costCurrency, costSource *string
		if ev.ReportedCostCurrency != "" {
			c := ev.ReportedCostCurrency
			costCurrency = &c
		}
		if ev.ReportedCostSource != "" {
			src := ev.ReportedCostSource
			costSource = &src
		}

		storeEv := &store.UsageEvent{
			HubID:                effectiveHubID,
			EventID:              ev.EventID,
			RunID:                attr.RunID,
			ProjectID:            attr.ProjectID,
			AgentID:              attr.AgentID,
			BillingUserID:        copyUsageStringPtr(attr.BillingUserID),
			BrokerID:             attr.BrokerID,
			Harness:              harness,
			Model:                telemetrycontract.ResolveModelLabel(ev.Model, ""),
			ModelSource:          ev.ModelSource,
			UsageSource:          ev.UsageSource,
			SourceKind:           ev.SourceKind,
			SourceIdentity:       ev.SourceIdentity,
			NativeRequestID:      nativeRequestID,
			SessionIDHash:        sessionIDHash,
			Status:               ev.Status,
			Calls:                ev.Calls,
			Input:                copyUsageInt64Ptr(ev.Input),
			Output:               copyUsageInt64Ptr(ev.Output),
			CacheRead:            copyUsageInt64Ptr(ev.CacheRead),
			CacheWrite:           copyUsageInt64Ptr(ev.CacheWrite),
			CacheWrite1h:         copyUsageInt64Ptr(ev.CacheWrite1h),
			Reasoning:            copyUsageInt64Ptr(ev.Reasoning),
			TokenStatus:          ev.TokenStatus,
			MalformedFields:      append([]string(nil), ev.MalformedFields...),
			ReportedCostMicros:   copyUsageInt64Ptr(ev.ReportedCostMicros),
			ReportedCostCurrency: costCurrency,
			ReportedCostSource:   costSource,
			EventTS:              eventTS,
			ReceivedAt:           receivedAt,
			ProducerEpoch:        ev.ProducerEpoch,
			ProducerSeq:          ev.ProducerSeq,
			Anomalous:            anomalous,
		}
		if storeEv.BrokerID == "" && brokerID != "" {
			storeEv.BrokerID = brokerID
		}
		validEvents = append(validEvents, storeEv)
	}

	// Build meter_health update for events and/or empty heartbeats.
	mhUpdate, err := buildMeterHealthUpdate(
		agentID, agent, &req, tokenRunID, receivedAt, lastSourceActivity, validEvents, resolveAttribution,
	)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	res, err := s.store.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
		AgentID:     agentID,
		Events:      validEvents,
		MeterHealth: mhUpdate,
	})
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	writeJSON(w, http.StatusOK, telemetrycontract.UsageIngestResponse{
		Accepted:   res.Accepted,
		Duplicates: res.Duplicates,
		Rejected:   rejected,
		Enforcement: &telemetrycontract.UsageEnforcement{
			Action: telemetrycontract.UsageEnforcementActionNone,
		},
	})
}

func buildMeterHealthUpdate(
	agentID string,
	agent *store.Agent,
	req *telemetrycontract.UsageIngestRequest,
	tokenRunID string,
	receivedAt time.Time,
	lastSourceActivity *time.Time,
	validEvents []*store.UsageEvent,
	resolveAttribution func(runID string) (*store.RunAttribution, string, string, error),
) (*store.UsageMeterHealthUpdate, error) {
	mhRunID := strings.TrimSpace(req.RunID)
	if mhRunID == "" {
		mhRunID = strings.TrimSpace(tokenRunID)
	}
	if mhRunID == "" && len(validEvents) > 0 {
		mhRunID = validEvents[len(validEvents)-1].RunID
	}
	if mhRunID == "" && len(req.Events) == 0 && agent != nil && agent.DeletedAt.IsZero() {
		mhRunID = agent.RunID
	}

	mhEpoch := strings.TrimSpace(req.ProducerEpoch)
	if mhEpoch == "" && len(validEvents) > 0 {
		mhEpoch = validEvents[len(validEvents)-1].ProducerEpoch
	}

	if mhRunID == "" || mhEpoch == "" {
		return nil, nil
	}

	// Verify mhRunID belongs to this agent before updating meter_health.
	attr, rejectCode, _, err := resolveAttribution(mhRunID)
	if err != nil {
		return nil, err
	}
	if rejectCode != "" || attr == nil {
		return nil, nil
	}

	var receivedSeqs []int64
	for _, ev := range validEvents {
		if ev.RunID == mhRunID && ev.ProducerEpoch == mhEpoch && ev.ProducerSeq > 0 {
			receivedSeqs = append(receivedSeqs, ev.ProducerSeq)
		}
	}

	var missing []store.SeqRange
	if req.MissingRanges != nil {
		missing = make([]store.SeqRange, len(req.MissingRanges))
		for i, r := range req.MissingRanges {
			missing[i] = store.SeqRange{Start: r.Start, End: r.End}
		}
	}

	return &store.UsageMeterHealthUpdate{
		AgentID:              agentID,
		RunID:                mhRunID,
		ProducerEpoch:        mhEpoch,
		LastReceiptAt:        receivedAt,
		ProducerSeq:          req.ProducerSeq,
		LastContiguousSeq:    copyUsageInt64Ptr(req.LastContiguousSeq),
		ReceivedSeqs:         receivedSeqs,
		MissingRanges:        missing,
		LastSourceActivityAt: lastSourceActivity,
		SupportedSignals:     append([]string(nil), req.SupportedSignals...),
		SchemaDriftDetected:  req.SchemaDriftDetected,
		FinalState:           req.FinalState,
	}, nil
}

func checkUsageEventSafeIntegers(ev *telemetrycontract.UsageEvent) error {
	if ev.Calls < 0 || ev.Calls > telemetrycontract.MaxSafeInteger {
		return fmt.Errorf("calls %d out of safe integer range [0, %d]", ev.Calls, int64(telemetrycontract.MaxSafeInteger))
	}
	if ev.ProducerSeq < 1 || ev.ProducerSeq > telemetrycontract.MaxSafeInteger {
		return fmt.Errorf("producer_seq %d out of safe integer range [1, %d]", ev.ProducerSeq, int64(telemetrycontract.MaxSafeInteger))
	}
	checks := []struct {
		name string
		val  *int64
	}{
		{"input", ev.Input},
		{"output", ev.Output},
		{"cache_read", ev.CacheRead},
		{"cache_write", ev.CacheWrite},
		{"cache_write_1h", ev.CacheWrite1h},
		{"reasoning", ev.Reasoning},
		{"reported_cost_micros", ev.ReportedCostMicros},
	}
	for _, c := range checks {
		if c.val != nil && (*c.val < 0 || *c.val > telemetrycontract.MaxSafeInteger) {
			return fmt.Errorf("%s %d out of safe integer range [0, %d]", c.name, *c.val, int64(telemetrycontract.MaxSafeInteger))
		}
	}
	return nil
}

func copyUsageStringPtr(s *string) *string {
	if s == nil {
		return nil
	}
	cp := *s
	return &cp
}

func copyUsageInt64Ptr(v *int64) *int64 {
	if v == nil {
		return nil
	}
	cp := *v
	return &cp
}
