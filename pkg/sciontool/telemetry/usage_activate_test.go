/*
Copyright 2026 The Scion Authors.
*/

package telemetry

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
)

// unsetUsageSource removes SCION_USAGE_SOURCE from the process environment
// for the duration of the test (t.Setenv registers the restore). Setting it
// to "" is not the same: an empty value is still a present key, which
// ActivateUsageSource treats as a runtime choice that takes precedence.
func unsetUsageSource(t *testing.T) {
	t.Helper()
	t.Setenv("SCION_USAGE_SOURCE", "")
	if err := os.Unsetenv("SCION_USAGE_SOURCE"); err != nil {
		t.Fatal(err)
	}
}

// startPipelineWithoutUsageSource starts a pipeline the way sciontool init
// does today: before the provisioner's env overlay is loaded, so
// SCION_USAGE_SOURCE is unset in the receiver's environment.
func startPipelineWithoutUsageSource(t *testing.T, harness string) *Pipeline {
	t.Helper()
	t.Setenv("SCION_HARNESS", harness)
	unsetUsageSource(t)
	p := NewWithConfig(&Config{Enabled: true, GRPCPort: availableTCPPort(t)})
	if err := p.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Stop(context.Background()) })
	if d := p.usageDeriver.Load(); d == nil || len(d.rules) != 0 {
		t.Fatalf("precondition: Start with SCION_USAGE_SOURCE unset must store a no-op deriver, got %+v", d)
	}
	return p
}

// TestPipelineActivateUsageSourceAfterStart pins ptone/scion#3391: a native
// usage source selected only after Start (by the provisioner's env overlay)
// must still activate the deriver for every harness with a native rule.
func TestPipelineActivateUsageSourceAfterStart(t *testing.T) {
	for _, tc := range []struct {
		harness    string
		metricRule bool
	}{
		{harness: "claude"},
		{harness: "codex"},
		{harness: "copilot", metricRule: true},
		{harness: "gemini-cli"},
	} {
		t.Run(tc.harness, func(t *testing.T) {
			p := startPipelineWithoutUsageSource(t, tc.harness)
			outcome, err := p.ActivateUsageSource(context.Background(), UsageSourceNative)
			if err != nil {
				t.Fatal(err)
			}
			if outcome != UsageActivated {
				t.Fatalf("ActivateUsageSource(native) = %q, want %q after Start", outcome, UsageActivated)
			}
			d := p.usageDeriver.Load()
			if d == nil || len(d.rules) == 0 {
				t.Fatal("deriver still has no rules after activation")
			}
			if got := d.HasMetricRule(); got != tc.metricRule {
				t.Fatalf("HasMetricRule = %v, want %v", got, tc.metricRule)
			}

			// A second activation must not replace the active deriver (its
			// dedupe and cumulative state would be lost).
			again, err := p.ActivateUsageSource(context.Background(), UsageSourceNative)
			if err != nil {
				t.Fatal(err)
			}
			if again != UsageActivationAlreadyActive || p.usageDeriver.Load() != d {
				t.Fatal("a repeated activation must leave the active deriver in place")
			}
		})
	}
}

func TestPipelineActivateUsageSourceNoOps(t *testing.T) {
	t.Run("non_native_values", func(t *testing.T) {
		p := startPipelineWithoutUsageSource(t, "claude")
		for _, source := range []string{"", "hooks", "NATIVE", "native ", "anything"} {
			outcome, err := p.ActivateUsageSource(context.Background(), source)
			if err != nil || outcome != UsageActivationNotNative {
				t.Fatalf("ActivateUsageSource(%q) = %q, %v; want %q, nil", source, outcome, err, UsageActivationNotNative)
			}
			if d := p.usageDeriver.Load(); len(d.rules) != 0 {
				t.Fatalf("ActivateUsageSource(%q) must not activate usage", source)
			}
		}
	})
	t.Run("harness_without_native_rule", func(t *testing.T) {
		p := startPipelineWithoutUsageSource(t, "opencode")
		outcome, err := p.ActivateUsageSource(context.Background(), UsageSourceNative)
		if err != nil || outcome != UsageActivationNoHarnessRule {
			t.Fatalf("got %q, %v; want %q, nil", outcome, err, UsageActivationNoHarnessRule)
		}
	})
	for _, tc := range []struct {
		name    string
		runtime string
	}{
		{name: "runtime_env_takes_precedence", runtime: "hooks"},
		// An explicit empty runtime value is a present key: the supervisor's
		// additive overlay merge keeps it empty for the harness child, so the
		// receiver must not derive native usage either.
		{name: "runtime_empty_value_takes_precedence", runtime: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SCION_HARNESS", "claude")
			t.Setenv("SCION_USAGE_SOURCE", tc.runtime)
			p := NewWithConfig(&Config{Enabled: true, GRPCPort: availableTCPPort(t)})
			if err := p.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = p.Stop(context.Background()) }()
			outcome, err := p.ActivateUsageSource(context.Background(), UsageSourceNative)
			if err != nil || outcome != UsageActivationRuntimeEnvPrecedence {
				t.Fatalf("got %q, %v; want %q, nil (runtime SCION_USAGE_SOURCE wins over the overlay)", outcome, err, UsageActivationRuntimeEnvPrecedence)
			}
			if d := p.usageDeriver.Load(); len(d.rules) != 0 {
				t.Fatalf("runtime SCION_USAGE_SOURCE=%q must keep native usage off", tc.runtime)
			}
		})
	}
	// When both the runtime env and the overlay select native (the
	// documented workaround applied on a fixed image), Start already built
	// the deriver and activation must leave that exact deriver in place.
	t.Run("runtime_and_overlay_native", func(t *testing.T) {
		t.Setenv("SCION_HARNESS", "claude")
		t.Setenv("SCION_USAGE_SOURCE", UsageSourceNative)
		p := NewWithConfig(&Config{Enabled: true, GRPCPort: availableTCPPort(t)})
		if err := p.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = p.Stop(context.Background()) }()
		before := p.usageDeriver.Load()
		if before == nil || len(before.rules) == 0 {
			t.Fatal("precondition: Start with runtime native must build an active deriver")
		}
		outcome, err := p.ActivateUsageSource(context.Background(), UsageSourceNative)
		if err != nil || outcome != UsageActivationRuntimeEnvPrecedence {
			t.Fatalf("got %q, %v; want %q, nil", outcome, err, UsageActivationRuntimeEnvPrecedence)
		}
		if p.usageDeriver.Load() != before {
			t.Fatal("activation must not replace the deriver Start built from the runtime env")
		}
	})
	t.Run("not_running", func(t *testing.T) {
		t.Setenv("SCION_HARNESS", "claude")
		unsetUsageSource(t)
		p := NewWithConfig(&Config{Enabled: true, GRPCPort: availableTCPPort(t)})
		outcome, err := p.ActivateUsageSource(context.Background(), UsageSourceNative)
		if err != nil || outcome != UsageActivationNotRunning {
			t.Fatalf("got %q, %v; want %q, nil before Start", outcome, err, UsageActivationNotRunning)
		}
		if p.usageDeriver.Load() != nil {
			t.Fatal("ActivateUsageSource must not store a deriver on a pipeline that is not running")
		}
	})
	t.Run("nil_pipeline", func(t *testing.T) {
		unsetUsageSource(t)
		var p *Pipeline
		if outcome, err := p.ActivateUsageSource(context.Background(), UsageSourceNative); err != nil || outcome != UsageActivationNotRunning {
			t.Fatalf("got %q, %v; want %q, nil", outcome, err, UsageActivationNotRunning)
		}
	})
}

// claudeAPIRequestLogs builds one native Claude api_request event with a
// unique request_id, so distinct events never collide in the dedupe LRU.
func claudeAPIRequestLogs(requestID string) []*logspb.ResourceLogs {
	attr := func(k, v string) *commonpb.KeyValue {
		return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v}}}
	}
	return []*logspb.ResourceLogs{{
		ScopeLogs: []*logspb.ScopeLogs{{
			Scope: &commonpb.InstrumentationScope{Name: "com.anthropic.claude_code.events"},
			LogRecords: []*logspb.LogRecord{{
				TimeUnixNano: uint64(time.Now().UnixNano()),
				Attributes: []*commonpb.KeyValue{
					attr("event.name", "api_request"),
					attr("request_id", requestID),
					attr("model", "test-model"),
					attr("input_tokens", "2"),
					attr("output_tokens", "3"),
				},
			}},
		}},
	}}
}

// TestPipelineActivateUsageSourceWhileEventsFlow activates native usage while
// native events are being handled concurrently. The swap must not panic or
// fail a request: events handled before the swap hit the no-op deriver and
// derive nothing, and once the swap is done every further event is derived
// exactly once.
func TestPipelineActivateUsageSourceWhileEventsFlow(t *testing.T) {
	t.Setenv("SCION_AGENT_ID", "activate-flow-agent")
	t.Setenv("SCION_PROJECT_ID", "activate-flow-project")
	p := startPipelineWithoutUsageSource(t, "claude")
	ctx := context.Background()

	const workers, perWorker = 4, 25
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, workers*perWorker)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			<-start
			for i := 0; i < perWorker; i++ {
				if err := p.handleLogs(ctx, claudeAPIRequestLogs(fmt.Sprintf("concurrent-%d-%d", w, i))); err != nil {
					errs <- err
				}
			}
		}(w)
	}
	close(start)
	outcome, err := p.ActivateUsageSource(ctx, UsageSourceNative)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("handleLogs during activation: %v", err)
	}
	if err != nil || outcome != UsageActivated {
		t.Fatalf("ActivateUsageSource = %q, %v; want %q, nil", outcome, err, UsageActivated)
	}

	during := p.UsageDiagnostics()
	// Sanity bound only: the replaced deriver is a no-op, so no event can be
	// derived twice across the swap and this cannot realistically fail. The
	// test's value is the no-panic/no-error run under concurrency (with
	// -race, the absence of a data race) and the exact post-swap count below.
	// Emitted points are not compared to Derived here: concurrent delta
	// flushes may aggregate same-attribute points, so the counts differ.
	if during.Derived > workers*perWorker {
		t.Fatalf("derived %d usage events from %d sent: an event was counted twice", during.Derived, workers*perWorker)
	}
	if during.Duplicate != 0 {
		t.Fatalf("duplicate = %d, want 0 for distinct events", during.Duplicate)
	}

	const after = 5
	for i := 0; i < after; i++ {
		if err := p.handleLogs(ctx, claudeAPIRequestLogs(fmt.Sprintf("after-%d", i))); err != nil {
			t.Fatal(err)
		}
	}
	if got := p.UsageDiagnostics().Derived - during.Derived; got != after {
		t.Fatalf("derived %d events after activation, want exactly %d", got, after)
	}
}

// TestPipelineActivateUsageSourceCopilotMetrics sends a copilot metric batch
// through handleMetrics before and after a late activation. It pins that
// handleMetrics reloads the deriver (and its HasMetricRule) per request
// rather than latching Start's no-op deriver.
func TestPipelineActivateUsageSourceCopilotMetrics(t *testing.T) {
	p := startPipelineWithoutUsageSource(t, "copilot")
	ctx := context.Background()

	if err := p.handleMetrics(ctx, copilotUsageBatchWithCalls()); err != nil {
		t.Fatalf("handleMetrics before activation: %v", err)
	}
	if got := p.UsageDiagnostics().Derived; got != 0 {
		t.Fatalf("derived %d before activation, want 0", got)
	}

	outcome, err := p.ActivateUsageSource(ctx, UsageSourceNative)
	if err != nil || outcome != UsageActivated {
		t.Fatalf("ActivateUsageSource = %q, %v; want %q, nil", outcome, err, UsageActivated)
	}
	if err := p.handleMetrics(ctx, copilotUsageBatchWithCalls()); err != nil {
		t.Fatalf("handleMetrics after activation: %v", err)
	}
	if got := p.UsageDiagnostics().Derived; got == 0 {
		t.Fatal("copilot metrics after activation derived nothing; handleMetrics must use the activated deriver")
	}
}
