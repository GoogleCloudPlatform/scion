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

// This file holds the agent.usage analytics event (WP2 design note
// telemetry/design/wp2-design.md, section 3). One event is produced per
// accepted usage increment -- the same increment that updates the
// canonical gen_ai.api.calls / scion.usage.tokens counters -- and never
// from session summaries. A repeat the counters' in-memory dedupe rejects
// is still offered to the outbox: a verified request ID repeat is
// suppressed as a certain duplicate, any other repeat is written as a
// further occurrence marked identity_ambiguous (counted only toward WP4's
// upper bound). Field types are fixed forever: a type change on an
// existing field becomes a BigQuery export error.

import (
	"errors"
	"fmt"
)

// Usage source selection values (SCION_USAGE_SOURCE).
const (
	UsageSourceNative = "native"
	UsageSourceHooks  = "hooks"
)

// Source identity kinds: how source_event_id was derived.
const (
	// SourceIdentityRequestID is a provider request ID that passed the
	// source's own shape check; a repeat is a certain duplicate.
	SourceIdentityRequestID = "request_id"
	// SourceIdentityComposite hashes the source record's identifying parts
	// (session, nanosecond times, sequence/attempt, canonical attributes).
	// Two records share it only if byte-identical; a repeat is ambiguous.
	SourceIdentityComposite = "composite"
	// SourceIdentityMetricInterval identifies a metric-derived delta by
	// stream and interval.
	SourceIdentityMetricInterval = "metric_interval"
	// SourceIdentityNone means the source carries no usable identity; the
	// event ID is random and the event is explicitly ambiguous.
	SourceIdentityNone = "none"
)

// Token status values: which of the source's defined token fields arrived.
const (
	TokenStatusReported    = "reported"
	TokenStatusPartial     = "partial"
	TokenStatusNotReported = "not_reported"
	TokenStatusMalformed   = "malformed"
)

// Input derivation values (written only when Input is present).
const (
	// InputDerivationReported means the source reports uncached input
	// directly (Claude).
	InputDerivationReported = "reported"
	// InputDerivationDerived means input = inclusive input minus reported
	// cache fields, computed only when every subtracted field was present.
	InputDerivationDerived = "derived"
)

// Model provenance values.
const (
	ModelSourceEvent      = "event"
	ModelSourceConfigured = "configured"
	ModelSourceUnknown    = "unknown"
)

// Metric temporality values for metric-derived usage.
const (
	TemporalityDelta           = "delta"
	TemporalityCumulativeDelta = "cumulative_delta"
)

// UsageEvent is one agent.usage analytics event. Unknown values are
// omitted: a nil token pointer is "the source did not report it", a
// non-nil pointer to 0 is a reported zero.
type UsageEvent struct {
	Envelope

	UsageSource         string `json:"usage_source"`
	SourceKind          string `json:"source_kind"`
	SourceSchemaVersion string `json:"source_schema_version"`
	SourceEventID       string `json:"source_event_id,omitempty"`
	SourceIdentity      string `json:"source_identity"`
	SourceOccurrence    int64  `json:"source_occurrence"`
	IdentityAmbiguous   bool   `json:"identity_ambiguous"`
	// ProducerEpoch identifies one durable producer outbox; ProducerSeq is
	// strictly increasing within it (advanced even when an event is refused
	// for overflow), so a gap is evidence of loss and a new epoch for the
	// same agent and launch means the outbox itself was lost.
	ProducerEpoch string `json:"producer_epoch"`
	ProducerSeq   int64  `json:"producer_seq"`
	// SourceTime is the source's own timestamp at full precision (RFC 3339,
	// up to nanoseconds), kept apart from the microsecond event_ts.
	SourceTime string `json:"source_time,omitempty"`

	Harness     string `json:"harness"`
	Model       string `json:"model"`
	ModelSource string `json:"model_source"`
	SessionID   string `json:"session_id,omitempty"`
	Status      string `json:"status"`
	Calls       int64  `json:"calls"`

	Input      *int64 `json:"input,omitempty"`
	Output     *int64 `json:"output,omitempty"`
	CacheRead  *int64 `json:"cache_read,omitempty"`
	CacheWrite *int64 `json:"cache_write,omitempty"`
	Reasoning  *int64 `json:"reasoning,omitempty"`
	// ReasoningOverlapsOutput is set (true) only with Reasoning, and only
	// where adapter evidence shows Output already includes reasoning.
	ReasoningOverlapsOutput *bool  `json:"reasoning_overlaps_output,omitempty"`
	InputDerivation         string `json:"input_derivation,omitempty"`
	TokenStatus             string `json:"token_status"`
	// MalformedFields is always written ([] when none).
	MalformedFields []string `json:"malformed_fields"`

	// IntervalStart is omitted when the source does not report it.
	IntervalStart string `json:"interval_start,omitempty"`
	IntervalEnd   string `json:"interval_end,omitempty"`
	Temporality   string `json:"temporality,omitempty"`

	// Harness-reported cost, never billed cost.
	ReportedCostMicros   *int64 `json:"reported_cost_micros,omitempty"`
	ReportedCostCurrency string `json:"reported_cost_currency,omitempty"`
	ReportedCostSource   string `json:"reported_cost_source,omitempty"`
}

// Validate checks the envelope plus the usage invariants that make a row
// interpretable: event type, enums, non-negative counts, presence rules.
func (e *UsageEvent) Validate() error {
	if e == nil {
		return errors.New("telemetrycontract: nil usage event")
	}
	if err := e.Envelope.Validate(); err != nil {
		return err
	}
	var errs []error
	if e.EventType != EventTypeUsage {
		errs = append(errs, fmt.Errorf("event_type = %q, want %q", e.EventType, EventTypeUsage))
	}
	if e.UsageSource != UsageSourceNative && e.UsageSource != UsageSourceHooks {
		errs = append(errs, fmt.Errorf("usage_source %q is not native or hooks", e.UsageSource))
	}
	if e.SourceKind == "" || e.SourceSchemaVersion == "" || e.Harness == "" || e.Model == "" {
		errs = append(errs, errors.New("source_kind, source_schema_version, harness and model are required"))
	}
	switch e.SourceIdentity {
	case SourceIdentityRequestID, SourceIdentityComposite, SourceIdentityMetricInterval:
		if e.SourceEventID == "" {
			errs = append(errs, fmt.Errorf("source_identity %q requires source_event_id", e.SourceIdentity))
		}
	case SourceIdentityNone:
		if e.SourceEventID != "" {
			errs = append(errs, errors.New("source_identity none must not carry source_event_id"))
		}
		if !e.IdentityAmbiguous {
			errs = append(errs, errors.New("source_identity none must be identity_ambiguous"))
		}
	default:
		errs = append(errs, fmt.Errorf("source_identity %q is not known", e.SourceIdentity))
	}
	if e.ProducerEpoch == "" || e.ProducerSeq < 1 {
		errs = append(errs, fmt.Errorf("producer_epoch %q / producer_seq %d: both required, seq >= 1", e.ProducerEpoch, e.ProducerSeq))
	}
	if e.SourceOccurrence < 1 {
		errs = append(errs, fmt.Errorf("source_occurrence = %d, want >= 1", e.SourceOccurrence))
	}
	if e.SourceOccurrence > 1 && !e.IdentityAmbiguous {
		errs = append(errs, errors.New("a repeated source occurrence must be identity_ambiguous"))
	}
	switch e.ModelSource {
	case ModelSourceEvent, ModelSourceConfigured, ModelSourceUnknown:
	default:
		errs = append(errs, fmt.Errorf("model_source %q is not known", e.ModelSource))
	}
	if e.Status != StatusSuccess && e.Status != StatusError {
		errs = append(errs, fmt.Errorf("status %q is not success or error", e.Status))
	}
	if e.Calls < 0 {
		errs = append(errs, fmt.Errorf("calls = %d is negative", e.Calls))
	}
	for name, p := range map[string]*int64{"input": e.Input, "output": e.Output, "cache_read": e.CacheRead, "cache_write": e.CacheWrite, "reasoning": e.Reasoning, "reported_cost_micros": e.ReportedCostMicros} {
		if p != nil && *p < 0 {
			errs = append(errs, fmt.Errorf("%s = %d is negative", name, *p))
		}
	}
	if e.ReasoningOverlapsOutput != nil && e.Reasoning == nil {
		errs = append(errs, errors.New("reasoning_overlaps_output requires reasoning"))
	}
	if (e.Input == nil) != (e.InputDerivation == "") {
		errs = append(errs, errors.New("input_derivation is required with input and only with input"))
	}
	if e.InputDerivation != "" && e.InputDerivation != InputDerivationReported && e.InputDerivation != InputDerivationDerived {
		errs = append(errs, fmt.Errorf("input_derivation %q is not known", e.InputDerivation))
	}
	switch e.TokenStatus {
	case TokenStatusReported, TokenStatusPartial, TokenStatusNotReported, TokenStatusMalformed:
	default:
		errs = append(errs, fmt.Errorf("token_status %q is not known", e.TokenStatus))
	}
	if (e.TokenStatus == TokenStatusMalformed) != (len(e.MalformedFields) > 0) {
		errs = append(errs, errors.New("malformed_fields is required with token_status malformed and only with it"))
	}
	if e.TokenStatus == TokenStatusNotReported && e.hasTokens() {
		errs = append(errs, errors.New("token_status not_reported must not carry token fields"))
	}
	if e.SourceTime != "" {
		if _, err := ParseTS(e.SourceTime); err != nil {
			errs = append(errs, fmt.Errorf("source_time: %w", err))
		}
	}
	interval := e.IntervalStart != "" || e.IntervalEnd != "" || e.Temporality != ""
	if interval {
		// interval_start may be unknown (omitted); interval_end and
		// temporality are what mark a metric-derived event.
		if e.IntervalStart != "" {
			if err := ValidateTS(e.IntervalStart); err != nil {
				errs = append(errs, fmt.Errorf("interval_start: %w", err))
			}
		}
		if err := ValidateTS(e.IntervalEnd); err != nil {
			errs = append(errs, fmt.Errorf("interval_end: %w", err))
		}
		if e.Temporality != TemporalityDelta && e.Temporality != TemporalityCumulativeDelta {
			errs = append(errs, fmt.Errorf("temporality %q is not known", e.Temporality))
		}
	}
	if (e.ReportedCostMicros == nil) != (e.ReportedCostCurrency == "") || (e.ReportedCostMicros == nil) != (e.ReportedCostSource == "") {
		errs = append(errs, errors.New("reported cost needs micros, currency and source together"))
	}
	if len(errs) > 0 {
		return fmt.Errorf("telemetrycontract: invalid usage event %s: %w", e.EventID, errors.Join(errs...))
	}
	return nil
}

func (e *UsageEvent) hasTokens() bool {
	return e.Input != nil || e.Output != nil || e.CacheRead != nil || e.CacheWrite != nil || e.Reasoning != nil
}
