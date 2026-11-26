/*
Copyright 2026 The Scion Authors.
*/

package commands

import (
	"os"
	"path/filepath"
	"testing"

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
