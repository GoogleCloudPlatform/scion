/*
Copyright 2026 The Scion Authors.
*/

package telemetry

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
)

// containerHarnessCasesPath is the case table that pkg/agent's
// TestStart_ContainerHarnessCases checks Start against (ptone/scion#5946).
// It is read in place, not copied.
const containerHarnessCasesPath = "../../agent/testdata/harness_env_cases.json"

type containerHarnessCase struct {
	Name             string `json:"name"`
	ContainerHarness string `json:"container_harness"`
	StaleHarness     string `json:"stale_harness"`
	Fixture          string `json:"fixture"`
	WantUsage        string `json:"want_usage"`
	StaleUsage       string `json:"stale_usage"`
}

func loadContainerHarnessCases(t *testing.T) []containerHarnessCase {
	t.Helper()
	data, err := os.ReadFile(containerHarnessCasesPath)
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Cases []containerHarnessCase `json:"cases"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		t.Fatal(err)
	}
	if len(file.Cases) == 0 {
		t.Fatal("no cases")
	}
	return file.Cases
}

// Usage outcomes, as named in the case table.
const (
	usageParsed    = "parsed"    // rules selected, the capture yields derived usage
	usageUnmatched = "unmatched" // rules selected, the capture yields nothing
	usageUnknown   = "unknown"   // no rule for the harness: usage stays unknown
)

func loadCaptureFixture(t *testing.T, name string) []*logspb.ResourceLogs {
	t.Helper()
	switch name {
	case "claude":
		return loadClaudeUsageFixture(t)
	case "gemini-cli":
		return loadGeminiCLIUsageFixture(t)
	}
	t.Fatalf("no capture fixture %q", name)
	return nil
}

// usageOutcome runs the real native usage path for SCION_HARNESS=harness:
// NewUsageDeriver selects the rules from the environment as in production,
// and the selected rules then derive each capture record (observeRecord
// with the record's resource attributes, as ProcessResourceLogs does) into
// a manual reader and an agent.usage analytics outbox. It returns the
// outcome, the harness label of every recorded point and the harness field
// of every staged agent.usage event.
func usageOutcome(t *testing.T, harness, fixture string) (string, map[string]bool, map[string]bool) {
	t.Helper()
	t.Setenv("SCION_HARNESS", harness)
	t.Setenv("SCION_USAGE_SOURCE", UsageSourceNative)
	t.Setenv("SCION_AGENT_ID", "agent-1")
	t.Setenv("SCION_PROJECT_ID", "proj-1")
	selected, err := NewUsageDeriver(context.Background(), &Config{Enabled: true, GRPCPort: availableTCPPort(t)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { shutdownTestDeriver(selected) })
	if len(selected.rules) == 0 {
		return usageUnknown, nil, nil
	}

	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	meter := mp.Meter(usageMetricScope)
	d := bareUsageDeriver(selected.rules...)
	if d.calls, err = meter.Int64Counter(telemetrycontract.MetricAPICalls); err != nil {
		t.Fatal(err)
	}
	if d.tokens, err = meter.Int64Counter(telemetrycontract.MetricUsageTokens); err != nil {
		t.Fatal(err)
	}
	outbox, err := OpenAnalyticsOutbox(filepath.Join(t.TempDir(), "outbox"))
	if err != nil {
		t.Fatal(err)
	}
	d.analytics = NewUsageAnalytics(outbox, telemetrycontract.UsageSourceNative)
	// ProcessResourceLogs's loop, without its flush of the (absent)
	// production providers.
	for _, rl := range loadCaptureFixture(t, fixture) {
		for _, sl := range rl.GetScopeLogs() {
			for _, record := range sl.GetLogRecords() {
				d.observeRecord(context.Background(), sl.GetScope().GetName(), rl.GetResource().GetAttributes(), record)
			}
		}
	}
	events := map[string]bool{}
	for _, line := range stagingLines(t, outbox) {
		events[decodeUsage(t, line).Harness] = true
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	labels := map[string]bool{}
	points := 0
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				continue
			}
			for _, p := range sum.DataPoints {
				points++
				v, _ := p.Attributes.Value(attribute.Key(telemetrycontract.HarnessLabel))
				labels[v.AsString()] = true
			}
		}
	}
	if d.Diagnostics().Derived == 0 {
		if points != 0 {
			t.Fatalf("no derived usage but %d points recorded", points)
		}
		if len(events) != 0 {
			t.Fatalf("no derived usage but agent.usage events staged with harness %v", sortedKeys(events))
		}
		return usageUnmatched, nil, nil
	}
	return usageParsed, labels, events
}

// TestContainerHarnessCasesUsageOutcome checks, for each case of the shared
// table, the usage-parsing outcome of the SCION_HARNESS value Start now
// emits (container_harness) and of the value it emitted before
// ptone/scion#5946 (stale_harness), over a real capture: a switch parses
// with the launched harness's rules (the stale name's rules match nothing);
// the Generic implementation selects no rules, so usage stays unknown
// rather than being parsed with the declared harness's rules; a legacy
// "gemini" name and an empty name select no rules both before and after.
// Derived points carry the emitted value as their harness label, and the
// agent.usage events staged from the same records carry it as their
// harness field; no event is staged when nothing is derived.
func TestContainerHarnessCasesUsageOutcome(t *testing.T) {
	for _, c := range loadContainerHarnessCases(t) {
		t.Run(c.Name, func(t *testing.T) {
			for _, side := range []struct {
				label, harness, want string
			}{
				{"emitted", c.ContainerHarness, c.WantUsage},
				{"stale", c.StaleHarness, c.StaleUsage},
			} {
				t.Run(side.label, func(t *testing.T) {
					got, labels, events := usageOutcome(t, side.harness, c.Fixture)
					if got != side.want {
						t.Fatalf("SCION_HARNESS=%q over the %s capture: usage %s, want %s", side.harness, c.Fixture, got, side.want)
					}
					if got == usageParsed && (len(labels) != 1 || !labels[side.harness]) {
						t.Fatalf("harness labels on derived points = %v, want only %q", sortedKeys(labels), side.harness)
					}
					if got == usageParsed && (len(events) != 1 || !events[side.harness]) {
						t.Fatalf("agent.usage harness fields = %v, want only %q", sortedKeys(events), side.harness)
					}
				})
			}
		})
	}
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestNoUsageRuleForGenericImplementation: "generic" names an
// implementation, not a vendor log format, so no usage rule may register
// for it; usage for a Generic-launched agent stays unknown.
func TestNoUsageRuleForGenericImplementation(t *testing.T) {
	for _, factory := range usageRuleFactories {
		if h := factory().Harness(); h == "generic" || h == "" {
			t.Fatalf("usage rule registered for harness %q", h)
		}
	}
	if rules := rulesForHarness("generic"); len(rules) != 0 {
		t.Fatalf("rulesForHarness(generic) = %d rules, want 0", len(rules))
	}
}

// TestContainerHarnessCasesResourceLabels: the resource attribute
// scion.harness (buildResource) and the authoritative identity
// (authoritativeIdentity) carry the emitted value for each case, including
// "generic". No other resource attribute depends on SCION_HARNESS: the two
// resources built for two harness values differ only in scion.harness.
// (buildResource is the only SCION_HARNESS reader in the provider setup, so
// no provider is chosen by harness.)
func TestContainerHarnessCasesResourceLabels(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range loadContainerHarnessCases(t) {
		if seen[c.ContainerHarness] {
			continue
		}
		seen[c.ContainerHarness] = true
		t.Run(c.ContainerHarness, func(t *testing.T) {
			t.Setenv("SCION_HARNESS", c.ContainerHarness)
			res, err := buildResource(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if v, ok := res.Set().Value("scion.harness"); !ok || v.AsString() != c.ContainerHarness {
				t.Fatalf("resource scion.harness = %q (present %v), want %q", v.AsString(), ok, c.ContainerHarness)
			}
			var identity string
			for _, kv := range authoritativeIdentity() {
				if kv.Key == "scion.harness" {
					identity = kv.Value.GetStringValue()
				}
			}
			if identity != c.ContainerHarness {
				t.Fatalf("authoritative scion.harness = %q, want %q", identity, c.ContainerHarness)
			}

			t.Setenv("SCION_HARNESS", c.ContainerHarness+"-other")
			other, err := buildResource(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			a, b := res.Set().ToSlice(), other.Set().ToSlice()
			if len(a) != len(b) {
				t.Fatalf("resource attribute count depends on SCION_HARNESS: %d vs %d", len(a), len(b))
			}
			for i := range a {
				if a[i].Key == "scion.harness" {
					continue
				}
				if a[i] != b[i] {
					t.Fatalf("resource attribute %q depends on SCION_HARNESS: %v vs %v", a[i].Key, a[i].Value.String(), b[i].Value.String())
				}
			}
		})
	}
}

// healthUsageFor builds the telemetry.health event a pipeline reports for
// SCION_HARNESS=harness and SCION_USAGE_SOURCE=source, with the usage
// source settled from the environment and, when the harness has native
// rules and the source is native, a deriver with those rules active (as
// Start installs one).
func healthUsageFor(t *testing.T, harness, source string) *telemetrycontract.HealthEvent {
	t.Helper()
	t.Setenv("SCION_HARNESS", harness)
	t.Setenv("SCION_USAGE_SOURCE", source)
	t.Setenv("SCION_AGENT_ID", "agent-1")
	t.Setenv("SCION_PROJECT_ID", "proj-1")
	p := NewWithConfig(&Config{Enabled: true})
	p.health.now, p.health.runID = time.Now, "run"
	if rules := rulesForHarness(harness); source == UsageSourceNative && len(rules) > 0 {
		p.usageDeriver.Store(bareUsageDeriver(rules...))
	}
	p.SettleUsageSource(nil, "")
	ev, err := buildHealthEvent(p.healthSnapshotInputs(telemetrycontract.HealthTriggerPeriodic))
	if err != nil {
		t.Fatal(err)
	}
	return ev
}

// TestContainerHarnessCasesHealthUsageCollection: the telemetry.health
// event reads SCION_HARNESS for its harness field and for whether a native
// usage rule exists. For each case of the shared table, the value Start now
// emits (and the stale value) is reported as the harness ("unknown" when
// empty), and with the native source the usage-collection state follows
// the usage outcome: enabled (source native) when the harness has rules,
// disabled with reason no_harness_rule when it has none, as for "generic".
func TestContainerHarnessCasesHealthUsageCollection(t *testing.T) {
	for _, c := range loadContainerHarnessCases(t) {
		t.Run(c.Name, func(t *testing.T) {
			for _, side := range []struct {
				label, harness, usage string
			}{
				{"emitted", c.ContainerHarness, c.WantUsage},
				{"stale", c.StaleHarness, c.StaleUsage},
			} {
				t.Run(side.label, func(t *testing.T) {
					ev := healthUsageFor(t, side.harness, UsageSourceNative)
					wantHarness := side.harness
					if wantHarness == "" {
						wantHarness = "unknown"
					}
					if ev.Harness != wantHarness {
						t.Fatalf("health harness = %q, want %q", ev.Harness, wantHarness)
					}
					wantState, wantReason := telemetrycontract.UsageCollectionEnabled, ""
					if side.usage == usageUnknown {
						wantState, wantReason = telemetrycontract.UsageCollectionDisabled, telemetrycontract.UsageDisabledNoHarnessRule
					}
					if ev.UsageCollection != wantState || ev.UsageDisabledReason != wantReason || ev.UsageSource != telemetrycontract.UsageSourceNative {
						t.Fatalf("SCION_HARNESS=%q: usage collection %q reason %q source %q, want %q %q %q",
							side.harness, ev.UsageCollection, ev.UsageDisabledReason, ev.UsageSource,
							wantState, wantReason, telemetrycontract.UsageSourceNative)
					}
				})
			}
		})
	}
}

// TestHealthUsageCollectionGenericHooks: with the hooks source, a
// Generic-launched agent reports usage collection enabled from hooks under
// harness "generic", with no static capability row (capability status
// unknown).
func TestHealthUsageCollectionGenericHooks(t *testing.T) {
	ev := healthUsageFor(t, "generic", telemetrycontract.UsageSourceHooks)
	if ev.Harness != "generic" || ev.UsageCollection != telemetrycontract.UsageCollectionEnabled ||
		ev.UsageSource != telemetrycontract.UsageSourceHooks || ev.UsageDisabledReason != "" ||
		ev.UsageCapabilityStatus != telemetrycontract.UsageCapabilityStatusUnknown {
		t.Fatalf("health = harness %q collection %q source %q reason %q capability status %q",
			ev.Harness, ev.UsageCollection, ev.UsageSource, ev.UsageDisabledReason, ev.UsageCapabilityStatus)
	}
}
