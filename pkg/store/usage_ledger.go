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
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math"
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
	UsageScopeKindGlobal      = "global"
)

// UsageRollupHourlyCoverageSection is the system hub_settings section that
// records the UTC hour-aligned timestamp when usage_rollups_hourly coverage
// began (set idempotently on first hourly rollup write).
// LegacyUsageRollupHourlyCoverageSection is the earlier internal.* key accepted
// on read for any rows already written before the rename.
const (
	UsageRollupHourlyCoverageSection       = "system.usage_rollups_hourly_coverage"
	LegacyUsageRollupHourlyCoverageSection = "internal.usage_rollups_hourly_coverage"
)

// UsageRetentionStateSection is the internal hub_settings section that
// persists shared usage-retention sweeper progress (last completed pass,
// resume index for multi-tick passes) and the monotonic hourly swept-through
// watermark across replicas and restarts.
const UsageRetentionStateSection = "system.usage_retention_state"

// Usage rollup granularities and supported groupBy dimensions.
const (
	UsageGranularityHour = "hour"
	UsageGranularityDay  = "day"

	UsageGroupByModel   = "model"
	UsageGroupByHarness = "harness"
	UsageGroupByAgent   = "agent"
	UsageGroupByUser    = "user"
	UsageGroupByProject = "project"
	UsageGroupByDay     = "day"
	UsageGroupByHour    = "hour"
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
	ID                      string    `json:"id,omitempty"`
	BucketStart             time.Time `json:"bucketStart"`
	ProjectID               string    `json:"projectId"`
	BillingUserID           string    `json:"billingUserId"`
	AgentID                 string    `json:"agentId"`
	Harness                 string    `json:"harness"`
	Model                   string    `json:"model"`
	UsageSource             string    `json:"usageSource"`
	Calls                   int64     `json:"calls"`
	ErrorCalls              int64     `json:"errorCalls"`
	Input                   int64     `json:"input"`
	Output                  int64     `json:"output"`
	CacheRead               int64     `json:"cacheRead"`
	CacheWrite              int64     `json:"cacheWrite"`
	CacheWrite1h            int64     `json:"cacheWrite1h"`
	Reasoning               int64     `json:"reasoning"`
	ReportedCostMicros      int64     `json:"reportedCostMicros"`
	Events                  int64     `json:"events"`
	NullTokenEvents         int64     `json:"nullTokenEvents"`
	InputKnownEvents        int64     `json:"inputKnownEvents"`
	OutputKnownEvents       int64     `json:"outputKnownEvents"`
	CacheReadKnownEvents    int64     `json:"cacheReadKnownEvents"`
	CacheWriteKnownEvents   int64     `json:"cacheWriteKnownEvents"`
	CacheWrite1hKnownEvents int64     `json:"cacheWrite1hKnownEvents"`
	ReasoningKnownEvents    int64     `json:"reasoningKnownEvents"`
	PresenceTrackedEvents   int64     `json:"presenceTrackedEvents"`
	FirstEventTS            time.Time `json:"firstEventTs"`
	LastEventTS             time.Time `json:"lastEventTs"`
}

// UsageRollupHourly is one row in usage_rollups_hourly (UTC hour-aligned
// bucket_start). Shares the exact same dimensions and counters as daily.
type UsageRollupHourly = UsageRollupDaily

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
	ID                string     `json:"id,omitempty"`
	AgentID           string     `json:"agentId"`
	RunID             string     `json:"runId"`
	ProducerEpoch     string     `json:"producerEpoch"`
	LastReceiptAt     time.Time  `json:"lastReceiptAt"`
	LastContiguousSeq int64      `json:"lastContiguousSeq"`
	MaxSeqSeen        int64      `json:"maxSeqSeen"`
	MissingRanges     []SeqRange `json:"missingRanges,omitempty"`
	// RejectedCount is the idempotent count of distinct Hub-rejected producer
	// seqs. It is exact while RejectedRanges has not exceeded its cap and
	// coalesced; once coalesced (RejectedRangesCoalesced() == true, i.e.,
	// sum(r.End-r.Start+1) > RejectedCount), RejectedCount is a lower bound.
	RejectedCount        int64      `json:"rejectedCount"`
	RejectedRanges       []SeqRange `json:"rejectedRanges,omitempty"`
	LastSourceActivityAt *time.Time `json:"lastSourceActivityAt,omitempty"`
	SupportedSignals     []string   `json:"supportedSignals,omitempty"`
	AnomalousCount       int64      `json:"anomalousCount"`
	SchemaDriftDetected  bool       `json:"schemaDriftDetected"`
	FinalState           string     `json:"finalState"`
	UpdatedAt            time.Time  `json:"updatedAt"`
}

// RejectedRangesCoalesced reports whether RejectedRanges has been coalesced at
// its storage cap (i.e. the total span covered by RejectedRanges exceeds
// RejectedCount). When false, RejectedCount is exact; when true, RejectedCount
// is a lower bound while remaining idempotent across retries.
func (m *MeterHealth) RejectedRangesCoalesced() bool {
	if m == nil || len(m.RejectedRanges) == 0 {
		return false
	}
	var span int64
	for _, r := range m.RejectedRanges {
		if r.End < r.Start {
			continue
		}
		d := r.End - r.Start
		if span > math.MaxInt64-d {
			return true
		}
		span += d
		if span == math.MaxInt64 {
			return span > m.RejectedCount
		}
		span++
		if span > m.RejectedCount {
			return true
		}
	}
	return span > m.RejectedCount
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
	RejectedSeqs         []int64
	LastSourceActivityAt *time.Time
	SupportedSignals     []string
	SchemaDriftDetected  bool
	FinalState           string
}

// LegacySessionMetricsEventID returns the deterministic usage_events.event_id
// for an agent_session_metrics segment keyed by (agent_id, session_id, started_at).
// Truncating startedAt to UTC microseconds ensures identical IDs across SQLite
// and Postgres and between live conversion and historical catch-up.
func LegacySessionMetricsEventID(agentID, sessionID string, startedAt time.Time) string {
	ts := startedAt.UTC().Truncate(time.Microsecond).Format(time.RFC3339Nano)
	sum := sha256.Sum256([]byte(agentID + "\x00" + sessionID + "\x00" + ts))
	return "legacy-sm:" + hex.EncodeToString(sum[:])
}

// SessionMetricsConversionMark identifies one agent_session_metrics row to
// stamp with converted_at inside IngestUsageBatch's transaction. When EventID
// is non-empty, the row is stamped only if that EventID was not rejected at
// persistence time (preventing backfill livelock on partial-batch rejections).
type SessionMetricsConversionMark struct {
	ID      string
	EventID string
}

// UsageIngestBatchInput holds validated, attribution-stamped usage events and
// an optional meter_health update to persist atomically in one transaction.
type UsageIngestBatchInput struct {
	// AgentID is the authenticated agent's ID (used for the Postgres advisory
	// lock hashtext('usage_rollup:' || agent_id)). When empty and Events span
	// multiple agents (e.g. legacy catch-up), locks are acquired per distinct
	// event AgentID in canonical sorted order.
	AgentID string
	// Events are validated, attribution-stamped usage events ready for
	// idempotent insert into usage_events.
	Events []*UsageEvent
	// MeterHealth, when non-nil, is applied in the same transaction.
	MeterHealth *UsageMeterHealthUpdate
	// IsBackfill indicates a historical/catch-up batch:
	//  - never creates or moves the hourly coverage start marker;
	//  - writes usage_rollups_hourly ONLY for events whose EventTS hour is at
	//    or after the existing hourly coverage start (if one exists);
	//  - always writes usage_rollups_daily and usage_scope_counters_daily.
	IsBackfill bool
	// MarkSessionMetricsConverted lists agent_session_metrics rows to stamp
	// with converted_at in the same transaction as the ledger/rollup writes.
	MarkSessionMetricsConverted []SessionMetricsConversionMark
}

// RunUsageSourceSummary reports which classes of usage_events exist for a run.
type RunUsageSourceSummary struct {
	HasTrack1Events bool // at least one row with usage_source != legacy_session_metrics
	HasLegacyEvents bool // at least one row with usage_source == legacy_session_metrics
}

// LegacySessionMetricsBackfillResult summarizes one catch-up backfill run over
// unconverted agent_session_metrics rows.
type LegacySessionMetricsBackfillResult struct {
	Processed int
	Converted int
	Skipped   int
}

// UsageIngestRowRejection records a single event rejected at persistence time
// due to a per-row data or constraint error (rather than failing the batch).
type UsageIngestRowRejection struct {
	EventID string
	Reason  string
}

// UsageIngestBatchResult reports the counts of newly inserted vs duplicate
// events persisted by IngestUsageBatch, plus any per-row DB rejections and the
// slice of newly inserted events (from RETURNING) for post-commit consumers
// such as Hub OTel re-emission.
//
// InsertedEvents is guaranteed committed only when the store is not inTx;
// callers executing inside an outer transaction must defer post-commit effects
// until that outer transaction commits.
type UsageIngestBatchResult struct {
	Accepted       int
	Duplicates     int
	Rejected       []UsageIngestRowRejection
	InsertedEvents []*UsageEvent
}

// UsageRollupTotals holds aggregated counter sums and presence counters over a
// query window or group. Saturated is true when any summed counter reached or
// exceeded math.MaxInt64 and was clamped.
type UsageRollupTotals struct {
	Calls                   int64
	ErrorCalls              int64
	Input                   int64
	Output                  int64
	CacheRead               int64
	CacheWrite              int64
	CacheWrite1h            int64
	Reasoning               int64
	ReportedCostMicros      int64
	Events                  int64
	NullTokenEvents         int64
	InputKnownEvents        int64
	OutputKnownEvents       int64
	CacheReadKnownEvents    int64
	CacheWriteKnownEvents   int64
	CacheWrite1hKnownEvents int64
	ReasoningKnownEvents    int64
	PresenceTrackedEvents   int64
	Saturated               bool
}

// UsageRollupGroup is one aggregated group row returned by QueryUsageRollups.
type UsageRollupGroup struct {
	BucketStart   *time.Time
	ProjectID     string
	BillingUserID string
	AgentID       string
	Harness       string
	Model         string
	UsageRollupTotals
}

// UsageRollupQuery describes a bounded SQL-aggregated rollup query over
// usage_rollups_hourly or usage_rollups_daily.
type UsageRollupQuery struct {
	Granularity   string // UsageGranularityHour or UsageGranularityDay
	Since         time.Time
	Until         time.Time
	ProjectID     string
	BillingUserID string
	AgentID       string
	Harness       string
	Model         string
	UsageSource   string
	GroupBy       []string
	Limit         int
}

// UsageRollupQueryResult holds overall totals and optional bounded grouped rows
// returned by QueryUsageRollups.
type UsageRollupQueryResult struct {
	Totals              UsageRollupTotals
	Groups              []*UsageRollupGroup
	Truncated           bool
	HasLegacyRollups    bool
	HasNonLegacyRollups bool
}

// UsageCoverageFilter scopes a single-query aggregate meter-health and
// measurement-coverage summary over non-deleted agents overlapping [Since, Until].
type UsageCoverageFilter struct {
	ProjectID     string
	BillingUserID string
	AgentID       string
	Since         time.Time
	Until         time.Time
	StaleAfter    time.Duration
	StartupGrace  time.Duration
	DBNow         time.Time
}

// UsageCoverageBucket aggregates agent counts sharing the same
// measurement-status derivation inputs (including current harness-config
// identity) so the Hub derives status through a single Go function
// (deriveMeasurementStatus) without per-agent queries.
type UsageCoverageBucket struct {
	HarnessConfig       string
	Harness             string
	CapabilityProjectID string
	IsRunning           bool
	HasMeterHealth      bool
	LatestFinalState    string
	IsReceiptStale      bool
	IsPastGrace         bool
	HasSchemaDrift      bool
	HasUsageData        bool
	HasLegacyOnly       bool
	AgentCount          int
}

// UsageScopeCoverage summarizes measurement coverage inputs across agents in scope.
type UsageScopeCoverage struct {
	Buckets       []UsageCoverageBucket
	LastReceiptAt *time.Time
}

// UsageAgentCountQuery scopes distinct active-agent counts over
// usage_rollups_hourly or usage_rollups_daily.
type UsageAgentCountQuery struct {
	Granularity    string // UsageGranularityHour or UsageGranularityDay
	Since          time.Time
	Until          time.Time
	ProjectID      string
	IncludeBuckets bool
	// Limit bounds the number of per-bucket rows returned when IncludeBuckets
	// is true (0 uses the store default). TotalUniqueAgents is always exact.
	Limit int
}

// UsageBucketAgentPair identifies a distinct agent active in a UTC rollup bucket.
type UsageBucketAgentPair struct {
	BucketStart time.Time
	AgentID     string
}

// UsageBucketCount holds a distinct count for one UTC rollup bucket.
type UsageBucketCount struct {
	BucketStart time.Time
	Count       int64
}

// UsageAgentCountResult holds exact distinct active-agent counts.
// TotalUniqueAgents is COUNT(DISTINCT agent_id) over the full window and is
// never derived by summing per-bucket counts.
type UsageAgentCountResult struct {
	TotalUniqueAgents int
	// BucketCounts is populated when IncludeBuckets is true and Granularity is "day".
	BucketCounts []UsageBucketCount
	// BucketAgents is populated when IncludeBuckets is true and Granularity is "hour"
	// so callers can deduplicate agents across UTC hours within the same local day.
	BucketAgents []UsageBucketAgentPair
	// Truncated is true when per-bucket rows exceeded Limit (newest buckets retained).
	Truncated bool
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

	// GetUsageHourlyCoverageStart returns the UTC hour-aligned timestamp when
	// usage_rollups_hourly coverage began, and ok=false if hourly rollups have
	// not yet received any writes.
	GetUsageHourlyCoverageStart(ctx context.Context) (startedAt time.Time, ok bool, err error)

	// CheckUsageHourlyConsistency compares SUM(events) between
	// usage_rollups_hourly and usage_rollups_daily over the inclusive whole UTC
	// days [utcDayStart(q.Since), utcDayStart(q.Until)] with the same scope and
	// dimension filters. Returns true when both sums match (or when the day
	// range is empty), and false when hourly rollups are incomplete relative to
	// daily rollups (for example after a rolling upgrade or rollback).
	CheckUsageHourlyConsistency(ctx context.Context, q UsageRollupQuery) (bool, error)

	// QueryUsageRollups aggregates usage_rollups_hourly or usage_rollups_daily
	// in SQL for the requested window, filters, and optional groupBy dimensions,
	// bounding returned groups to q.Limit.
	QueryUsageRollups(ctx context.Context, q UsageRollupQuery) (*UsageRollupQueryResult, error)

	// GetUsageScopeCoverage computes aggregate agent measurement coverage
	// (measured/unmeasured/stale counts and latest last_receipt_at) in a single
	// bounded SQL query for a project, billing user, or global hub scope.
	GetUsageScopeCoverage(ctx context.Context, filter UsageCoverageFilter) (*UsageScopeCoverage, error)

	// CountUsageActiveAgents computes exact distinct active-agent counts over
	// usage_rollups_hourly or usage_rollups_daily for a window, optionally
	// returning per-bucket counts (daily) or (bucket_start, agent_id) pairs
	// (hourly) for timezone-aware local-day deduplication.
	CountUsageActiveAgents(ctx context.Context, q UsageAgentCountQuery) (*UsageAgentCountResult, error)

	// HasUsageEventsForRun reports whether at least one usage_events row exists
	// for (agentID, runID).
	HasUsageEventsForRun(ctx context.Context, agentID, runID string) (bool, error)

	// GetRunUsageSourceSummary reports whether (agentID, runID) has non-legacy
	// Track 1 usage events and/or legacy_session_metrics usage events.
	GetRunUsageSourceSummary(ctx context.Context, agentID, runID string) (RunUsageSourceSummary, error)

	// ConvertLegacySessionMetricsRow converts a single agent_session_metrics
	// row into a legacy_session_metrics ledger row (through IngestUsageBatch)
	// and atomically stamps converted_at on the source row in the same
	// transaction. If Track 1 events already exist for the run/session, it
	// skips inserting a ledger row while still stamping converted_at.
	ConvertLegacySessionMetricsRow(ctx context.Context, hubID string, row *AgentSessionMetrics, preferredRunID string) (converted bool, err error)

	// BackfillLegacySessionMetrics processes unconverted agent_session_metrics
	// rows (WHERE converted_at IS NULL) in bounded batches, converting each
	// non-overlapping row through IngestUsageBatch and stamping converted_at
	// atomically in the same transaction. Safe to re-run after crashes,
	// restarts, experiment toggles, and retention deletion of old ledger rows.
	BackfillLegacySessionMetrics(ctx context.Context, hubID string) (LegacySessionMetricsBackfillResult, error)

	// DBNow returns the current UTC timestamp from the database clock
	// (Tokenomics design §4.2 item 7).
	DBNow(ctx context.Context) (time.Time, error)

	// CountStaleMetersByProject returns the number of stale open runs per
	// project_id whose latest open meter_health receipt is strictly older than
	// staleBefore (and not superseded by a newer non-open epoch).
	CountStaleMetersByProject(ctx context.Context, staleBefore time.Time) (map[string]int64, error)

	UsageRetentionStore
}

// UsageExportCursor is the analytics export watermark for usage_events (P8).
// When provided to SweepUsageEventsBatch, only rows at or before this cursor
// are eligible for retention deletion; rows newer than the cursor are kept
// until exported.
type UsageExportCursor struct {
	ReceivedAt time.Time
	// EventID is an optional tie-breaker matching the (received_at, event_id)
	// index. When empty, all rows with received_at <= ReceivedAt are eligible.
	EventID string
}

// UsageRetentionState holds durable state for the Hub DB usage-ledger
// retention sweeper (stored in hub_settings under UsageRetentionStateSection).
type UsageRetentionState struct {
	// LastCompletedAt is the UTC timestamp when the last full retention pass
	// drained every registered target.
	LastCompletedAt *time.Time `json:"last_completed_at,omitempty"`
	// NextTargetIndex is the 0-based target index to resume from on the next
	// 1-minute tick while InProgress is true.
	NextTargetIndex int `json:"next_target_index,omitempty"`
	// InProgress is true when a pass did not finish draining all targets within
	// a tick budget and should continue on subsequent 1-minute ticks without
	// waiting sweep_interval.
	InProgress bool `json:"in_progress,omitempty"`
	// HourlySweptThrough is the highest UTC hour-aligned cutoff that the
	// usage_rollups_hourly sweeper has swept through. Monotonic: never moves
	// backward, preventing silent under-reporting if hourly_rollups_days is
	// later increased.
	HourlySweptThrough *time.Time `json:"hourly_swept_through,omitempty"`
}

// UsageRetentionStore defines bounded-batch retention sweep operations for the
// Hub DB Track 1 usage tables (Tokenomics design §4.2, D8/D8a). Each method
// deletes at most batchSize rows in one transaction and returns the deleted
// row count.
type UsageRetentionStore interface {
	// GetUsageRetentionState loads the persisted usage retention sweeper state
	// and hourly swept-through watermark. Returns a zero-value state (not an
	// error) when no row exists yet.
	GetUsageRetentionState(ctx context.Context) (*UsageRetentionState, error)

	// SaveUsageRetentionState persists the usage retention sweeper state and
	// hourly swept-through watermark in hub_settings.
	SaveUsageRetentionState(ctx context.Context, state *UsageRetentionState) error

	// SweepUsageEventsBatch deletes up to batchSize usage_events rows with
	// event_ts < cutoff (and at or before exportCursor when non-nil).
	SweepUsageEventsBatch(ctx context.Context, cutoff time.Time, exportCursor *UsageExportCursor, batchSize int) (int64, error)

	// SweepUsageRollupsHourlyBatch deletes up to batchSize usage_rollups_hourly
	// rows with bucket_start < cutoff.
	SweepUsageRollupsHourlyBatch(ctx context.Context, cutoff time.Time, batchSize int) (int64, error)

	// SweepUsageRollupsDailyBatch deletes up to batchSize usage_rollups_daily
	// rows with bucket_start < cutoff.
	SweepUsageRollupsDailyBatch(ctx context.Context, cutoff time.Time, batchSize int) (int64, error)

	// SweepUsageScopeCountersDailyBatch deletes up to batchSize
	// usage_scope_counters_daily rows with day_utc < cutoff.
	SweepUsageScopeCountersDailyBatch(ctx context.Context, cutoff time.Time, batchSize int) (int64, error)

	// SweepMeterHealthBatch deletes up to batchSize meter_health rows with
	// updated_at < cutoff that do not belong to the current run of a live
	// (non-deleted) agent (D8 rule).
	SweepMeterHealthBatch(ctx context.Context, cutoff time.Time, batchSize int) (int64, error)

	// SweepRunAttributionBatch deletes up to batchSize run_attribution rows
	// with created_at < cutoff, legal_hold = false, not the current run of a
	// live (non-deleted) agent, and with no remaining usage_events or
	// meter_health rows referencing the run (D8a rule).
	SweepRunAttributionBatch(ctx context.Context, cutoff time.Time, batchSize int) (int64, error)
}
