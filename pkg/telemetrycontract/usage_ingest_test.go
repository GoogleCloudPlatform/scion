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
	"math"
	"testing"
)

func TestUsageIngestWireRoundTrip(t *testing.T) {
	ev := validUsageEvent()
	ev.RunID = "run-42"
	ev.CacheWrite1h = int64p(1200)
	if err := ev.Validate(); err != nil {
		t.Fatalf("Validate with RunID + CacheWrite1h: %v", err)
	}
	if _, err := Marshal(ev); err != nil {
		t.Fatalf("Marshal with RunID + CacheWrite1h: %v", err)
	}

	contig := int64(7)
	req := UsageIngestRequest{
		Events:               []UsageEvent{*ev},
		RunID:                "run-42",
		ProducerEpoch:        "ep1",
		ProducerSeq:          7,
		LastContiguousSeq:    &contig,
		MissingRanges:        []UsageSeqRange{{Start: 2, End: 3}},
		LastSourceActivityAt: FormatTS(testObservedTS),
		SupportedSignals:     []string{"calls", "input", "output", "cache_read", "cache_write"},
		FinalState:           MeterFinalStateOpen,
	}
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	var gotReq UsageIngestRequest
	if err := json.Unmarshal(raw, &gotReq); err != nil {
		t.Fatalf("unmarshal request: %v", err)
	}
	if len(gotReq.Events) != 1 || gotReq.Events[0].RunID != "run-42" || *gotReq.Events[0].CacheWrite1h != 1200 {
		t.Fatalf("round-tripped event mismatch: %+v", gotReq.Events)
	}
	if gotReq.ProducerEpoch != "ep1" || gotReq.ProducerSeq != 7 || *gotReq.LastContiguousSeq != 7 {
		t.Fatalf("round-tripped heartbeat fields mismatch: %+v", gotReq)
	}

	resp := UsageIngestResponse{
		Accepted:   1,
		Duplicates: 0,
		Rejected:   []UsageIngestRejection{},
		Enforcement: &UsageEnforcement{
			Action: UsageEnforcementActionNone,
		},
	}
	respBytes, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	var gotResp UsageIngestResponse
	if err := json.Unmarshal(respBytes, &gotResp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if gotResp.Accepted != 1 || gotResp.Enforcement == nil || gotResp.Enforcement.Action != UsageEnforcementActionNone {
		t.Fatalf("round-tripped response mismatch: %+v", gotResp)
	}

	capResp := UsageCapabilitiesResponse{
		UsageIngestEnabled:   true,
		SuppressDirectOTel:   true,
		MaxBatchEvents:       UsageIngestMaxBatchEvents,
		MaxBatchBytes:        UsageIngestMaxBatchBytes,
		HeartbeatIntervalSec: DefaultUsageHeartbeatIntervalSec,
		DrainIntervalSec:     DefaultUsageDrainIntervalSec,
		HubID:                "hub-1",
		RunID:                "run-42",
	}
	capBytes, err := json.Marshal(capResp)
	if err != nil {
		t.Fatalf("marshal capabilities: %v", err)
	}
	var gotCap UsageCapabilitiesResponse
	if err := json.Unmarshal(capBytes, &gotCap); err != nil {
		t.Fatalf("unmarshal capabilities: %v", err)
	}
	if !gotCap.UsageIngestEnabled || gotCap.MaxBatchEvents != 500 || gotCap.MaxBatchBytes != 256*1024 {
		t.Fatalf("round-tripped capabilities mismatch: %+v", gotCap)
	}
}

func TestUsageEventSummableTokens(t *testing.T) {
	ev := &UsageEvent{
		Input:        int64p(8),
		Output:       int64p(5),
		CacheRead:    int64p(3),
		CacheWrite:   int64p(2),
		CacheWrite1h: int64p(1), // subset of cache_write: not added
		Reasoning:    int64p(2), // subset of output: not added
	}
	got, overflow := ev.SummableTokens()
	if overflow || got != 18 {
		t.Fatalf("SummableTokens() = (%d, %v), want (18, false)", got, overflow)
	}

	overflowEv := &UsageEvent{
		Input:  int64p(math.MaxInt64 - 1),
		Output: int64p(10),
	}
	if _, overflow := overflowEv.SummableTokens(); !overflow {
		t.Fatal("SummableTokens() expected overflow = true")
	}
}

func TestUsageIngestPaths(t *testing.T) {
	if got := UsageIngestAgentPath("agent-1"); got != "/api/v1/agents/agent-1/usage/events" {
		t.Errorf("UsageIngestAgentPath = %q", got)
	}
	if got := UsageCapabilitiesAgentPath("agent-1"); got != "/api/v1/agents/agent-1/usage/capabilities" {
		t.Errorf("UsageCapabilitiesAgentPath = %q", got)
	}
	if got := UsageIngestBrokerPath("broker-1", "agent-1"); got != "/api/v1/brokers/broker-1/agents/agent-1/usage/events" {
		t.Errorf("UsageIngestBrokerPath = %q", got)
	}
}

func TestUsageEventAdditionalUsageSources(t *testing.T) {
	for _, src := range []string{UsageSourceNative, UsageSourceHooks, UsageSourceSQLiteNative, UsageSourceFileTailer, UsageSourceLegacySessionMetrics} {
		ev := validUsageEvent()
		ev.UsageSource = src
		if err := ev.Validate(); err != nil {
			t.Errorf("Validate(%q) = %v, want nil", src, err)
		}
	}
}
