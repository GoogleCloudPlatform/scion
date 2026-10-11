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

package agent

import (
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

func TestK8sNFSUIDIgnoredWarning(t *testing.T) {
	tests := []struct {
		name    string
		runtime string
		nfs     *config.V1NFSConfig
		warn    bool
	}{
		{"kubernetes uid 7", "kubernetes", &config.V1NFSConfig{UID: 7}, true},
		{"kubernetes uid unset", "kubernetes", &config.V1NFSConfig{}, false},
		{"kubernetes uid 1000", "kubernetes", &config.V1NFSConfig{UID: 1000}, false},
		{"kubernetes no nfs block", "kubernetes", nil, false},
		{"docker uid 7", "docker", &config.V1NFSConfig{UID: 7}, false},
		{"podman uid 7", "podman", &config.V1NFSConfig{UID: 7}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := k8sNFSUIDIgnoredWarning(tt.runtime, tt.nfs)
			if (got != "") != tt.warn {
				t.Fatalf("k8sNFSUIDIgnoredWarning(%q, %+v) = %q; want warning=%v", tt.runtime, tt.nfs, got, tt.warn)
			}
			if !tt.warn {
				return
			}
			for _, want := range []string{config.NFSUIDKey, "7", "not applied", "kubernetes", "uid 1000"} {
				if !strings.Contains(got, want) {
					t.Errorf("warning %q does not mention %q", got, want)
				}
			}
		})
	}
}

func TestWarnK8sNFSUIDIgnored_OncePerValue(t *testing.T) {
	resetK8sNFSUIDWarned()
	t.Cleanup(resetK8sNFSUIDWarned)

	var logged []string
	logf := func(msg string, _ ...any) { logged = append(logged, msg) }

	// Silent cases never log and do not consume the once-per-value slot.
	for _, nfs := range []*config.V1NFSConfig{nil, {}, {UID: 1000}} {
		if warnK8sNFSUIDIgnored("kubernetes", nfs, logf) {
			t.Fatalf("warned for kubernetes with %+v", nfs)
		}
	}
	if warnK8sNFSUIDIgnored("docker", &config.V1NFSConfig{UID: 7}, logf) {
		t.Fatal("warned for docker")
	}
	if len(logged) != 0 {
		t.Fatalf("logged %q for silent cases", logged)
	}

	if !warnK8sNFSUIDIgnored("kubernetes", &config.V1NFSConfig{UID: 7}, logf) {
		t.Fatal("did not warn for kubernetes uid 7")
	}
	if warnK8sNFSUIDIgnored("kubernetes", &config.V1NFSConfig{UID: 7}, logf) {
		t.Fatal("warned twice for kubernetes uid 7")
	}
	if !warnK8sNFSUIDIgnored("kubernetes", &config.V1NFSConfig{UID: 8}, logf) {
		t.Fatal("did not warn for a new value, uid 8")
	}
	if len(logged) != 2 {
		t.Fatalf("logged %d warnings, want 2: %q", len(logged), logged)
	}

	resetK8sNFSUIDWarned()
	if !warnK8sNFSUIDIgnored("kubernetes", &config.V1NFSConfig{UID: 7}, logf) {
		t.Fatal("did not warn again after reset")
	}
}
