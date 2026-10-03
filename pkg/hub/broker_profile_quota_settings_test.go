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

package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// max_agents (ptone/scion#2728) round-trips through the DB settings and
// the settings overlay, and is what the hub's limit lookup resolves.
func TestPutServerConfigDB_MaxAgents_RoundTrip(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	dir := filepath.Join(tmpHome, ".scion")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "settings.yaml"), []byte(`schema_version: "1"
runtimes:
  docker:
    type: docker
profiles:
  local:
    runtime: docker
    max_agents: 3
`), 0o600); err != nil {
		t.Fatal(err)
	}
	old := config.GetGlobalSettingsOverlay()
	t.Cleanup(func() { config.SetGlobalSettingsOverlay(old) })
	config.SetGlobalSettingsOverlay(config.NewSettingsOverlay())

	srv, _, ops := newTestDBServer(t)
	put := func(body string) {
		t.Helper()
		rr := httptest.NewRecorder()
		srv.handlePutServerConfigDB(rr, adminRequest(http.MethodPut, "/api/v1/admin/server-config", body), ops)
		if rr.Code != http.StatusOK {
			t.Fatalf("PUT %s: expected 200, got %d: %s", body, rr.Code, rr.Body.String())
		}
	}
	get := func() ServerConfigDBResponse {
		t.Helper()
		rr := httptest.NewRecorder()
		srv.handleGetServerConfigDB(rr, adminRequest(http.MethodGet, "/api/v1/admin/server-config", ""), ops)
		if rr.Code != http.StatusOK {
			t.Fatalf("GET: expected 200, got %d: %s", rr.Code, rr.Body.String())
		}
		var resp ServerConfigDBResponse
		if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		return resp
	}

	put(`{"runtimes": {"k8s": {"type": "kubernetes", "max_agents": 7}}, "profiles": {"gke": {"runtime": "k8s", "max_agents": 4}, "gke2": {"runtime": "k8s"}}}`)
	resp := get()
	if got := resp.Profiles["gke"].MaxAgents; got != 4 {
		t.Fatalf("GET after PUT: profiles.gke.max_agents = %d, want 4", got)
	}
	if got := resp.Runtimes["k8s"].MaxAgents; got != 7 {
		t.Fatalf("GET after PUT: runtimes.k8s.max_agents = %d, want 7", got)
	}

	// Edit another field of the profile the way the admin form does, then
	// write a different section.
	profiles := resp.Profiles
	gke := profiles["gke"]
	gke.Timezone = "Europe/Paris"
	profiles["gke"] = gke
	body, err := json.Marshal(map[string]interface{}{"profiles": profiles})
	if err != nil {
		t.Fatal(err)
	}
	put(string(body))
	put(`{"server": {"hub": {"admin_mode": false}}}`)
	resp = get()
	if got := resp.Profiles["gke"].MaxAgents; got != 4 {
		t.Errorf("after editing another field: profiles.gke.max_agents = %d, want 4", got)
	}

	ApplySnapshot(srv, ops.Snapshot())

	// The overlay resolves it.
	gs, _, err := config.LoadGlobalSettingsWithOverlay()
	if err != nil {
		t.Fatal(err)
	}
	if limit, source := gs.ResolveAgentLimit("gke"); limit != 4 || source != "profiles.gke.max_agents" {
		t.Errorf("overlay: gke = %d (%s), want 4 from profiles.gke.max_agents", limit, source)
	}

	// And so does the hub's own limit lookup.
	vs, ok := srv.agentLimitSettings(context.Background())
	if !ok || vs == nil {
		t.Fatalf("agentLimitSettings: ok=%v vs=%v", ok, vs)
	}
	for _, tt := range []struct {
		profile string
		limit   int
		source  string
	}{
		{"gke", 4, "profiles.gke.max_agents"},
		{"gke2", 7, "runtimes.k8s.max_agents"},
	} {
		if limit, source := vs.ResolveAgentLimit(tt.profile); limit != tt.limit || source != tt.source {
			t.Errorf("agentLimitSettings %s = %d (%s), want %d (%s)", tt.profile, limit, source, tt.limit, tt.source)
		}
	}
}

func TestPutServerConfigDB_MaxAgents_NegativeRejected(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	srv, _, ops := newTestDBServer(t)
	for _, body := range []string{
		`{"profiles": {"gke": {"runtime": "k8s", "max_agents": -1}}}`,
		`{"runtimes": {"k8s": {"type": "kubernetes", "max_agents": -1}}}`,
	} {
		rr := httptest.NewRecorder()
		srv.handlePutServerConfigDB(rr, adminRequest(http.MethodPut, "/api/v1/admin/server-config", body), ops)
		if rr.Code < 400 {
			t.Errorf("PUT %s: expected a 4xx, got %d: %s", body, rr.Code, rr.Body.String())
		}
	}
}
