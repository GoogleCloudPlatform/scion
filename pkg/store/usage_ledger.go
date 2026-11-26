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

package store

import (
	"context"
	"errors"
	"strings"
	"time"
)

// CanonicalHarnessName normalizes a known harness name to its canonical
// identifier, or returns "" when name is not one of the built-in harness names.
// Parity with config.ShellHarnessConfigName is pinned by a test in pkg/hub so
// pkg/store stays free of a dependency on pkg/config.
func CanonicalHarnessName(name string) string {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "claude":
		return "claude"
	case "gemini", "gemini-cli":
		return "gemini-cli"
	case "opencode":
		return "opencode"
	case "codex":
		return "codex"
	case "generic":
		return "generic"
	case "shell":
		return "shell"
	default:
		return ""
	}
}

// ResolveRunAttributionHarness resolves the canonical harness type stored on
// run_attribution from the agent's own stored config records without fabricating
// a harness from the harness-config name:
//  1. AppliedConfig.Harness (stamped by the Hub at create),
//  2. AppliedConfig.InlineConfig.Harness,
//  3. EffectiveConfig.Harness (broker-reported).
//
// Each non-empty candidate is canonicalized via CanonicalHarnessName (keeping
// the trimmed value when it is a custom/non-builtin harness type). Returns ""
// when none of the three is set.
func ResolveRunAttributionHarness(ac *AgentAppliedConfig, ec *AgentEffectiveConfig) string {
	canonicalOrTrim := func(raw string) string {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" {
			return ""
		}
		if c := CanonicalHarnessName(trimmed); c != "" {
			return c
		}
		return trimmed
	}
	if ac != nil {
		if h := canonicalOrTrim(ac.Harness); h != "" {
			return h
		}
		if ac.InlineConfig != nil {
			if h := canonicalOrTrim(ac.InlineConfig.Harness); h != "" {
				return h
			}
		}
	}
	if ec != nil {
		if h := canonicalOrTrim(ec.Harness); h != "" {
			return h
		}
	}
	return ""
}

// Usage ledger scope kinds for usage_scope_counters_daily.
const (
	UsageScopeKindProject     = "project"
	UsageScopeKindUser        = "user"
	UsageScopeKindUserProject = "user_project"
)

// Usage ledger source values for usage_events.usage_source and
// usage_rollups_daily.usage_source.
const (
	UsageSourceNative               = "native"
	UsageSourceHooks                = "hooks"
	UsageSourceSQLiteNative         = "sqlite_native"
	UsageSourceFileTailer           = "file_tailer"
	UsageSourceLegacySessionMetrics = "legacy_session_metrics"
)

// MeterHealthFinalState values for meter_health.final_state.
const (
	MeterHealthFinalStateOpen       = "open"
	MeterHealthFinalStateFinal      = "final"
	MeterHealthFinalStateIncomplete = "incomplete"
)

// ErrRunAttributionDigestConflict is returned when a broker resolved-config
// report attempts to overwrite an already-populated fill-once digest or
// harness_build on run_attribution with a different non-empty value.
var ErrRunAttributionDigestConflict = errors.New("run attribution digest conflict")

// SeqRange represents an inclusive missing producer sequence range [Start, End]
// tracked on meter_health.missing_ranges.
type SeqRange struct {
	Start int64 `json:"start"`
	End   int64 `json:"end"`
}

// RunAttribution is the Hub-frozen immutable per-run attribution snapshot
// created atomically when a run_id is minted or adopted. Every Track 1 usage
// event/rollup and Track 2 manifest/usage record binds to a run_attribution row.
type RunAttribution struct {
	RunID               string            `json:"runId"`
	LaunchID            string            `json:"launchId,omitempty"`
	Generation          int               `json:"generation"`
	PriorRunID          string            `json:"priorRunId,omitempty"`
	HubID               string            `json:"hubId,omitempty"`
	ProjectID           string            `json:"projectId"`
	AgentID             string            `json:"agentId"`
	AgentSlug           string            `json:"agentSlug,omitempty"`
	BrokerID            string            `json:"brokerId,omitempty"`
	BillingUserID       *string           `json:"billingUserId,omitempty"`
	OwnerID             *string           `json:"ownerId,omitempty"`
	CreatedBy           *string           `json:"createdBy,omitempty"`
	Harness             string            `json:"harness,omitempty"`
	HarnessConfig       string            `json:"harnessConfig,omitempty"`
	Template            string            `json:"template,omitempty"`
	HarnessConfigDigest *string           `json:"harnessConfigDigest,omitempty"`
	TemplateDigest      *string           `json:"templateDigest,omitempty"`
	HarnessBuild        *string           `json:"harnessBuild,omitempty"`
	LabelsAtLaunch      map[string]string `json:"labelsAtLaunch,omitempty"`
	MeteringWriterMode  string            `json:"meteringWriterMode,omitempty"`
	UsageLiveSource     string            `json:"usageLiveSource,omitempty"`
	CapturePolicy       string            `json:"capturePolicy,omitempty"`
	NoticeVersion       string            `json:"noticeVersion,omitempty"`
	BackfillExisting    bool              `json:"backfillExisting"`
	LegalHold           bool              `json:"legalHold"`
	CreatedAt           time.Time         `json:"createdAt"`
}

// RunAttributionDigestUpdate carries the broker-reported resolved-config
// digests to fill once on a run_attribution row (WHERE column IS NULL).
type RunAttributionDigestUpdate struct {
	HarnessConfigDigest *string
	TemplateDigest      *string
	HarnessBuild        *string
}

// UsageEvent is one row in the append-only operational usage_events ledger.
// Unknown token counts are nil (SQL NULL), never fabricated as 0.
type UsageEvent struct {
	ID                   string    `json:"id,omitempty"`
	HubID                string    `json:"hubId"`
	EventID              string    `json:"eventId"`
	RunID                string    `json:"runId"`
	ProjectID            string    `json:"projectId"`
	AgentID              string    `json:"agentId"`
	BillingUserID        *string   `json:"billingUserId,omitempty"`
	BrokerID             string    `json:"brokerId,omitempty"`
	Harness              string    `json:"harness"`
	Model                string    `json:"model"`
	ModelSource          string    `json:"modelSource"`
	UsageSource          string    `json:"usageSource"`
	SourceKind           string    `json:"sourceKind,omitempty"`
	SourceIdentity       string    `json:"sourceIdentity,omitempty"`
	NativeRequestID      *string   `json:"nativeRequestId,omitempty"`
	SessionIDHash        *string   `json:"sessionIdHash,omitempty"`
	Status               string    `json:"status"`
	Calls                int64     `json:"calls"`
	Input                *int64    `json:"input,omitempty"`
	Output               *int64    `json:"output,omitempty"`
	CacheRead            *int64    `json:"cacheRead,omitempty"`
	CacheWrite           *int64    `json:"cacheWrite,omitempty"`
	CacheWrite1h         *int64    `json:"cacheWrite1h,omitempty"`
	Reasoning            *int64    `json:"reasoning,omitempty"`
	TokenStatus          string    `json:"tokenStatus"`
	MalformedFields      []string  `json:"malformedFields,omitempty"`
	ReportedCostMicros   *int64    `json:"reportedCostMicros,omitempty"`
	ReportedCostCurrency *string   `json:"reportedCostCurrency,omitempty"`
	ReportedCostSource   *string   `json:"reportedCostSource,omitempty"`
	EventTS              time.Time `json:"eventTs"`
	ReceivedAt           time.Time `json:"receivedAt"`
	ProducerEpoch        string    `json:"producerEpoch,omitempty"`
	ProducerSeq          int64     `json:"producerSeq"`
	Anomalous            bool      `json:"anomalous"`
	ChargedGroupIDs      []string  `json:"chargedGroupIds,omitempty"`
	RelocatedFromHubID   *string   `json:"relocatedFromHubId,omitempty"`
}

// UsageRollupDaily is one row in usage_rollups_daily. BillingUserID uses ""
// for NULL billing_user_id so the composite unique key is dialect-uniform.
type UsageRollupDaily struct {
	ID                 string    `json:"id,omitempty"`
	BucketStart        time.Time `json:"bucketStart"`
	ProjectID          string    `json:"projectId"`
	BillingUserID      string    `json:"billingUserId"`
	AgentID            string    `json:"agentId"`
	Harness            string    `json:"harness"`
	Model              string    `json:"model"`
	UsageSource        string    `json:"usageSource"`
	Calls              int64     `json:"calls"`
	ErrorCalls         int64     `json:"errorCalls"`
	Input              int64     `json:"input"`
	Output             int64     `json:"output"`
	CacheRead          int64     `json:"cacheRead"`
	CacheWrite         int64     `json:"cacheWrite"`
	CacheWrite1h       int64     `json:"cacheWrite1h"`
	Reasoning          int64     `json:"reasoning"`
	ReportedCostMicros int64     `json:"reportedCostMicros"`
	Events             int64     `json:"events"`
	NullTokenEvents    int64     `json:"nullTokenEvents"`
	FirstEventTS       time.Time `json:"firstEventTs"`
	LastEventTS        time.Time `json:"lastEventTs"`
}

// UsageScopeCounterDaily is one row in usage_scope_counters_daily for O(1)
// exact quota accounting. For ScopeKind == "user_project", ScopeID is
// "<user_id>:<project_id>".
type UsageScopeCounterDaily struct {
	ID              string    `json:"id,omitempty"`
	ScopeKind       string    `json:"scopeKind"`
	ScopeID         string    `json:"scopeId"`
	DayUTC          time.Time `json:"dayUtc"`
	TokensSummable  int64     `json:"tokensSummable"`
	Calls           int64     `json:"calls"`
	UnmeasuredCalls int64     `json:"unmeasuredCalls"`
}

// MeterHealth tracks per-run producer liveness, sequence continuity, and
// drift/anomaly counters in meter_health.
type MeterHealth struct {
	ID                   string     `json:"id,omitempty"`
	AgentID              string     `json:"agentId"`
	RunID                string     `json:"runId"`
	ProducerEpoch        string     `json:"producerEpoch"`
	LastReceiptAt        time.Time  `json:"lastReceiptAt"`
	LastContiguousSeq    int64      `json:"lastContiguousSeq"`
	MaxSeqSeen           int64      `json:"maxSeqSeen"`
	MissingRanges        []SeqRange `json:"missingRanges,omitempty"`
	LastSourceActivityAt *time.Time `json:"lastSourceActivityAt,omitempty"`
	SupportedSignals     []string   `json:"supportedSignals,omitempty"`
	AnomalousCount       int64      `json:"anomalousCount"`
	SchemaDriftDetected  bool       `json:"schemaDriftDetected"`
	FinalState           string     `json:"finalState"`
	UpdatedAt            time.Time  `json:"updatedAt"`
}

// RunAttributionConfig configures atomic run_attribution writes on the store
// (installed by the Hub when tokenomics.usage_ledger is enabled, cleared when
// disabled).
type RunAttributionConfig struct {
	HubID string
}

// UsageMeterHealthUpdate describes a meter_health update applied atomically
// inside IngestUsageBatch (for event batches and/or empty heartbeats).
// The Hub is the sole authority on contiguity and sequence gaps; callers pass
// ProducerSeq (highest emitted seq) and ReceivedSeqs (seqs actually received).
type UsageMeterHealthUpdate struct {
	AgentID              string
	RunID                string
	ProducerEpoch        string
	LastReceiptAt        time.Time
	ProducerSeq          int64
	ReceivedSeqs         []int64
	LastSourceActivityAt *time.Time
	SupportedSignals     []string
	SchemaDriftDetected  bool
	FinalState           string
}

// UsageIngestBatchInput holds validated, attribution-stamped usage events and
// an optional meter_health update to persist atomically in one transaction.
type UsageIngestBatchInput struct {
	// AgentID is the authenticated agent's ID (used for the Postgres advisory
	// lock hashtext('usage_rollup:' || agent_id)).
	AgentID string
	// Events are validated, attribution-stamped usage events ready for
	// idempotent insert into usage_events.
	Events []*UsageEvent
	// MeterHealth, when non-nil, is applied in the same transaction.
	MeterHealth *UsageMeterHealthUpdate
}

// UsageIngestRowRejection records a single event rejected at persistence time
// due to a per-row data or constraint error (rather than failing the batch).
type UsageIngestRowRejection struct {
	EventID string
	Reason  string
}

// UsageIngestBatchResult reports the counts of newly inserted vs duplicate
// events persisted by IngestUsageBatch, plus any per-row DB rejections.
type UsageIngestBatchResult struct {
	Accepted   int
	Duplicates int
	Rejected   []UsageIngestRowRejection
}

// UsageTokenPresence reports per-(model, harness, usage_source) non-null event
// counts from usage_events over a query window so read APIs can distinguish
// measured 0 from all-NULL (unknown) buckets in usage_rollups_daily (§3.1).
type UsageTokenPresence struct {
	Model             string
	Harness           string
	UsageSource       string
	EventCount        int64
	InputCount        int64
	OutputCount       int64
	CacheReadCount    int64
	CacheWriteCount   int64
	CacheWrite1hCount int64
	ReasoningCount    int64
}

// UsageLedgerStore defines persistence operations for the Tokenomics Track 1
// operational usage ledger tables introduced in P1.1.
type UsageLedgerStore interface {
	// EnsureRunAttribution inserts attr if no row with attr.RunID exists
	// (INSERT ... ON CONFLICT (run_id) DO NOTHING). Returns created=true if a
	// new row was inserted, false if a row already existed (left untouched).
	EnsureRunAttribution(ctx context.Context, attr *RunAttribution) (created bool, err error)

	// GetRunAttribution returns the run_attribution row for runID, or
	// ErrNotFound if none exists.
	GetRunAttribution(ctx context.Context, runID string) (*RunAttribution, error)

	// ListRunAttributionsByAgent returns run_attribution rows for agentID
	// ordered by created_at descending (newest first).
	ListRunAttributionsByAgent(ctx context.Context, agentID string, limit int) ([]*RunAttribution, error)

	// FillRunAttributionDigests fills nil digest/build columns on an existing
	// run_attribution row (fill-once: WHERE col IS NULL). Returns
	// ErrRunAttributionDigestConflict if any requested column already holds a
	// different non-empty value, or ErrNotFound if runID does not exist.
	FillRunAttributionDigests(ctx context.Context, runID string, upd RunAttributionDigestUpdate) error

	// ResolveBillingUserID resolves the immutable billing_user_id for an agent
	// per Tokenomics design §3.2:
	//  1. agent.OwnerID if it resolves to a User row;
	//  2. first element of agent.Ancestry (from index 0) that resolves to a User row;
	//  3. agent.CreatedBy if it resolves to a User row;
	//  4. nil (NULL) for principal-less agents.
	ResolveBillingUserID(ctx context.Context, ownerID, createdBy string, ancestry []string) (*string, error)

	// BackfillActiveRunAttributions runs the idempotent active-run backfill of
	// run_attribution rows for non-deleted agents that have a non-empty run_id
	// and no run_attribution row yet. Invoked at Hub startup when
	// tokenomics.usage_ledger is enabled and on every OFF -> ON transition.
	// Returns the number of newly inserted rows.
	BackfillActiveRunAttributions(ctx context.Context, hubID string) (int, error)

	// GetMeterHealth returns all meter_health rows for (agentID, runID).
	GetMeterHealth(ctx context.Context, agentID, runID string) ([]*MeterHealth, error)

	// ListMeterHealthByAgent returns all meter_health rows for agentID ordered
	// by last_receipt_at descending.
	ListMeterHealthByAgent(ctx context.Context, agentID string) ([]*MeterHealth, error)

	// EnsureRunAttributionForAgent ensures a run_attribution row exists for
	// the agent's current run_id (lazy attribution fallback for the narrow
	// OFF->ON race), built from the Hub's agents row, never from event payload.
	EnsureRunAttributionForAgent(ctx context.Context, agentID, runID, hubID string) (*RunAttribution, error)

	// IngestUsageBatch atomically inserts events into usage_events
	// (ON CONFLICT (hub_id, event_id) DO NOTHING RETURNING), increments
	// usage_rollups_daily and usage_scope_counters_daily ONLY for newly
	// inserted rows (in canonical key-sorted order to prevent deadlocks),
	// and updates meter_health (events and/or heartbeats).
	IngestUsageBatch(ctx context.Context, in *UsageIngestBatchInput) (*UsageIngestBatchResult, error)

	// ListUsageRollupsDailyByAgent returns usage_rollups_daily rows for agentID
	// with bucket_start in [sinceDay, untilDay] (inclusive, UTC day boundaries).
	ListUsageRollupsDailyByAgent(ctx context.Context, agentID string, sinceDay, untilDay time.Time) ([]*UsageRollupDaily, error)

	// GetUsageTokenPresenceByAgent returns per-(model, harness, usage_source)
	// non-null counts for each canonical token column in usage_events for
	// agentID with event_ts in [since, until] (inclusive).
	GetUsageTokenPresenceByAgent(ctx context.Context, agentID string, since, until time.Time) ([]*UsageTokenPresence, error)

	// HasUsageEventsForRun reports whether at least one usage_events row exists
	// for (agentID, runID).
	HasUsageEventsForRun(ctx context.Context, agentID, runID string) (bool, error)

	// DBNow returns the current UTC timestamp from the database clock
	// (Tokenomics design §4.2 item 7).
	DBNow(ctx context.Context) (time.Time, error)
}
