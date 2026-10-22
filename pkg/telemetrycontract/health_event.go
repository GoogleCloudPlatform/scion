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

// Analytics (usage outbox) states.
const (
	AnalyticsStateEnabled  = "enabled"
	AnalyticsStateDisabled = "disabled"
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
	PendingBytes                                         int64
	// OldestPendingFileAgeMs < 0 means nothing is pending (omitted).
	OldestPendingFileAgeMs int64
	RetryBackoffMs         int64
	ProducerEpoch          string
	ProducerSeqLast        int64
	// StatsFailed means the outbox's on-disk pending state could not be
	// read: PendingBytes is then not a measured value.
	StatsFailed bool
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
	UsageCapabilityStatus string   `json:"usage_capability_status,omitempty"`
	UsageCapabilities     []string `json:"usage_capabilities,omitempty"`
	UsageGranularity      string   `json:"usage_granularity,omitempty"`
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
	AnalyticsSendRetries   *int64 `json:"analytics_send_retries,omitempty"`
	AnalyticsTorn          *int64 `json:"analytics_torn,omitempty"`
	AnalyticsHubIDMismatch *int64 `json:"analytics_hub_id_mismatch,omitempty"`
	AnalyticsPendingBytes  *int64 `json:"analytics_pending_bytes,omitempty"`
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
	// AnalyticsStatsFailed is true when the outbox's pending state could
	// not be read from disk (analytics_pending_bytes is then not measured,
	// and a final event is not_drained). Present only with analytics.
	AnalyticsStatsFailed *bool `json:"analytics_stats_failed,omitempty"`

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

	// LossIndicated is true when any INCLUDED loss counter in this
	// snapshot is non-zero (design note section 3.2). false never means
	// loss-free: hook-process append failures, for example, are counted in
	// those processes and are absent here. Always written.
	LossIndicated bool `json:"loss_indicated"`

	// FinalResidualRecords are metric/log records still unconfirmed when
	// the final flush gave up (trigger final only).
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
		if e.AnalyticsDisabledReason == "" {
			errs = append(errs, errors.New("disabled analytics requires analytics_disabled_reason"))
		}
		if e.hasAnalyticsCounters() {
			errs = append(errs, errors.New("analytics_* counters are omitted when analytics is disabled"))
		}
	default:
		errs = append(errs, fmt.Errorf("analytics_state %q is not known", e.AnalyticsState))
	}
	if (e.AnalyticsStatsFailed == nil) != (e.AnalyticsPendingBytes == nil) {
		errs = append(errs, errors.New("analytics_stats_failed is reported with the analytics_* counters"))
	}
	if !groupComplete(e.usageCounters()) || !groupComplete(e.analyticsCounters()) {
		errs = append(errs, errors.New("usage_* and analytics_* counters are each reported as a complete group or not at all"))
	}
	if e.AnalyticsState == AnalyticsStateEnabled && !e.hasAnalyticsCounters() {
		errs = append(errs, errors.New("enabled analytics requires the analytics_* counters"))
	}
	if err := e.checkNonNegative(); err != nil {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		return fmt.Errorf("telemetrycontract: invalid health event %s: %w", e.EventID, errors.Join(errs...))
	}
	return nil
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
		"health_send_failures": e.HealthSendFailures, "health_superseded": e.HealthSuperseded,
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
		"analytics_overflow": e.AnalyticsOverflow, "analytics_overflow_total": e.AnalyticsOverflowTotal,
		"analytics_append_failed": e.AnalyticsAppendFailed, "analytics_dropped_no_identity": e.AnalyticsDroppedNoIdentity,
		"analytics_finalized": e.AnalyticsFinalized, "analytics_delivered": e.AnalyticsDelivered,
		"analytics_send_failures": e.AnalyticsSendFailures, "analytics_send_retries": e.AnalyticsSendRetries,
		"analytics_torn": e.AnalyticsTorn, "analytics_hub_id_mismatch": e.AnalyticsHubIDMismatch,
		"analytics_pending_bytes": e.AnalyticsPendingBytes, "analytics_retry_backoff_ms": e.AnalyticsRetryBackoffMs,
		"producer_seq_last": e.ProducerSeqLast,
	}
}

func (e *HealthEvent) optionalCounters() map[string]*int64 {
	out := e.usageCounters()
	for k, v := range e.analyticsCounters() {
		out[k] = v
	}
	out["analytics_oldest_pending_file_age_ms"] = e.AnalyticsOldestPendingFileAgeMs
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
	for _, p := range e.analyticsCounters() {
		if p != nil {
			return true
		}
	}
	return e.AnalyticsOldestPendingFileAgeMs != nil || e.ProducerEpoch != "" || e.AnalyticsStatsFailed != nil
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
	e.AnalyticsOverflow, e.AnalyticsOverflowTotal = i64(c.Overflow), i64(c.OverflowTotal)
	e.AnalyticsAppendFailed, e.AnalyticsDroppedNoIdentity = i64(c.AppendFailed), i64(c.DroppedNoIdentity)
	e.AnalyticsFinalized, e.AnalyticsDelivered = i64(c.Finalized), i64(c.Delivered)
	e.AnalyticsSendFailures, e.AnalyticsSendRetries = i64(c.SendFailures), i64(c.SendRetries)
	e.AnalyticsTorn, e.AnalyticsHubIDMismatch = i64(c.Torn), i64(c.HubIDMismatch)
	e.AnalyticsPendingBytes, e.AnalyticsRetryBackoffMs = i64(c.PendingBytes), i64(c.RetryBackoffMs)
	e.AnalyticsOldestPendingFileAgeMs = nil
	if c.OldestPendingFileAgeMs >= 0 {
		e.AnalyticsOldestPendingFileAgeMs = i64(c.OldestPendingFileAgeMs)
	}
	e.ProducerEpoch = c.ProducerEpoch
	e.ProducerSeqLast = i64(c.ProducerSeqLast)
	failed := c.StatsFailed
	e.AnalyticsStatsFailed = &failed
}
