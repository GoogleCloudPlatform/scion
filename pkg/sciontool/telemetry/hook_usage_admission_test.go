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

// TestHookUsageTokensPassStrictGCPCloudAdmission is a regression test for
// round 1 review finding H1 (fork PR ptone/scion#2150): the hook handler's
// scion.usage.tokens points used to carry agent_id and project_id (from
// metricAttrs()'s baseAttrs), but GCP admission's allowlist for that one
// metric is narrower (cloudUsageTokenFields = harness, model, token_type;
// see gcp_metric_identity.go), so the whole OTLP request -- including that
// flush's gen_ai.api.calls, agent.tool.calls and agent.session.count points
// -- was rejected. On 69049bd7f (phase 2 round 1, the regression this
// fixes), a point built the old way fails admission with "unsupported Cloud
// Monitoring point dimension"; the shape built by
// telemetrycontract.UsageTokenPointAttrs (what the fixed
// hooks/handlers.recordTokenMetrics now emits) passes.
//
// This uses the shared-helper alternative the review offered, not an
// external test package driving the real handler: pkg/sciontool/telemetry
// can't import pkg/sciontool/hooks/handlers from a same-package (white-box)
// test, because handlers already imports telemetry and Go rejects that as
// an import cycle in the test binary ("import cycle not allowed in test").
// An external "telemetry_test" package could import handlers, but couldn't
// reach the unexported receiverPolicy/metricStreams/gcpIdentityMetrics this
// test needs to drive real admission. So instead, both
// hooks/handlers.recordTokenMetrics and this test build the point's
// producer label set from the single shared
// telemetrycontract.UsageTokenPointAttrs helper (see its doc comment): if
// that helper ever regresses to include a forbidden key, this test fails
// against the real admission code the exporter uses, the same way it would
// have caught H1.
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
	// cached=150, "cached" mapping to the canonical cache_read).
	meter := providers.MeterProvider.Meter(hookMetricScope)
	tokens, err := meter.Int64Counter(telemetrycontract.MetricUsageTokens, metric.WithUnit("{token}"))
	if err != nil {
		t.Fatal(err)
	}
	var baseAttrs []attribute.KeyValue
	for _, kv := range telemetrycontract.UsageTokenPointAttrs("muse-code", "model") {
		baseAttrs = append(baseAttrs, attribute.String(kv.Key, kv.Value))
	}
	for tokenType, n := range map[string]int64{
		telemetrycontract.TokenTypeInput:     1200,
		telemetrycontract.TokenTypeOutput:    400,
		telemetrycontract.TokenTypeCacheRead: 150,
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
