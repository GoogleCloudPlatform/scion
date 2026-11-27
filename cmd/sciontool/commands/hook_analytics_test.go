/*
Copyright 2026 The Scion Authors.
*/

package commands

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/telemetry"
)

// The hook producer resolves the outbox from the agent home (its HOME), the
// same directory init passes to the pipeline (telemetry.AnalyticsOutboxDirFor
// of the resolved agent home), and does no I/O until a usage event is
// recorded (WP2 r1 C1, R4).
func TestHookUsageAnalyticsLazyAgentHomeOutbox(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SCION_USAGE_SOURCE", "hooks")
	cfg := &telemetry.Config{Enabled: true, CloudEnabled: true, CloudProvider: "gcp", ProjectID: "p"}
	if !telemetry.AnalyticsEnabledForHooks(cfg) {
		t.Fatal("fixture config must enable hook analytics")
	}
	a := hookUsageAnalytics(cfg)
	if a == nil {
		t.Fatal("hook analytics disabled for a GCP config with SCION_USAGE_SOURCE=hooks")
	}
	dir := telemetry.AnalyticsOutboxDirFor(home)
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("hookUsageAnalytics must not touch the outbox before a usage event")
	}
	if got := a.Outbox().Dir(); got != filepath.Join(home, ".scion", telemetry.AnalyticsOutboxDirName) || got != dir {
		t.Errorf("hook outbox = %q, want the agent home's %q", got, dir)
	}

	t.Setenv("SCION_USAGE_SOURCE", "native")
	if hookUsageAnalytics(cfg) != nil {
		t.Error("hook analytics must be off unless SCION_USAGE_SOURCE=hooks")
	}
}

func TestHookUsageAnalytics_HubOnlyAndDualRunDiscovery(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SCION_USAGE_SOURCE", "hooks")
	t.Setenv(telemetry.EnvUsageHubIngest, "1")

	// Hub-only config (no GCP exporter): does no I/O at construction and never
	// creates the GCP outbox directory.
	hubOnlyCfg := &telemetry.Config{Enabled: true, CloudEnabled: false}
	if !telemetry.AnalyticsEnabledForHooks(hubOnlyCfg) {
		t.Fatal("AnalyticsEnabledForHooks = false in Hub-only mode, want true")
	}
	a := hookUsageAnalytics(hubOnlyCfg)
	if a == nil {
		t.Fatal("hookUsageAnalytics returned nil in Hub-only mode")
	}
	if _, err := os.Stat(telemetry.AnalyticsOutboxDirFor(home)); !os.IsNotExist(err) {
		t.Error("hookUsageAnalytics must not create GCP outbox dir in Hub-only mode")
	}
	if _, err := os.Stat(telemetry.HubAnalyticsOutboxDirFor(home)); !os.IsNotExist(err) {
		t.Error("hookUsageAnalytics must not touch Hub outbox before a usage event")
	}
	if a.Outbox() != nil {
		t.Error("Hub-only hook Outbox() should be nil when Hub outbox is not marked active on disk")
	}
}

// TestRunHookTelemetry_CreatesMuseTurnUsageStateFile exercises the production
// wiring in runHookTelemetry (hook.go): with HOME set to a temp dir, one
// muse-code model-end event must create $HOME/.scion/muse-turn-usage.json so
// subsequent steps compute per-step deltas instead of failing closed.
func TestRunHookTelemetry_CreatesMuseTurnUsageStateFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SCION_TELEMETRY_ENABLED", "true")
	t.Setenv("SCION_HARNESS", "muse-code")
	t.Setenv("SCION_USAGE_SOURCE", "hooks")
	t.Setenv("SCION_TELEMETRY_CLOUD_ENABLED", "false")
	t.Setenv("SCION_OTEL_ENDPOINT", "")

	ev := &hooks.Event{
		Name:    hooks.EventModelEnd,
		RawName: "PostLLMCall",
		Dialect: "muse-code",
		Data: hooks.EventData{
			SessionID:          "sess-wire-1",
			Model:              "muse-spark",
			InputTokens:        600,
			OutputTokens:       120,
			CachedTokens:       200,
			ReasoningTokens:    40,
			InputTokensDerived: true,
			Raw: map[string]interface{}{
				"turn_id":     "turn-wire-1",
				"request_id":  "turn-wire-1:0:1",
				"response_id": "resp_wire_0",
			},
		},
	}

	runHookTelemetry(context.Background(), ev, nil)

	statePath := filepath.Join(home, ".scion", "muse-turn-usage.json")
	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("expected muse turn usage state file at %s: %v", statePath, err)
	}
	if !strings.Contains(string(raw), "sess-wire-1:turn-wire-1") {
		t.Errorf("state file %s = %s, want entry for sess-wire-1:turn-wire-1", statePath, raw)
	}
}

func TestHookUsageAnalytics_UsageSourcesTable(t *testing.T) {
	gcpCfg := &telemetry.Config{Enabled: true, CloudEnabled: true, CloudProvider: "gcp", ProjectID: "p"}
	hubOnlyCfg := &telemetry.Config{Enabled: true, CloudEnabled: false}

	cases := []struct {
		name       string
		source     string
		hubIngest  string
		cfg        *telemetry.Config
		wantNonNil bool
	}{
		{name: "hooks_gcp", source: "hooks", cfg: gcpCfg, wantNonNil: true},
		{name: "sqlite_native_gcp", source: "sqlite_native", cfg: gcpCfg, wantNonNil: true},
		{name: "sqlite_native_hub_only", source: "sqlite_native", hubIngest: "1", cfg: hubOnlyCfg, wantNonNil: true},
		{name: "native_rejected", source: "native", cfg: gcpCfg, wantNonNil: false},
		{name: "empty_rejected", source: "", cfg: gcpCfg, wantNonNil: false},
		{name: "unknown_rejected", source: "other", cfg: gcpCfg, wantNonNil: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("SCION_USAGE_SOURCE", tc.source)
			t.Setenv(telemetry.EnvUsageHubIngest, tc.hubIngest)

			got := hookUsageAnalytics(tc.cfg)
			if (got != nil) != tc.wantNonNil {
				t.Fatalf("hookUsageAnalytics(source=%q) = %v, wantNonNil=%v", tc.source, got, tc.wantNonNil)
			}
			// Must remain lazy and never create outbox dirs before an event is recorded.
			if _, err := os.Stat(telemetry.AnalyticsOutboxDirFor(home)); !os.IsNotExist(err) {
				t.Error("hookUsageAnalytics must not create GCP outbox dir eagerly")
			}
			if _, err := os.Stat(telemetry.HubAnalyticsOutboxDirFor(home)); !os.IsNotExist(err) {
				t.Error("hookUsageAnalytics must not create Hub outbox dir eagerly")
			}
		})
	}
}

func TestMinimalEnv_AdvertisesSQLiteNativeUsageSource(t *testing.T) {
	home := t.TempDir()
	m := &containerProvisionManifest{
		AgentName:        "agent-test",
		AgentHome:        home,
		AgentWorkspace:   filepath.Join(home, "workspace"),
		HarnessBundleDir: filepath.Join(home, "bundle"),
	}
	m.HarnessConfig.Harness = "antigravity"

	env := minimalEnv(home, m)
	var found string
	for _, entry := range env {
		if strings.HasPrefix(entry, "SCION_USAGE_SOURCES_SUPPORTED=") {
			found = strings.TrimPrefix(entry, "SCION_USAGE_SOURCES_SUPPORTED=")
			break
		}
	}
	if found == "" {
		t.Fatal("minimalEnv did not set SCION_USAGE_SOURCES_SUPPORTED")
	}
	hasSQLiteNative := false
	for _, tok := range strings.Split(found, ",") {
		if tok == "sqlite_native" {
			hasSQLiteNative = true
			break
		}
	}
	if !hasSQLiteNative {
		t.Fatalf("SCION_USAGE_SOURCES_SUPPORTED = %q, want it to include sqlite_native", found)
	}
}
