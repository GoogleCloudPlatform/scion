/*
Copyright 2026 The Scion Authors.
*/

package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// TestStartTelemetryTLSInvalidEnvWarns checks that an invalid OTLP TLS
// setting in the final agent env (here a client certificate without its
// key) gives a start warning, not a failed start. In the container
// sciontool runs the agent without telemetry in that case.
func TestStartTelemetryTLSInvalidEnvWarns(t *testing.T) {
	for _, tc := range []struct {
		name        string
		env         string
		wantWarning bool
	}{
		{"certificate without key", `"SCION_OTEL_CERT_FILE": "/certs/client.pem"`, true},
		{"complete pair", `"SCION_OTEL_CERT_FILE": "/certs/client.pem", "SCION_OTEL_KEY_FILE": "/certs/client-key.pem"`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			t.Chdir(tmpDir)
			t.Setenv("HOME", tmpDir)
			// An empty SCION_ variable is unset for the settings loader, so
			// values from the surrounding environment cannot leak in.
			for _, name := range []string{"SCION_OTEL_INSECURE", "SCION_OTEL_SKIP_TLS_VERIFY", "SCION_OTEL_CA_FILE", "SCION_OTEL_CERT_FILE", "SCION_OTEL_KEY_FILE"} {
				t.Setenv(name, "")
			}

			globalScionDir := filepath.Join(tmpDir, ".scion")
			hcDir := filepath.Join(globalScionDir, "harness-configs", "test-harness")
			mustMkdirAll(t, hcDir)
			writeTestFile(t, filepath.Join(hcDir, "config.yaml"), "harness: gemini\nuser: scion\nimage: test-image:latest\n")
			tplDir := filepath.Join(globalScionDir, "templates", "default")
			mustMkdirAll(t, tplDir)
			writeTestFile(t, filepath.Join(tplDir, "scion-agent.json"), `{"default_harness_config": "test-harness"}`)
			writeTestFile(t, filepath.Join(globalScionDir, "settings.yaml"), "schema_version: \"1\"\nactive_profile: local\nprofiles:\n  local:\n    runtime: docker\n")

			projectScionDir := filepath.Join(tmpDir, "project", ".scion")
			agentDir := filepath.Join(projectScionDir, "agents", "tls-test")
			mustMkdirAll(t, filepath.Join(agentDir, "home"))
			writeTestFile(t, filepath.Join(agentDir, "scion-agent.json"), `{"harness": "generic", "env": {`+tc.env+`}}`)

			ran := false
			mockRT := &runtime.MockRuntime{
				ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
					return []api.AgentInfo{}, nil
				},
				RunFunc: func(ctx context.Context, config runtime.RunConfig) (string, error) {
					ran = true
					return "mock-id", nil
				},
			}

			info, err := NewManager(mockRT).Start(context.Background(), api.StartOptions{
				Name:        "tls-test",
				ProjectPath: projectScionDir,
				BrokerMode:  true,
				NoAuth:      true,
			})
			if err != nil {
				t.Fatalf("Start must not fail on telemetry TLS settings, got: %v", err)
			}
			if !ran {
				t.Fatal("the runtime was not asked to run the agent")
			}
			var got bool
			for _, w := range info.Warnings {
				if strings.Contains(w, "cert_file and key_file must be set together") {
					got = true
				}
			}
			if got != tc.wantWarning {
				t.Fatalf("telemetry TLS warning present = %t, want %t (warnings: %v)", got, tc.wantWarning, info.Warnings)
			}
		})
	}
}

func writeTestFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}
