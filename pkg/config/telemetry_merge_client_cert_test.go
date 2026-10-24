/*
Copyright 2026 The Scion Authors.
*/

package config

import (
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

func telemetryWithClientCert(cert, key string) *api.ScionConfig {
	return &api.ScionConfig{Telemetry: &api.TelemetryConfig{Cloud: &api.TelemetryCloudConfig{
		Endpoint: "otel.example:4317",
		TLS:      &api.TelemetryTLS{CertFile: cert, KeyFile: key},
	}}}
}

// The telemetry client certificate and key merge as one setting per layer.
func TestMergeTelemetryConfig_ClientCertKeyPair(t *testing.T) {
	cases := []struct {
		name               string
		override           *api.ScionConfig
		wantCert, wantKey  string
		wantValidationFail bool
	}{
		{"override sets only cert", telemetryWithClientCert("/o/cert.pem", ""), "/o/cert.pem", "", true},
		{"override sets only key", telemetryWithClientCert("", "/o/key.pem"), "", "/o/key.pem", true},
		{"override sets both", telemetryWithClientCert("/o/cert.pem", "/o/key.pem"), "/o/cert.pem", "/o/key.pem", false},
		{"override sets neither", telemetryWithClientCert("", ""), "/b/cert.pem", "/b/key.pem", false},
		{"override has no tls", &api.ScionConfig{Telemetry: &api.TelemetryConfig{Cloud: &api.TelemetryCloudConfig{Endpoint: "e"}}}, "/b/cert.pem", "/b/key.pem", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := telemetryWithClientCert("/b/cert.pem", "/b/key.pem")
			got := MergeScionConfig(base, tc.override)
			tls := got.Telemetry.Cloud.TLS
			if tls.CertFile != tc.wantCert || tls.KeyFile != tc.wantKey {
				t.Errorf("merged cert/key = %q/%q, want %q/%q", tls.CertFile, tls.KeyFile, tc.wantCert, tc.wantKey)
			}
			if err := ValidateTelemetryTLS(tls); (err != nil) != tc.wantValidationFail {
				t.Errorf("ValidateTelemetryTLS = %v, want failure %v", err, tc.wantValidationFail)
			}
			if b := base.Telemetry.Cloud.TLS; b.CertFile != "/b/cert.pem" || b.KeyFile != "/b/key.pem" {
				t.Errorf("base modified: %q/%q", b.CertFile, b.KeyFile)
			}
		})
	}
}
