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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

func TestHealthz(t *testing.T) {
	srv := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status %d, got %d", http.StatusOK, w.Code)
	}

	var resp HealthResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.Status != "healthy" {
		t.Errorf("expected status 'healthy', got '%s'", resp.Status)
	}
}

func TestReadyz(t *testing.T) {
	srv := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status %d, got %d", http.StatusOK, w.Code)
	}
}

func TestHostInfo(t *testing.T) {
	srv := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/info", nil)
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status %d, got %d", http.StatusOK, w.Code)
	}

	var resp BrokerInfoResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.BrokerID != "test-broker-id" {
		t.Errorf("expected brokerId 'test-broker-id', got '%s'", resp.BrokerID)
	}

	if resp.Capabilities == nil {
		t.Fatal("expected capabilities to be present")
	}
	if !resp.Capabilities.EmptyPerAgentWorkspace {
		t.Error("expected capabilities.emptyPerAgentWorkspace to be true (design #2703 P2)")
	}
	if !resp.Capabilities.AsyncStart {
		t.Error("expected capabilities.asyncStart to be true (ptone/scion#4669), as the heartbeat reports")
	}
}

// TestHostInfo_EmptyPerAgentFollowsDefaultRuntime pins that /api/v1/info
// advertises EmptyPerAgentWorkspace per default runtime (false for one that
// opts out, as Cloud Run does), matching the heartbeat.
func TestHostInfo_EmptyPerAgentFollowsDefaultRuntime(t *testing.T) {
	srv := newTestServer(t)
	srv.runtime = &noEmptyPerAgentTestRuntime{MockRuntime: &runtime.MockRuntime{NameFunc: func() string { return "cloudrun" }}}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/info", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, w.Code)
	}
	var resp BrokerInfoResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.Capabilities == nil {
		t.Fatal("expected capabilities to be present")
	}
	if resp.Capabilities.EmptyPerAgentWorkspace {
		t.Error("capabilities.emptyPerAgentWorkspace = true, want false for a default runtime that opts out")
	}
}

// TestBuildInfoProfiles_CloudRunSandbox_Task92 is the broker-level pin test
// for task #92. When the broker runs cloudrun-sandbox and the settings define
// a "default" profile with runtime cloudrun-sandbox, buildInfoProfiles must
// return that profile. Before the fix, the workstation defaults (local/docker,
// remote/kubernetes) were seeded; buildInfoProfiles filtered out docker
// (local-only) and returned only remote/kubernetes — which cannot work on
// this tier. With the fix, the "default" profile survives alongside
// "remote" (from embedded defaults merge), and autoSelectProfile does NOT
// fire (length > 1), so "Use broker default" is shown — which resolves to
// active_profile "default" → cloudrun-sandbox.
//
// The assertion is on the PROFILE LIST contents, not just the selected value.
// The bug was a list of length one containing the WRONG entry.
func TestBuildInfoProfiles_CloudRunSandbox_Task92(t *testing.T) {
	tmpDir := t.TempDir()
	origHome := os.Getenv("HOME")
	_ = os.Setenv("HOME", tmpDir)
	defer func() { _ = os.Setenv("HOME", origHome) }()

	// Seed settings matching the fixed Cloud Run sandbox template:
	// active_profile=default, profiles.default.runtime=cloudrun-sandbox
	// LoadEffectiveSettings also merges the embedded defaults, which add
	// profiles.local (docker) and profiles.remote (kubernetes). The filter
	// in buildInfoProfiles then drops local (local-only) but keeps both
	// default and remote.
	scionDir := filepath.Join(tmpDir, ".scion")
	if err := os.MkdirAll(scionDir, 0755); err != nil {
		t.Fatal(err)
	}
	settingsYAML := `schema_version: "1"
active_profile: default
runtimes:
  cloudrun-sandbox:
    type: cloudrun-sandbox
profiles:
  default:
    runtime: cloudrun-sandbox
`
	if err := os.WriteFile(filepath.Join(scionDir, "settings.yaml"), []byte(settingsYAML), 0644); err != nil {
		t.Fatal(err)
	}

	srv := &Server{}
	profiles := srv.buildInfoProfiles("cloudrun-sandbox")

	// Build a lookup map for assertions.
	byName := make(map[string]BrokerProfile, len(profiles))
	for _, p := range profiles {
		byName[p.Name] = p
	}

	// CRITICAL ASSERTION: "default" profile with type "cloudrun-sandbox" MUST
	// be present. This is the fix — before task #92, no cloudrun-sandbox
	// profile existed.
	defaultP, hasDefault := byName["default"]
	if !hasDefault {
		names := make([]string, len(profiles))
		for i, p := range profiles {
			names[i] = fmt.Sprintf("%s(%s)", p.Name, p.Type)
		}
		t.Fatalf("profile 'default' missing; profiles are: %v", names)
	}
	if defaultP.Type != "cloudrun-sandbox" {
		t.Errorf("profile 'default' type = %q, want %q", defaultP.Type, "cloudrun-sandbox")
	}
	if !defaultP.Available {
		t.Error("profile 'default' should be available")
	}

	// GUARD ASSERTION: "local" profile (docker) MUST be filtered out. If it
	// is present, the filter is not working and local-only profiles leak to
	// a non-local broker.
	if _, hasLocal := byName["local"]; hasLocal {
		t.Error("profile 'local' (docker) should be filtered out on a cloudrun-sandbox broker")
	}

	// The profile list must have length > 1 so that autoSelectProfile does
	// NOT fire. "remote" (from embedded defaults merge) is expected alongside
	// "default". With length > 1, the UI shows "Use broker default" which
	// resolves to active_profile "default" → cloudrun-sandbox → works.
	if len(profiles) < 2 {
		t.Errorf("expected >= 2 profiles (so autoSelectProfile does not fire), got %d", len(profiles))
	}
}

// TestBuildInfoProfiles_OldWorkstationDefaults_Task92_Regression is the RED
// counterpart of the pin test. When the WORKSTATION defaults are seeded
// (local/docker + remote/kubernetes) and the broker runtime is cloudrun-sandbox,
// buildInfoProfiles returns only remote/kubernetes — which is the bug.
//
// This test documents the defective state so the pin test's GREEN is meaningful.
// If someone re-introduces the old defaults for Cloud Run sandbox, this test
// shows what would happen: the UI would see only "remote (kubernetes)".
func TestBuildInfoProfiles_OldWorkstationDefaults_Task92_Regression(t *testing.T) {
	tmpDir := t.TempDir()
	origHome := os.Getenv("HOME")
	_ = os.Setenv("HOME", tmpDir)
	defer func() { _ = os.Setenv("HOME", origHome) }()

	// Seed the OLD (broken) workstation defaults: local/docker + remote/kubernetes.
	scionDir := filepath.Join(tmpDir, ".scion")
	if err := os.MkdirAll(scionDir, 0755); err != nil {
		t.Fatal(err)
	}
	brokenSettingsYAML := `schema_version: "1"
active_profile: local
runtimes:
  docker:
    type: docker
  kubernetes:
    type: kubernetes
profiles:
  local:
    runtime: docker
  remote:
    runtime: kubernetes
`
	if err := os.WriteFile(filepath.Join(scionDir, "settings.yaml"), []byte(brokenSettingsYAML), 0644); err != nil {
		t.Fatal(err)
	}

	srv := &Server{}
	profiles := srv.buildInfoProfiles("cloudrun-sandbox")

	// Document the bug: docker is filtered out (local-only), only kubernetes
	// remains. This is the defect — the profile list contains only an entry
	// that cannot work on this tier.
	if len(profiles) != 1 {
		t.Fatalf("expected 1 profile (regression confirms filter), got %d", len(profiles))
	}
	if profiles[0].Name != "remote" || profiles[0].Type != "kubernetes" {
		t.Errorf("regression: expected remote/kubernetes, got %s/%s", profiles[0].Name, profiles[0].Type)
	}
	// This is the symptom: the ONLY available profile is kubernetes, which
	// cannot work on Cloud Run sandbox. autoSelectProfile() in the UI would
	// auto-select it because profiles.length === 1.
}

// TestBuildInfoProfiles_FallbackFires_Task92 verifies the architect's Shape B
// claim by execution: when ALL profiles are dropped by buildInfoProfiles
// (both local/docker and remote/kubernetes filtered out), the existing
// len(profiles)==0 fallback fires and returns exactly one synthetic profile
// {Name: "default", Type: defaultRuntimeType, Available: true}.
//
// With defaultRuntimeType="cloudrun-sandbox", this means the fallback produces
// the correct profile for the single-node tier. autoSelectProfile in the UI
// fires (length == 1), and the profile resolves to cloudrun-sandbox.
//
// This test also verifies the workstation case (docker broker) is unaffected
// by the filter: when the broker IS local, the filter condition
// !isLocalOnlyRuntime(defaultRuntimeType) is false, so NO profiles are
// dropped — kubernetes remains available as a legitimate product feature.
//
// These three facts were measured by a verification instrument that was
// deleted under a stop order (architect 03:08). This committed pin restores
// them as reproducible evidence.
func TestBuildInfoProfiles_FallbackFires_Task92(t *testing.T) {
	tmpDir := t.TempDir()
	origHome := os.Getenv("HOME")
	_ = os.Setenv("HOME", tmpDir)
	defer func() { _ = os.Setenv("HOME", origHome) }()

	// Seed the ORIGINAL workstation defaults — the settings that exist
	// on a fresh deploy without the task-92 fix.
	scionDir := filepath.Join(tmpDir, ".scion")
	if err := os.MkdirAll(scionDir, 0755); err != nil {
		t.Fatal(err)
	}
	settingsYAML := `schema_version: "1"
active_profile: local
runtimes:
  docker:
    type: docker
  kubernetes:
    type: kubernetes
profiles:
  local:
    runtime: docker
  remote:
    runtime: kubernetes
`
	if err := os.WriteFile(filepath.Join(scionDir, "settings.yaml"), []byte(settingsYAML), 0644); err != nil {
		t.Fatal(err)
	}

	srv := &Server{}

	// === FACT 1: Current filter behavior on cloudrun-sandbox broker ===
	// With workstation defaults, only remote/kubernetes survives the filter.
	// local/docker is dropped because isLocalOnlyRuntime("docker")=true and
	// the broker is non-local (!isLocalOnlyRuntime("cloudrun-sandbox")=true).
	profiles := srv.buildInfoProfiles("cloudrun-sandbox")
	if len(profiles) != 1 {
		t.Fatalf("FACT 1: expected 1 profile (remote/kubernetes only), got %d", len(profiles))
	}
	if profiles[0].Name != "remote" || profiles[0].Type != "kubernetes" {
		t.Errorf("FACT 1: expected remote/kubernetes, got %s/%s", profiles[0].Name, profiles[0].Type)
	}

	// === FACT 2: Filter predicate analysis ===
	// isLocalOnlyRuntime("kubernetes") is false — this is WHY kubernetes
	// survives the filter. The filter asks "is this local-only?" when it
	// should ask "can this broker serve this runtime?".
	if isLocalOnlyRuntime("kubernetes") {
		t.Fatal("FACT 2: isLocalOnlyRuntime('kubernetes') should be false — this is load-bearing")
	}
	if !isLocalOnlyRuntime("docker") {
		t.Fatal("FACT 2: isLocalOnlyRuntime('docker') should be true")
	}

	// === FACT 3: Workstation broker preserves all profiles ===
	// On a docker broker, !isLocalOnlyRuntime("docker") = !true = false,
	// so the filter condition is false for ALL profiles → nothing is dropped.
	// This means kubernetes IS available on a workstation, which is correct
	// (a workstation legitimately offers k8s as a product feature).
	workstationProfiles := srv.buildInfoProfiles("docker")
	byName := make(map[string]BrokerProfile, len(workstationProfiles))
	for _, p := range workstationProfiles {
		byName[p.Name] = p
	}
	if _, ok := byName["local"]; !ok {
		t.Error("FACT 3: local profile should be present on docker broker")
	}
	if _, ok := byName["remote"]; !ok {
		t.Error("FACT 3: remote profile should be present on docker broker — workstation legitimately offers k8s")
	}
	if len(workstationProfiles) < 2 {
		t.Errorf("FACT 3: expected >= 2 profiles on docker broker, got %d", len(workstationProfiles))
	}

	// === SUPPLEMENTARY: len(profiles)==0 fallback produces correct result ===
	// The existing fallback at handlers.go:226-230 returns:
	//   {Name: "default", Type: defaultRuntimeType, Available: true}
	// To exercise this, seed settings with ONLY local-only profiles. On a
	// cloudrun-sandbox broker, all local-only profiles are dropped → the
	// filter loop produces an empty list → the fallback fires.
	fallbackDir := t.TempDir()
	fallbackScionDir := filepath.Join(fallbackDir, ".scion")
	if err := os.MkdirAll(fallbackScionDir, 0755); err != nil {
		t.Fatal(err)
	}
	// Override ALL profiles (including embedded defaults' "remote") to
	// local-only runtimes. koanf merge overwrites nested map values, so
	// profiles.remote.runtime = podman replaces the embedded kubernetes.
	localOnlyYAML := `schema_version: "1"
active_profile: local
runtimes:
  docker:
    type: docker
  podman:
    type: podman
profiles:
  local:
    runtime: docker
  remote:
    runtime: podman
`
	if err := os.WriteFile(filepath.Join(fallbackScionDir, "settings.yaml"), []byte(localOnlyYAML), 0644); err != nil {
		t.Fatal(err)
	}
	_ = os.Setenv("HOME", fallbackDir)
	// Note: origHome defer from the top of this function will restore HOME.

	fallbackProfiles := srv.buildInfoProfiles("cloudrun-sandbox")
	if len(fallbackProfiles) != 1 {
		t.Fatalf("FALLBACK: expected 1 profile from len==0 fallback, got %d", len(fallbackProfiles))
	}
	if fallbackProfiles[0].Name != "default" {
		t.Errorf("FALLBACK: expected name 'default', got %q", fallbackProfiles[0].Name)
	}
	if fallbackProfiles[0].Type != "cloudrun-sandbox" {
		t.Errorf("FALLBACK: expected type 'cloudrun-sandbox', got %q", fallbackProfiles[0].Type)
	}
}

// newErrorRuntimeTestServer returns a test server whose default runtime is
// the degraded *runtime.ErrorRuntime the broker falls back to when runtime
// resolution fails at startup (ptone/scion#2766).
func newErrorRuntimeTestServer(t *testing.T) *Server {
	t.Helper()
	srv := newTestServer(t)
	srv.runtime = &runtime.ErrorRuntime{Err: errors.New("failed to build kubernetes client")}
	return srv
}

func TestHealthInfoHealthyRuntime(t *testing.T) {
	srv := newTestServer(t)

	health := srv.GetHealthInfo(context.Background())
	if health.Status != "healthy" {
		t.Errorf("status = %q, want healthy", health.Status)
	}
	if got := health.Checks["mock"]; got != "available" {
		t.Errorf("checks[mock] = %q, want available", got)
	}
	if _, ok := health.Checks["runtime"]; ok {
		t.Errorf("checks[runtime] present for a healthy runtime: %v", health.Checks)
	}
}

func TestHealthInfoErrorRuntimeDegraded(t *testing.T) {
	srv := newErrorRuntimeTestServer(t)

	health := srv.GetHealthInfo(context.Background())
	if health.Status != "degraded" {
		t.Errorf("status = %q, want degraded", health.Status)
	}
	if got := health.Checks["runtime"]; got != "unavailable" {
		t.Errorf("checks[runtime] = %q, want unavailable", got)
	}
	if _, ok := health.Checks["error"]; ok {
		t.Errorf("checks[error] must not be reported: %v", health.Checks)
	}
}

func TestHealthzErrorRuntimeStatusCode(t *testing.T) {
	srv := newErrorRuntimeTestServer(t)

	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	// /healthz is a liveness signal: it stays 200 and reports degraded in
	// the body, so a restart loop does not hide a persistent runtime fault.
	if w.Code != http.StatusOK {
		t.Errorf("healthz code = %d, want %d", w.Code, http.StatusOK)
	}
	var resp HealthResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Status != "degraded" {
		t.Errorf("healthz status = %q, want degraded", resp.Status)
	}
	if got := resp.Checks["runtime"]; got != "unavailable" {
		t.Errorf("healthz checks[runtime] = %q, want unavailable", got)
	}
}

func TestReadyzErrorRuntimeNotReady(t *testing.T) {
	srv := newErrorRuntimeTestServer(t)

	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("readyz code = %d, want %d", w.Code, http.StatusServiceUnavailable)
	}
	var body map[string]string
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["status"] != "not_ready" {
		t.Errorf("readyz status = %q, want not_ready", body["status"])
	}
}

// TestHealthEndpointsConcurrentSwapRuntime runs /healthz and /readyz while
// SwapRuntime flips the default runtime between a usable and a degraded
// one. Under -race it checks that the health paths read s.runtime under
// s.mu; each response must match one of the two runtimes.
func TestHealthEndpointsConcurrentSwapRuntime(t *testing.T) {
	srv := newTestServer(t)
	good := &runtime.MockRuntime{}
	bad := &runtime.ErrorRuntime{Err: errors.New("failed to build kubernetes client")}
	handler := srv.Handler()

	var wg sync.WaitGroup
	wg.Add(1)
	defer wg.Wait()
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			if i%2 == 0 {
				srv.SwapRuntime(bad, config.RuntimeBuildRecord{})
			} else {
				srv.SwapRuntime(good, config.RuntimeBuildRecord{})
			}
		}
	}()

	for i := 0; i < 200; i++ {
		health := srv.GetHealthInfo(context.Background())
		switch health.Status {
		case "healthy":
			if health.Checks["runtime"] != "" {
				t.Fatalf("healthy with checks[runtime]: %v", health.Checks)
			}
		case "degraded":
			if health.Checks["runtime"] != "unavailable" {
				t.Fatalf("degraded without checks[runtime]: %v", health.Checks)
			}
		default:
			t.Fatalf("unexpected status %q", health.Status)
		}

		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		if w.Code != http.StatusOK && w.Code != http.StatusServiceUnavailable {
			t.Fatalf("readyz code = %d", w.Code)
		}

		w = httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/info", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("info code = %d", w.Code)
		}
	}
}
