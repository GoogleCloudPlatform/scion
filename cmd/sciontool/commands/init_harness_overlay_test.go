/*
Copyright 2025 The Scion Authors.
*/

package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/telemetry"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// claudeUsageFixture is the recorded Claude native OTLP log export the
// telemetry package's usage tests derive from.
const claudeUsageFixture = "../../../pkg/sciontool/telemetry/testdata/usage/claude-2.1.280.pb.json"

func freeLoopbackPort(t *testing.T) int {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := lis.Addr().(*net.TCPAddr).Port
	if err := lis.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}

// postClaudeUsageFixture sends the native Claude usage events to the
// pipeline's OTLP/HTTP receiver, as the harness child would.
func postClaudeUsageFixture(t *testing.T, httpPort int) {
	t.Helper()
	data, err := os.ReadFile(claudeUsageFixture)
	if err != nil {
		t.Fatal(err)
	}
	var req collogspb.ExportLogsServiceRequest
	if err := protojson.Unmarshal(data, &req); err != nil {
		t.Fatal(err)
	}
	body, err := proto.Marshal(&req)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post("http://127.0.0.1:"+strconv.Itoa(httpPort)+"/v1/logs", "application/x-protobuf", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("OTLP post status = %d", resp.StatusCode)
	}
}

// TestLoadHarnessEnvOverlayActivatesUsageDeriver pins the RunInit side of
// the AC-1.5 fix (ptone/scion#3511): init starts telemetry before pre-start
// provisioning, so SCION_USAGE_SOURCE=native reaches it only through the
// harness env overlay. Loading the overlay must hand it to the running
// pipeline so native usage is derived, without touching init's own env.
func TestLoadHarnessEnvOverlayActivatesUsageDeriver(t *testing.T) {
	httpPort := freeLoopbackPort(t)
	t.Setenv(telemetry.EnvEnabled, "true")
	t.Setenv(telemetry.EnvCloudEnabled, "false")
	t.Setenv(telemetry.EnvGRPCPort, "0")
	t.Setenv(telemetry.EnvHTTPPort, strconv.Itoa(httpPort))
	t.Setenv(telemetry.EnvHarness, "claude")
	t.Setenv(telemetry.EnvUsageSource, "")
	t.Setenv(hooks.HarnessOutputsDirEnv, "")
	t.Setenv(hooks.HarnessSecretsDirEnv, "")

	pipeline := telemetry.New()
	if pipeline == nil {
		t.Fatal("expected a telemetry pipeline")
	}
	if err := pipeline.Start(context.Background()); err != nil {
		t.Fatalf("start telemetry: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = pipeline.Stop(ctx)
	})

	// Before the overlay is loaded the deriver only saw init's env.
	postClaudeUsageFixture(t, httpPort)
	if diag := pipeline.UsageDiagnostics(); diag.Derived != 0 {
		t.Fatalf("precondition: usage derived before the overlay was loaded: %+v", diag)
	}

	agentHome := t.TempDir()
	bundleDir := filepath.Join(agentHome, ".scion", "harness")
	outputs := filepath.Join(bundleDir, "outputs")
	if err := os.MkdirAll(outputs, 0o755); err != nil {
		t.Fatal(err)
	}
	overlayJSON, err := json.Marshal(map[string]string{
		telemetry.EnvUsageSource:       "native",
		hooks.NativeTelemetryPolicyKey: "enabled",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outputs, "env.json"), overlayJSON, 0o600); err != nil {
		t.Fatal(err)
	}
	req := hooks.HarnessManifestRequirement{
		Required:       true,
		EnvOverlayPath: "$HOME/.scion/harness/outputs/env.json",
		BundleDir:      bundleDir,
	}

	overlay, policy, err := loadHarnessEnvOverlay(req, agentHome, pipeline)
	if err != nil {
		t.Fatalf("load overlay: %v", err)
	}
	if overlay[telemetry.EnvUsageSource] != "native" {
		t.Fatalf("overlay = %v", overlay)
	}
	if _, ok := overlay[hooks.NativeTelemetryPolicyKey]; ok || policy != "enabled" {
		t.Fatalf("policy marker not extracted: overlay = %v, policy = %q", overlay, policy)
	}

	postClaudeUsageFixture(t, httpPort)
	if diag := pipeline.UsageDiagnostics(); diag.Derived == 0 {
		t.Fatalf("loading the harness overlay must activate native usage derivation: %+v", diag)
	}
	if v := os.Getenv(telemetry.EnvUsageSource); v != "" {
		t.Fatalf("SCION_USAGE_SOURCE leaked into init's env: %q", v)
	}
}

// TestLoadHarnessEnvOverlayFailures pins the startup-abort contract the
// extraction from RunInit must preserve.
func TestLoadHarnessEnvOverlayFailures(t *testing.T) {
	t.Setenv(hooks.HarnessOutputsDirEnv, "")
	t.Setenv(hooks.HarnessSecretsDirEnv, "")
	write := func(t *testing.T, content string) (hooks.HarnessManifestRequirement, string) {
		t.Helper()
		agentHome := t.TempDir()
		bundleDir := filepath.Join(agentHome, ".scion", "harness")
		outputs := filepath.Join(bundleDir, "outputs")
		if err := os.MkdirAll(outputs, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(outputs, "env.json"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return hooks.HarnessManifestRequirement{EnvOverlayPath: "$HOME/.scion/harness/outputs/env.json", BundleDir: bundleDir}, agentHome
	}

	t.Run("malformed overlay aborts a required harness", func(t *testing.T) {
		req, home := write(t, "{not json")
		req.Required = true
		if _, _, err := loadHarnessEnvOverlay(req, home, nil); err == nil {
			t.Fatal("expected an error")
		}
	})
	t.Run("malformed overlay is ignored for an optional harness", func(t *testing.T) {
		req, home := write(t, "{not json")
		overlay, _, err := loadHarnessEnvOverlay(req, home, nil)
		if err != nil || overlay != nil {
			t.Fatalf("overlay = %v, err = %v", overlay, err)
		}
	})
	t.Run("invalid policy marker aborts", func(t *testing.T) {
		req, home := write(t, `{"`+hooks.NativeTelemetryPolicyKey+`": "maybe"}`)
		if _, _, err := loadHarnessEnvOverlay(req, home, nil); err == nil {
			t.Fatal("expected an error")
		}
	})
	t.Run("no overlay declared", func(t *testing.T) {
		overlay, policy, err := loadHarnessEnvOverlay(hooks.HarnessManifestRequirement{}, t.TempDir(), nil)
		if err != nil || overlay != nil || policy != "" {
			t.Fatalf("overlay = %v, policy = %q, err = %v", overlay, policy, err)
		}
	})
}
