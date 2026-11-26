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

package commands

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/telemetry"
	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
)

func TestProbeAndConfigureUsageIngest_Negative(t *testing.T) {
	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
	}{
		{
			name: "404 older hub or experiment off",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusNotFound)
			},
		},
		{
			name: "capabilities disabled",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(telemetrycontract.UsageCapabilitiesResponse{
					UsageIngestEnabled: false,
				})
			},
		},
		{
			name: "500 probe error",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(telemetry.EnvUsageHubIngest, "1") // must be cleared on negative probe
			srv := httptest.NewServer(tc.handler)
			defer srv.Close()

			cfg := &telemetry.Config{
				Enabled:      true,
				CloudEnabled: false,
				GRPCPort:     0,
				HTTPPort:     0,
			}
			p := telemetry.NewWithConfig(cfg)
			agentHome := t.TempDir()
			p.SetAnalyticsOutbox(agentHome, 0, 0)

			newClient := func() *hub.Client {
				return hub.NewClientWithConfig(srv.URL, "tok-1", "agent-1")
			}
			if ok := probeAndConfigureUsageIngest(context.Background(), p, newClient); ok {
				t.Fatal("probeAndConfigureUsageIngest = true, want false")
			}
			if got := os.Getenv(telemetry.EnvUsageHubIngest); got != "0" {
				t.Errorf("%s = %q, want \"0\"", telemetry.EnvUsageHubIngest, got)
			}
			if telemetry.AnalyticsEnabledForHooks(cfg) {
				t.Error("AnalyticsEnabledForHooks = true on negative probe without GCP exporter, want false")
			}

			// Without GCP exporter and negative probe, pipeline keeps old behavior:
			// analytics disabled with AnalyticsDisabledNoGCPExporter, outbox not created.
			if err := p.Start(context.Background()); err != nil {
				t.Fatalf("Pipeline.Start: %v", err)
			}
			defer func() {
				stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_ = p.Stop(stopCtx)
			}()
			if _, ok, reason := p.AnalyticsDiagnostics(); ok || reason != telemetry.AnalyticsDisabledNoGCPExporter {
				t.Errorf("AnalyticsDiagnostics() = ok:%v reason:%q, want ok:false reason:%q", ok, reason, telemetry.AnalyticsDisabledNoGCPExporter)
			}
			if _, err := os.Stat(telemetry.AnalyticsOutboxDirFor(agentHome)); !os.IsNotExist(err) {
				t.Error("GCP outbox directory should not be created when probe is negative and no GCP exporter is configured")
			}
			if _, err := os.Stat(telemetry.HubAnalyticsOutboxDirFor(agentHome)); !os.IsNotExist(err) {
				t.Error("Hub outbox directory should not be created when probe is negative")
			}
		})
	}
}

func TestProbeAndConfigureUsageIngest_PositiveWithoutGCPExporter(t *testing.T) {
	t.Setenv("SCION_AGENT_ID", "agent-1")
	t.Setenv("SCION_HARNESS", "claude")
	t.Setenv("SCION_USAGE_SOURCE", "native")
	t.Setenv(telemetry.EnvUsageHubIngest, "")

	var mu sync.Mutex
	var gotCapsProbe bool
	var gotIngestReqs []telemetrycontract.UsageIngestRequest

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Scion-Agent-Token"); got != "tok-1" {
			t.Errorf("X-Scion-Agent-Token = %q, want tok-1", got)
		}
		switch r.URL.Path {
		case "/api/v1/agents/agent-1/usage/capabilities":
			mu.Lock()
			gotCapsProbe = true
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(telemetrycontract.UsageCapabilitiesResponse{
				UsageIngestEnabled:   true,
				SuppressDirectOTel:   true, // ignored in P1.2
				MaxBatchEvents:       500,
				MaxBatchBytes:        262144,
				HeartbeatIntervalSec: 60,
				DrainIntervalSec:     10,
				HubID:                "hub-1",
				RunID:                "run-probe-1",
			})
		case "/api/v1/agents/agent-1/usage/events":
			var req telemetrycontract.UsageIngestRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			mu.Lock()
			gotIngestReqs = append(gotIngestReqs, req)
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(telemetrycontract.UsageIngestResponse{
				Accepted: len(req.Events),
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	cfg := &telemetry.Config{
		Enabled:      true,
		CloudEnabled: false, // No GCP exporter
		GRPCPort:     0,
		HTTPPort:     0,
	}
	p := telemetry.NewWithConfig(cfg)
	agentHome := t.TempDir()
	p.SetAnalyticsOutbox(agentHome, 0, 0)

	newClient := func() *hub.Client {
		return hub.NewClientWithConfig(srv.URL, "tok-1", "agent-1")
	}
	if ok := probeAndConfigureUsageIngest(context.Background(), p, newClient); !ok {
		t.Fatal("probeAndConfigureUsageIngest = false, want true")
	}
	if got := os.Getenv(telemetry.EnvUsageHubIngest); got != "1" {
		t.Errorf("%s = %q, want \"1\"", telemetry.EnvUsageHubIngest, got)
	}

	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Pipeline.Start: %v", err)
	}
	if _, ok, reason := p.AnalyticsDiagnostics(); !ok || reason != "" {
		t.Fatalf("AnalyticsDiagnostics() = ok:%v reason:%q, want ok:true reason:\"\"", ok, reason)
	}

	stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := p.Stop(stopCtx); err != nil {
		t.Fatalf("Pipeline.Stop: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if !gotCapsProbe {
		t.Error("expected capabilities probe GET request")
	}
	if len(gotIngestReqs) == 0 {
		t.Fatal("expected at least one usage ingest heartbeat/final request")
	}
	sawFinal := false
	for _, req := range gotIngestReqs {
		if req.RunID != "run-probe-1" {
			t.Errorf("req.RunID = %q, want run-probe-1", req.RunID)
		}
		if req.ProducerEpoch == "" {
			t.Error("req.ProducerEpoch is empty")
		}
		if req.FinalState == telemetrycontract.MeterFinalStateFinal {
			sawFinal = true
		}
	}
	if !sawFinal {
		t.Errorf("expected final_state=%q on shutdown, got requests: %+v", telemetrycontract.MeterFinalStateFinal, gotIngestReqs)
	}
}

// O1: transient 503 on first probe recovers via bounded init retry (503 -> 200)
// and via periodic re-probe when init retries are exhausted.
func TestProbeAndConfigureUsageIngest_Transient503Then200(t *testing.T) {
	origMax := usageCapabilitiesProbeMaxAttempts
	origBackoff := usageCapabilitiesProbeBaseBackoff
	usageCapabilitiesProbeMaxAttempts = 3
	usageCapabilitiesProbeBaseBackoff = 5 * time.Millisecond
	t.Cleanup(func() {
		usageCapabilitiesProbeMaxAttempts = origMax
		usageCapabilitiesProbeBaseBackoff = origBackoff
	})

	t.Run("init retry recovers from 503 then 200", func(t *testing.T) {
		var calls int
		var mu sync.Mutex
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			calls++
			c := calls
			mu.Unlock()
			if c == 1 {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte("restarting"))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(telemetrycontract.UsageCapabilitiesResponse{
				UsageIngestEnabled: true,
				MaxBatchEvents:     500,
				MaxBatchBytes:      262144,
				HubID:              "hub-1",
				RunID:              "run-retry-ok",
			})
		}))
		defer srv.Close()

		p := telemetry.NewWithConfig(&telemetry.Config{Enabled: true, CloudEnabled: false, GRPCPort: 0, HTTPPort: 0})
		p.SetAnalyticsOutbox(t.TempDir(), 0, 0)
		newClient := func() *hub.Client { return hub.NewClientWithConfig(srv.URL, "tok-1", "agent-1") }

		if ok := probeAndConfigureUsageIngest(context.Background(), p, newClient); !ok {
			t.Fatal("probeAndConfigureUsageIngest = false after 503 -> 200, want true")
		}
		if got := os.Getenv(telemetry.EnvUsageHubIngest); got != "1" {
			t.Errorf("%s = %q, want \"1\"", telemetry.EnvUsageHubIngest, got)
		}
		mu.Lock()
		gotCalls := calls
		mu.Unlock()
		if gotCalls != 2 {
			t.Errorf("probe calls = %d, want 2", gotCalls)
		}
	})

	t.Run("Hub-only negative after init retries stays off Hub ledger with no heartbeats", func(t *testing.T) {
		var mu sync.Mutex
		var probeCalls int
		var ingestCalls int
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/api/v1/agents/agent-1/usage/capabilities":
				mu.Lock()
				probeCalls++
				c := probeCalls
				mu.Unlock()
				if c <= 3 {
					// All 3 init attempts return 503.
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(telemetrycontract.UsageCapabilitiesResponse{
					UsageIngestEnabled: true,
					MaxBatchEvents:     500,
					MaxBatchBytes:      262144,
					HubID:              "hub-1",
					RunID:              "run-reprobe-hubonly",
				})
			case "/api/v1/agents/agent-1/usage/events":
				mu.Lock()
				ingestCalls++
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(telemetrycontract.UsageIngestResponse{})
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		}))
		defer srv.Close()

		agentHome := t.TempDir()
		p := telemetry.NewWithConfig(&telemetry.Config{Enabled: true, CloudEnabled: false, GRPCPort: 0, HTTPPort: 0})
		p.SetAnalyticsOutbox(agentHome, 0, 0)
		p.SetHubUsageProbeInterval(15 * time.Millisecond)
		newClient := func() *hub.Client { return hub.NewClientWithConfig(srv.URL, "tok-1", "agent-1") }

		if ok := probeAndConfigureUsageIngest(context.Background(), p, newClient); ok {
			t.Fatal("probeAndConfigureUsageIngest = true while all 3 init probes returned 503, want false")
		}
		if err := p.Start(context.Background()); err != nil {
			t.Fatalf("Pipeline.Start: %v", err)
		}

		// Wait past several probe intervals to confirm Hub-only does NOT late-activate.
		time.Sleep(60 * time.Millisecond)

		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := p.Stop(stopCtx); err != nil {
			t.Fatalf("Pipeline.Stop: %v", err)
		}

		if _, ok, reason := p.AnalyticsDiagnostics(); ok || reason != telemetry.AnalyticsDisabledNoGCPExporter {
			t.Errorf("AnalyticsDiagnostics() = ok:%v reason:%q, want ok:false reason:%q", ok, reason, telemetry.AnalyticsDisabledNoGCPExporter)
		}
		if got := os.Getenv(telemetry.EnvUsageHubIngest); got != "0" {
			t.Errorf("%s = %q, want \"0\"", telemetry.EnvUsageHubIngest, got)
		}
		if _, err := os.Stat(telemetry.AnalyticsOutboxDirFor(agentHome)); !os.IsNotExist(err) {
			t.Error("GCP outbox directory should not be created in Hub-only mode when init probe failed")
		}
		if _, err := os.Stat(telemetry.HubAnalyticsOutboxDirFor(agentHome)); !os.IsNotExist(err) {
			t.Error("Hub outbox directory should not be created in Hub-only mode when init probe failed")
		}
		mu.Lock()
		gotProbes, gotIngests := probeCalls, ingestCalls
		mu.Unlock()
		if gotProbes != 3 {
			t.Errorf("probeCalls = %d, want 3 (init retries only, no background re-probe in Hub-only mode)", gotProbes)
		}
		if gotIngests != 0 {
			t.Errorf("ingestCalls = %d, want 0 (no heartbeats or usage events sent)", gotIngests)
		}
	})
}

func TestBuildHubUsageSinkConfig_PrefersLaunchIDOverProbeRunID(t *testing.T) {
	caps := &telemetrycontract.UsageCapabilitiesResponse{
		UsageIngestEnabled: true,
		RunID:              "run-from-probe",
	}
	newClient := func() *hub.Client { return hub.NewClientWithConfig("http://127.0.0.1:1", "tok", "agent-1") }

	t.Run("SCION_LAUNCH_ID wins when set", func(t *testing.T) {
		t.Setenv(hub.EnvLaunchID, "run-from-container")
		cfg := buildHubUsageSinkConfig(caps, newClient)
		if cfg.RunID != "run-from-container" {
			t.Errorf("cfg.RunID = %q, want %q", cfg.RunID, "run-from-container")
		}
	})

	t.Run("falls back to probe run_id when SCION_LAUNCH_ID is unset", func(t *testing.T) {
		t.Setenv(hub.EnvLaunchID, "")
		cfg := buildHubUsageSinkConfig(caps, newClient)
		if cfg.RunID != "run-from-probe" {
			t.Errorf("cfg.RunID = %q, want %q", cfg.RunID, "run-from-probe")
		}
	})
}
