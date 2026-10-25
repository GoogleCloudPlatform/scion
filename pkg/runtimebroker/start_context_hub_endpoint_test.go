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

package runtimebroker

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildStartContext_HubEndpoint(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.HubEndpoint = "https://hub.example.com"
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)

	// Without HTTPRequest, the connection endpoint is empty, so the broker's
	// own HubEndpoint is used.
	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:      "agent-1",
		Operation: opCreate,
	})
	if err != nil {
		t.Fatal(err)
	}
	if sc.Opts.Env["SCION_HUB_ENDPOINT"] != "https://hub.example.com" {
		t.Errorf("expected SCION_HUB_ENDPOINT='https://hub.example.com', got %q", sc.Opts.Env["SCION_HUB_ENDPOINT"])
	}
	if sc.Opts.Env["SCION_HUB_URL"] != "https://hub.example.com" {
		t.Errorf("expected SCION_HUB_URL='https://hub.example.com', got %q", sc.Opts.Env["SCION_HUB_URL"])
	}
}

// TestBuildStartContext_TrustedHubEndpointOperatorTiersOnly proves
// StartOptions.TrustedHubEndpoint is populated only from an
// operator-derived tier (the broker's own configured HubEndpoint here; the
// request HubEndpoint and the hub connection endpoint rank above it but are
// exercised at the hubenv_test.go unit level), while the DELIVERED
// SCION_HUB_ENDPOINT env value still follows the full resolution chain
// regardless of which tier supplied it. A tenant-controllable resolved-env
// value and a project-settings-only value both reach the agent's own env,
// but neither ever reaches TrustedHubEndpoint.
func TestBuildStartContext_TrustedHubEndpointOperatorTiersOnly(t *testing.T) {
	const tenantEndpoint = "http://169.254.169.254"

	t.Run("resolved-env-only value is delivered but not trusted", func(t *testing.T) {
		cfg := DefaultServerConfig()
		cfg.StateDir = t.TempDir()
		srv := newTestServerForStartContext(t, cfg)

		sc, err := srv.buildStartContext(context.Background(), startContextInputs{
			Name:        "agent-1",
			Operation:   opCreate,
			ResolvedEnv: map[string]string{"SCION_HUB_ENDPOINT": tenantEndpoint},
		})
		if err != nil {
			t.Fatal(err)
		}
		// Existence control: the value really does reach the agent's own
		// env, so the TrustedHubEndpoint assertion below is not vacuously
		// passing because nothing was resolved at all.
		if sc.Opts.Env["SCION_HUB_ENDPOINT"] != tenantEndpoint {
			t.Fatalf("existence control failed: SCION_HUB_ENDPOINT = %q, want %q", sc.Opts.Env["SCION_HUB_ENDPOINT"], tenantEndpoint)
		}
		if sc.Opts.TrustedHubEndpoint != "" {
			t.Errorf("TrustedHubEndpoint = %q, want \"\" (a resolved-env-only value must never be trusted for egress)", sc.Opts.TrustedHubEndpoint)
		}
	})

	t.Run("broker config endpoint is trusted even with a resolved-env value present", func(t *testing.T) {
		const operatorEndpoint = "https://hub.operator.example"
		cfg := DefaultServerConfig()
		cfg.StateDir = t.TempDir()
		cfg.HubEndpoint = operatorEndpoint
		srv := newTestServerForStartContext(t, cfg)

		sc, err := srv.buildStartContext(context.Background(), startContextInputs{
			Name:        "agent-2",
			Operation:   opCreate,
			ResolvedEnv: map[string]string{"SCION_HUB_ENDPOINT": tenantEndpoint},
		})
		if err != nil {
			t.Fatal(err)
		}
		if sc.Opts.TrustedHubEndpoint != operatorEndpoint {
			t.Errorf("TrustedHubEndpoint = %q, want %q (the operator-configured broker endpoint)", sc.Opts.TrustedHubEndpoint, operatorEndpoint)
		}
	})

	// Project settings is itself a hub-resolved file for a hub-managed
	// project — the same tenant-reachable precondition the resolved-env
	// tier is excluded for.
	t.Run("project-settings-only value is delivered but not trusted", func(t *testing.T) {
		const settingsEndpoint = "https://settings.example.com"
		projectDir := t.TempDir()
		if err := os.WriteFile(filepath.Join(projectDir, "settings.yaml"), []byte("hub:\n  endpoint: "+settingsEndpoint+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		cfg := DefaultServerConfig()
		cfg.StateDir = t.TempDir()
		srv := newTestServerForStartContext(t, cfg)

		sc, err := srv.buildStartContext(context.Background(), startContextInputs{
			Name:        "agent-3",
			Operation:   opCreate,
			ProjectPath: projectDir,
		})
		if err != nil {
			t.Fatal(err)
		}
		if sc.Opts.Env["SCION_HUB_ENDPOINT"] != settingsEndpoint {
			t.Fatalf("existence control failed: SCION_HUB_ENDPOINT = %q, want %q", sc.Opts.Env["SCION_HUB_ENDPOINT"], settingsEndpoint)
		}
		if sc.Opts.TrustedHubEndpoint != "" {
			t.Errorf("TrustedHubEndpoint = %q, want \"\" (a project-settings-only value must never be trusted for egress)", sc.Opts.TrustedHubEndpoint)
		}
	})
}

// TestBuildStartContext_CloudrunSandboxHubEndpoint verifies hub endpoint
// behaviour for the cloudrun-sandbox runtime. SCION_METADATA_BIND_ADDRESS
// pins an explicit TEST-NET-3 (RFC 5737) documentation address so the result
// is deterministic instead of depending on interface discovery (unavailable
// in CI). The endpoint must be http://<bind-address>:<port> with the port
// read from the broker's own HubListenPort config — not a public
// IAP-fronted URL, which the sandbox cannot authenticate against.
func TestBuildStartContext_CloudrunSandboxHubEndpoint(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	cfg.HubListenPort = 8080
	srv := newTestServerForStartContextRuntime(t, cfg, "cloudrun-sandbox")

	t.Setenv("SCION_METADATA_BIND_ADDRESS", "203.0.113.5")

	r := httptest.NewRequest("POST", "/api/v1/agents", nil)

	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:        "agent-sandbox",
		HTTPRequest: r,
		Operation:   opCreate,
	})
	if err != nil {
		t.Fatalf("buildStartContext() unexpected error: %v", err)
	}

	const want = "http://203.0.113.5:8080"
	if ep := sc.Opts.Env["SCION_HUB_ENDPOINT"]; ep != want {
		t.Fatalf("SCION_HUB_ENDPOINT = %q, want %q (bind address plus HubListenPort)", ep, want)
	}

	// Metadata vars must still be localhost — emulator runs inside sandbox.
	host := sc.Opts.Env["GCE_METADATA_HOST"]
	root := sc.Opts.Env["GCE_METADATA_ROOT"]
	if host != "localhost:18380" {
		t.Errorf("expected GCE_METADATA_HOST='localhost:18380', got %q", host)
	}
	if root != "localhost:18380" {
		t.Errorf("expected GCE_METADATA_ROOT='localhost:18380', got %q", root)
	}
}

// TestBuildStartContext_CloudrunSandboxHubNeverRunApp is a durable guard:
// assert the negative so the test survives refactoring and catches the
// shipped defect pattern. If buildStartContext succeeds for cloudrun-sandbox,
// the hub endpoint must NEVER be a public IAP-fronted URL.
func TestBuildStartContext_CloudrunSandboxHubNeverRunApp(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	cfg.HubListenPort = 8080
	srv := newTestServerForStartContextRuntime(t, cfg, "cloudrun-sandbox")

	r := httptest.NewRequest("POST", "/api/v1/agents", nil)

	// Simulate a hub-dispatched agent whose resolvedEnv carries the public URL.
	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:        "agent-sandbox-iap",
		ResolvedEnv: map[string]string{"SCION_HUB_ENDPOINT": "https://my-instance-xyz.run.app"},
		HTTPRequest: r,
		Operation:   opCreate,
	})
	if err != nil {
		// In CI this fails (no link-local) — that is correct.
		return
	}
	ep := sc.Opts.Env["SCION_HUB_ENDPOINT"]
	if strings.Contains(ep, "run.app") {
		t.Fatalf("cloudrun-sandbox SCION_HUB_ENDPOINT must never contain run.app, got %q", ep)
	}
}
