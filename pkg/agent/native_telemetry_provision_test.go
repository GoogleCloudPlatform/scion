package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/telemetry"
)

func TestAgentStartNativeTelemetryConflictHasNoRuntimeChild(t *testing.T) {
	for _, tc := range []struct {
		name       string
		cloud      *api.TelemetryCloudConfig
		env        map[string]string
		secrets    []api.ResolvedSecret
		wantReason string
	}{
		{
			name:       "explicit provider conflict",
			cloud:      &api.TelemetryCloudConfig{Provider: "otlp"},
			env:        map[string]string{"SCION_TELEMETRY_CLOUD_PROVIDER": "gcp"},
			wantReason: "conflicting telemetry cloud provider",
		},
		{
			name:       "late provider secret conflict",
			cloud:      &api.TelemetryCloudConfig{Endpoint: "https://generic.invalid/v1"},
			secrets:    []api.ResolvedSecret{{Name: "synthetic-provider", Type: "environment", Target: "SCION_TELEMETRY_CLOUD_PROVIDER", Value: "synthetic"}},
			wantReason: "late telemetry backend secret target: SCION_TELEMETRY_CLOUD_PROVIDER",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, entry := range os.Environ() {
				key, _, _ := strings.Cut(entry, "=")
				if strings.HasPrefix(key, "SCION_") || strings.HasPrefix(key, "OTEL_") || strings.HasPrefix(key, "GOOGLE_") || key == "CLOUDSDK_CORE_PROJECT" {
					t.Setenv(key, "")
					if err := os.Unsetenv(key); err != nil {
						t.Fatal(err)
					}
				}
			}
			home := t.TempDir()
			t.Setenv("HOME", home)
			globalScion := filepath.Join(home, ".scion")
			harnessDir := filepath.Join(globalScion, "harness-configs", "claude-cfg")
			if err := os.MkdirAll(harnessDir, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(harnessDir, "config.yaml"), []byte("harness: claude\nuser: scion\nimage: test-image:latest\nprovisioner:\n  type: container-script\n  interface_version: 1\n  command: [\"python3\", \"/home/scion/.scion/harness/provision.py\"]\n"), 0600); err != nil {
				t.Fatal(err)
			}
			templateDir := filepath.Join(globalScion, "templates", "default")
			if err := os.MkdirAll(templateDir, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(templateDir, "scion-agent.json"), []byte(`{"default_harness_config":"claude-cfg"}`), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(globalScion, "settings.yaml"), []byte("schema_version: \"1\"\nactive_profile: local\nprofiles:\n  local:\n    runtime: docker\n"), 0600); err != nil {
				t.Fatal(err)
			}
			projectScion := filepath.Join(home, "project", ".scion")
			if err := os.MkdirAll(projectScion, 0700); err != nil {
				t.Fatal(err)
			}
			enabled := true
			inline := &api.ScionConfig{Telemetry: &api.TelemetryConfig{Enabled: &enabled, Cloud: tc.cloud}}
			const agentName = "phase5-prechild-sentinel"
			_, _, _, effective, err := GetAgent(context.Background(), agentName, "default", "", "", projectScion, "", "", "", "", inline)
			if err != nil {
				t.Fatalf("resolve test fixture: %v", err)
			}
			if effective.Harness != "claude" || effective.Telemetry == nil || effective.Telemetry.Enabled == nil || !*effective.Telemetry.Enabled || effective.Telemetry.Cloud == nil || effective.Telemetry.Cloud.Provider != tc.cloud.Provider || effective.Telemetry.Cloud.Endpoint != tc.cloud.Endpoint {
				t.Fatal("test fixture did not resolve enabled Claude telemetry")
			}
			var runs, deletes, pulls int
			rt := &runtime.MockRuntime{
				ListFunc: func(context.Context, map[string]string) ([]api.AgentInfo, error) { return nil, nil },
				RunFunc: func(context.Context, runtime.RunConfig) (string, error) {
					runs++
					return "unexpected-child", nil
				},
				DeleteFunc:      func(context.Context, string) error { deletes++; return nil },
				PullImageFunc:   func(context.Context, string) error { pulls++; return nil },
				ImageExistsFunc: func(context.Context, string) (bool, error) { return true, nil },
			}
			mgr := NewManager(rt)
			defer mgr.Close()
			info, err := mgr.Start(context.Background(), api.StartOptions{
				Name: agentName, Template: "default", ProjectPath: projectScion,
				NoAuth: true, Env: tc.env, ResolvedSecrets: tc.secrets, InlineConfig: inline,
			})
			want := "failed to determine native telemetry backend: " + tc.wantReason
			if err == nil || err.Error() != want || info != nil {
				t.Fatalf("Start result: infoPresent=%t, err=%v, want error=%q", info != nil, err, want)
			}
			if runs != 0 || deletes != 0 || pulls != 0 {
				t.Fatalf("runtime child side effects: runs=%d deletes=%d pulls=%d", runs, deletes, pulls)
			}
		})
	}
}

func TestNativeTelemetryProvisionBackend(t *testing.T) {
	home := t.TempDir()
	cases := []struct {
		name    string
		cloud   *api.TelemetryCloudConfig
		env     map[string]string
		secrets []api.ResolvedSecret
		want    string
		wantErr bool
	}{
		{name: "nested gcp", cloud: &api.TelemetryCloudConfig{Provider: "gcp"}, want: "gcp"},
		{name: "override gcp", env: map[string]string{"SCION_TELEMETRY_CLOUD_PROVIDER": "gcp"}, want: "gcp"},
		{name: "matching", cloud: &api.TelemetryCloudConfig{Provider: "gcp"}, env: map[string]string{"SCION_TELEMETRY_CLOUD_PROVIDER": "gcp"}, want: "gcp"},
		{name: "conflict", cloud: &api.TelemetryCloudConfig{Provider: "otlp"}, env: map[string]string{"SCION_TELEMETRY_CLOUD_PROVIDER": "gcp"}, wantErr: true},
		{name: "late named secret", secrets: []api.ResolvedSecret{{Name: "scion-telemetry-gcp-credentials", Type: "file"}}, want: "gcp"},
		{name: "late provider secret", cloud: &api.TelemetryCloudConfig{Endpoint: "https://generic.invalid/v1"}, secrets: []api.ResolvedSecret{{Name: "OTHER", Type: "environment", Target: "SCION_TELEMETRY_CLOUD_PROVIDER", Value: "gcp"}}, wantErr: true},
		{name: "late default-type provider secret", cloud: &api.TelemetryCloudConfig{Endpoint: "https://generic.invalid/v1"}, secrets: []api.ResolvedSecret{{Name: "OTHER", Target: "SCION_TELEMETRY_CLOUD_PROVIDER", Value: "gcp"}}, wantErr: true},
		{name: "late credential env secret", cloud: &api.TelemetryCloudConfig{Endpoint: "https://generic.invalid/v1"}, secrets: []api.ResolvedSecret{{Name: "OTHER", Type: "environment", Target: "SCION_OTEL_GCP_CREDENTIALS", Value: "/private/key.json"}}, wantErr: true},
		{name: "late well-known file secret", cloud: &api.TelemetryCloudConfig{Endpoint: "https://generic.invalid/v1"}, secrets: []api.ResolvedSecret{{Name: "OTHER", Type: "file", Target: "~/.scion/telemetry-gcp-credentials.json"}}, wantErr: true},
		{name: "late absolute well-known file secret", cloud: &api.TelemetryCloudConfig{Endpoint: "https://generic.invalid/v1"}, secrets: []api.ResolvedSecret{{Name: "OTHER", Type: "file", Target: "/home/scion/.scion/other/../telemetry-gcp-credentials.json"}}, wantErr: true},
		{name: "explicit credential path", env: map[string]string{"SCION_OTEL_GCP_CREDENTIALS": "/private/key.json"}, want: "gcp"},
		{name: "generic provider", cloud: &api.TelemetryCloudConfig{Provider: "otlp"}, want: "otlp"},
		{name: "generic endpoint", cloud: &api.TelemetryCloudConfig{Endpoint: "https://generic.invalid/v1"}, want: "otlp"},
		{name: "generic env endpoint", env: map[string]string{"SCION_OTEL_ENDPOINT": "https://generic.invalid/v1"}, want: "otlp"},
		{name: "ambiguous", want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &api.TelemetryConfig{Cloud: tc.cloud}
			got, err := nativeTelemetryProvisionEnv(home, cfg, tc.env, tc.secrets)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v, wantErr=%t", err, tc.wantErr)
			}
			if err == nil && got["SCION_TELEMETRY_CLOUD_PROVIDER"] != tc.want {
				t.Fatalf("provider=%q, want %q", got["SCION_TELEMETRY_CLOUD_PROVIDER"], tc.want)
			}
		})
	}
	credentialPath := filepath.Join(home, ".scion", "telemetry-gcp-credentials.json")
	if err := os.MkdirAll(filepath.Dir(credentialPath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(credentialPath, nil, 0600); err != nil {
		t.Fatal(err)
	}
	got, err := nativeTelemetryProvisionEnv(home, &api.TelemetryConfig{}, nil, nil)
	if err != nil || got["SCION_TELEMETRY_CLOUD_PROVIDER"] != "gcp" {
		t.Fatalf("well-known credential hint=%q, err=%v", got["SCION_TELEMETRY_CLOUD_PROVIDER"], err)
	}
}

func TestNativeTelemetryProvisionGuardScopedToEnabledClaude(t *testing.T) {
	home := t.TempDir()
	late := []api.ResolvedSecret{{Name: "OTHER", Type: "environment", Target: "SCION_TELEMETRY_CLOUD_PROVIDER"}}
	enabled, disabled := true, false
	for _, tc := range []struct {
		name      string
		harness   string
		telemetry *api.TelemetryConfig
		wantErr   bool
	}{
		{"enabled Claude", "claude", &api.TelemetryConfig{Enabled: &enabled}, true},
		{"default-enabled Claude", "claude", &api.TelemetryConfig{}, true},
		{"disabled Claude", "claude", &api.TelemetryConfig{Enabled: &disabled}, false},
		{"absent Claude telemetry", "claude", nil, false},
		{"enabled Gemini", "gemini-cli", &api.TelemetryConfig{Enabled: &enabled}, false},
		{"enabled Codex", "codex", &api.TelemetryConfig{Enabled: &enabled}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := map[string]string{"SCION_OTEL_GRPC_PORT": "14317"}
			got, err := nativeTelemetryProvisionEnvForHarness(tc.harness, home, tc.telemetry, input, late)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v, wantErr=%t", err, tc.wantErr)
			}
			if !tc.wantErr && got["SCION_OTEL_GRPC_PORT"] != "14317" {
				t.Fatal("original port not preserved")
			}
		})
	}
}

func TestNativeTelemetryStagedProviderMatchesReceiverMode(t *testing.T) {
	for _, tc := range []struct {
		name            string
		configured      string
		override        string
		endpoint        string
		lateCredentials bool
		wellKnown       bool
		wantGCP         bool
		wantErr         bool
		wantAmbiguous   bool
	}{
		{name: "nested gcp", configured: "gcp", wantGCP: true},
		{name: "override gcp", override: "gcp", wantGCP: true},
		{name: "matching gcp", configured: "gcp", override: "gcp", wantGCP: true},
		{name: "conflicting providers", configured: "otlp", override: "gcp", wantErr: true},
		{name: "late named credential", lateCredentials: true, wantGCP: true},
		{name: "well-known credential", wellKnown: true, wantGCP: true},
		{name: "generic endpoint", endpoint: "https://generic.invalid/v1"},
		{name: "ambiguous", wantAmbiguous: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("SCION_TELEMETRY_CLOUD_PROVIDER", "")
			t.Setenv("SCION_OTEL_GCP_CREDENTIALS", "")
			t.Setenv("SCION_OTEL_ENDPOINT", "")
			t.Setenv("SCION_GCP_PROJECT_ID", "test-project")
			restore := telemetry.SetTelemetryTestSandboxed()
			defer restore()
			cfg := &api.TelemetryConfig{Cloud: &api.TelemetryCloudConfig{Provider: tc.configured, Endpoint: tc.endpoint}}
			env := map[string]string{}
			if tc.override != "" {
				env["SCION_TELEMETRY_CLOUD_PROVIDER"] = tc.override
			}
			var secrets []api.ResolvedSecret
			if tc.lateCredentials {
				secrets = []api.ResolvedSecret{{Name: "scion-telemetry-gcp-credentials", Type: "file"}}
			}
			credentialPath := filepath.Join(home, ".scion", "telemetry-gcp-credentials.json")
			if tc.wellKnown || tc.lateCredentials {
				if err := os.MkdirAll(filepath.Dir(credentialPath), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(credentialPath, []byte("{}"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			staged, err := nativeTelemetryProvisionEnv(home, cfg, env, secrets)
			if (err != nil) != tc.wantErr {
				t.Fatalf("staging error=%v, wantErr=%t", err, tc.wantErr)
			}
			if err != nil {
				return // Conflict prevents native provisioning and child launch.
			}
			hint := staged["SCION_TELEMETRY_CLOUD_PROVIDER"]
			if (hint == "") != tc.wantAmbiguous {
				t.Fatalf("hint=%q, wantAmbiguous=%t", hint, tc.wantAmbiguous)
			}
			for key, value := range config.TelemetryConfigToEnv(cfg) {
				if _, present := env[key]; !present {
					env[key] = value
				}
			}
			if tc.lateCredentials {
				env["SCION_OTEL_GCP_CREDENTIALS"] = credentialPath
			}
			for key, value := range env {
				t.Setenv(key, value)
			}
			actual := telemetry.LoadConfig()
			if !tc.wantAmbiguous && (hint == "gcp") != actual.IsGCP() {
				t.Fatalf("staged provider=%q, receiver GCP=%t", hint, actual.IsGCP())
			}
			if !tc.wantAmbiguous && actual.IsGCP() != tc.wantGCP {
				t.Fatalf("receiver GCP=%t, want %t", actual.IsGCP(), tc.wantGCP)
			}
		})
	}
}
