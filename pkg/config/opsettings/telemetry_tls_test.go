/*
Copyright 2026 The Scion Authors.
*/

package opsettings

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestValidate_TelemetryTLS checks that a telemetry section document gets
// the same telemetry.cloud.tls rules as settings.yaml, so the hub admin
// settings API (OperationalSettings.Update validates the merged document)
// cannot store an invalid pair.
func TestValidate_TelemetryTLS(t *testing.T) {
	for _, tc := range []struct {
		name      string
		doc       string
		wantError string
	}{
		{"no cloud", `{"enabled":true}`, ""},
		{"no tls", `{"cloud":{"endpoint":"collector:4317"}}`, ""},
		{"pair", `{"cloud":{"tls":{"cert_file":"/certs/c.pem","key_file":"/certs/k.pem"}}}`, ""},
		{"certificate only", `{"cloud":{"tls":{"cert_file":"/certs/c.pem"}}}`, "cert_file and key_file must be set together"},
		{"key only", `{"cloud":{"tls":{"key_file":"/certs/k.pem"}}}`, "cert_file and key_file must be set together"},
		{"plaintext with client certificate", `{"cloud":{"tls":{"enabled":false,"cert_file":"/certs/c.pem","key_file":"/certs/k.pem"}}}`, "conflicts"},
		{"plaintext with CA", `{"cloud":{"tls":{"enabled":false,"ca_file":"/certs/ca.pem"}}}`, "conflicts"},
		// Paths are agent-container paths: a path that does not exist on the
		// hub is not an error.
		{"pair of paths absent here", `{"cloud":{"tls":{"cert_file":"/nonexistent/c.pem","key_file":"/nonexistent/k.pem"}}}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			errs := Validate("telemetry", json.RawMessage(tc.doc))
			if tc.wantError == "" {
				if len(errs) > 0 {
					t.Fatalf("unexpected errors: %v", errs)
				}
				return
			}
			if len(errs) != 1 {
				t.Fatalf("errors = %v, want exactly one", errs)
			}
			if !strings.Contains(errs[0].Message, tc.wantError) {
				t.Fatalf("error = %q, want it to contain %q", errs[0].Message, tc.wantError)
			}
			if errs[0].Path != "cloud/tls" {
				t.Fatalf("error path = %q, want cloud/tls", errs[0].Path)
			}
		})
	}
}
