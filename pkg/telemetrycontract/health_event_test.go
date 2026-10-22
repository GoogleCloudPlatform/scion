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
	ev.SetAnalyticsCounters(HealthAnalyticsCounters{ProducerEpoch: "e", ProducerSeqLast: 2, OldestPendingFileAgeMs: -1})
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
			e.SetAnalyticsCounters(HealthAnalyticsCounters{OldestPendingFileAgeMs: -1})
			e.AnalyticsAppended, e.AnalyticsCertainDuplicates, e.AnalyticsOverflow, e.AnalyticsOverflowTotal = nil, nil, nil, nil
			e.AnalyticsAppendFailed, e.AnalyticsDroppedNoIdentity, e.AnalyticsFinalized, e.AnalyticsDelivered = nil, nil, nil, nil
			e.AnalyticsSendFailures, e.AnalyticsSendRetries, e.AnalyticsTorn, e.AnalyticsHubIDMismatch = nil, nil, nil, nil
			e.AnalyticsPendingBytes, e.AnalyticsRetryBackoffMs, e.ProducerSeqLast = nil, nil, nil
			e.AnalyticsStatsFailed, e.ProducerEpoch, e.AnalyticsOldestPendingFileAgeMs = nil, "", nil
		}, "requires the analytics_* counters"},
		"negative counter": {func(e *HealthEvent) { e.MetricExportRetries = -1 }, "metric_export_retries"},
		"negative optional": {func(e *HealthEvent) {
			v := int64(-5)
			e.AnalyticsPendingBytes = &v
		}, "analytics_pending_bytes"},
		"stats_failed without counters": {func(e *HealthEvent) { e.AnalyticsStatsFailed = nil }, "analytics_stats_failed"},
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
