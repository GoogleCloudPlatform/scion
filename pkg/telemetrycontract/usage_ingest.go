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

package telemetrycontract

import (
	"fmt"
	"net/url"
	"time"
)

// Usage ingest wire limits and thresholds (Tokenomics design §4.1).
const (
	// UsageIngestMaxBatchEvents is the maximum number of UsageEvent items
	// accepted in a single ingest batch request.
	UsageIngestMaxBatchEvents = 500

	// UsageIngestMaxBatchBytes is the maximum HTTP request body size (256 KiB)
	// accepted by the usage ingest endpoint.
	UsageIngestMaxBatchBytes = 256 * 1024

	// UsageIngestAnomalousTokenThreshold is the per-event SummableTokens
	// threshold above which an event is stored and rolled up with
	// anomalous=true and excluded from automatic hard quota enforcement.
	UsageIngestAnomalousTokenThreshold int64 = 50_000_000

	// UsageIngestMaxFutureSkew is the maximum amount by which an event's
	// event_ts may exceed the Hub's received_at clock before being clamped to
	// received_at.
	UsageIngestMaxFutureSkew = 5 * time.Minute

	// DefaultUsageHeartbeatIntervalSec is the default cadence (seconds) for
	// empty heartbeat batches sent by sciontool when idle.
	DefaultUsageHeartbeatIntervalSec = 60

	// DefaultUsageDrainIntervalSec is the default drain cadence (seconds) for
	// buffered usage events in sciontool.
	DefaultUsageDrainIntervalSec = 10
)

// MeterHealth final_state values carried in UsageIngestRequest.FinalState.
const (
	MeterFinalStateOpen       = "open"
	MeterFinalStateFinal      = "final"
	MeterFinalStateIncomplete = "incomplete"
)

// ValidMeterFinalState reports whether s is empty or a valid final_state value.
func ValidMeterFinalState(s string) bool {
	switch s {
	case "", MeterFinalStateOpen, MeterFinalStateFinal, MeterFinalStateIncomplete:
		return true
	default:
		return false
	}
}

// Machine-readable per-event rejection codes returned in
// UsageIngestResponse.Rejected. Rejected events are dead-lettered by the
// client rather than retried indefinitely.
const (
	UsageRejectInvalidEvent     = "invalid_event"
	UsageRejectAgentMismatch    = "agent_mismatch"
	UsageRejectHubMismatch      = "hub_mismatch"
	UsageRejectUnknownRun       = "unknown_run"
	UsageRejectRunAgentMismatch = "run_agent_mismatch"
	UsageRejectValueOutOfRange  = "value_out_of_range"
	UsageRejectInvalidTimestamp = "invalid_timestamp"
	UsageRejectAgentDeleted     = "agent_deleted"
)

// UsageEnforcementAction values returned in UsageEnforcement.Action.
// P1.2 returns UsageEnforcementActionNone; P3 populates active actions.
const (
	UsageEnforcementActionNone            = "none"
	UsageEnforcementActionWarn            = "warn"
	UsageEnforcementActionPauseNew        = "pause_new"
	UsageEnforcementActionSuspendAndPause = "suspend_and_pause"
)

// UsageIngestAgentPath returns the agent-authenticated usage ingest path for
// agentID: POST /api/v1/agents/{id}/usage/events.
func UsageIngestAgentPath(agentID string) string {
	return fmt.Sprintf("/api/v1/agents/%s/usage/events", url.PathEscape(agentID))
}

// UsageCapabilitiesAgentPath returns the agent-authenticated capability probe
// path for agentID: GET /api/v1/agents/{id}/usage/capabilities.
func UsageCapabilitiesAgentPath(agentID string) string {
	return fmt.Sprintf("/api/v1/agents/%s/usage/capabilities", url.PathEscape(agentID))
}

// UsageIngestBrokerPath returns the broker-authenticated harvest ingest path:
// POST /api/v1/brokers/{bid}/agents/{id}/usage/events.
func UsageIngestBrokerPath(brokerID, agentID string) string {
	return fmt.Sprintf("/api/v1/brokers/%s/agents/%s/usage/events", url.PathEscape(brokerID), url.PathEscape(agentID))
}

// UsageSeqRange represents an inclusive missing producer sequence range
// [Start, End] reported in meter health updates.
type UsageSeqRange struct {
	Start int64 `json:"start"`
	End   int64 `json:"end"`
}

// UsageIngestRequest is the wire payload for POST /api/v1/agents/{id}/usage/events
// and POST /api/v1/brokers/{bid}/agents/{id}/usage/events.
//
// An empty Events slice is valid and acts as a meter-health heartbeat when
// ProducerEpoch (and RunID or agent token run_id) is supplied.
type UsageIngestRequest struct {
	// Events is the ordered batch of canonical usage events to ingest
	// (0..UsageIngestMaxBatchEvents).
	Events []UsageEvent `json:"events"`

	// RunID is the agent run identifier for heartbeats and the fallback run_id
	// for events in this batch that omit UsageEvent.RunID.
	RunID string `json:"run_id,omitempty"`

	// ProducerEpoch identifies the producer outbox epoch for meter_health
	// updates (heartbeats and batch-level continuity).
	ProducerEpoch string `json:"producer_epoch,omitempty"`

	// ProducerSeq is the highest sequence number emitted by the producer in
	// ProducerEpoch as of this batch/heartbeat.
	ProducerSeq int64 `json:"producer_seq,omitempty"`

	// LastContiguousSeq is the producer's optional contiguous sequence
	// watermark (all seqs <= this value were produced without local drops).
	LastContiguousSeq *int64 `json:"last_contiguous_seq,omitempty"`

	// MissingRanges optionally reports inclusive sequence gaps known to the
	// producer (for example, local outbox overflow drops).
	MissingRanges []UsageSeqRange `json:"missing_ranges,omitempty"`

	// LastSourceActivityAt is the RFC 3339 timestamp of the last observed
	// harness/source activity, allowing meter_health to distinguish an idle
	// agent from a disconnected or stalled meter.
	LastSourceActivityAt string `json:"last_source_activity_at,omitempty"`

	// SupportedSignals lists the usage signal categories the producer can
	// report for this run (e.g. ["calls", "input", "output", "cache_read",
	// "cache_write", "reasoning"]).
	SupportedSignals []string `json:"supported_signals,omitempty"`

	// SchemaDriftDetected signals that the producer detected an unknown
	// harness schema/version and degraded (e.g. calls_only).
	SchemaDriftDetected bool `json:"schema_drift_detected,omitempty"`

	// FinalState optionally transitions meter_health.final_state from "open"
	// to "final" or "incomplete" (e.g. shutdown drain or post-exit harvest).
	FinalState string `json:"final_state,omitempty"`
}

// UsageIngestRejection describes a single event rejected during ingest
// validation.
type UsageIngestRejection struct {
	Index   int    `json:"index"`
	EventID string `json:"event_id,omitempty"`
	Code    string `json:"code"`
	Reason  string `json:"reason"`
}

// UsageEnforcement carries quota/budget enforcement signals back to the agent
// on ingest responses. In P1.2 this is a neutral placeholder
// (Action = UsageEnforcementActionNone); P3 populates active directives.
type UsageEnforcement struct {
	Action  string `json:"action"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

// UsageIngestResponse is returned by POST /api/v1/agents/{id}/usage/events
// and POST /api/v1/brokers/{bid}/agents/{id}/usage/events.
type UsageIngestResponse struct {
	Accepted    int                    `json:"accepted"`
	Duplicates  int                    `json:"duplicates"`
	Rejected    []UsageIngestRejection `json:"rejected"`
	Enforcement *UsageEnforcement      `json:"enforcement,omitempty"`
}

// UsageCapabilitiesResponse is returned by
// GET /api/v1/agents/{id}/usage/capabilities when tokenomics.usage_ledger is
// enabled. When the experiment is disabled (or on older Hubs), the endpoint
// returns 404 and sciontool keeps its legacy direct-export behavior.
type UsageCapabilitiesResponse struct {
	UsageIngestEnabled   bool   `json:"usage_ingest_enabled"`
	SuppressDirectOTel   bool   `json:"suppress_direct_otel"`
	MaxBatchEvents       int    `json:"max_batch_events"`
	MaxBatchBytes        int    `json:"max_batch_bytes"`
	HeartbeatIntervalSec int    `json:"heartbeat_interval_sec"`
	DrainIntervalSec     int    `json:"drain_interval_sec"`
	HubID                string `json:"hub_id,omitempty"`
	RunID                string `json:"run_id,omitempty"`
}
