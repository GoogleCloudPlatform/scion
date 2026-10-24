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
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func validHealth() *HealthEvent {
	ts := time.Date(2026, 10, 22, 6, 0, 0, 0, time.UTC)
	ev := &HealthEvent{
		Envelope:              NewEnvelope(EventTypeHealth, "h1-0123456789abcdef0123456789abcdef", "sciontool/project/p/agent/a", ts, ts),
		PipelineRunID:         "run-1",
		HealthSeq:             1,
		Trigger:               HealthTriggerStart,
		Harness:               "claude",
		UsageSource:           UsageSourceNative,
		UsageSourceOrigin:     UsageSourceOriginOverlay,
		UsageCollection:       UsageCollectionEnabled,
		UsageCapabilityStatus: UsageCapabilityStatusStatic,
		UsageCapabilities:     []string{UsageCapabilityCalls, UsageCapabilityInput},
		UsageGranularity:      UsageGranularityRequest,
		UsageRulesVersion:     "1",
		AnalyticsState:        AnalyticsStateEnabled,
		DeliveryState:         "running",
	}
	ev.HubID, ev.AgentID, ev.ProjectID = "hub-1", "a", "p"
	ev.SetUsageCounters(HealthUsageCounters{LogRecordsReceived: 3})
	ev.SetAnalyticsCounters(HealthAnalyticsCounters{ProducerEpoch: "e", ProducerSeqLast: 2})
	return ev
}

func TestHealthEventRoundTrip(t *testing.T) {
	ev := validHealth()
	line, err := Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	var back HealthEvent
	if err := Unmarshal(line, &back); err != nil {
		t.Fatal(err)
	}
	again, err := Marshal(&back)
	if err != nil || string(again) != string(line) {
		t.Fatalf("round trip changed bytes:\n%s\n%s", line, again)
	}
	var raw map[string]any
	if err := json.Unmarshal(line, &raw); err != nil {
		t.Fatal(err)
	}
	// Flat columns; omitted = not applicable.
	if _, ok := raw["analytics_oldest_pending_file_age_ms"]; ok {
		t.Error("oldest pending age must be omitted when nothing is pending")
	}
	if raw["producer_epoch"] != "e" || raw["usage_log_records_received"] != float64(3) {
		t.Errorf("flat fields: %v %v", raw["producer_epoch"], raw["usage_log_records_received"])
	}
	if raw["usage_tokens_positive_only"] != false || raw["loss_indicated"] != false {
		t.Error("plain bools are always written")
	}
	for _, k := range []string{"final_flush_outcome", "final_residual_records", "usage_disabled_reason"} {
		if _, ok := raw[k]; ok {
			t.Errorf("%s must be omitted here", k)
		}
	}
}

func TestHealthEventValidate(t *testing.T) {
	residual := int64(0)
	cases := map[string]struct {
		mutate func(*HealthEvent)
		want   string
	}{
		"wrong type":            {func(e *HealthEvent) { e.EventType = EventTypeUsage }, "event_type"},
		"no run":                {func(e *HealthEvent) { e.PipelineRunID = "" }, "pipeline_run_id"},
		"bad trigger":           {func(e *HealthEvent) { e.Trigger = "boot" }, "trigger"},
		"final without outcome": {func(e *HealthEvent) { e.Trigger = HealthTriggerFinal }, "final_flush_outcome"},
		"final without residual": {func(e *HealthEvent) {
			e.Trigger, e.FinalFlushOutcome = HealthTriggerFinal, FinalFlushDrained
		}, "final_residual_records"},
		"outcome on periodic": {func(e *HealthEvent) {
			e.Trigger, e.FinalFlushOutcome, e.FinalResidualRecords = HealthTriggerPeriodic, FinalFlushDrained, &residual
		}, "only set on trigger final"},
		"disabled without reason": {func(e *HealthEvent) {
			e.UsageCollection, e.UsageCapabilities, e.UsageCapabilityStatus, e.UsageGranularity = UsageCollectionDisabled, nil, "", ""
		}, "usage_disabled_reason"},
		"capabilities while disabled": {func(e *HealthEvent) {
			e.UsageCollection, e.UsageDisabledReason = UsageCollectionDisabled, UsageDisabledSourceUnset
		}, "only reported when usage collection is enabled"},
		"reason while enabled":    {func(e *HealthEvent) { e.UsageDisabledReason = UsageDisabledSourceUnset }, "only set when usage collection is disabled"},
		"unknown capability":      {func(e *HealthEvent) { e.UsageCapabilities = []string{"calls", "dollars"} }, "dollars"},
		"repeated capability":     {func(e *HealthEvent) { e.UsageCapabilities = []string{"calls", "calls"} }, "repeated"},
		"static without list":     {func(e *HealthEvent) { e.UsageCapabilities = nil }, "requires usage_capabilities"},
		"unknown with list":       {func(e *HealthEvent) { e.UsageCapabilityStatus = UsageCapabilityStatusUnknown }, "must not list"},
		"usage counters on hooks": {func(e *HealthEvent) { e.UsageSource = UsageSourceHooks }, "native usage source"},
		"partial usage group":     {func(e *HealthEvent) { e.UsageClampedInput = nil }, "complete group"},
		"analytics counters while disabled": {func(e *HealthEvent) {
			e.AnalyticsState, e.AnalyticsDisabledReason = AnalyticsStateDisabled, "outbox_unavailable"
		}, "omitted when analytics is disabled"},
		"enabled analytics without counters": {func(e *HealthEvent) {
			clearAnalytics(e)
		}, "requires the analytics_* counters"},
		"negative counter": {func(e *HealthEvent) { e.MetricExportRetries = -1 }, "metric_export_retries"},
		"negative optional": {func(e *HealthEvent) {
			v := int64(-5)
			e.AnalyticsPendingBytes = &v
		}, "analytics_pending_bytes"},
		"stats_failed without counters": {func(e *HealthEvent) { e.AnalyticsStatsFailed = nil }, "analytics_stats_failed"},
		"nil capabilities": {func(e *HealthEvent) {
			e.UsageCapabilities = nil
			e.UsageCapabilityStatus = UsageCapabilityStatusUnknown
		}, "must not be nil"},
		"unknown analytics reason": {func(e *HealthEvent) {
			clearAnalytics(e)
			e.AnalyticsState, e.AnalyticsDisabledReason = AnalyticsStateDisabled, "unknown"
		}, "analytics_disabled_reason"},
		"stats_failed with placeholders": {func(e *HealthEvent) {
			failed := true
			e.AnalyticsStatsFailed = &failed
		}, "must be omitted"},
		"measured without epoch": {func(e *HealthEvent) { e.ProducerEpoch = "" }, "required unless analytics_stats_failed"},
		"retained below pending": {func(e *HealthEvent) {
			*e.AnalyticsRetainedBytes, *e.AnalyticsPendingBytes = 1, 2
		}, "cannot be smaller"},
		"drained with stats failed": {func(e *HealthEvent) {
			drainedFinal(e)
			e.SetAnalyticsCounters(HealthAnalyticsCounters{StatsFailed: true})
		}, "drained contradicts"},
		"drained with pending": {func(e *HealthEvent) {
			drainedFinal(e)
			*e.AnalyticsPendingBytes, *e.AnalyticsRetainedBytes = 10, 10
		}, "drained contradicts"},
		"drained with held": {func(e *HealthEvent) {
			drainedFinal(e)
			held := true
			e.AnalyticsHeld = &held
		}, "drained contradicts"},
		"drained with residual": {func(e *HealthEvent) {
			drainedFinal(e)
			*e.FinalResidualRecords = 1
		}, "drained contradicts"},
		"drained with queued records": {func(e *HealthEvent) {
			drainedFinal(e)
			e.QueueRecords = 3
		}, "drained contradicts"},
		"bad origin": {func(e *HealthEvent) { e.UsageSourceOrigin = "" }, "usage_source_origin"},
	}
	for name, c := range cases {
		ev := validHealth()
		c.mutate(ev)
		err := ev.Validate()
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want containing %q", name, err, c.want)
		}
		if _, merr := Marshal(ev); merr == nil {
			t.Errorf("%s: Marshal accepted an invalid event", name)
		}
	}
	if err := validHealth().Validate(); err != nil {
		t.Fatalf("valid event rejected: %v", err)
	}
}

// Shared column names keep one type across event types (one BigQuery
// table for all of scion-analytics): producer_epoch is a string on both
// agent.usage and telemetry.health.
func TestHealthSharedColumnTypes(t *testing.T) {
	line, err := Marshal(validHealth())
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	_ = json.Unmarshal(line, &raw)
	if _, ok := raw["producer_epoch"].(string); !ok {
		t.Fatal("producer_epoch must be a string")
	}
	for _, k := range []string{"hub_id", "agent_id", "project_id", "harness", "usage_source", "event_id", "producer_namespace"} {
		if _, ok := raw[k].(string); !ok {
			t.Errorf("%s must be a string", k)
		}
	}
	if _, ok := raw["health_seq"].(float64); !ok {
		t.Error("health_seq must be a number (integer)")
	}
}

// clearAnalytics removes every analytics_* field.
func clearAnalytics(e *HealthEvent) {
	e.AnalyticsAppended, e.AnalyticsCertainDuplicates, e.AnalyticsOverflow, e.AnalyticsOverflowTotal = nil, nil, nil, nil
	e.AnalyticsAppendFailed, e.AnalyticsDroppedNoIdentity, e.AnalyticsFinalized, e.AnalyticsDelivered = nil, nil, nil, nil
	e.AnalyticsSendFailures, e.AnalyticsSendRetries, e.AnalyticsTorn, e.AnalyticsHubIDMismatch = nil, nil, nil, nil
	e.AnalyticsConflicts, e.AnalyticsQuarantined, e.AnalyticsDeadLetters = nil, nil, nil
	e.AnalyticsOversizeTotal, e.AnalyticsAppendFailedTotal, e.AnalyticsDeadLettersTotal = nil, nil, nil
	e.AnalyticsSyncUncertain, e.AnalyticsCorruptIntents, e.AnalyticsInvalid, e.AnalyticsRetainedBytes = nil, nil, nil, nil
	e.AnalyticsUnresolved, e.AnalyticsCommitDeferred, e.AnalyticsSyncUncertainTotal, e.AnalyticsCommitDeferredTotal = nil, nil, nil, nil
	e.AnalyticsHeld, e.AnalyticsInvalidLines = nil, nil
	e.AnalyticsPendingBytes, e.AnalyticsRetryBackoffMs, e.ProducerSeqLast = nil, nil, nil
	e.AnalyticsStatsFailed, e.ProducerEpoch, e.AnalyticsOldestPendingFileAgeMs = nil, "", nil
	if e.hasAnalyticsCounters() {
		panic("clearAnalytics missed a field")
	}
}

// With analytics_stats_failed the on-disk fields are omitted, not zeros,
// while the in-memory counters stay.
func TestHealthStatsFailedOmitsDiskFields(t *testing.T) {
	ev := validHealth()
	ev.SetAnalyticsCounters(HealthAnalyticsCounters{Appended: 3, ProducerEpoch: "e", ProducerSeqLast: 9, PendingBytes: 1, StatsFailed: true})
	line, err := Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	_ = json.Unmarshal(line, &raw)
	for _, k := range []string{"producer_epoch", "producer_seq_last", "analytics_pending_bytes", "analytics_retained_bytes",
		"analytics_overflow_total", "analytics_oversize_total", "analytics_append_failed_total", "analytics_dead_letters_total",
		"analytics_oldest_pending_file_age_ms", "analytics_held", "analytics_sync_uncertain_total", "analytics_commit_deferred_total"} {
		if _, ok := raw[k]; ok {
			t.Errorf("%s must be omitted when stats failed", k)
		}
	}
	if raw["analytics_appended"] != float64(3) || raw["analytics_stats_failed"] != true {
		t.Errorf("in-memory counters and the flag stay: %v %v", raw["analytics_appended"], raw["analytics_stats_failed"])
	}
}

// drainedFinal turns the valid start event into a consistent drained final
// (Validate accepts it; TestHealthDrainedFinalIsValid pins that), so each
// contradiction case above changes exactly one input.
func drainedFinal(e *HealthEvent) {
	zero := int64(0)
	e.Trigger, e.FinalFlushOutcome, e.FinalResidualRecords = HealthTriggerFinal, FinalFlushDrained, &zero
}

func TestHealthDrainedFinalIsValid(t *testing.T) {
	ev := validHealth()
	drainedFinal(ev)
	if err := ev.Validate(); err != nil {
		t.Fatalf("baseline drained final rejected: %v", err)
	}
}

// withOutboxSubject sets a complete outbox-subject group on a valid event.
func withOutboxSubject(e *HealthEvent, pinned string, relocated *bool) {
	e.SetAnalyticsCounters(HealthAnalyticsCounters{ProducerEpoch: "e", ProducerSeqLast: 2, Outbox: &HealthOutboxSubject{
		PinnedHubID: pinned, Relocated: relocated, HubMismatchTotal: 1, RelocatedRefusedTotal: 0,
	}})
}

// The outbox-subject group (WP3 hub-origin plan v5): complete or absent,
// only with a successful ledger read; relocated=false requires hub_id ==
// the pin; relocated=true with hub_id == the pin (return to origin) is
// valid, and so is an unknown relocated.
func TestHealthOutboxSubjectValidate(t *testing.T) {
	valid := map[string]func(*HealthEvent){
		"absent":                  func(*HealthEvent) {},
		"same origin false":       func(e *HealthEvent) { withOutboxSubject(e, "hub-1", boolp(false)) },
		"relocated true, new hub": func(e *HealthEvent) { withOutboxSubject(e, "hub-0", boolp(true)) },
		"return to origin true":   func(e *HealthEvent) { withOutboxSubject(e, "hub-1", boolp(true)) },
		"relocated unknown":       func(e *HealthEvent) { withOutboxSubject(e, "hub-0", nil) },
	}
	for name, mutate := range valid {
		ev := validHealth()
		mutate(ev)
		if _, err := Marshal(ev); err != nil {
			t.Errorf("%s: rejected: %v", name, err)
		}
	}
	invalid := map[string]struct {
		mutate func(*HealthEvent)
		want   string
	}{
		"false with another hub":  {func(e *HealthEvent) { withOutboxSubject(e, "hub-0", boolp(false)) }, "outbox_relocated=false requires"},
		"relocated without group": {func(e *HealthEvent) { e.OutboxRelocated = boolp(true) }, "outbox_relocated is reported only with"},
		"incomplete group": {func(e *HealthEvent) {
			withOutboxSubject(e, "hub-1", nil)
			e.OutboxRelocatedRefusedTotal = nil
		}, "complete group"},
		"with stats failed": {func(e *HealthEvent) {
			withOutboxSubject(e, "hub-1", nil)
			failed := true
			e.AnalyticsStatsFailed = &failed
		}, "successful ledger read"},
		"negative counter": {func(e *HealthEvent) {
			withOutboxSubject(e, "hub-1", nil)
			*e.OutboxHubMismatchTotal = -1
		}, "outbox_hub_mismatch_total"},
	}
	for name, c := range invalid {
		ev := validHealth()
		c.mutate(ev)
		err := ev.Validate()
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want containing %q", name, err, c.want)
		}
		if _, merr := Marshal(ev); merr == nil {
			t.Errorf("%s: Marshal accepted an invalid event", name)
		}
	}
}

// SetAnalyticsCounters drops the outbox subject when the ledger read failed.
func TestHealthOutboxSubjectOmittedOnStatsFailure(t *testing.T) {
	ev := validHealth()
	ev.SetAnalyticsCounters(HealthAnalyticsCounters{StatsFailed: true, Outbox: &HealthOutboxSubject{PinnedHubID: "hub-1", Relocated: boolp(true)}})
	if ev.OutboxPinnedHubID != "" || ev.OutboxRelocated != nil || ev.OutboxHubMismatchTotal != nil || ev.OutboxRelocatedRefusedTotal != nil {
		t.Fatalf("stats failure must omit the outbox subject: %+v", ev)
	}
	if _, err := Marshal(ev); err != nil {
		t.Fatal(err)
	}
}
