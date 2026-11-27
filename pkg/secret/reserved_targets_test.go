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

package secret

import "testing"

func TestIsReservedEnvTarget(t *testing.T) {
	tests := []struct {
		name   string
		target string
		want   bool
	}{
		{"scion prefixed name", "SCION_METADATA_MODE", true},
		{"scion prefix alone", "SCION_", true},
		{"gce metadata host", "GCE_METADATA_HOST", true},
		{"gce metadata root", "GCE_METADATA_ROOT", true},
		{"ordinary name", "MY_APP_TOKEN", false},
		{"empty target", "", false},
		{"case-sensitive: lowercase prefix does not match", "scion_metadata_mode", false},
		{"substring but not prefix", "NOT_SCION_METADATA_MODE", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsReservedEnvTarget(tt.target); got != tt.want {
				t.Errorf("IsReservedEnvTarget(%q) = %v, want %v", tt.target, got, tt.want)
			}
		})
	}
}

func TestIsReservedTelemetryCredentialFileTarget(t *testing.T) {
	tests := []struct {
		name   string
		target string
		want   bool
	}{
		{"tilde in-home path", "~/.scion/telemetry-gcp-credentials.json", true},
		{"double slash and dot segments", "~//.scion/./telemetry-gcp-credentials.json", true},
		{"absolute home path", "/home/scion/.scion/telemetry-gcp-credentials.json", true},
		{"absolute with dotdot collapsing to target", "/home/scion/.scion/other/../telemetry-gcp-credentials.json", true},
		{"relative in-home path", ".scion/telemetry-gcp-credentials.json", true},
		{"windows backslashes", `~\.scion\telemetry-gcp-credentials.json`, true},
		{"uppercase .SCION directory", "~/.SCION/telemetry-gcp-credentials.json", true},
		{"uppercase filename", "~/.scion/TELEMETRY-GCP-CREDENTIALS.JSON", true},
		{"mixed case absolute path", "/home/scion/.SCION/Telemetry-GCP-Credentials.json", true},
		{"unicode long-s in directory", "~/.ſcion/telemetry-gcp-credentials.json", true},
		{"unicode long-s in filename", "~/.scion/telemetry-gcp-credentialſ.json", true},
		{"bare filename without .scion dir", "telemetry-gcp-credentials.json", false},
		{"missing segment boundary", "/tmp/x.scion/telemetry-gcp-credentials.json", false},
		{"nested subdirectory under .scion", "~/.scion/other/telemetry-gcp-credentials.json", false},
		{"different filename", "~/.scion/telemetry-gcp-credentials.json.bak", false},
		{"ordinary file target", "~/.config/gcp/sa.json", false},
		{"empty target", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsReservedTelemetryCredentialFileTarget(tt.target); got != tt.want {
				t.Errorf("IsReservedTelemetryCredentialFileTarget(%q) = %v, want %v", tt.target, got, tt.want)
			}
		})
	}
}

func TestIsReservedFileTarget(t *testing.T) {
	target := "~/.scion/telemetry-gcp-credentials.json"
	tests := []struct {
		scope  string
		target string
		want   bool
	}{
		{ScopeHub, target, false},
		{ScopeProject, target, true},
		{ScopeUser, target, true},
		{ScopeAgent, target, true},
		{ScopeRuntimeBroker, target, true},
		{ScopeProject, "~/.config/gcp/sa.json", false},
	}
	for _, tt := range tests {
		if got := IsReservedFileTarget(tt.scope, tt.target); got != tt.want {
			t.Errorf("IsReservedFileTarget(%q, %q) = %v, want %v", tt.scope, tt.target, got, tt.want)
		}
	}
}

func TestIsReservedNonHubSecret(t *testing.T) {
	tests := []struct {
		name       string
		scope      string
		secretName string
		secretType string
		target     string
		want       bool
	}{
		{"hub reserved name and target allowed", ScopeHub, telemetryCredentialFileName, "file", "~/.scion/telemetry-gcp-credentials.json", false},
		{"project reserved name with arbitrary file target", ScopeProject, telemetryCredentialFileName, "file", "~/.config/evil.json", true},
		{"user reserved name with environment type", ScopeUser, telemetryCredentialFileName, "environment", "OTHER_ENV", true},
		{"agent reserved name with on_demand type", ScopeAgent, telemetryCredentialFileName, "on_demand", telemetryCredentialFileName, true},
		{"broker reserved name with empty type", ScopeRuntimeBroker, telemetryCredentialFileName, "", "", true},
		{"project non-reserved name with reserved target", ScopeProject, "OTHER", "file", "~/.scion/telemetry-gcp-credentials.json", true},
		{"project non-reserved name with uppercase reserved target", ScopeProject, "OTHER", "file", "~/.SCION/TELEMETRY-GCP-CREDENTIALS.JSON", true},
		{"project file with empty target falling back to reserved name-path", ScopeProject, ".scion/telemetry-gcp-credentials.json", "file", "", true},
		{"project env secret with path-like target is not file", ScopeProject, "OTHER", "environment", "~/.scion/telemetry-gcp-credentials.json", false},
		{"project ordinary file secret allowed", ScopeProject, "OTHER", "file", "~/.config/gcp/sa.json", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsReservedNonHubSecret(tt.scope, tt.secretName, tt.secretType, tt.target); got != tt.want {
				t.Errorf("IsReservedNonHubSecret(%q, %q, %q, %q) = %v, want %v",
					tt.scope, tt.secretName, tt.secretType, tt.target, got, tt.want)
			}
		})
	}
}
