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

// This file holds the telemetry.health analytics event (WP3 design note
// telemetry/design/wp3-design.md, section 3). One event is a snapshot of an
// agent's telemetry configuration and delivery diagnostics: which usage
// source is selected, what it can report, and the fixed-cardinality
// counters that tell a quiet agent apart from one whose usage collection
// was never enabled or whose delivery is failing. Field types are fixed
// forever: a type change on an existing field becomes a BigQuery export
// error.

import (
	"errors"
	"fmt"
)

// Health triggers: why a snapshot was taken.
const (
	// HealthTriggerStart is the first snapshot of a run, taken once the
	// usage-source decision has settled.
	HealthTriggerStart = "start"
	// HealthTriggerPeriodic is a timed snapshot.
	HealthTriggerPeriodic = "periodic"
	// HealthTriggerFinal is taken on graceful stop after the final flush.
	HealthTriggerFinal = "final"
)

// Usage collection states.
const (
	UsageCollectionEnabled      = "enabled"
	UsageCollectionDisabled     = "disabled"
	UsageCollectionUndetermined = "undetermined"
)

// Reasons usage collection is disabled.
const (
	UsageDisabledSourceUnset        = "source_unset"
	UsageDisabledSourceUnrecognized = "source_unrecognized"
	UsageDisabledNativePolicy       = "native_policy_disabled"
	UsageDisabledNoHarnessRule      = "no_harness_rule"
	UsageDisabledDeriverFailed      = "deriver_failed"
)

// Where the effective usage source came from.
const (
	UsageSourceOriginRuntimeEnv = "runtime_env"
	UsageSourceOriginOverlay    = "overlay"
	UsageSourceOriginUnset      = "unset"
)

// Usage capabilities: the categories an adapter can report.
const (
	UsageCapabilityCalls        = "calls"
	UsageCapabilityInput        = "input"
	UsageCapabilityOutput       = "output"
	UsageCapabilityCacheRead    = "cache_read"
	UsageCapabilityCacheWrite   = "cache_write"
	UsageCapabilityReasoning    = "reasoning"
	UsageCapabilityReportedCost = "reported_cost"
)

// Usage granularity: what one usage event stands for.
const (
	UsageGranularityRequest    = "request"
	UsageGranularityInterval   = "interval"
	UsageGranularityInvocation = "invocation"
)

// Analytics (usage outbox) states and disabled reasons.
const (
	AnalyticsStateEnabled  = "enabled"
	AnalyticsStateDisabled = "disabled"

	AnalyticsDisabledOutboxUnavailable = "outbox_unavailable"
	AnalyticsDisabledNoGCPExporter     = "no_gcp_exporter"
)

// Final flush outcomes (trigger final only): whether every queue drained.
// Not a statement that the run was loss-free (see LossIndicated).
const (
	FinalFlushDrained    = "drained"
	FinalFlushNotDrained = "not_drained"
)

// Capability provenance.
const (
	UsageCapabilityStatusStatic  = "static_table"
	UsageCapabilityStatusUnknown = "unknown"
)

// HealthUsageCounters are the native usage deriver's counters, a producer-
// side helper: SetUsageCounters copies them into HealthEvent's flat,
// omittable fields. Populations: WP3 design section 1.1.
type HealthUsageCounters struct {
	LogRecordsReceived, LogRecordsRecognized, LogRecordsMalformed   int64
	MetricBatchesReceived, MetricBatchesMalformed, MetricIncrements int64
	ObservationsIgnored, ObservationsDuplicate, ObservationsDerived int64
	ClampedInput, StaleBackwards, BaselinedAfterCap                 int64
}

// HealthAnalyticsCounters are the usage outbox's counters, a producer-side
// helper for SetAnalyticsCounters. Populations: WP3 design section 1.3.
type HealthAnalyticsCounters struct {
	Appended, CertainDuplicates, Overflow, OverflowTotal int64
	AppendFailed, DroppedNoIdentity                      int64
	Finalized, Delivered, SendFailures, SendRetries      int64
	Torn, HubIDMismatch                                  int64
	Conflicts, Quarantined, DeadLetters                  int64
	OversizeTotal, AppendFailedTotal, DeadLettersTotal   int64
	SyncUncertain, CorruptIntents, Invalid               int64
	Unresolved, CommitDeferred                           int64
	InvalidLines                                         int64
	SyncUncertainTotal, CommitDeferredTotal              int64
	// Held is true when an unresolved append holds the outbox (on disk).
	Held          bool
	RetainedBytes int64
	PendingBytes  int64
	// OldestPendingFileAgeMs is nil when nothing is pending (omitted).
	OldestPendingFileAgeMs *int64
	RetryBackoffMs         int64
	ProducerEpoch          string
	ProducerSeqLast        int64
	// Outbox is the outbox-subject group (WP3 hub-origin plan v5, section
	// 1a), nil when it is not reported: this process had not resolved its
	// hub before the ledger read, the outbox has no pin yet, there is no
	// drainer (the cached no-outbox mode), or the read failed.
	// SetAnalyticsCounters ignores it when StatsFailed.
	Outbox *HealthOutboxSubject
	// StatsFailed means some on-disk read failed (pending, retained,
	// epoch, seq, the held intent or a persisted total):
	// SetAnalyticsCounters then omits PendingBytes, RetainedBytes, the
	// *Total counters, ProducerEpoch, ProducerSeqLast, Held and the pending
	// age (unknown, never placeholders).
	StatsFailed bool
}

// HealthOutboxSubject describes the usage OUTBOX (its origin), not this
// process: the pin as read, the persisted sticky relocation marker and the
// persisted guard counters, all scoped to the outbox directory (= its
// producer_epoch). Relocated is nil when unknown (neither the marker nor a
// verified same-origin comparison).
type HealthOutboxSubject struct {
	PinnedHubID           string
	Relocated             *bool
	HubMismatchTotal      int64
	RelocatedRefusedTotal int64
}

// HealthEvent is one telemetry.health analytics event.
type HealthEvent struct {
	Envelope

	// PipelineRunID is a random ID per sciontool pipeline run (not the
	// launch RunID, which is launch_id).
	PipelineRunID string `json:"pipeline_run_id"`
	HealthSeq     int64  `json:"health_seq"`
	Trigger       string `json:"trigger"`

	Harness             string `json:"harness"`
	UsageSource         string `json:"usage_source,omitempty"`
	UsageSourceOrigin   string `json:"usage_source_origin"`
	UsageCollection     string `json:"usage_collection"`
	UsageDisabledReason string `json:"usage_disabled_reason,omitempty"`
	// UsageCapabilityStatus is "static_table" when UsageCapabilities comes
	// from adapter evidence at this build, "unknown" when there is none.
	UsageCapabilityStatus string `json:"usage_capability_status,omitempty"`
	// UsageCapabilities is always written and never nil: the list with
	// static_table, [] otherwise (read it with usage_capability_status).
	UsageCapabilities []string `json:"usage_capabilities"`
	UsageGranularity  string   `json:"usage_granularity,omitempty"`
	// UsageTokensPositiveOnly is true when the adapter drops zero token
	// values, so a missing category may be a zero. Always written.
	UsageTokensPositiveOnly bool   `json:"usage_tokens_positive_only"`
	UsageRulesVersion       string `json:"usage_rules_version"`

	AnalyticsState          string `json:"analytics_state"`
	AnalyticsDisabledReason string `json:"analytics_disabled_reason,omitempty"`
	DeliveryState           string `json:"delivery_state"`

	// Native usage counters: present only when usage_source is native.
	// Omitted means not applicable, never 0.
	UsageLogRecordsReceived     *int64 `json:"usage_log_records_received,omitempty"`
	UsageLogRecordsRecognized   *int64 `json:"usage_log_records_recognized,omitempty"`
	UsageLogRecordsMalformed    *int64 `json:"usage_log_records_malformed,omitempty"`
	UsageMetricBatchesReceived  *int64 `json:"usage_metric_batches_received,omitempty"`
	UsageMetricBatchesMalformed *int64 `json:"usage_metric_batches_malformed,omitempty"`
	UsageMetricIncrements       *int64 `json:"usage_metric_increments,omitempty"`
	UsageObservationsIgnored    *int64 `json:"usage_observations_ignored,omitempty"`
	UsageObservationsDuplicate  *int64 `json:"usage_observations_duplicate,omitempty"`
	UsageObservationsDerived    *int64 `json:"usage_observations_derived,omitempty"`
	UsageClampedInput           *int64 `json:"usage_clamped_input,omitempty"`
	UsageStaleBackwards         *int64 `json:"usage_stale_backwards,omitempty"`
	UsageBaselinedAfterCap      *int64 `json:"usage_baselined_after_cap,omitempty"`

	// Usage outbox counters: present only when analytics is enabled.
	AnalyticsAppended          *int64 `json:"analytics_appended,omitempty"`
	AnalyticsCertainDuplicates *int64 `json:"analytics_certain_duplicates,omitempty"`
	AnalyticsOverflow          *int64 `json:"analytics_overflow,omitempty"`
	AnalyticsOverflowTotal     *int64 `json:"analytics_overflow_total,omitempty"`
	AnalyticsAppendFailed      *int64 `json:"analytics_append_failed,omitempty"`
	AnalyticsDroppedNoIdentity *int64 `json:"analytics_dropped_no_identity,omitempty"`
	AnalyticsFinalized         *int64 `json:"analytics_finalized,omitempty"`
	AnalyticsDelivered         *int64 `json:"analytics_delivered,omitempty"`
	AnalyticsSendFailures      *int64 `json:"analytics_send_failures,omitempty"`
	// AnalyticsSendRetries counts send attempts of a line whose previous
	// attempt failed (a failure can be the last attempt before stop).
	AnalyticsSendRetries *int64 `json:"analytics_send_retries,omitempty"`
	AnalyticsTorn        *int64 `json:"analytics_torn,omitempty"`
	// Conflicts: a verified request_id seen again with different content
	// (written as an ambiguous occurrence; quality, not loss). Quarantined:
	// ready lines in a truncated file set aside. DeadLetters: lines the
	// destination permanently rejected. *_total are persisted across runs
	// (all producers): oversize refusals, append failures, dead letters.
	AnalyticsConflicts         *int64 `json:"analytics_conflicts,omitempty"`
	AnalyticsQuarantined       *int64 `json:"analytics_quarantined,omitempty"`
	AnalyticsDeadLetters       *int64 `json:"analytics_dead_letters,omitempty"`
	AnalyticsOversizeTotal     *int64 `json:"analytics_oversize_total,omitempty"`
	AnalyticsAppendFailedTotal *int64 `json:"analytics_append_failed_total,omitempty"`
	AnalyticsDeadLettersTotal  *int64 `json:"analytics_dead_letters_total,omitempty"`
	// SyncUncertain: appends whose durability recovery decided (written;
	// quality, not loss). CorruptIntents: unreadable append intents set
	// aside, so whether their line landed is unknown (possible loss).
	// Invalid: events this process could not append (outbox unavailable
	// or append error; includes analytics_append_failed). RetainedBytes:
	// gauge of everything the outbox cap applies to (pending, dead-lettered
	// and quarantined bytes).
	AnalyticsSyncUncertain  *int64 `json:"analytics_sync_uncertain,omitempty"`
	AnalyticsCorruptIntents *int64 `json:"analytics_corrupt_intents,omitempty"`
	AnalyticsInvalid        *int64 `json:"analytics_invalid,omitempty"`
	AnalyticsRetainedBytes  *int64 `json:"analytics_retained_bytes,omitempty"`
	// Unresolved: appends whose own line could not be synced; the intent
	// decides later (not known lost). CommitDeferred: durable lines whose
	// index commit was completed later by recovery (quality). *_total are
	// persisted across runs. Held (on disk): an unresolved append holds the
	// outbox, so appends and finalization wait (nothing ships behind it).
	AnalyticsUnresolved     *int64 `json:"analytics_unresolved,omitempty"`
	AnalyticsCommitDeferred *int64 `json:"analytics_commit_deferred,omitempty"`
	// AnalyticsInvalidLines: complete outbox lines the current build
	// rejected (WP2 dead-letters them, so they are also in the dead letter
	// counts; quality detail, not counted twice in loss_indicated).
	AnalyticsInvalidLines        *int64 `json:"analytics_invalid_lines,omitempty"`
	AnalyticsSyncUncertainTotal  *int64 `json:"analytics_sync_uncertain_total,omitempty"`
	AnalyticsCommitDeferredTotal *int64 `json:"analytics_commit_deferred_total,omitempty"`
	AnalyticsHeld                *bool  `json:"analytics_held,omitempty"`
	AnalyticsHubIDMismatch       *int64 `json:"analytics_hub_id_mismatch,omitempty"`
	AnalyticsPendingBytes        *int64 `json:"analytics_pending_bytes,omitempty"`
	// AnalyticsOldestPendingFileAgeMs is the age of the oldest pending
	// outbox FILE's last modification: a lower bound on the oldest pending
	// event's age. Omitted when nothing is pending.
	AnalyticsOldestPendingFileAgeMs *int64 `json:"analytics_oldest_pending_file_age_ms,omitempty"`
	AnalyticsRetryBackoffMs         *int64 `json:"analytics_retry_backoff_ms,omitempty"`
	// ProducerEpoch (string, as on agent.usage) and ProducerSeqLast are the
	// outbox's persisted epoch and last assigned producer_seq (all
	// producers).
	ProducerEpoch   string `json:"producer_epoch,omitempty"`
	ProducerSeqLast *int64 `json:"producer_seq_last,omitempty"`
	// AnalyticsStatsFailed is true when any on-disk read of the outbox
	// failed (pending, retained, epoch, seq, the held intent or a persisted
	// total): the disk-derived fields are then omitted (unknown) and a final
	// event is not_drained. Present only with analytics.
	AnalyticsStatsFailed *bool `json:"analytics_stats_failed,omitempty"`
	// The outbox-subject group (names frozen by token-analyst-2; WP3
	// hub-origin plan v5, section 1a) describes the outbox's origin, never
	// this process's (that is hub_id). OutboxPinnedHubID is the outbox pin
	// as read, never rewritten and never used as hub_id. OutboxRelocated is
	// PERSISTED, STICKY evidence that a relocation was detected, not a
	// current comparison: after a return to origin, hub_id may equal the pin
	// with relocated=true. false means a verified same origin with no
	// marker. The two *_total counters are persisted, monotonic, scoped to
	// the outbox directory (= producer_epoch). All four are omitted
	// (unknown) on analytics_stats_failed, without an outbox, or when this
	// process had not resolved its hub before the ledger read.
	OutboxPinnedHubID           string `json:"outbox_pinned_hub_id,omitempty"`
	OutboxRelocated             *bool  `json:"outbox_relocated,omitempty"`
	OutboxHubMismatchTotal      *int64 `json:"outbox_hub_mismatch_total,omitempty"`
	OutboxRelocatedRefusedTotal *int64 `json:"outbox_relocated_refused_total,omitempty"`

	MetricPointsAccepted    int64 `json:"metric_points_accepted"`
	MetricPointsRejected    int64 `json:"metric_points_rejected"`
	MetricPointsFiltered    int64 `json:"metric_points_filtered"`
	MetricPointsDelivered   int64 `json:"metric_points_delivered"`
	MetricPointsDropped     int64 `json:"metric_points_dropped"`
	MetricPointsUnconfirmed int64 `json:"metric_points_unconfirmed"`
	MetricExportAttempts    int64 `json:"metric_export_attempts"`
	MetricExportFailures    int64 `json:"metric_export_failures"`
	MetricExportRetries     int64 `json:"metric_export_retries"`

	LogRecordsAccepted    int64 `json:"log_records_accepted"`
	LogRecordsRejected    int64 `json:"log_records_rejected"`
	LogRecordsFiltered    int64 `json:"log_records_filtered"`
	LogRecordsDelivered   int64 `json:"log_records_delivered"`
	LogRecordsDropped     int64 `json:"log_records_dropped"`
	LogRecordsUnconfirmed int64 `json:"log_records_unconfirmed"`
	LogExportAttempts     int64 `json:"log_export_attempts"`
	LogExportFailures     int64 `json:"log_export_failures"`
	LogExportRetries      int64 `json:"log_export_retries"`

	QueueBytes   int64 `json:"queue_bytes"`
	QueueRecords int64 `json:"queue_records"`

	HealthSendFailures int64 `json:"health_send_failures"`
	HealthSuperseded   int64 `json:"health_superseded"`
	// HealthRejected counts earlier health events of this run that were
	// dropped because they can never be delivered: permanently rejected by
	// the destination (also counted in health_send_failures), or failing
	// contract validation at freeze time (a producer bug).
	HealthRejected int64 `json:"health_rejected"`

	// LossIndicated is true when any INCLUDED loss counter in this
	// snapshot is non-zero (design note section 3.2). false never means
	// loss-free: hook-process append failures, for example, are counted in
	// those processes and are absent here. Always written.
	LossIndicated bool `json:"loss_indicated"`

	// FinalResidualRecords are metric records still unconfirmed when the
	// final metric flush gave up (trigger final only).
	FinalResidualRecords *int64 `json:"final_residual_records,omitempty"`
	// FinalFlushOutcome says whether the queues drained, not whether the
	// run was loss-free.
	FinalFlushOutcome string `json:"final_flush_outcome,omitempty"`
}

var validCapabilities = map[string]bool{
	UsageCapabilityCalls: true, UsageCapabilityInput: true, UsageCapabilityOutput: true,
	UsageCapabilityCacheRead: true, UsageCapabilityCacheWrite: true, UsageCapabilityReasoning: true,
	UsageCapabilityReportedCost: true,
}

// Validate checks the health invariants that make a row interpretable:
// event type, enums, the enabled/disabled presence rules and non-negative
// counters.
func (e *HealthEvent) Validate() error {
	if e == nil {
		return errors.New("telemetrycontract: nil health event")
	}
	if err := e.Envelope.Validate(); err != nil {
		return err
	}
	var errs []error
	if e.EventType != EventTypeHealth {
		errs = append(errs, fmt.Errorf("event_type = %q, want %q", e.EventType, EventTypeHealth))
	}
	if e.PipelineRunID == "" || e.HealthSeq < 1 {
		errs = append(errs, fmt.Errorf("pipeline_run_id %q / health_seq %d: both required, seq >= 1", e.PipelineRunID, e.HealthSeq))
	}
	switch e.Trigger {
	case HealthTriggerStart, HealthTriggerPeriodic:
		if e.FinalFlushOutcome != "" || e.FinalResidualRecords != nil {
			errs = append(errs, errors.New("final_flush_outcome and final_residual_records are only set on trigger final"))
		}
	case HealthTriggerFinal:
		if e.FinalFlushOutcome != FinalFlushDrained && e.FinalFlushOutcome != FinalFlushNotDrained {
			errs = append(errs, fmt.Errorf("final_flush_outcome %q is not drained or not_drained", e.FinalFlushOutcome))
		}
		if e.FinalFlushOutcome == FinalFlushDrained {
			stats := e.AnalyticsStatsFailed != nil && *e.AnalyticsStatsFailed
			pending := e.AnalyticsPendingBytes != nil && *e.AnalyticsPendingBytes > 0 ||
				e.AnalyticsHeld != nil && *e.AnalyticsHeld
			residual := e.FinalResidualRecords != nil && *e.FinalResidualRecords > 0
			if stats || pending || residual || e.QueueRecords > 0 {
				errs = append(errs, errors.New("final_flush_outcome drained contradicts stats_failed, pending bytes, a held outbox, residual records or queued records"))
			}
		}
		if e.FinalResidualRecords == nil {
			errs = append(errs, errors.New("trigger final requires final_residual_records"))
		} else if *e.FinalResidualRecords < 0 {
			errs = append(errs, errors.New("final_residual_records is negative"))
		}
	default:
		errs = append(errs, fmt.Errorf("trigger %q is not known", e.Trigger))
	}
	if e.Harness == "" || e.UsageRulesVersion == "" || e.DeliveryState == "" {
		errs = append(errs, errors.New("harness, usage_rules_version and delivery_state are required"))
	}
	if e.UsageSource != "" && e.UsageSource != UsageSourceNative && e.UsageSource != UsageSourceHooks {
		errs = append(errs, fmt.Errorf("usage_source %q is not native or hooks", e.UsageSource))
	}
	switch e.UsageSourceOrigin {
	case UsageSourceOriginRuntimeEnv, UsageSourceOriginOverlay, UsageSourceOriginUnset:
	default:
		errs = append(errs, fmt.Errorf("usage_source_origin %q is not known", e.UsageSourceOrigin))
	}
	enabled := e.UsageCollection == UsageCollectionEnabled
	switch e.UsageCollection {
	case UsageCollectionEnabled:
		if e.UsageSource == "" || e.UsageGranularity == "" {
			errs = append(errs, errors.New("enabled usage collection requires usage_source and usage_granularity"))
		}
		switch e.UsageCapabilityStatus {
		case UsageCapabilityStatusStatic:
			if len(e.UsageCapabilities) == 0 {
				errs = append(errs, errors.New("usage_capability_status static_table requires usage_capabilities"))
			}
		case UsageCapabilityStatusUnknown:
			if len(e.UsageCapabilities) > 0 {
				errs = append(errs, errors.New("usage_capability_status unknown must not list usage_capabilities"))
			}
		default:
			errs = append(errs, fmt.Errorf("usage_capability_status %q is not known", e.UsageCapabilityStatus))
		}
	case UsageCollectionDisabled:
		switch e.UsageDisabledReason {
		case UsageDisabledSourceUnset, UsageDisabledSourceUnrecognized, UsageDisabledNativePolicy, UsageDisabledNoHarnessRule, UsageDisabledDeriverFailed:
		default:
			errs = append(errs, fmt.Errorf("usage_disabled_reason %q is not known", e.UsageDisabledReason))
		}
	case UsageCollectionUndetermined:
	default:
		errs = append(errs, fmt.Errorf("usage_collection %q is not known", e.UsageCollection))
	}
	if e.UsageCollection != UsageCollectionDisabled && e.UsageDisabledReason != "" {
		errs = append(errs, errors.New("usage_disabled_reason is only set when usage collection is disabled"))
	}
	if !enabled && (e.UsageCapabilityStatus != "" || len(e.UsageCapabilities) > 0 || e.UsageGranularity != "" || e.UsageTokensPositiveOnly) {
		errs = append(errs, errors.New("usage capabilities are only reported when usage collection is enabled"))
	}
	if e.UsageCapabilities == nil {
		errs = append(errs, errors.New("usage_capabilities must not be nil (it would be written as null); use [] when none are known"))
	}
	seen := map[string]bool{}
	for _, c := range e.UsageCapabilities {
		if !validCapabilities[c] || seen[c] {
			errs = append(errs, fmt.Errorf("usage_capabilities entry %q is unknown or repeated", c))
		}
		seen[c] = true
	}
	switch e.UsageGranularity {
	case "", UsageGranularityRequest, UsageGranularityInterval, UsageGranularityInvocation:
	default:
		errs = append(errs, fmt.Errorf("usage_granularity %q is not known", e.UsageGranularity))
	}
	if e.hasUsageCounters() && e.UsageSource != UsageSourceNative {
		errs = append(errs, errors.New("usage_* counters are only reported for the native usage source"))
	}
	switch e.AnalyticsState {
	case AnalyticsStateEnabled:
		if e.AnalyticsDisabledReason != "" {
			errs = append(errs, errors.New("analytics_disabled_reason is only set when analytics is disabled"))
		}
	case AnalyticsStateDisabled:
		switch e.AnalyticsDisabledReason {
		case AnalyticsDisabledOutboxUnavailable, AnalyticsDisabledNoGCPExporter:
		default:
			errs = append(errs, fmt.Errorf("analytics_disabled_reason %q is not known", e.AnalyticsDisabledReason))
		}
		if e.hasAnalyticsCounters() {
			errs = append(errs, errors.New("analytics_* counters are omitted when analytics is disabled"))
		}
	default:
		errs = append(errs, fmt.Errorf("analytics_state %q is not known", e.AnalyticsState))
	}
	if (e.AnalyticsStatsFailed == nil) != (e.AnalyticsAppended == nil) {
		errs = append(errs, errors.New("analytics_stats_failed is reported with the analytics_* counters"))
	}
	if e.AnalyticsRetainedBytes != nil && e.AnalyticsPendingBytes != nil && *e.AnalyticsRetainedBytes < *e.AnalyticsPendingBytes {
		errs = append(errs, errors.New("analytics_retained_bytes includes the pending bytes and cannot be smaller"))
	}
	disk := e.analyticsDiskCounters()
	if !groupComplete(disk) {
		errs = append(errs, errors.New("the on-disk analytics fields are reported as a complete group or not at all"))
	}
	diskSet := e.AnalyticsPendingBytes != nil || e.ProducerEpoch != "" || e.AnalyticsHeld != nil
	if e.AnalyticsStatsFailed != nil {
		if *e.AnalyticsStatsFailed && (diskSet || e.AnalyticsOldestPendingFileAgeMs != nil) {
			errs = append(errs, errors.New("with analytics_stats_failed the on-disk fields must be omitted (unknown), never placeholders"))
		}
		if !*e.AnalyticsStatsFailed && (e.AnalyticsPendingBytes == nil || e.ProducerEpoch == "" || e.AnalyticsHeld == nil) {
			errs = append(errs, errors.New("measured on-disk fields (pending, retained, totals, producer_epoch, producer_seq_last) are required unless analytics_stats_failed"))
		}
	}
	if !groupComplete(e.usageCounters()) || !groupComplete(e.analyticsCounters()) {
		errs = append(errs, errors.New("usage_* and analytics_* counters are each reported as a complete group or not at all"))
	}
	if e.AnalyticsState == AnalyticsStateEnabled && !e.hasAnalyticsCounters() {
		errs = append(errs, errors.New("enabled analytics requires the analytics_* counters"))
	}
	errs = append(errs, e.outboxSubjectErrors()...)
	if err := e.checkNonNegative(); err != nil {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		return fmt.Errorf("telemetrycontract: invalid health event %s: %w", e.EventID, errors.Join(errs...))
	}
	return nil
}

// outboxSubjectErrors checks the outbox-subject group: complete or absent,
// only with a successful ledger read, and relocated=false only when hub_id
// equals the pin. There is deliberately no rule that true requires a
// different hub (the sticky return-to-origin case is valid).
func (e *HealthEvent) outboxSubjectErrors() []error {
	set := 0
	for _, present := range []bool{e.OutboxPinnedHubID != "", e.OutboxHubMismatchTotal != nil, e.OutboxRelocatedRefusedTotal != nil} {
		if present {
			set++
		}
	}
	if set == 0 {
		if e.OutboxRelocated != nil {
			return []error{errors.New("outbox_relocated is reported only with outbox_pinned_hub_id and the outbox_*_total counters")}
		}
		return nil
	}
	var errs []error
	if set != 3 {
		errs = append(errs, errors.New("outbox_pinned_hub_id and the outbox_*_total counters are reported as a complete group or not at all"))
	}
	if e.AnalyticsStatsFailed == nil || *e.AnalyticsStatsFailed {
		errs = append(errs, errors.New("the outbox-subject fields require a successful ledger read (analytics_stats_failed false)"))
	}
	if e.OutboxRelocated != nil && !*e.OutboxRelocated && e.HubID != e.OutboxPinnedHubID {
		errs = append(errs, fmt.Errorf("outbox_relocated=false requires hub_id %q to equal outbox_pinned_hub_id %q", e.HubID, e.OutboxPinnedHubID))
	}
	return errs
}

func (e *HealthEvent) checkNonNegative() error {
	values := map[string]int64{
		"metric_points_accepted": e.MetricPointsAccepted, "metric_points_rejected": e.MetricPointsRejected,
		"metric_points_filtered": e.MetricPointsFiltered, "metric_points_delivered": e.MetricPointsDelivered,
		"metric_points_dropped": e.MetricPointsDropped, "metric_points_unconfirmed": e.MetricPointsUnconfirmed,
		"metric_export_attempts": e.MetricExportAttempts, "metric_export_failures": e.MetricExportFailures,
		"metric_export_retries": e.MetricExportRetries,
		"log_records_accepted":  e.LogRecordsAccepted, "log_records_rejected": e.LogRecordsRejected,
		"log_records_filtered": e.LogRecordsFiltered, "log_records_delivered": e.LogRecordsDelivered,
		"log_records_dropped": e.LogRecordsDropped, "log_records_unconfirmed": e.LogRecordsUnconfirmed,
		"log_export_attempts": e.LogExportAttempts, "log_export_failures": e.LogExportFailures,
		"log_export_retries": e.LogExportRetries,
		"queue_bytes":        e.QueueBytes, "queue_records": e.QueueRecords,
		"health_send_failures": e.HealthSendFailures, "health_superseded": e.HealthSuperseded, "health_rejected": e.HealthRejected,
	}
	for k, p := range e.optionalCounters() {
		if p != nil {
			values[k] = *p
		}
	}
	var errs []error
	for k, v := range values {
		if v < 0 {
			errs = append(errs, fmt.Errorf("%s = %d is negative", k, v))
		}
	}
	return errors.Join(errs...)
}

func (e *HealthEvent) usageCounters() map[string]*int64 {
	return map[string]*int64{
		"usage_log_records_received": e.UsageLogRecordsReceived, "usage_log_records_recognized": e.UsageLogRecordsRecognized,
		"usage_log_records_malformed": e.UsageLogRecordsMalformed, "usage_metric_batches_received": e.UsageMetricBatchesReceived,
		"usage_metric_batches_malformed": e.UsageMetricBatchesMalformed, "usage_metric_increments": e.UsageMetricIncrements,
		"usage_observations_ignored": e.UsageObservationsIgnored, "usage_observations_duplicate": e.UsageObservationsDuplicate,
		"usage_observations_derived": e.UsageObservationsDerived, "usage_clamped_input": e.UsageClampedInput,
		"usage_stale_backwards": e.UsageStaleBackwards, "usage_baselined_after_cap": e.UsageBaselinedAfterCap,
	}
}

func (e *HealthEvent) analyticsCounters() map[string]*int64 {
	return map[string]*int64{
		"analytics_appended": e.AnalyticsAppended, "analytics_certain_duplicates": e.AnalyticsCertainDuplicates,
		"analytics_overflow":      e.AnalyticsOverflow,
		"analytics_append_failed": e.AnalyticsAppendFailed, "analytics_dropped_no_identity": e.AnalyticsDroppedNoIdentity,
		"analytics_finalized": e.AnalyticsFinalized, "analytics_delivered": e.AnalyticsDelivered,
		"analytics_send_failures": e.AnalyticsSendFailures, "analytics_send_retries": e.AnalyticsSendRetries,
		"analytics_torn": e.AnalyticsTorn, "analytics_hub_id_mismatch": e.AnalyticsHubIDMismatch,
		"analytics_conflicts": e.AnalyticsConflicts, "analytics_quarantined": e.AnalyticsQuarantined,
		"analytics_dead_letters":   e.AnalyticsDeadLetters,
		"analytics_sync_uncertain": e.AnalyticsSyncUncertain, "analytics_corrupt_intents": e.AnalyticsCorruptIntents,
		"analytics_invalid": e.AnalyticsInvalid, "analytics_retry_backoff_ms": e.AnalyticsRetryBackoffMs,
		"analytics_unresolved": e.AnalyticsUnresolved, "analytics_commit_deferred": e.AnalyticsCommitDeferred,
		"analytics_invalid_lines": e.AnalyticsInvalidLines,
	}
}

// analyticsDiskCounters are the fields read from the outbox directory:
// present only when analytics_stats_failed is false (a failed read is
// omitted, never a placeholder).
func (e *HealthEvent) analyticsDiskCounters() map[string]*int64 {
	return map[string]*int64{
		"analytics_overflow_total": e.AnalyticsOverflowTotal, "analytics_oversize_total": e.AnalyticsOversizeTotal,
		"analytics_append_failed_total": e.AnalyticsAppendFailedTotal, "analytics_dead_letters_total": e.AnalyticsDeadLettersTotal,
		"analytics_retained_bytes": e.AnalyticsRetainedBytes, "analytics_pending_bytes": e.AnalyticsPendingBytes,
		"producer_seq_last":              e.ProducerSeqLast,
		"analytics_sync_uncertain_total": e.AnalyticsSyncUncertainTotal, "analytics_commit_deferred_total": e.AnalyticsCommitDeferredTotal,
	}
}

func (e *HealthEvent) optionalCounters() map[string]*int64 {
	out := e.usageCounters()
	for _, m := range []map[string]*int64{e.analyticsCounters(), e.analyticsDiskCounters()} {
		for k, v := range m {
			out[k] = v
		}
	}
	out["analytics_oldest_pending_file_age_ms"] = e.AnalyticsOldestPendingFileAgeMs
	out["outbox_hub_mismatch_total"] = e.OutboxHubMismatchTotal
	out["outbox_relocated_refused_total"] = e.OutboxRelocatedRefusedTotal
	return out
}

func (e *HealthEvent) hasUsageCounters() bool {
	for _, p := range e.usageCounters() {
		if p != nil {
			return true
		}
	}
	return false
}

func (e *HealthEvent) hasAnalyticsCounters() bool {
	for _, m := range []map[string]*int64{e.analyticsCounters(), e.analyticsDiskCounters()} {
		for _, p := range m {
			if p != nil {
				return true
			}
		}
	}
	return e.AnalyticsOldestPendingFileAgeMs != nil || e.ProducerEpoch != "" || e.AnalyticsStatsFailed != nil || e.AnalyticsHeld != nil ||
		e.OutboxPinnedHubID != "" || e.OutboxRelocated != nil || e.OutboxHubMismatchTotal != nil || e.OutboxRelocatedRefusedTotal != nil
}

// groupComplete reports whether a counter group is all set or all unset.
func groupComplete(m map[string]*int64) bool {
	set := 0
	for _, p := range m {
		if p != nil {
			set++
		}
	}
	return set == 0 || set == len(m)
}

func i64(v int64) *int64 { return &v }

// SetUsageCounters fills the native usage counter fields from c.
func (e *HealthEvent) SetUsageCounters(c HealthUsageCounters) {
	e.UsageLogRecordsReceived, e.UsageLogRecordsRecognized = i64(c.LogRecordsReceived), i64(c.LogRecordsRecognized)
	e.UsageLogRecordsMalformed = i64(c.LogRecordsMalformed)
	e.UsageMetricBatchesReceived, e.UsageMetricBatchesMalformed = i64(c.MetricBatchesReceived), i64(c.MetricBatchesMalformed)
	e.UsageMetricIncrements = i64(c.MetricIncrements)
	e.UsageObservationsIgnored, e.UsageObservationsDuplicate = i64(c.ObservationsIgnored), i64(c.ObservationsDuplicate)
	e.UsageObservationsDerived = i64(c.ObservationsDerived)
	e.UsageClampedInput, e.UsageStaleBackwards, e.UsageBaselinedAfterCap = i64(c.ClampedInput), i64(c.StaleBackwards), i64(c.BaselinedAfterCap)
}

// SetAnalyticsCounters fills the usage outbox counter fields from c.
func (e *HealthEvent) SetAnalyticsCounters(c HealthAnalyticsCounters) {
	e.AnalyticsAppended, e.AnalyticsCertainDuplicates = i64(c.Appended), i64(c.CertainDuplicates)
	e.AnalyticsOverflow, e.AnalyticsAppendFailed = i64(c.Overflow), i64(c.AppendFailed)
	e.AnalyticsDroppedNoIdentity = i64(c.DroppedNoIdentity)
	e.AnalyticsFinalized, e.AnalyticsDelivered = i64(c.Finalized), i64(c.Delivered)
	e.AnalyticsSendFailures, e.AnalyticsSendRetries = i64(c.SendFailures), i64(c.SendRetries)
	e.AnalyticsTorn, e.AnalyticsHubIDMismatch = i64(c.Torn), i64(c.HubIDMismatch)
	e.AnalyticsConflicts, e.AnalyticsQuarantined, e.AnalyticsDeadLetters = i64(c.Conflicts), i64(c.Quarantined), i64(c.DeadLetters)
	e.AnalyticsSyncUncertain, e.AnalyticsCorruptIntents = i64(c.SyncUncertain), i64(c.CorruptIntents)
	e.AnalyticsInvalid, e.AnalyticsRetryBackoffMs = i64(c.Invalid), i64(c.RetryBackoffMs)
	e.AnalyticsUnresolved, e.AnalyticsCommitDeferred = i64(c.Unresolved), i64(c.CommitDeferred)
	e.AnalyticsInvalidLines = i64(c.InvalidLines)
	failed := c.StatsFailed
	e.AnalyticsStatsFailed = &failed
	// On-disk values: omitted (unknown) when any read failed.
	e.AnalyticsOverflowTotal, e.AnalyticsOversizeTotal, e.AnalyticsAppendFailedTotal, e.AnalyticsDeadLettersTotal = nil, nil, nil, nil
	e.AnalyticsRetainedBytes, e.AnalyticsPendingBytes, e.ProducerSeqLast = nil, nil, nil
	e.AnalyticsOldestPendingFileAgeMs, e.ProducerEpoch = nil, ""
	e.AnalyticsSyncUncertainTotal, e.AnalyticsCommitDeferredTotal, e.AnalyticsHeld = nil, nil, nil
	e.OutboxPinnedHubID, e.OutboxRelocated, e.OutboxHubMismatchTotal, e.OutboxRelocatedRefusedTotal = "", nil, nil, nil
	if failed {
		return
	}
	if o := c.Outbox; o != nil {
		e.OutboxPinnedHubID = o.PinnedHubID
		e.OutboxHubMismatchTotal, e.OutboxRelocatedRefusedTotal = i64(o.HubMismatchTotal), i64(o.RelocatedRefusedTotal)
		if o.Relocated != nil {
			relocated := *o.Relocated
			e.OutboxRelocated = &relocated
		}
	}
	e.AnalyticsOverflowTotal, e.AnalyticsOversizeTotal = i64(c.OverflowTotal), i64(c.OversizeTotal)
	e.AnalyticsAppendFailedTotal, e.AnalyticsDeadLettersTotal = i64(c.AppendFailedTotal), i64(c.DeadLettersTotal)
	e.AnalyticsRetainedBytes, e.AnalyticsPendingBytes = i64(c.RetainedBytes), i64(c.PendingBytes)
	e.ProducerEpoch, e.ProducerSeqLast = c.ProducerEpoch, i64(c.ProducerSeqLast)
	e.AnalyticsSyncUncertainTotal, e.AnalyticsCommitDeferredTotal = i64(c.SyncUncertainTotal), i64(c.CommitDeferredTotal)
	held := c.Held
	e.AnalyticsHeld = &held
	if c.OldestPendingFileAgeMs != nil {
		e.AnalyticsOldestPendingFileAgeMs = i64(*c.OldestPendingFileAgeMs)
	}
}
