// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package opsettings

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestValidateTelemetryTLS(t *testing.T) {
	for _, tc := range []struct {
		name      string
		doc       string
		wantError string
	}{
		{"no cloud", `{"enabled":true}`, ""},
		{"no tls", `{"cloud":{"endpoint":"collector:4317"}}`, ""},
		{"null tls", `{"cloud":{"tls":null}}`, ""},
		{"pair", `{"cloud":{"tls":{"cert_file":"/certs/c.pem","key_file":"/certs/k.pem"}}}`, ""},
		{"certificate only", `{"cloud":{"tls":{"cert_file":"/certs/c.pem"}}}`, "cert_file and key_file must be set together"},
		{"key only", `{"cloud":{"tls":{"key_file":"/certs/k.pem"}}}`, "cert_file and key_file must be set together"},
		{"plaintext with client certificate", `{"cloud":{"tls":{"enabled":false,"cert_file":"/certs/c.pem","key_file":"/certs/k.pem"}}}`, "conflicts"},
		{"plaintext with CA", `{"cloud":{"tls":{"enabled":false,"ca_file":"/certs/ca.pem"}}}`, "conflicts"},
		// Agent-container paths: a path that does not exist here is valid.
		{"pair of paths absent here", `{"cloud":{"tls":{"cert_file":"/nonexistent/c.pem","key_file":"/nonexistent/k.pem"}}}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			errs := ValidateTelemetryTLS(json.RawMessage(tc.doc))
			if tc.wantError == "" {
				if len(errs) > 0 {
					t.Fatalf("unexpected errors: %v", errs)
				}
				return
			}
			if len(errs) != 1 {
				t.Fatalf("errors = %v, want exactly one", errs)
			}
			if errs[0].Path != "cloud/tls" {
				t.Fatalf("error path = %q, want cloud/tls", errs[0].Path)
			}
			if !strings.Contains(errs[0].Message, tc.wantError) {
				t.Fatalf("error = %q, want it to contain %q", errs[0].Message, tc.wantError)
			}
		})
	}
}

// TestValidate_TelemetryHasNoTLSUnitRule pins that the section validator
// does not apply the tls unit rule. Validate also judges stored keys one at
// a time when the hub prunes carried keys, so the rule there would silently
// delete a stored half pair (see ValidateTelemetryTLS).
func TestValidate_TelemetryHasNoTLSUnitRule(t *testing.T) {
	for _, doc := range []string{
		`{"cloud":{"tls":{"cert_file":"/certs/c.pem"}}}`,
		`{"cloud":{"tls":{"enabled":false,"ca_file":"/certs/ca.pem"}}}`,
	} {
		if errs := Validate("telemetry", json.RawMessage(doc)); len(errs) > 0 {
			t.Fatalf("Validate rejected %s with %v; the tls unit rule must stay out of Validate", doc, errs)
		}
		if errs := ValidateTelemetryTLS(json.RawMessage(doc)); len(errs) != 1 {
			t.Fatalf("ValidateTelemetryTLS(%s) = %v, want one error", doc, errs)
		}
	}
}
