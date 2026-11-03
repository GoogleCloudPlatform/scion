/*
Copyright 2026 The Scion Authors.
*/

package telemetry

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// cloudDefaultEnv sets up an environment with telemetry on, no cloud
// destination, no credentials file to discover (HOME is empty) and
// ephemeral ports, and lets LoadConfig keep CloudEnabled under go test.
// cloudEnabled == "" leaves SCION_TELEMETRY_CLOUD_ENABLED unset.
func cloudDefaultEnv(t *testing.T, cloudEnabled string) {
	t.Helper()
	clearTelemetryEnv()
	t.Cleanup(SetTelemetryTestSandboxed())
	t.Setenv("HOME", t.TempDir())
	t.Setenv(EnvEnabled, "true")
	t.Setenv(EnvGRPCPort, "0")
	t.Setenv(EnvHTTPPort, "0")
	t.Setenv("SCION_USAGE_SOURCE", "")
	// t.Setenv first so the original value is restored after the test.
	t.Setenv(EnvCloudEnabled, cloudEnabled)
	if cloudEnabled == "" {
		if err := os.Unsetenv(EnvCloudEnabled); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLoadConfig_CloudEnabledExplicit(t *testing.T) {
	cases := []struct {
		env          string
		wantEnabled  bool
		wantExplicit bool
	}{
		{"", true, false},
		{"true", true, true},
		{"false", false, true},
		// Set but empty, or misspelt: ParseBoolEnv takes the default, so
		// the value is not explicit either.
		{"<set-empty>", true, false},
		{"ture", true, false},
	}
	for _, tc := range cases {
		t.Run("env="+tc.env, func(t *testing.T) {
			if tc.env == "<set-empty>" {
				cloudDefaultEnv(t, "")
				t.Setenv(EnvCloudEnabled, "")
			} else {
				cloudDefaultEnv(t, tc.env)
			}
			cfg := LoadConfig()
			if cfg.CloudEnabled != tc.wantEnabled || cfg.CloudEnabledExplicit != tc.wantExplicit {
				t.Fatalf("CloudEnabled=%v CloudEnabledExplicit=%v, want %v %v",
					cfg.CloudEnabled, cfg.CloudEnabledExplicit, tc.wantEnabled, tc.wantExplicit)
			}
		})
	}
}

// TestPipeline_DefaultCloudWithoutDestination_RunsLocally: with cloud export
// on only by default and no destination configured, Start succeeds and runs
// the local pipeline with cloud export off, as with cloud export disabled
// (ptone/scion#7104). Before, Start failed and no local pipeline ran.
func TestPipeline_DefaultCloudWithoutDestination_RunsLocally(t *testing.T) {
	cloudDefaultEnv(t, "")
	p := New()
	if p == nil {
		t.Fatal("New() = nil, want a pipeline")
	}
	if !p.Config().CloudEnabled || p.Config().CloudEnabledExplicit || p.Config().IsCloudConfigured() {
		t.Fatalf("precondition: want cloud enabled by default with no destination, got %+v", p.Config())
	}

	ctx := context.Background()
	if err := p.Start(ctx); err != nil {
		t.Fatalf("Start() = %v, want nil", err)
	}
	if !p.IsRunning() {
		t.Fatal("pipeline not running after Start")
	}
	if p.Config().CloudEnabled {
		t.Error("CloudEnabled still set after Start; intake would reject every batch")
	}
	if got := p.DeliveryState(); got != "running" {
		t.Errorf("DeliveryState() = %q, want running", got)
	}

	stopCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := p.Stop(stopCtx); err != nil {
		t.Fatalf("Stop() = %v", err)
	}
	if got := p.DeliveryState(); got != "disabled" {
		t.Errorf("DeliveryState() after Stop = %q, want disabled", got)
	}
}

// TestPipeline_EmptyCloudEnabledWithoutDestination_RunsLocally: a set but
// empty SCION_TELEMETRY_CLOUD_ENABLED is the default, not an explicit
// request, so Start does not fail.
func TestPipeline_EmptyCloudEnabledWithoutDestination_RunsLocally(t *testing.T) {
	cloudDefaultEnv(t, "")
	t.Setenv(EnvCloudEnabled, "")
	p := New()
	if p == nil {
		t.Fatal("New() = nil, want a pipeline")
	}
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start() = %v, want nil", err)
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := p.Stop(stopCtx); err != nil {
		t.Fatalf("Stop() = %v", err)
	}
}

// TestPipeline_ExplicitCloudWithoutDestination_Fails: cloud export asked for
// explicitly with no destination is still a visible error.
func TestPipeline_ExplicitCloudWithoutDestination_Fails(t *testing.T) {
	cloudDefaultEnv(t, "true")
	p := New()
	if p == nil {
		t.Fatal("New() = nil, want a pipeline")
	}
	err := p.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "destination is not configured") {
		t.Fatalf("Start() = %v, want the destination-not-configured error", err)
	}
	if p.IsRunning() {
		t.Error("pipeline running after a failed Start")
	}
	if got := p.DeliveryState(); got != "failed" {
		t.Errorf("DeliveryState() = %q, want failed", got)
	}
}
