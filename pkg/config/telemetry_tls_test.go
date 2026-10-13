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
		cert, key   string
	}{
		{"unset", V1TelemetryTLSConfig{}, "", "", "", ""},
		{"verified TLS", V1TelemetryTLSConfig{Enabled: boolValue(true), InsecureSkipVerify: boolValue(false)}, "false", "false", "", ""},
		{"skip verification with TLS", V1TelemetryTLSConfig{Enabled: boolValue(true), InsecureSkipVerify: boolValue(true)}, "false", "true", "", ""},
		{"explicit plaintext", V1TelemetryTLSConfig{Enabled: boolValue(false), InsecureSkipVerify: boolValue(false)}, "true", "false", "", ""},
		{"client certificate", V1TelemetryTLSConfig{Enabled: boolValue(true), CertFile: "/certs/client.pem", KeyFile: "/certs/client-key.pem"}, "false", "", "/certs/client.pem", "/certs/client-key.pem"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			apiConfig := ConvertV1TelemetryToAPI(&V1TelemetryConfig{Cloud: &V1TelemetryCloudConfig{TLS: &tc.tls}})
			env := TelemetryConfigToEnv(apiConfig)
			if env["SCION_OTEL_INSECURE"] != tc.plain || env["SCION_OTEL_SKIP_TLS_VERIFY"] != tc.skip {
				t.Fatalf("transport env = %#v, want plaintext=%q skip=%q", env, tc.plain, tc.skip)
			}
			// Unset client certificate paths must emit no variable at all.
			for name, want := range map[string]string{"SCION_OTEL_CERT_FILE": tc.cert, "SCION_OTEL_KEY_FILE": tc.key} {
				got, present := env[name]
				if want == "" && present {
					t.Fatalf("%s emitted as %q while unset", name, got)
				}
				if got != want {
					t.Fatalf("%s = %q, want %q", name, got, want)
				}
			}
		})
	}
}

func TestTelemetryTLSClientCertValidation(t *testing.T) {
	for _, tc := range []struct {
		name      string
		env       map[string]string
		wantError string
	}{
		{"neither", map[string]string{}, ""},
		{"both", map[string]string{"SCION_OTEL_CERT_FILE": "/certs/client.pem", "SCION_OTEL_KEY_FILE": "/certs/client-key.pem"}, ""},
		{"certificate only", map[string]string{"SCION_OTEL_CERT_FILE": "/certs/client.pem"}, "cert_file and key_file must be set together"},
		{"key only", map[string]string{"SCION_OTEL_KEY_FILE": "/certs/client-key.pem"}, "cert_file and key_file must be set together"},
		{"plaintext with client certificate", map[string]string{"SCION_OTEL_INSECURE": "true", "SCION_OTEL_CERT_FILE": "/certs/client.pem", "SCION_OTEL_KEY_FILE": "/certs/client-key.pem"}, "conflicts"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			for _, name := range []string{"SCION_OTEL_INSECURE", "SCION_OTEL_SKIP_TLS_VERIFY", "SCION_OTEL_CA_FILE", "SCION_OTEL_CERT_FILE", "SCION_OTEL_KEY_FILE"} {
				t.Setenv(name, tc.env[name])
			}
			settings, err := LoadVersionedSettings("")
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) {
					t.Fatalf("LoadVersionedSettings error = %v, want %q", err, tc.wantError)
				}
				return
			}
			if err != nil {
				t.Fatalf("LoadVersionedSettings: %v", err)
			}
			if tc.env["SCION_OTEL_CERT_FILE"] == "" {
				return
			}
			if settings.Telemetry == nil || settings.Telemetry.Cloud == nil || settings.Telemetry.Cloud.TLS == nil {
				t.Fatalf("client certificate env did not reach telemetry.cloud.tls: %#v", settings.Telemetry)
			}
			tls := settings.Telemetry.Cloud.TLS
			if tls.CertFile != "/certs/client.pem" || tls.KeyFile != "/certs/client-key.pem" {
				t.Fatalf("cert_file=%q key_file=%q", tls.CertFile, tls.KeyFile)
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
	// An unparseable plaintext switch is ignored with a warning; see
	// TestLoadVersionedSettings_BadOtelInsecureFallsBack.
	t.Setenv("SCION_OTEL_INSECURE", "not-a-bool")
	if _, err := LoadVersionedSettings(""); err != nil {
		t.Fatalf("invalid plaintext switch must not fail the load: %v", err)
	}
}

func TestTelemetryTLSEmptyPlaintextEnvIsUnset(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SCION_OTEL_INSECURE", "")
	settings, err := LoadVersionedSettings("")
	if err != nil {
		t.Fatalf("empty SCION_OTEL_INSECURE should be unset: %v", err)
	}
	if settings.Telemetry != nil && settings.Telemetry.Cloud != nil && settings.Telemetry.Cloud.TLS != nil && settings.Telemetry.Cloud.TLS.Enabled != nil && !*settings.Telemetry.Cloud.TLS.Enabled {
		t.Fatal("empty SCION_OTEL_INSECURE unexpectedly selected plaintext transport")
	}
}
