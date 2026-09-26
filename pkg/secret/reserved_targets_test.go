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
