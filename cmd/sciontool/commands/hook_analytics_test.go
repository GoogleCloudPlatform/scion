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
