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

package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricpb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
)

func makeTestUsageEvent(t *testing.T, id string) *telemetrycontract.UsageEvent {
	t.Helper()
	now := time.Now().UTC()
	inTok := int64(25)
	return &telemetrycontract.UsageEvent{
		Envelope:            telemetrycontract.NewEnvelope(telemetrycontract.EventTypeUsage, id, "sciontool/agent/ag-hard", now, now),
		UsageSource:         telemetrycontract.UsageSourceNative,
		SourceKind:          "claude.api_request",
		SourceSchemaVersion: "1",
		SourceEventID:       "rid-" + id,
		SourceIdentity:      telemetrycontract.SourceIdentityRequestID,
		RunID:               "run-hard-1",
		Harness:             "claude",
		Model:               "claude-sonnet-4-6",
		ModelSource:         telemetrycontract.ModelSourceEvent,
		Status:              telemetrycontract.StatusSuccess,
		Calls:               1,
		Input:               &inTok,
		InputDerivation:     telemetrycontract.InputDerivationReported,
		TokenStatus:         telemetrycontract.TokenStatusReported,
		MalformedFields:     []string{},
	}
}

func appendReadyUsageEvents(t *testing.T, o *AnalyticsOutbox, hubID string, count int, prefix string) {
	t.Helper()
	if _, err := o.PinHubID(hubID); err != nil {
		t.Fatalf("PinHubID: %v", err)
	}
	for i := 0; i < count; i++ {
		ev := makeTestUsageEvent(t, prefix+"-"+string(rune('a'+i)))
		if _, err := o.AppendUsage(ev); err != nil {
			t.Fatalf("AppendUsage(%d): %v", i, err)
		}
	}
	if _, err := o.finalize(context.Background(), hubID); err != nil {
		t.Fatalf("finalize: %v", err)
	}
}

// --- Item 1: Dual-run shutdown budget split / Hub-first final drain ---

func TestDualRunShutdown_HubFirstNotStarvedBySlowGCP(t *testing.T) {
	gcpOutbox, err := OpenAnalyticsOutbox(filepath.Join(t.TempDir(), "gcp"))
	if err != nil {
		t.Fatalf("OpenAnalyticsOutbox(gcp): %v", err)
	}
	hubOutbox, err := OpenAnalyticsOutbox(filepath.Join(t.TempDir(), "hub"))
	if err != nil {
		t.Fatalf("OpenAnalyticsOutbox(hub): %v", err)
	}
	appendReadyUsageEvents(t, gcpOutbox, "hub-1", 2, "gcp")
	appendReadyUsageEvents(t, hubOutbox, "hub-1", 2, "hub")

	var mu sync.Mutex
	var hubEvents int
	var finalHeartbeatState string
	var gcpCalled atomic.Bool

	sink := NewHubUsageSink(hubOutbox, HubUsageSinkConfig{
		Post: func(_ context.Context, req telemetrycontract.UsageIngestRequest) (*telemetrycontract.UsageIngestResponse, error) {
			mu.Lock()
			defer mu.Unlock()
			hubEvents += len(req.Events)
			if req.FinalState != "" {
				finalHeartbeatState = req.FinalState
			}
			return &telemetrycontract.UsageIngestResponse{Accepted: len(req.Events)}, nil
		},
		RunID:             "run-hard-1",
		HubID:             "hub-1",
		DrainInterval:     time.Hour,
		HeartbeatInterval: time.Hour,
	})

	// Slow/hanging GCP send blocks until shutdown ctx expires.
	gcpSend := func(ctx context.Context, _ []byte) error {
		gcpCalled.Store(true)
		<-ctx.Done()
		return ctx.Err()
	}

	d := NewDualAnalyticsDrainer(gcpOutbox, gcpSend, sink, func(context.Context) (string, error) { return "hub-1", nil })
	stopCtx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	d.Stop(stopCtx)

	mu.Lock()
	defer mu.Unlock()
	if hubEvents != 2 {
		t.Fatalf("hubEvents drained = %d, want 2 (slow GCP must not starve Hub final drain)", hubEvents)
	}
	if finalHeartbeatState != telemetrycontract.MeterFinalStateFinal {
		t.Fatalf("finalHeartbeatState = %q, want %q", finalHeartbeatState, telemetrycontract.MeterFinalStateFinal)
	}
	if !gcpCalled.Load() {
		t.Fatal("expected GCP send to still be attempted after Hub finished")
	}
}

func TestDualRunShutdown_SlowHubBoundedLeavesShareForGCP(t *testing.T) {
	gcpOutbox, err := OpenAnalyticsOutbox(filepath.Join(t.TempDir(), "gcp"))
	if err != nil {
		t.Fatalf("OpenAnalyticsOutbox(gcp): %v", err)
	}
	hubOutbox, err := OpenAnalyticsOutbox(filepath.Join(t.TempDir(), "hub"))
	if err != nil {
		t.Fatalf("OpenAnalyticsOutbox(hub): %v", err)
	}
	appendReadyUsageEvents(t, gcpOutbox, "hub-1", 2, "gcp")
	appendReadyUsageEvents(t, hubOutbox, "hub-1", 2, "hub")

	var hubBatchCanceled atomic.Bool
	var finalHeartbeatState atomic.Value
	var gcpDrained atomic.Int32

	sink := NewHubUsageSink(hubOutbox, HubUsageSinkConfig{
		Post: func(ctx context.Context, req telemetrycontract.UsageIngestRequest) (*telemetrycontract.UsageIngestResponse, error) {
			if len(req.Events) > 0 {
				// Hang the batch drain until its sub-budget expires.
				<-ctx.Done()
				hubBatchCanceled.Store(true)
				return nil, ctx.Err()
			}
			// Terminal heartbeat should still receive its reserved slice!
			finalHeartbeatState.Store(req.FinalState)
			return &telemetrycontract.UsageIngestResponse{}, nil
		},
		RunID:             "run-hard-1",
		HubID:             "hub-1",
		DrainInterval:     time.Hour,
		HeartbeatInterval: time.Hour,
	})

	gcpSend := func(_ context.Context, _ []byte) error {
		gcpDrained.Add(1)
		return nil
	}

	d := NewDualAnalyticsDrainer(gcpOutbox, gcpSend, sink, func(context.Context) (string, error) { return "hub-1", nil })
	stopCtx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()
	d.Stop(stopCtx)

	if !hubBatchCanceled.Load() {
		t.Fatal("expected hanging Hub batch drain to be bounded by its share")
	}
	if got, _ := finalHeartbeatState.Load().(string); got != telemetrycontract.MeterFinalStateIncomplete {
		t.Fatalf("finalHeartbeatState = %q, want %q (heartbeat reservation must survive slow batch drain)", got, telemetrycontract.MeterFinalStateIncomplete)
	}
	if gcpDrained.Load() != 2 {
		t.Fatalf("gcpDrained = %d, want 2 (GCP must keep its 30%% share when Hub stalls)", gcpDrained.Load())
	}
}

func TestDualRunShutdown_FastHubUnusedBudgetFlowsToGCP(t *testing.T) {
	gcpOutbox, err := OpenAnalyticsOutbox(filepath.Join(t.TempDir(), "gcp"))
	if err != nil {
		t.Fatalf("OpenAnalyticsOutbox(gcp): %v", err)
	}
	hubOutbox, err := OpenAnalyticsOutbox(filepath.Join(t.TempDir(), "hub"))
	if err != nil {
		t.Fatalf("OpenAnalyticsOutbox(hub): %v", err)
	}
	appendReadyUsageEvents(t, gcpOutbox, "hub-1", 1, "gcp")
	appendReadyUsageEvents(t, hubOutbox, "hub-1", 1, "hub")

	sink := NewHubUsageSink(hubOutbox, HubUsageSinkConfig{
		Post: func(_ context.Context, req telemetrycontract.UsageIngestRequest) (*telemetrycontract.UsageIngestResponse, error) {
			return &telemetrycontract.UsageIngestResponse{Accepted: len(req.Events)}, nil
		},
		RunID:             "run-hard-1",
		HubID:             "hub-1",
		DrainInterval:     time.Hour,
		HeartbeatInterval: time.Hour,
	})

	var gcpDrained atomic.Int32
	// GCP takes 250ms, which exceeds 30% of 500ms (150ms) and only succeeds
	// because fast Hub (~5ms) releases its unused budget back to GCP.
	gcpSend := func(ctx context.Context, _ []byte) error {
		timer := time.NewTimer(250 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-timer.C:
			gcpDrained.Add(1)
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	d := NewDualAnalyticsDrainer(gcpOutbox, gcpSend, sink, func(context.Context) (string, error) { return "hub-1", nil })
	stopCtx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	d.Stop(stopCtx)

	if gcpDrained.Load() != 1 {
		t.Fatalf("gcpDrained = %d, want 1 (unused Hub budget should flow to GCP)", gcpDrained.Load())
	}
}

// R1: When the Hub hangs on BOTH batch drain AND terminal heartbeat, the 70%
// dual-run split must cap the Hub final pass so GCP still gets its 30% share
// and drains before the outer shutdown deadline expires. Removing the 70% cap
// gives the Hub 100% of ctx and causes gcpDrained == 0.
func TestDualRunShutdown_HungHubOnBothBatchAndHeartbeatLeavesShareForGCP(t *testing.T) {
	gcpOutbox, err := OpenAnalyticsOutbox(filepath.Join(t.TempDir(), "gcp"))
	if err != nil {
		t.Fatalf("OpenAnalyticsOutbox(gcp): %v", err)
	}
	hubOutbox, err := OpenAnalyticsOutbox(filepath.Join(t.TempDir(), "hub"))
	if err != nil {
		t.Fatalf("OpenAnalyticsOutbox(hub): %v", err)
	}
	appendReadyUsageEvents(t, gcpOutbox, "hub-1", 2, "gcp")
	appendReadyUsageEvents(t, hubOutbox, "hub-1", 2, "hub")

	var hubBatchCanceled atomic.Bool
	var hubHeartbeatCanceled atomic.Bool
	var gcpDrained atomic.Int32
	var firstGCPAt atomic.Int64

	sink := NewHubUsageSink(hubOutbox, HubUsageSinkConfig{
		Post: func(ctx context.Context, req telemetrycontract.UsageIngestRequest) (*telemetrycontract.UsageIngestResponse, error) {
			// Hang unconditionally on both batch drain and terminal heartbeat.
			<-ctx.Done()
			if len(req.Events) > 0 {
				hubBatchCanceled.Store(true)
			} else {
				hubHeartbeatCanceled.Store(true)
			}
			return nil, ctx.Err()
		},
		RunID:             "run-hard-1",
		HubID:             "hub-1",
		DrainInterval:     time.Hour,
		HeartbeatInterval: time.Hour,
	})

	gcpSend := func(ctx context.Context, _ []byte) error {
		firstGCPAt.CompareAndSwap(0, time.Now().UnixNano())
		if ctx.Err() != nil {
			return ctx.Err()
		}
		gcpDrained.Add(1)
		return nil
	}

	d := NewDualAnalyticsDrainer(gcpOutbox, gcpSend, sink, func(context.Context) (string, error) { return "hub-1", nil })
	stopBudget := 600 * time.Millisecond
	start := time.Now()
	stopCtx, cancel := context.WithTimeout(context.Background(), stopBudget)
	defer cancel()
	d.Stop(stopCtx)

	if !hubBatchCanceled.Load() {
		t.Fatal("expected hanging Hub batch drain to be canceled by its sub-budget")
	}
	if !hubHeartbeatCanceled.Load() {
		t.Fatal("expected hanging Hub terminal heartbeat to be canceled by the 70% Hub budget")
	}
	if gcpDrained.Load() != 2 {
		t.Fatalf("gcpDrained = %d, want 2 (70/30 dual-run split must leave 30%% share for GCP even when both Hub batch and heartbeat hang)", gcpDrained.Load())
	}
	// Verify Hub handed off to GCP well before the outer stopBudget (70% cap).
	gcpStartNs := firstGCPAt.Load()
	if gcpStartNs == 0 {
		t.Fatal("expected GCP send to be invoked")
	}
	hubElapsed := time.Unix(0, gcpStartNs).Sub(start)
	if hubElapsed >= stopBudget*9/10 {
		t.Fatalf("Hub consumed %v of %v stopBudget before GCP ran; expected bounded ~70%% share", hubElapsed, stopBudget)
	}
}

// --- Item 2: Surface hub_analytics health in snapshot and diagnostics ---

func TestHubAnalyticsHealth_StatesBacklogDeadLettersAndDiagnostics(t *testing.T) {
	t.Setenv("SCION_AGENT_ID", "ag-health-hub")
	t.Setenv("SCION_PROJECT_ID", "proj-health-hub")
	t.Setenv("SCION_HARNESS", "claude")
	t.Setenv("SCION_USAGE_SOURCE", "native")

	// Capture sciontool log output to verify logDeliverySnapshotDisk emits hub_analytics=.
	logFile := filepath.Join(t.TempDir(), "agent.log")
	log.SetLogPath(logFile)
	t.Cleanup(func() { log.SetLogPath("/tmp/agent.log") })

	// 1. GCP-only (no Hub sink) omits hub_analytics completely.
	t.Run("no hub sink omits hub_analytics", func(t *testing.T) {
		p := NewWithConfig(&Config{Enabled: true, CloudEnabled: false, GRPCPort: 0, HTTPPort: 0})
		p.SetAnalyticsOutbox(t.TempDir(), -1, -1)
		if err := p.Start(context.Background()); err != nil {
			t.Fatalf("Start: %v", err)
		}
		defer func() {
			stopCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_ = p.Stop(stopCtx)
		}()
		if _, ok := p.HubAnalyticsDiagnostics(); ok {
			t.Fatal("HubAnalyticsDiagnostics() = ok:true, want false when Hub sink is not configured")
		}
		in := p.healthSnapshotInputs(telemetrycontract.HealthTriggerStart)
		in.PipelineRunID = "run-no-hub"
		ev, err := buildHealthEvent(in)
		if err != nil {
			t.Fatalf("buildHealthEvent: %v", err)
		}
		ev.HubID = "hub-1"
		if ev.HubAnalytics != nil {
			t.Fatalf("expected ev.HubAnalytics == nil, got %+v", ev.HubAnalytics)
		}
		raw, err := telemetrycontract.Marshal(ev)
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		if strings.Contains(string(raw), "hub_analytics") {
			t.Fatalf("expected hub_analytics omitted from JSON, got %s", raw)
		}
		if got := p.hubAnalyticsSnapshotLine(false); got != "disabled" {
			t.Fatalf("hubAnalyticsSnapshotLine(false) = %q, want disabled", got)
		}
	})

	// 2. Active -> dead letter + last_drain_at -> degraded + backlog -> revoked.
	t.Run("lifecycle states backlog dead letters and diagnostics", func(t *testing.T) {
		var mode atomic.Int32 // 0=ok+reject1, 1=transient-fail, 2=404-revoke
		p := NewWithConfig(&Config{Enabled: true, CloudEnabled: false, GRPCPort: 0, HTTPPort: 0})
		p.SetAnalyticsOutbox(t.TempDir(), -1, -1)
		p.SetHubUsageSink(HubUsageSinkConfig{
			Post: func(_ context.Context, req telemetrycontract.UsageIngestRequest) (*telemetrycontract.UsageIngestResponse, error) {
				switch mode.Load() {
				case 0:
					resp := &telemetrycontract.UsageIngestResponse{Accepted: len(req.Events)}
					if len(req.Events) >= 2 {
						resp.Accepted = len(req.Events) - 1
						resp.Rejected = []telemetrycontract.UsageIngestRejection{{
							Index:   1,
							EventID: req.Events[1].EventID,
							Code:    "invalid_event",
							Reason:  "bad test event",
						}}
					}
					return resp, nil
				case 1:
					return nil, errors.New("hub temporary 503")
				default:
					return nil, ErrUsageIngestRevoked
				}
			},
			RunID:             "run-hub-health",
			HubID:             "hub-health-1",
			DrainInterval:     time.Hour,
			HeartbeatInterval: time.Hour,
		})

		ctx := context.Background()
		if err := p.Start(ctx); err != nil {
			t.Fatalf("Start: %v", err)
		}
		defer func() {
			stopCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			_ = p.Stop(stopCtx)
		}()

		// Stop background loop so we drive RunOnce deterministically.
		p.analytics.drainer.Stop(ctx)

		// Initially active with zero backlog and no drain yet.
		ha, ok := p.HubAnalyticsDiagnostics()
		if !ok {
			t.Fatal("HubAnalyticsDiagnostics() = false, want true")
		}
		if ha.State != telemetrycontract.HubAnalyticsStateActive {
			t.Fatalf("initial state = %q, want active", ha.State)
		}
		if ha.LastDrainAt != "" {
			t.Fatalf("initial LastDrainAt = %q, want empty", ha.LastDrainAt)
		}
		if ha.PendingBytes == nil || *ha.PendingBytes != 0 {
			t.Fatalf("initial PendingBytes = %v, want 0", ha.PendingBytes)
		}

		// Record 2 events and drain: 1 accepted, 1 rejected -> dead letter.
		for i := 0; i < 2; i++ {
			if _, err := p.analytics.native.Record(UsageObservation{
				SourceKind:      "claude.api_request",
				Identity:        telemetrycontract.SourceIdentityRequestID,
				SourceEventID:   "req_aaaaaaaaaaaaaaaa" + string(rune('1'+i)),
				Model:           "claude-sonnet-4-6",
				Status:          telemetrycontract.StatusSuccess,
				Calls:           1,
				Input:           int64Ptr(10),
				InputDerivation: telemetrycontract.InputDerivationReported,
				TokenStatus:     telemetrycontract.TokenStatusReported,
			}); err != nil {
				t.Fatalf("Record(%d): %v", i, err)
			}
		}
		p.analytics.drainer.RunOnce(ctx)

		ha, ok = p.HubAnalyticsDiagnostics()
		if !ok {
			t.Fatal("HubAnalyticsDiagnostics() = false after drain")
		}
		if ha.State != telemetrycontract.HubAnalyticsStateActive {
			t.Fatalf("state after drain = %q, want active", ha.State)
		}
		if ha.LastDrainAt == "" {
			t.Fatal("expected LastDrainAt populated after successful non-empty drain")
		}
		if ha.Delivered != 1 {
			t.Fatalf("Delivered = %d, want 1", ha.Delivered)
		}
		if ha.DeadLetters != 1 || ha.DeadLettersTotal == nil || *ha.DeadLettersTotal != 1 {
			t.Fatalf("dead letters = (%d, %v), want (1, 1)", ha.DeadLetters, ha.DeadLettersTotal)
		}

		// HealthEvent built now must validate, include hub_analytics, and set loss_indicated=true.
		inPeriodic := p.healthSnapshotInputs(telemetrycontract.HealthTriggerPeriodic)
		inPeriodic.PipelineRunID = "run-hub-health"
		ev, err := buildHealthEvent(inPeriodic)
		if err != nil {
			t.Fatalf("buildHealthEvent: %v", err)
		}
		ev.HubID = "hub-health-1"
		if ev.HubAnalytics == nil || ev.HubAnalytics.DeadLetters != 1 || ev.HubAnalytics.LastDrainAt == "" {
			t.Fatalf("ev.HubAnalytics = %+v, want dead_letters=1 and last_drain_at set", ev.HubAnalytics)
		}
		if !ev.LossIndicated {
			t.Fatal("expected LossIndicated=true when Hub has dead letters")
		}
		if _, err := telemetrycontract.Marshal(ev); err != nil {
			t.Fatalf("Marshal(ev): %v", err)
		}

		// Record another event and fail transiently -> degraded with backlog.
		mode.Store(1)
		if _, err := p.analytics.native.Record(UsageObservation{
			SourceKind:      "claude.api_request",
			Identity:        telemetrycontract.SourceIdentityRequestID,
			SourceEventID:   "req_bbbbbbbbbbbbbbbb1",
			Model:           "claude-sonnet-4-6",
			Status:          telemetrycontract.StatusSuccess,
			Calls:           1,
			Input:           int64Ptr(15),
			InputDerivation: telemetrycontract.InputDerivationReported,
			TokenStatus:     telemetrycontract.TokenStatusReported,
		}); err != nil {
			t.Fatalf("Record(degraded): %v", err)
		}
		p.analytics.drainer.RunOnce(ctx)

		ha, ok = p.HubAnalyticsDiagnostics()
		if !ok {
			t.Fatal("HubAnalyticsDiagnostics() = false when degraded")
		}
		if ha.State != telemetrycontract.HubAnalyticsStateDegraded {
			t.Fatalf("state after transient failure = %q, want degraded", ha.State)
		}
		if ha.RetryBackoffMs <= 0 {
			t.Fatalf("RetryBackoffMs = %d, want > 0", ha.RetryBackoffMs)
		}
		if ha.PendingBytes == nil || *ha.PendingBytes <= 0 {
			t.Fatalf("PendingBytes = %v, want > 0", ha.PendingBytes)
		}
		if ha.SendFailures == 0 {
			t.Fatalf("SendFailures = %d, want > 0", ha.SendFailures)
		}

		// Final health snapshot with pending Hub backlog must report not_drained.
		inFinal := p.healthSnapshotInputs(telemetrycontract.HealthTriggerFinal)
		inFinal.PipelineRunID = "run-hub-health"
		finalEv, err := buildHealthEvent(inFinal)
		if err != nil {
			t.Fatalf("buildHealthEvent(final): %v", err)
		}
		finalEv.HubID = "hub-health-1"
		if finalEv.FinalFlushOutcome != telemetrycontract.FinalFlushNotDrained {
			t.Fatalf("FinalFlushOutcome = %q, want not_drained", finalEv.FinalFlushOutcome)
		}
		if _, err := telemetrycontract.Marshal(finalEv); err != nil {
			t.Fatalf("Marshal(finalEv): %v", err)
		}

		// Verify diagnostic log snapshots surface hub_analytics (both inline and disk).
		p.logDeliverySnapshotDisk(true, false)
		p.logDeliverySnapshotDisk(true, true)
		logBytes, err := os.ReadFile(logFile)
		if err != nil {
			t.Fatalf("read log file: %v", err)
		}
		logText := string(logBytes)
		if !strings.Contains(logText, "hub_analytics={State:degraded") || !strings.Contains(logText, "disk=not_read") {
			t.Fatalf("expected inline hub_analytics snapshot in log, got:\n%s", logText)
		}
		if !strings.Contains(logText, "DeadLettersTotal:1") {
			t.Fatalf("expected disk hub_analytics snapshot with DeadLettersTotal:1 in log, got:\n%s", logText)
		}

		// Now revoke (404) -> state=revoked.
		mode.Store(2)
		p.analytics.drainer.mu.Lock()
		p.analytics.drainer.hubRetryAfter = time.Time{}
		p.analytics.drainer.mu.Unlock()
		p.analytics.drainer.RunOnce(ctx)

		ha, ok = p.HubAnalyticsDiagnostics()
		if !ok {
			t.Fatal("HubAnalyticsDiagnostics() = false when revoked")
		}
		if ha.State != telemetrycontract.HubAnalyticsStateRevoked {
			t.Fatalf("state after 404 = %q, want revoked", ha.State)
		}
	})
}

// --- Item 3: Flush-then-flip at suppression switches & deadlock proof ---

func sumMetricValues(rms []*metricpb.ResourceMetrics, name string) (points int, total int64) {
	for _, rm := range rms {
		for _, sm := range rm.GetScopeMetrics() {
			for _, m := range sm.GetMetrics() {
				if m.GetName() != name {
					continue
				}
				for _, dp := range m.GetSum().GetDataPoints() {
					points++
					total += dp.GetAsInt()
				}
			}
		}
	}
	return points, total
}

func TestPipeline_FlushThenFlip_SuppressFalseToTrueAndInverse(t *testing.T) {
	t.Run("false to true exports pre-flip increment directly once and strips post-flip", func(t *testing.T) {
		t.Setenv("SCION_AGENT_ID", "ag-ftf-1")
		t.Setenv("SCION_PROJECT_ID", "proj-ftf-1")
		t.Setenv("SCION_HARNESS", "claude")
		t.Setenv("SCION_USAGE_SOURCE", "native")

		addr, backend := startCaptureOTLPBackend(t)
		p := NewWithConfig(&Config{
			Enabled:      true,
			CloudEnabled: true,
			Endpoint:     addr,
			Protocol:     "grpc",
			Insecure:     true,
			GRPCPort:     0,
			HTTPPort:     0,
		})
		p.SetAnalyticsOutbox(t.TempDir(), -1, -1)
		var hubSuppress atomic.Bool
		p.SetHubUsageSink(HubUsageSinkConfig{
			Post: func(_ context.Context, req telemetrycontract.UsageIngestRequest) (*telemetrycontract.UsageIngestResponse, error) {
				return &telemetrycontract.UsageIngestResponse{
					Accepted:           len(req.Events),
					SuppressDirectOTel: hubSuppress.Load(),
				}, nil
			},
			RunID:              "run-ftf-1",
			HubID:              "hub-ftf-1",
			SuppressDirectOTel: false,
			DrainInterval:      time.Hour,
			HeartbeatInterval:  time.Hour,
		})

		ctx := context.Background()
		if err := p.Start(ctx); err != nil {
			t.Fatalf("Start: %v", err)
		}
		p.analytics.drainer.Stop(ctx)
		if p.SuppressDirectOTel() {
			t.Fatal("expected initial SuppressDirectOTel=false")
		}

		deriver := p.usageDeriver.Load()
		if deriver == nil {
			t.Fatal("expected active usage deriver")
		}

		// Record a delta directly onto the deriver's OTel counters WITHOUT
		// triggering ForceFlush so it sits pending before the periodic reader
		// (60s) fires.
		deriver.record(ctx, usageIncrement{
			Model:  "claude-sonnet-4-6",
			Status: telemetrycontract.StatusSuccess,
			Calls:  1,
			Tokens: map[string]int64{telemetrycontract.TokenTypeInput: 42},
		})

		// Flip false -> true via Hub heartbeat revalidation before periodic
		// reader fires. Flush-then-flip must push the pending 1 call / 42 tokens
		// through handleMetrics under the OLD suppress=false decision.
		hubSuppress.Store(true)
		if err := p.analytics.drainer.hubSink.SendHeartbeat(ctx, ""); err != nil {
			t.Fatalf("SendHeartbeat: %v", err)
		}
		if !p.SuppressDirectOTel() {
			t.Fatal("expected SuppressDirectOTel=true after heartbeat flip")
		}

		// Now derive a second increment under suppress=true and flush it:
		// it must be stripped by handleMetrics.
		deriver.record(ctx, usageIncrement{
			Model:  "claude-sonnet-4-6",
			Status: telemetrycontract.StatusSuccess,
			Calls:  1,
			Tokens: map[string]int64{telemetrycontract.TokenTypeInput: 99},
		})
		if err := deriver.ForceFlush(ctx); err != nil {
			t.Fatalf("ForceFlush post-flip: %v", err)
		}

		stopCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if err := p.Stop(stopCtx); err != nil {
			t.Fatalf("Stop: %v", err)
		}

		backend.mu.Lock()
		defer backend.mu.Unlock()
		callPts, callSum := sumMetricValues(backend.metrics, telemetrycontract.MetricAPICalls)
		tokPts, tokSum := sumMetricValues(backend.metrics, telemetrycontract.MetricUsageTokens)
		if callPts != 1 || callSum != 1 {
			t.Fatalf("gen_ai.api.calls exported = (%d points, sum %d), want (1, 1)", callPts, callSum)
		}
		if tokPts != 1 || tokSum != 42 {
			t.Fatalf("scion.usage.tokens exported = (%d points, sum %d), want (1, 42)", tokPts, tokSum)
		}
	})

	t.Run("true to false strips pre-flip increment and exports post-flip directly", func(t *testing.T) {
		t.Setenv("SCION_AGENT_ID", "ag-ftf-2")
		t.Setenv("SCION_PROJECT_ID", "proj-ftf-2")
		t.Setenv("SCION_HARNESS", "claude")
		t.Setenv("SCION_USAGE_SOURCE", "native")

		addr, backend := startCaptureOTLPBackend(t)
		p := NewWithConfig(&Config{
			Enabled:      true,
			CloudEnabled: true,
			Endpoint:     addr,
			Protocol:     "grpc",
			Insecure:     true,
			GRPCPort:     0,
			HTTPPort:     0,
		})
		p.SetAnalyticsOutbox(t.TempDir(), -1, -1)
		var hubSuppress atomic.Bool
		hubSuppress.Store(true)
		p.SetHubUsageSink(HubUsageSinkConfig{
			Post: func(_ context.Context, req telemetrycontract.UsageIngestRequest) (*telemetrycontract.UsageIngestResponse, error) {
				return &telemetrycontract.UsageIngestResponse{
					Accepted:           len(req.Events),
					SuppressDirectOTel: hubSuppress.Load(),
				}, nil
			},
			RunID:              "run-ftf-2",
			HubID:              "hub-ftf-2",
			SuppressDirectOTel: true,
			DrainInterval:      time.Hour,
			HeartbeatInterval:  time.Hour,
		})

		ctx := context.Background()
		if err := p.Start(ctx); err != nil {
			t.Fatalf("Start: %v", err)
		}
		p.analytics.drainer.Stop(ctx)
		if !p.SuppressDirectOTel() {
			t.Fatal("expected initial SuppressDirectOTel=true")
		}

		deriver := p.usageDeriver.Load()
		if deriver == nil {
			t.Fatal("expected active usage deriver")
		}

		// Record a pending delta under suppress=true before periodic reader fires.
		deriver.record(ctx, usageIncrement{
			Model:  "claude-sonnet-4-6",
			Status: telemetrycontract.StatusSuccess,
			Calls:  1,
			Tokens: map[string]int64{telemetrycontract.TokenTypeInput: 77},
		})

		// Flip true -> false via Hub heartbeat revalidation. Flush-then-flip
		// must flush and strip the 77 tokens under the OLD suppress=true decision.
		hubSuppress.Store(false)
		if err := p.analytics.drainer.hubSink.SendHeartbeat(ctx, ""); err != nil {
			t.Fatalf("SendHeartbeat: %v", err)
		}
		if p.SuppressDirectOTel() {
			t.Fatal("expected SuppressDirectOTel=false after heartbeat flip")
		}

		// Derive a second increment under suppress=false and flush it:
		// ONLY this post-flip increment (1 call, 19 tokens) must be exported.
		deriver.record(ctx, usageIncrement{
			Model:  "claude-sonnet-4-6",
			Status: telemetrycontract.StatusSuccess,
			Calls:  1,
			Tokens: map[string]int64{telemetrycontract.TokenTypeInput: 19},
		})
		if err := deriver.ForceFlush(ctx); err != nil {
			t.Fatalf("ForceFlush post-flip: %v", err)
		}

		stopCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if err := p.Stop(stopCtx); err != nil {
			t.Fatalf("Stop: %v", err)
		}

		backend.mu.Lock()
		defer backend.mu.Unlock()
		callPts, callSum := sumMetricValues(backend.metrics, telemetrycontract.MetricAPICalls)
		tokPts, tokSum := sumMetricValues(backend.metrics, telemetrycontract.MetricUsageTokens)
		if callPts != 1 || callSum != 1 {
			t.Fatalf("gen_ai.api.calls exported = (%d points, sum %d), want (1, 1)", callPts, callSum)
		}
		if tokPts != 1 || tokSum != 19 {
			t.Fatalf("scion.usage.tokens exported = (%d points, sum %d), want (1, 19)", tokPts, tokSum)
		}
	})
}

// TestPipeline_FlushThenFlip_NoDeadlock proves that holding suppressMu.Lock()
// across UsageDeriver.ForceFlush (which round-trips through the loopback
// receiver into Pipeline.handleMetrics) cannot deadlock under concurrent
// log/metric derivation, including Copilot's metric-sourced rule where
// handleMetrics invokes ProcessResourceMetrics.
func TestPipeline_FlushThenFlip_NoDeadlock(t *testing.T) {
	for _, harness := range []string{"claude", "copilot"} {
		t.Run(harness, func(t *testing.T) {
			t.Setenv("SCION_AGENT_ID", "ag-dl-"+harness)
			t.Setenv("SCION_PROJECT_ID", "proj-dl-"+harness)
			t.Setenv("SCION_HARNESS", harness)
			t.Setenv("SCION_USAGE_SOURCE", "native")

			addr, _ := startCaptureOTLPBackend(t)
			p := NewWithConfig(&Config{
				Enabled:      true,
				CloudEnabled: true,
				Endpoint:     addr,
				Protocol:     "grpc",
				Insecure:     true,
				GRPCPort:     0,
				HTTPPort:     0,
			})
			p.SetAnalyticsOutbox(t.TempDir(), -1, -1)
			ctx := context.Background()
			if err := p.Start(ctx); err != nil {
				t.Fatalf("Start: %v", err)
			}
			defer func() {
				stopCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
				defer cancel()
				_ = p.Stop(stopCtx)
			}()

			done := make(chan struct{})
			go func() {
				defer close(done)
				var wg sync.WaitGroup
				wg.Add(2)
				// Goroutine 1: rapid suppression flips (each takes suppressMu.Lock
				// and calls UsageDeriver.ForceFlush over loopback -> handleMetrics).
				go func() {
					defer wg.Done()
					for i := 0; i < 20; i++ {
						p.setSuppressDirectOTel(i%2 == 0)
					}
				}()
				// Goroutine 2: concurrent log/metric intake triggering UsageDeriver.
				go func() {
					defer wg.Done()
					for i := 0; i < 20; i++ {
						if harness == "claude" {
							_ = p.handleLogs(ctx, []*logspb.ResourceLogs{{
								ScopeLogs: []*logspb.ScopeLogs{{
									Scope: &commonpb.InstrumentationScope{Name: claudeUsageScope},
									LogRecords: []*logspb.LogRecord{{
										TimeUnixNano: uint64(time.Now().UnixNano()) + uint64(i),
										Attributes: []*commonpb.KeyValue{
											metricStringLabel("event.name", "api_request"),
											metricStringLabel("model", "claude-sonnet-4-6"),
											{Key: "input_tokens", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: 10}}},
										},
									}},
								}},
							}})
						} else {
							now := uint64(time.Now().UnixNano()) + uint64(i)
							_ = p.handleMetrics(ctx, []*metricpb.ResourceMetrics{{
								Resource: &resourcepb.Resource{
									Attributes: []*commonpb.KeyValue{
										metricStringLabel("service.name", "github-copilot"),
									},
								},
								ScopeMetrics: []*metricpb.ScopeMetrics{{
									Scope: &commonpb.InstrumentationScope{Name: "github.copilot"},
									Metrics: []*metricpb.Metric{{
										Name: "github.copilot.api.calls",
										Data: &metricpb.Metric_Sum{
											Sum: &metricpb.Sum{
												AggregationTemporality: metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA,
												IsMonotonic:            true,
												DataPoints: []*metricpb.NumberDataPoint{{
													StartTimeUnixNano: now - 1000,
													TimeUnixNano:      now,
													Value:             &metricpb.NumberDataPoint_AsInt{AsInt: 1},
													Attributes:        []*commonpb.KeyValue{metricStringLabel("model", "gpt-4.1")},
												}},
											},
										},
									}},
								}},
							}})
						}
					}
				}()
				wg.Wait()
			}()

			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatalf("deadlock detected during concurrent %s derivation and flush-then-flip suppression switches", harness)
			}
		})
	}
}

func TestHealthEvent_HubAnalyticsJSONRoundTrip(t *testing.T) {
	ev := &telemetrycontract.HealthEvent{
		Envelope:                telemetrycontract.NewEnvelope(telemetrycontract.EventTypeHealth, "h1-0123456789abcdef0123456789abcdef", "sciontool/agent/a", time.Now().UTC(), time.Now().UTC()),
		PipelineRunID:           "run-json",
		HealthSeq:               1,
		Trigger:                 telemetrycontract.HealthTriggerStart,
		Harness:                 "claude",
		UsageSourceOrigin:       telemetrycontract.UsageSourceOriginUnset,
		UsageCollection:         telemetrycontract.UsageCollectionDisabled,
		UsageDisabledReason:     telemetrycontract.UsageDisabledSourceUnset,
		UsageCapabilities:       []string{},
		UsageRulesVersion:       "1",
		AnalyticsState:          telemetrycontract.AnalyticsStateDisabled,
		AnalyticsDisabledReason: telemetrycontract.AnalyticsDisabledNoGCPExporter,
		DeliveryState:           "disabled",
		HubAnalytics: &telemetrycontract.HealthHubAnalytics{
			State:       telemetrycontract.HubAnalyticsStateDegraded,
			StatsFailed: true,
		},
	}
	ev.HubID, ev.AgentID = "hub-1", "a"
	b, err := telemetrycontract.Marshal(ev)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	ha, ok := raw["hub_analytics"].(map[string]any)
	if !ok {
		t.Fatalf("expected nested hub_analytics object in JSON, got %s", b)
	}
	if ha["state"] != "degraded" || ha["stats_failed"] != true {
		t.Fatalf("unexpected hub_analytics payload: %v", ha)
	}
}

// O1: Deterministic test pinning UsageDeriver's withSuppressGuard read lock.
// Blocks inside recordAnalytics (via UsageAnalytics's onRecord hook), starts
// setSuppressDirectOTel(true), and asserts the flip cannot complete until the
// guarded recordAnalytics+record section returns. Removing withSuppressGuard
// allows setSuppressDirectOTel(true) to finish immediately while recordAnalytics
// is still blocked, failing the test.
func TestPipeline_SuppressGuardSerializesRecordAndFlip(t *testing.T) {
	t.Setenv("SCION_AGENT_ID", "ag-guard-1")
	t.Setenv("SCION_PROJECT_ID", "proj-guard-1")
	t.Setenv("SCION_HARNESS", "claude")
	t.Setenv("SCION_USAGE_SOURCE", "native")

	addr, backend := startCaptureOTLPBackend(t)
	p := NewWithConfig(&Config{
		Enabled:      true,
		CloudEnabled: true,
		Endpoint:     addr,
		Protocol:     "grpc",
		Insecure:     true,
		GRPCPort:     0,
		HTTPPort:     0,
	})
	p.SetAnalyticsOutbox(t.TempDir(), -1, -1)
	p.SetHubUsageSink(HubUsageSinkConfig{
		Post: func(_ context.Context, req telemetrycontract.UsageIngestRequest) (*telemetrycontract.UsageIngestResponse, error) {
			return &telemetrycontract.UsageIngestResponse{Accepted: len(req.Events)}, nil
		},
		RunID:              "run-guard-1",
		HubID:              "hub-guard-1",
		SuppressDirectOTel: false,
		DrainInterval:      time.Hour,
		HeartbeatInterval:  time.Hour,
	})

	ctx := context.Background()
	if err := p.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	p.analytics.drainer.Stop(ctx)

	enteredRecord := make(chan struct{})
	releaseRecord := make(chan struct{})
	var enteredOnce sync.Once
	p.analytics.native.setRunAndActivity("run-guard-1", func(time.Time) {
		enteredOnce.Do(func() {
			close(enteredRecord)
			<-releaseRecord
		})
	})

	logDone := make(chan error, 1)
	go func() {
		logDone <- p.handleLogs(ctx, []*logspb.ResourceLogs{{
			ScopeLogs: []*logspb.ScopeLogs{{
				Scope: &commonpb.InstrumentationScope{Name: claudeUsageScope},
				LogRecords: []*logspb.LogRecord{{
					TimeUnixNano: uint64(time.Now().UnixNano()),
					Attributes: []*commonpb.KeyValue{
						metricStringLabel("event.name", "api_request"),
						metricStringLabel("model", "claude-sonnet-4-6"),
						{Key: "input_tokens", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: 33}}},
					},
				}},
			}},
		}})
	}()

	select {
	case <-enteredRecord:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for handleLogs to enter recordAnalytics")
	}

	flipDone := make(chan struct{})
	go func() {
		p.setSuppressDirectOTel(true)
		close(flipDone)
	}()

	// While recordAnalytics+record holds suppressMu.RLock(), setSuppressDirectOTel
	// must wait and cannot complete.
	select {
	case <-flipDone:
		close(releaseRecord)
		t.Fatal("setSuppressDirectOTel completed while recordAnalytics+record was still in flight; suppressGuard is not serializing them")
	case <-time.After(50 * time.Millisecond):
	}

	close(releaseRecord)

	select {
	case err := <-logDone:
		if err != nil {
			t.Fatalf("handleLogs: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("handleLogs timed out after releasing recordAnalytics")
	}
	select {
	case <-flipDone:
	case <-time.After(5 * time.Second):
		t.Fatal("setSuppressDirectOTel timed out after releasing recordAnalytics")
	}

	if !p.SuppressDirectOTel() {
		t.Fatal("expected SuppressDirectOTel=true after flip completed")
	}

	stopCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := p.Stop(stopCtx); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// The in-flight increment derived under suppress=false must have been
	// exported directly (not stripped by the concurrent flip to true).
	backend.mu.Lock()
	defer backend.mu.Unlock()
	callPts, callSum := sumMetricValues(backend.metrics, telemetrycontract.MetricAPICalls)
	tokPts, tokSum := sumMetricValues(backend.metrics, telemetrycontract.MetricUsageTokens)
	if callPts != 1 || callSum != 1 {
		t.Fatalf("gen_ai.api.calls exported = (%d points, sum %d), want (1, 1)", callPts, callSum)
	}
	if tokPts != 1 || tokSum != 33 {
		t.Fatalf("scion.usage.tokens exported = (%d points, sum %d), want (1, 33)", tokPts, tokSum)
	}
}

// O2: When ForceFlush fails or times out before a suppression switch,
// setSuppressDirectOTel still performs the best-effort flip, logs at Warn, and
// increments UsageDiagnostics.SuppressFlushFailed.
func TestPipeline_FlushThenFlip_FailedFlushWarnsAndCountsInUsageDiagnostics(t *testing.T) {
	t.Setenv("SCION_AGENT_ID", "ag-flushfail-1")
	t.Setenv("SCION_PROJECT_ID", "proj-flushfail-1")
	t.Setenv("SCION_HARNESS", "claude")
	t.Setenv("SCION_USAGE_SOURCE", "native")

	origTimeout := usageDeriverFlushTimeout
	usageDeriverFlushTimeout = 150 * time.Millisecond
	t.Cleanup(func() { usageDeriverFlushTimeout = origTimeout })

	logFile := filepath.Join(t.TempDir(), "agent.log")
	log.SetLogPath(logFile)
	t.Cleanup(func() { log.SetLogPath("/tmp/agent.log") })

	p := NewWithConfig(&Config{
		Enabled:      true,
		CloudEnabled: false,
		GRPCPort:     0,
		HTTPPort:     0,
	})
	p.SetAnalyticsOutbox(t.TempDir(), -1, -1)
	ctx := context.Background()
	if err := p.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		stopCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		_ = p.Stop(stopCtx)
	}()

	// Stop the loopback receiver so the deriver's pre-flip ForceFlush fails.
	if err := p.receiver.Stop(ctx); err != nil {
		t.Fatalf("receiver.Stop: %v", err)
	}

	deriver := p.usageDeriver.Load()
	if deriver == nil {
		t.Fatal("expected active usage deriver")
	}
	deriver.record(ctx, usageIncrement{
		Model:  "claude-sonnet-4-6",
		Status: telemetrycontract.StatusSuccess,
		Calls:  1,
		Tokens: map[string]int64{telemetrycontract.TokenTypeInput: 10},
	})

	p.setSuppressDirectOTel(true)

	if !p.SuppressDirectOTel() {
		t.Fatal("expected best-effort flip to still set SuppressDirectOTel=true when ForceFlush fails")
	}
	if got := p.UsageDiagnostics().SuppressFlushFailed; got != 1 {
		t.Fatalf("UsageDiagnostics().SuppressFlushFailed = %d, want 1", got)
	}
	logBytes, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	if !strings.Contains(string(logBytes), "[WARN] Usage deriver flush before suppression switch failed") {
		t.Fatalf("expected WARN log for failed pre-flip flush, got:\n%s", string(logBytes))
	}
}
