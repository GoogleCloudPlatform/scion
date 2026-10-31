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

package config

import "testing"

func TestIsAgentHostAliasHost(t *testing.T) {
	tests := []struct {
		host string
		want bool
	}{
		{"host.docker.internal", true},
		{"HOST.Docker.Internal", true},
		{"host.docker.internal.", true},
		{"host.containers.internal", true},
		{"scion-hub.internal", true},
		{"", false},
		{"localhost", false},
		{"127.0.0.1", false},
		{"172.17.0.1", false},
		{"169.254.1.2", false},
		{"::1", false},
		{"hub.example.com", false},
		{"scion-hub.scion.svc.cluster.local", false},
		{"host.docker.internal.example.com", false},
		{"evil.host.docker.internal", false},
		{"host.docker.internal..", false},
	}
	for _, tt := range tests {
		if got := IsAgentHostAliasHost(tt.host); got != tt.want {
			t.Errorf("IsAgentHostAliasHost(%q) = %v, want %v", tt.host, got, tt.want)
		}
	}
}
