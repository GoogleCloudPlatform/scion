package config

import (
	"strings"
	"testing"
)

func TestTelemetryTLSEnvBridge(t *testing.T) {
	boolValue := func(v bool) *bool { return &v }
	for _, tc := range []struct {
		name        string
		tls         V1TelemetryTLSConfig
		plain, skip string
	}{
		{"unset", V1TelemetryTLSConfig{}, "", ""},
		{"verified TLS", V1TelemetryTLSConfig{Enabled: boolValue(true), InsecureSkipVerify: boolValue(false)}, "false", "false"},
		{"skip verification with TLS", V1TelemetryTLSConfig{Enabled: boolValue(true), InsecureSkipVerify: boolValue(true)}, "false", "true"},
		{"explicit plaintext", V1TelemetryTLSConfig{Enabled: boolValue(false), InsecureSkipVerify: boolValue(false)}, "true", "false"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			apiConfig := ConvertV1TelemetryToAPI(&V1TelemetryConfig{Cloud: &V1TelemetryCloudConfig{TLS: &tc.tls}})
			env := TelemetryConfigToEnv(apiConfig)
			if env["SCION_OTEL_INSECURE"] != tc.plain || env["SCION_OTEL_SKIP_TLS_VERIFY"] != tc.skip {
				t.Fatalf("transport env = %#v, want plaintext=%q skip=%q", env, tc.plain, tc.skip)
			}
		})
	}
}

func TestTelemetryTLSPlaintextEnvOverridesSettingsAndRejectsConflict(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SCION_OTEL_INSECURE", "true")
	t.Setenv("SCION_OTEL_SKIP_TLS_VERIFY", "false")
	settings, err := LoadVersionedSettings("")
	if err != nil {
		t.Fatal(err)
	}
	if settings.Telemetry == nil || settings.Telemetry.Cloud == nil || settings.Telemetry.Cloud.TLS == nil || settings.Telemetry.Cloud.TLS.Enabled == nil || *settings.Telemetry.Cloud.TLS.Enabled {
		t.Fatalf("plaintext env did not disable TLS: %#v", settings.Telemetry)
	}
	t.Setenv("SCION_OTEL_SKIP_TLS_VERIFY", "true")
	if _, err := LoadVersionedSettings(""); err == nil || !strings.Contains(err.Error(), "conflicts") {
		t.Fatalf("conflicting TLS env = %v", err)
	}
	t.Setenv("SCION_OTEL_INSECURE", "false")
	if _, err := LoadVersionedSettings(""); err != nil {
		t.Fatalf("verified/skip TLS env rejected: %v", err)
	}
}
