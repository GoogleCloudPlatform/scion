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

package runtimebroker

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// TestWebPTYCapability_PerRuntime pins the WebPTY capability per default
// runtime, in both places a broker reports it: the static capabilities sent
// at registration and on every heartbeat (StaticCapabilities and the
// StaticCapabilityNames registration list), and GET /api/v1/info. The two
// must agree, and must follow what the PTY attach path is known to serve:
// docker, podman and the Apple container runtime through "<name> exec -it",
// kubernetes through the client-go exec, cloudrun-sandbox through the
// sandbox binary. Every other runtime reports false: cloudrun (no exec
// primitive on the PTY path), substrate (opts out of attach), a runtime
// this broker does not know, and no runtime at all.
func TestWebPTYCapability_PerRuntime(t *testing.T) {
	tests := []struct {
		name string
		rt   runtime.Runtime
		want bool
	}{
		{"docker", runtime.NewDockerRuntime(), true},
		{"podman", &runtime.PodmanRuntime{}, true},
		{"container", &runtime.AppleContainerRuntime{}, true},
		{"kubernetes", &runtime.KubernetesRuntime{}, true},
		{"cloudrun-sandbox", &runtime.CloudRunSandboxRuntime{}, true},
		{"cloudrun", &runtime.CloudRunRuntime{}, false},
		{"substrate", &runtime.SubstrateRuntime{}, false},
		// A runtime the allow-list does not know defaults to false, even
		// one whose name matches a supported runtime.
		{"some-future-runtime", &runtime.MockRuntime{NameFunc: func() string { return "some-future-runtime" }}, false},
		{"docker", &runtime.MockRuntime{NameFunc: func() string { return "docker" }}, false},
	}
	for _, tc := range tests {
		t.Run(fmt.Sprintf("%s/%T", tc.name, tc.rt), func(t *testing.T) {
			if got := tc.rt.Name(); got != tc.name {
				t.Fatalf("precondition: runtime Name() = %q, want %q", got, tc.name)
			}

			if got := StaticCapabilities(tc.rt).WebPTY; got != tc.want {
				t.Errorf("StaticCapabilities(%s).WebPTY = %v, want %v", tc.name, got, tc.want)
			}
			if got := slices.Contains(StaticCapabilityNames(tc.rt), "webPty"); got != tc.want {
				t.Errorf("StaticCapabilityNames(%s) contains webPty = %v, want %v", tc.name, got, tc.want)
			}

			srv := newTestServer(t)
			srv.runtime = tc.rt
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/info", nil))
			if w.Code != http.StatusOK {
				t.Fatalf("GET /api/v1/info status = %d, want %d; body = %s", w.Code, http.StatusOK, w.Body.String())
			}
			var resp BrokerInfoResponse
			if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
				t.Fatalf("decode info response: %v", err)
			}
			if resp.Capabilities == nil {
				t.Fatal("info response has no capabilities")
			}
			if resp.Capabilities.WebPTY != tc.want {
				t.Errorf("/api/v1/info capabilities.webPty = %v, want %v for %s", resp.Capabilities.WebPTY, tc.want, tc.name)
			}
		})
	}
}

// TestWebPTYCapability_NilRuntime pins that a broker with no runtime yet
// reports no web terminal, in both the static capabilities and their names.
func TestWebPTYCapability_NilRuntime(t *testing.T) {
	if StaticCapabilities(nil).WebPTY {
		t.Error("StaticCapabilities(nil).WebPTY = true, want false")
	}
	if slices.Contains(StaticCapabilityNames(nil), "webPty") {
		t.Errorf("StaticCapabilityNames(nil) = %v, want no webPty", StaticCapabilityNames(nil))
	}
}

// TestGetStats_NotImplemented pins that the agent stats action answers 501
// with the stats_not_implemented error code, not a 200 carrying zero values.
func TestGetStats_NotImplemented(t *testing.T) {
	srv := newTestServer(t)

	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/agents/test-agent-1/stats", nil))

	if w.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want %d; body = %s", w.Code, http.StatusNotImplemented, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var body ErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error response: %v; body = %s", err, w.Body.String())
	}
	if body.Error.Code != ErrCodeStatsNotImplemented {
		t.Errorf("error code = %q, want %q", body.Error.Code, ErrCodeStatsNotImplemented)
	}
	if body.Error.Message == "" {
		t.Error("error message is empty")
	}
	if strings.Contains(w.Body.String(), "cpuUsagePercent") || strings.Contains(w.Body.String(), "memoryUsageBytes") {
		t.Errorf("501 body carries stats fields: %s", w.Body.String())
	}
}
