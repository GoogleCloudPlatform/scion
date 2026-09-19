package harness

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	harnessFS "github.com/GoogleCloudPlatform/scion/harnesses"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/supervisor"
)

// This joins the bundled provisioner output to the real supervisor child env.
// It verifies routing configuration, not emission by a vendor binary.
func TestNativeTelemetryProvisionedChildEnv(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 unavailable")
	}
	for _, harnessName := range []string{"claude", "claude-gcp", "gemini-cli", "codex"} {
		for _, enabled := range []bool{true, false} {
			bundleHarnessName := harnessName
			if harnessName == "claude-gcp" {
				bundleHarnessName = "claude"
			}
			name := harnessName + "/disabled"
			if enabled {
				name = harnessName + "/enabled"
			}
			t.Run(name, func(t *testing.T) {
				home := t.TempDir()
				bundle := filepath.Join(home, ".scion", "harness")
				if err := os.MkdirAll(filepath.Join(bundle, "inputs"), 0755); err != nil {
					t.Fatal(err)
				}
				for _, file := range []string{"provision.py", "scion_harness.py"} {
					data, err := fs.ReadFile(harnessFS.FS, bundleHarnessName+"/"+file)
					if err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(bundle, file), data, 0755); err != nil {
						t.Fatal(err)
					}
				}
				telemetry := map[string]any{"enabled": enabled}
				if bundleHarnessName == "claude" {
					provider := "otlp"
					if harnessName == "claude-gcp" {
						provider = "gcp"
					}
					telemetry["cloud"] = map[string]any{"provider": provider}
				}
				input, _ := json.Marshal(map[string]any{"telemetry": telemetry, "env": map[string]string{"SCION_OTEL_GRPC_PORT": "14317"}})
				if err := os.WriteFile(filepath.Join(bundle, "inputs", "telemetry.json"), input, 0644); err != nil {
					t.Fatal(err)
				}
				manifest, _ := json.Marshal(map[string]any{
					"harness_bundle_dir": bundle,
					"harness_config":     map[string]any{"no_auth": map[string]string{"behavior": "allow"}},
				})
				manifestPath := filepath.Join(bundle, "manifest.json")
				if err := os.WriteFile(manifestPath, manifest, 0644); err != nil {
					t.Fatal(err)
				}
				cmd := exec.Command(python, filepath.Join(bundle, "provision.py"), "--manifest", manifestPath)
				cmd.Env = []string{"HOME=" + home, "PATH=" + os.Getenv("PATH"), "PYTHONDONTWRITEBYTECODE=1"}
				if output, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("provision: %v: %s", err, output)
				}
				data, err := os.ReadFile(filepath.Join(bundle, "outputs", "env.json"))
				if err != nil {
					t.Fatal(err)
				}
				var overlay map[string]string
				if err := json.Unmarshal(data, &overlay); err != nil {
					t.Fatal(err)
				}
				policy := overlay[hooks.NativeTelemetryPolicyKey]
				delete(overlay, hooks.NativeTelemetryPolicyKey)
				wantPolicy := "disabled"
				if enabled {
					wantPolicy = "enabled"
				}
				if policy != wantPolicy {
					t.Fatalf("policy=%q, want %q", policy, wantPolicy)
				}
				out := filepath.Join(home, "child-env")
				cfg := supervisor.DefaultConfig()
				cfg.NativeTelemetryPolicy = policy
				cfg.EnvOverlay = overlay
				code, err := supervisor.New(cfg).Run(context.Background(), []string{"sh", "-c", `env > "` + out + `"`})
				if code != 0 || err != nil {
					t.Fatalf("child code=%d err=%v", code, err)
				}
				child, err := os.ReadFile(out)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(child), hooks.NativeTelemetryPolicyKey+"=") {
					t.Fatal("policy marker leaked to child")
				}
				if harnessName == "gemini-cli" && !strings.Contains(string(child), "GEMINI_TELEMETRY_TRACES_ENABLED=false\n") {
					t.Fatal("Gemini detailed traces are not disabled in child env")
				}
				if harnessName == "claude-gcp" {
					if !strings.Contains(string(child), "OTEL_METRICS_EXPORTER=none\n") {
						t.Fatal("GCP Claude metrics were not disabled in child env")
					}
					wantLogs := "OTEL_LOGS_EXPORTER=none\n"
					if enabled {
						wantLogs = "OTEL_LOGS_EXPORTER=otlp\n"
					}
					if !strings.Contains(string(child), wantLogs) {
						t.Fatal("GCP Claude log exporter mismatch in child env")
					}
					blocked := filepath.Join(home, "blocked-child")
					t.Setenv("OTEL_METRICS_EXPORTER", "otlp")
					if code, err := supervisor.New(cfg).Run(context.Background(), []string{"sh", "-c", `touch "` + blocked + `"`}); err == nil || code == 0 {
						t.Fatalf("inherited metric exporter started child: code=%d err=%v", code, err)
					}
					if _, err := os.Stat(blocked); !os.IsNotExist(err) {
						t.Fatalf("child ran after inherited conflict: %v", err)
					}
					// The same generated overlay must reject fetched secrets that
					// would undo the GCP logs-first decision.
					cfg.SecretOverrides = map[string]string{"OTEL_METRICS_EXPORTER": "otlp"}
					if code, err := supervisor.New(cfg).Run(context.Background(), []string{"sh", "-c", `touch "` + blocked + `"`}); err == nil || code == 0 {
						t.Fatalf("secret metric exporter started child: code=%d err=%v", code, err)
					}
				}
				for key, value := range overlay {
					if key == "CODEX_HOME" || strings.HasPrefix(key, "OTEL_") || strings.HasPrefix(key, "GEMINI_TELEMETRY_") || key == "CLAUDE_CODE_ENABLE_TELEMETRY" {
						if !strings.Contains(string(child), key+"="+value+"\n") {
							t.Fatalf("child missing generated %s", key)
						}
					}
				}
			})
		}
	}
}
