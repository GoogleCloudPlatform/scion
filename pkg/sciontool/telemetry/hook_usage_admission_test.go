package telemetry

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	metricpb "go.opentelemetry.io/proto/otlp/metrics/v1"
)

// pointAttrsForTest converts contract label pairs into OTel attributes,
// mirroring hooks/handlers.toOTelAttrs. Duplicated rather than shared: this
// test lives in package telemetry, which hooks/handlers already imports, so
// importing handlers back here for the helper would be a cycle.
func pointAttrsForTest(kvs []telemetrycontract.LabelKV) []attribute.KeyValue {
	attrs := make([]attribute.KeyValue, len(kvs))
	for i, kv := range kvs {
		attrs[i] = attribute.String(kv.Key, kv.Value)
	}
	return attrs
}

// TestHookUsageTokensPassStrictGCPCloudAdmission asserts that a
// scion.usage.tokens point built from telemetrycontract.UsageTokenPointAttrs
// -- the exact shape hooks/handlers.recordTokenMetrics emits -- survives
// real GCP admission (receiver -> policy -> metricStreams{gcp:true} ->
// gcpIdentityMetrics). MetricUsageTokens's GCP allowlist
// (cloudUsageTokenFields = harness, model, token_type; gcp_metric_identity.go)
// is narrower than the general hook-counter allowlist, so a point carrying
// any other key, such as agent_id or project_id, gets the whole OTLP request
// rejected -- taking that flush's gen_ai.api.calls, agent.tool.calls and
// agent.session.count points down with it.
//
// This guards the shared UsageTokenPointAttrs helper against real admission,
// not the handler's use of it: pkg/sciontool/telemetry can't import
// pkg/sciontool/hooks/handlers from a same-package (white-box) test, because
// handlers already imports telemetry and Go rejects that as an import cycle
// in the test binary ("import cycle not allowed in test"). An external
// "telemetry_test" package could import handlers, but couldn't reach the
// unexported receiverPolicy/metricStreams/gcpIdentityMetrics this test needs
// to drive real admission. hooks/handlers.TestTelemetryHandler_UsageTokenLabelsMatchContract
// is the complementary handler-side guard: it asserts the handler emits
// exactly UsageTokenPointAttrs' keys plus token_type, nothing more. Together
// the two pin the end-to-end behavior even though neither package can import
// the other in tests.
//
// The token values mirror the muse-code PostLLMCall fixture
// (hooks/handlers/muse_dialect_test.go's TestMuseCodeHookTokensReachUsageMetric),
// which proves the mapping is correct at the SDK level; this test completes
// that AC ("muse-code hook tokens ... reach the dashboard", design §5/§9
// Phase 2) by proving the resulting point shape survives GCP admission.
func TestHookUsageTokensPassStrictGCPCloudAdmission(t *testing.T) {
	for key, value := range map[string]string{
		"SCION_AGENT_ID": "agent", "SCION_AGENT_SLUG": "slug", "SCION_PROJECT_ID": "project",
		"SCION_HARNESS": "muse-code", "SCION_MODEL": "model",
		EnvProjectID: "cloud-project",
	} {
		t.Setenv(key, value)
	}

	port := availableTCPPort(t)
	cfg := &Config{Enabled: true, CloudProvider: "gcp", GRPCPort: port}
	results := make(chan error, 1)
	receiver := NewReceiver(cfg, nil, WithMetricHandler(func(_ context.Context, rms []*metricpb.ResourceMetrics) error {
		decision := newReceiverPolicy(cfg).processMetrics(rms)
		if decision.Reason != "" {
			results <- fmt.Errorf("policy: %s", decision.Reason)
			return nil
		}
		state := newMetricStreams()
		state.gcp = true
		if err := state.add(decision.Data); err != nil {
			results <- err
			return nil
		}
		_, err := gcpIdentityMetrics(state.snapshot())
		results <- err
		return nil
	}))
	if err := receiver.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = receiver.Stop(context.Background()) }()

	providers, err := NewProviders(context.Background(), cfg, false)
	if err != nil {
		t.Fatal(err)
	}

	// Mirrors hooks/handlers.recordTokenMetrics exactly: the same helper,
	// the same hook instrumentation scope and unit, one point per token
	// type present (muse-code's PostLLMCall: input=1200, output=400,
	// cached=150, "cached" mapping to the canonical cache_read). cache_write
	// and reasoning are included too (ptone/scion#2053 phase 2's hook token
	// plumbing, GoogleCloudPlatform/scion#2057 review): the closed
	// token_type enum already covers them, so this pins that the narrower
	// scion.usage.tokens allowlist admits every enum member, not just the
	// three a harness happened to populate first.
	meter := providers.MeterProvider.Meter(hookMetricScope)
	tokens, err := meter.Int64Counter(telemetrycontract.MetricUsageTokens, metric.WithUnit("{token}"))
	if err != nil {
		t.Fatal(err)
	}
	baseAttrs := pointAttrsForTest(telemetrycontract.UsageTokenPointAttrs("muse-code", "model"))
	for tokenType, n := range map[string]int64{
		telemetrycontract.TokenTypeInput:      1200,
		telemetrycontract.TokenTypeOutput:     400,
		telemetrycontract.TokenTypeCacheRead:  150,
		telemetrycontract.TokenTypeCacheWrite: 75,
		telemetrycontract.TokenTypeReasoning:  60,
	} {
		attrs := append(append([]attribute.KeyValue{}, baseAttrs...), attribute.String(telemetrycontract.TokenTypeLabel, tokenType))
		tokens.Add(context.Background(), n, metric.WithAttributes(attrs...))
	}

	if err := providers.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-results:
		if err != nil {
			t.Fatalf("hook-emitted scion.usage.tokens points rejected by GCP admission: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("hook metric export did not reach receiver")
	}
}
