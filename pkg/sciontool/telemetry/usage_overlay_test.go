/*
Copyright 2025 The Scion Authors.
*/

package telemetry

import (
	"bytes"
	"context"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	"google.golang.org/protobuf/proto"
)

// startPipelineWithoutUsageSource starts a pipeline the way sciontool init
// does: before the harness env overlay is loaded, so init's own environment
// names the harness but carries no SCION_USAGE_SOURCE.
func startPipelineWithoutUsageSource(t *testing.T) *Pipeline {
	t.Helper()
	clearTelemetryEnv()
	t.Setenv(EnvEnabled, "true")
	t.Setenv(EnvCloudEnabled, "false")
	t.Setenv(EnvGRPCPort, "0")
	t.Setenv(EnvHTTPPort, "0")
	t.Setenv(EnvHarness, "claude")
	t.Setenv(EnvUsageSource, "")

	p := New()
	if p == nil {
		t.Fatal("expected a pipeline")
	}
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = p.Stop(ctx)
	})
	return p
}

func postLogsToPipeline(t *testing.T, p *Pipeline) {
	t.Helper()
	body, err := proto.Marshal(&collogspb.ExportLogsServiceRequest{ResourceLogs: loadClaudeUsageFixture(t)})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	p.receiver.handleHTTPLogs(rec, otlpHTTPRequest("/v1/logs", bytes.NewReader(body)))
	if rec.Code != 200 {
		t.Fatalf("post status = %d: %s", rec.Code, rec.Body.String())
	}
}

// TestPipelineUsageDeriverActivatesFromHarnessEnvOverlay is the regression
// test for the AC-1.5 failure (ptone/scion#2253): init starts the pipeline
// before the harness env overlay supplies SCION_USAGE_SOURCE=native, so the
// deriver built in Start is a no-op. Applying the overlay must activate it
// without the source ever entering this process's environment.
func TestPipelineUsageDeriverActivatesFromHarnessEnvOverlay(t *testing.T) {
	p := startPipelineWithoutUsageSource(t)

	if len(p.usageDeriver.Load().rules) != 0 {
		t.Fatal("precondition: deriver must be a no-op before the overlay is applied")
	}

	overlay := map[string]string{EnvUsageSource: "native", "SCION_TEST_OVERLAY_ONLY_KEY": "1"}
	if err := p.ApplyHarnessEnvOverlay(context.Background(), overlay); err != nil {
		t.Fatalf("apply overlay: %v", err)
	}

	deriver := p.usageDeriver.Load()
	if len(deriver.rules) == 0 {
		t.Fatal("deriver must be active once the harness overlay declares SCION_USAGE_SOURCE=native")
	}
	if got := deriver.selection; got != (UsageSelection{Source: "native", Harness: "claude"}) {
		t.Fatalf("selection = %+v", got)
	}

	postLogsToPipeline(t, p)
	if diag := p.UsageDiagnostics(); diag.Derived == 0 {
		t.Fatalf("no usage derived from native events after overlay: %+v", diag)
	}

	// Nothing from the overlay leaks into init's own environment.
	if v, ok := os.LookupEnv(EnvUsageSource); ok && v != "" {
		t.Fatalf("SCION_USAGE_SOURCE leaked into process env: %q", v)
	}
	if _, ok := os.LookupEnv("SCION_TEST_OVERLAY_ONLY_KEY"); ok {
		t.Fatal("unrelated overlay key leaked into process env")
	}

	// Re-applying the same overlay keeps the running deriver.
	if err := p.ApplyHarnessEnvOverlay(context.Background(), overlay); err != nil {
		t.Fatal(err)
	}
	if p.usageDeriver.Load() != deriver {
		t.Fatal("an unchanged selection must not rebuild the deriver")
	}
}

// TestPipelineHarnessEnvOverlayWithoutNativeSourceStaysNoOp pins that an
// overlay declaring hooks (or nothing) leaves derivation off (D4/D10), and
// that an overlay value overrides init's env just as it does for the child.
func TestPipelineHarnessEnvOverlayWithoutNativeSourceStaysNoOp(t *testing.T) {
	p := startPipelineWithoutUsageSource(t)
	for _, overlay := range []map[string]string{nil, {EnvUsageSource: "hooks"}, {EnvUsageSource: "native", EnvHarness: "some-future-harness"}} {
		if err := p.ApplyHarnessEnvOverlay(context.Background(), overlay); err != nil {
			t.Fatal(err)
		}
		if len(p.usageDeriver.Load().rules) != 0 {
			t.Fatalf("overlay %v must not enable derivation", overlay)
		}
	}
}

func TestPipelineApplyHarnessEnvOverlayNilAndStoppedSafe(t *testing.T) {
	var nilPipeline *Pipeline
	if err := nilPipeline.ApplyHarnessEnvOverlay(context.Background(), map[string]string{EnvUsageSource: "native"}); err != nil {
		t.Fatal(err)
	}
	p := NewWithConfig(&Config{Enabled: true})
	if err := p.ApplyHarnessEnvOverlay(context.Background(), map[string]string{EnvUsageSource: "native"}); err != nil {
		t.Fatal(err)
	}
	if p.usageDeriver.Load() != nil {
		t.Fatal("a pipeline that never started must not build a deriver")
	}
}
