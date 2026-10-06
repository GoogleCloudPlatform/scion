/*
Copyright 2026 The Scion Authors.
*/

package commands

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/telemetry"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// unsetEnvForTest removes key from the process environment for the duration
// of the test (t.Setenv registers the restore). Setting it to "" is not the
// same: an empty value is still a present key.
func unsetEnvForTest(t *testing.T, key string) {
	t.Helper()
	t.Setenv(key, "")
	if err := os.Unsetenv(key); err != nil {
		t.Fatal(err)
	}
}

// TestInitOverlayUsageSourceActivatesNativeUsage is the init lifecycle
// regression test for ptone/scion#3391. It drives loadHarnessEnvOverlay, the
// step RunInit calls to load, validate and hand over the provisioner's env
// overlay, in RunInit's order: the telemetry pipeline starts with
// SCION_USAGE_SOURCE unset, the provisioner's generated overlay is then
// loaded from the manifest's declared path, and only after that does a
// native Claude api_request event arrive. When native usage is selected the
// canonical counters must be derived (1 call point + 4 token-type points).
func TestInitOverlayUsageSourceActivatesNativeUsage(t *testing.T) {
	for _, tc := range []struct {
		name string
		// runtime, when non-nil, is SCION_USAGE_SOURCE in init's own
		// environment; nil means the variable is genuinely unset.
		runtime    *string
		overlay    map[string]string
		wantPoints int64
		wantErr    string
	}{
		{
			name:       "native_from_overlay",
			overlay:    map[string]string{"SCION_USAGE_SOURCE": "native", hooks.NativeTelemetryPolicyKey: "enabled"},
			wantPoints: 5,
		},
		{
			name:       "native_without_policy_marker",
			overlay:    map[string]string{"SCION_USAGE_SOURCE": "native"},
			wantPoints: 5,
		},
		{
			name:    "native_with_disabled_policy",
			overlay: map[string]string{"SCION_USAGE_SOURCE": "native", hooks.NativeTelemetryPolicyKey: "disabled"},
		},
		{
			name:    "runtime_empty_value_takes_precedence",
			runtime: new(string),
			overlay: map[string]string{"SCION_USAGE_SOURCE": "native"},
		},
		{
			name:    "hooks_source",
			overlay: map[string]string{"SCION_USAGE_SOURCE": "hooks"},
		},
		{
			name:    "unsupported_source",
			overlay: map[string]string{"SCION_USAGE_SOURCE": "Native"},
		},
		{
			name:    "no_source",
			overlay: map[string]string{"OTHER": "value"},
		},
		{
			name:    "invalid_policy_marker",
			overlay: map[string]string{"SCION_USAGE_SOURCE": "native", hooks.NativeTelemetryPolicyKey: "bogus"},
			wantErr: "invalid native telemetry policy marker",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SCION_AGENT_ID", "init-usage-agent")
			t.Setenv("SCION_PROJECT_ID", "init-usage-project")
			t.Setenv("SCION_HARNESS", "claude")
			unsetEnvForTest(t, hooks.HarnessOutputsDirEnv)
			unsetEnvForTest(t, hooks.HarnessSecretsDirEnv)
			if tc.runtime != nil {
				t.Setenv("SCION_USAGE_SOURCE", *tc.runtime)
			} else {
				unsetEnvForTest(t, "SCION_USAGE_SOURCE")
			}

			// Port 0: the receiver binds an ephemeral port, which Config()
			// reports once Start has bound it.
			pipeline := telemetry.NewWithConfig(&telemetry.Config{Enabled: true})
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			if err := pipeline.Start(ctx); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = pipeline.Stop(context.Background()) }()
			port := pipeline.Config().GRPCPort

			// The provisioner runs after the pipeline is up and writes its
			// overlay to the path the harness manifest declares.
			agentHome := t.TempDir()
			bundleDir := filepath.Join(agentHome, ".scion", "harness")
			if err := os.MkdirAll(filepath.Join(bundleDir, "outputs"), 0o700); err != nil {
				t.Fatal(err)
			}
			data, err := json.Marshal(tc.overlay)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(bundleDir, "outputs", "env.json"), data, 0o600); err != nil {
				t.Fatal(err)
			}
			req := hooks.HarnessManifestRequirement{
				Required:       true,
				EnvOverlayPath: "$HOME/.scion/harness/outputs/env.json",
				BundleDir:      bundleDir,
			}

			overlay, policy, err := loadHarnessEnvOverlay(ctx, req, agentHome, pipeline)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("loadHarnessEnvOverlay error = %v, want %q", err, tc.wantErr)
				}
				// A fresh activation succeeding proves loadHarnessEnvOverlay
				// activated nothing before rejecting the overlay.
				if outcome, err := pipeline.ActivateUsageSource(ctx, telemetry.UsageSourceNative); err != nil || outcome != telemetry.UsageActivated {
					t.Fatalf("an invalid overlay must not activate usage, later ActivateUsageSource = %q, %v; want %q", outcome, err, telemetry.UsageActivated)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := overlay[hooks.NativeTelemetryPolicyKey]; ok {
				t.Fatal("the policy marker must be stripped from the child overlay")
			}
			if want := tc.overlay[hooks.NativeTelemetryPolicyKey]; policy != want {
				t.Fatalf("policy = %q, want %q", policy, want)
			}
			if got, want := overlay["SCION_USAGE_SOURCE"], tc.overlay["SCION_USAGE_SOURCE"]; got != want {
				t.Fatalf("child overlay SCION_USAGE_SOURCE = %q, want %q", got, want)
			}

			got, present := os.LookupEnv("SCION_USAGE_SOURCE")
			if tc.runtime == nil && present {
				t.Fatalf("handoff must not copy overlay values into the init environment, got SCION_USAGE_SOURCE=%q", got)
			}

			sendClaudeAPIRequest(ctx, t, port)

			wantDerived := int64(0)
			if tc.wantPoints > 0 {
				wantDerived = 1
			}
			if got := pipeline.UsageDiagnostics().Derived; got != wantDerived {
				t.Fatalf("usage derived = %d, want %d", got, wantDerived)
			}
			if got := pipeline.Diagnostics()["metrics"].Accepted; got != tc.wantPoints {
				t.Fatalf("metric points accepted = %d, want %d", got, tc.wantPoints)
			}
		})
	}
}

// TestLoadHarnessEnvOverlayNonRequiredLoadFailure pins the "log and launch
// without an overlay" contract: a malformed overlay for a non-required
// harness is not a fatal init error, yields no overlay or policy, and never
// activates native usage, even if the file names SCION_USAGE_SOURCE=native.
func TestLoadHarnessEnvOverlayNonRequiredLoadFailure(t *testing.T) {
	t.Setenv("SCION_HARNESS", "claude")
	unsetEnvForTest(t, hooks.HarnessOutputsDirEnv)
	unsetEnvForTest(t, hooks.HarnessSecretsDirEnv)
	unsetEnvForTest(t, "SCION_USAGE_SOURCE")

	pipeline := telemetry.NewWithConfig(&telemetry.Config{Enabled: true})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := pipeline.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pipeline.Stop(context.Background()) }()

	agentHome := t.TempDir()
	bundleDir := filepath.Join(agentHome, ".scion", "harness")
	if err := os.MkdirAll(filepath.Join(bundleDir, "outputs"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "outputs", "env.json"), []byte(`{"SCION_USAGE_SOURCE": "native"`), 0o600); err != nil {
		t.Fatal(err)
	}
	req := hooks.HarnessManifestRequirement{
		Required:       false,
		EnvOverlayPath: "$HOME/.scion/harness/outputs/env.json",
		BundleDir:      bundleDir,
	}

	overlay, policy, err := loadHarnessEnvOverlay(ctx, req, agentHome, pipeline)
	if err != nil {
		t.Fatalf("non-required load failure must not be fatal, got %v", err)
	}
	if overlay != nil || policy != "" {
		t.Fatalf("got overlay %v, policy %q; want nil, \"\"", overlay, policy)
	}
	if outcome, err := pipeline.ActivateUsageSource(ctx, telemetry.UsageSourceNative); err != nil || outcome != telemetry.UsageActivated {
		t.Fatalf("native usage must still be inactive after a failed overlay load, ActivateUsageSource = %q, %v", outcome, err)
	}
}

func sendClaudeAPIRequest(ctx context.Context, t *testing.T, port int) {
	t.Helper()
	conn, err := grpc.NewClient(net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	attr := func(k, v string) *commonpb.KeyValue {
		return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v}}}
	}
	record := &logspb.LogRecord{
		TimeUnixNano: uint64(time.Now().UnixNano()),
		Attributes: []*commonpb.KeyValue{
			attr("event.name", "api_request"),
			attr("model", "test-model"),
			attr("input_tokens", "2"),
			attr("output_tokens", "501"),
			attr("cache_read_tokens", "72623"),
			attr("cache_creation_tokens", "7212"),
		},
	}
	_, err = collogspb.NewLogsServiceClient(conn).Export(ctx, &collogspb.ExportLogsServiceRequest{
		ResourceLogs: []*logspb.ResourceLogs{{
			ScopeLogs: []*logspb.ScopeLogs{{
				Scope:      &commonpb.InstrumentationScope{Name: "com.anthropic.claude_code.events"},
				LogRecords: []*logspb.LogRecord{record},
			}},
		}},
	})
	if err != nil {
		t.Fatalf("export native event: %v", err)
	}
}

// TestLoadHarnessEnvOverlayStartupContract pins the two loadHarnessEnvOverlay
// outcomes the usage-source tests above do not reach: a malformed overlay for
// a required container-script harness must abort startup (the child would
// otherwise launch without its credentials), and a harness that declares no
// overlay yields nothing and no error.
func TestLoadHarnessEnvOverlayStartupContract(t *testing.T) {
	unsetEnvForTest(t, hooks.HarnessOutputsDirEnv)
	unsetEnvForTest(t, hooks.HarnessSecretsDirEnv)

	t.Run("malformed_overlay_aborts_required_harness", func(t *testing.T) {
		agentHome := t.TempDir()
		bundleDir := filepath.Join(agentHome, ".scion", "harness")
		if err := os.MkdirAll(filepath.Join(bundleDir, "outputs"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(bundleDir, "outputs", "env.json"), []byte("{not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		req := hooks.HarnessManifestRequirement{
			Required:       true,
			EnvOverlayPath: "$HOME/.scion/harness/outputs/env.json",
			BundleDir:      bundleDir,
		}
		_, _, err := loadHarnessEnvOverlay(context.Background(), req, agentHome, nil)
		if err == nil || !strings.Contains(err.Error(), "invalid harness env overlay") {
			t.Fatalf("loadHarnessEnvOverlay error = %v, want an invalid harness env overlay error", err)
		}
	})

	t.Run("no_overlay_declared", func(t *testing.T) {
		overlay, policy, err := loadHarnessEnvOverlay(context.Background(), hooks.HarnessManifestRequirement{}, t.TempDir(), nil)
		if err != nil || overlay != nil || policy != "" {
			t.Fatalf("got overlay %v, policy %q, err %v; want nil, \"\", nil", overlay, policy, err)
		}
	})
}
