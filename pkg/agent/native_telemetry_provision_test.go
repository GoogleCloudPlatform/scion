package agent

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/telemetry"
)

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
