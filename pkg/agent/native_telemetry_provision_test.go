package agent

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
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
