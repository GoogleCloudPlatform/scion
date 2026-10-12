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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// safe_to_evict on runtime and profile entries round-trips the admin
// server-config API in file mode and in database mode.

const safeToEvictPutBody = `{"runtimes":{"gke":{"type":"kubernetes","safe_to_evict":false}},
	"profiles":{"gke":{"runtime":"gke"},"evictable":{"runtime":"gke","safe_to_evict":true}}}`

func assertSafeToEvictEntries(t *testing.T, runtimes map[string]config.V1RuntimeConfig, profiles map[string]config.V1ProfileConfig) {
	t.Helper()
	rt, ok := runtimes["gke"]
	if !ok || rt.SafeToEvict == nil || *rt.SafeToEvict {
		t.Errorf("runtimes.gke.safe_to_evict: want false, got %+v", rt.SafeToEvict)
	}
	if p := profiles["gke"]; p.SafeToEvict != nil {
		t.Errorf("profiles.gke.safe_to_evict: want unset, got %v", *p.SafeToEvict)
	}
	if p := profiles["evictable"]; p.SafeToEvict == nil || !*p.SafeToEvict {
		t.Errorf("profiles.evictable.safe_to_evict: want true, got %+v", p.SafeToEvict)
	}
}

func TestHandlePutServerConfig_SafeToEvict_FileModeRoundTrip(t *testing.T) {
	srv := &Server{}
	rr, settingsPath := fileModePutServerConfig(t, srv, safeToEvictPutBody)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "safe_to_evict: false") || !strings.Contains(string(data), "safe_to_evict: true") {
		t.Errorf("settings.yaml should carry both values, got: %s", data)
	}

	// Loaded back the way the broker loads it, the resolver sees the values.
	projectDir := filepath.Join(filepath.Dir(filepath.Dir(settingsPath)), "proj", ".scion")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	vs, _, err := config.LoadEffectiveSettings(projectDir)
	if err != nil {
		t.Fatal(err)
	}
	assertSafeToEvictEntries(t, vs.Runtimes, vs.Profiles)
	if v, src := vs.ResolveSafeToEvictWithSource("gke"); v == nil || *v || src != "runtimes.gke.safe_to_evict" {
		t.Errorf("resolve gke: got %v from %q", v, src)
	}
}

func TestPutServerConfigDB_SafeToEvict_RoundTrip(t *testing.T) {
	srv, _, ops := newTestDBServer(t)
	rr := httptest.NewRecorder()
	srv.handlePutServerConfigDB(rr, adminRequest(http.MethodPut, "/api/v1/admin/server-config", safeToEvictPutBody), ops)
	if rr.Code != http.StatusOK {
		t.Fatalf("PUT: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	snap := ops.Snapshot()
	assertSafeToEvictEntries(t, snap.Runtimes, snap.Profiles)

	getRR := httptest.NewRecorder()
	srv.handleGetServerConfigDB(getRR, adminRequest(http.MethodGet, "/api/v1/admin/server-config", ""), ops)
	if getRR.Code != http.StatusOK {
		t.Fatalf("GET: expected 200, got %d: %s", getRR.Code, getRR.Body.String())
	}
	var resp ServerConfigDBResponse
	if err := json.Unmarshal(getRR.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	assertSafeToEvictEntries(t, resp.Runtimes, resp.Profiles)
}

func TestPutServerConfigDB_SafeToEvict_NonBooleanRejected(t *testing.T) {
	srv, _, ops := newTestDBServer(t)
	rr := httptest.NewRecorder()
	srv.handlePutServerConfigDB(rr, adminRequest(http.MethodPut, "/api/v1/admin/server-config",
		`{"runtimes":{"gke":{"type":"kubernetes","safe_to_evict":"false"}}}`), ops)
	if rr.Code == http.StatusOK {
		t.Fatalf("expected a non-boolean safe_to_evict to be rejected, got 200: %s", rr.Body.String())
	}
}

// The settings.yaml fallback used before any DB write (bootstrap koanf)
// carries the value too.
func TestSnapshot_SafeToEvict_FromBootstrapFile(t *testing.T) {
	fileK := newFileKoanf(t, map[string]interface{}{
		"runtimes.gke.type":                "kubernetes",
		"runtimes.gke.safe_to_evict":       false,
		"profiles.evictable.runtime":       "gke",
		"profiles.evictable.safe_to_evict": true,
		"profiles.gke.runtime":             "gke",
	})
	ops := NewOperationalSettings(newFakeHubSettingStore(), fileK, emptyKoanf())
	snap := ops.Snapshot()
	assertSafeToEvictEntries(t, snap.Runtimes, snap.Profiles)
}

// putWarnings decodes the "warnings" list from a PUT response.
func putWarnings(t *testing.T, rr *httptest.ResponseRecorder) []string {
	t.Helper()
	var resp struct {
		Warnings []string `json:"warnings"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	return resp.Warnings
}

// safe_to_evict on a non-Kubernetes runtime is saved and ignored; the save
// response carries the same warning as config validate. A Kubernetes or
// remote runtime gets none.
func TestHandlePutServerConfig_SafeToEvict_FileModeWarnings(t *testing.T) {
	srv := &Server{}
	rr, _ := fileModePutServerConfig(t, srv, `{"runtimes":{"docker":{"type":"docker","safe_to_evict":false},
		"far":{"type":"remote","safe_to_evict":false}},
		"profiles":{"local":{"runtime":"docker","safe_to_evict":true},"far":{"runtime":"far","safe_to_evict":true}}}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	got := putWarnings(t, rr)
	if len(got) != 2 || !strings.Contains(got[0], "profiles.local.safe_to_evict") || !strings.Contains(got[1], "runtimes.docker.safe_to_evict") {
		t.Errorf("warnings: got %q", got)
	}

	rr, _ = fileModePutServerConfig(t, srv, safeToEvictPutBody)
	if got := putWarnings(t, rr); len(got) != 0 {
		t.Errorf("kubernetes runtime: want no warnings, got %q", got)
	}
}

func TestPutServerConfigDB_SafeToEvict_Warnings(t *testing.T) {
	srv, _, ops := newTestDBServer(t)
	put := func(body string) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		srv.handlePutServerConfigDB(rr, adminRequest(http.MethodPut, "/api/v1/admin/server-config", body), ops)
		if rr.Code != http.StatusOK {
			t.Fatalf("PUT: expected 200, got %d: %s", rr.Code, rr.Body.String())
		}
		return rr
	}

	rr := put(`{"runtimes":{"docker":{"type":"docker"},"gke":{"type":"kubernetes","safe_to_evict":false}}}`)
	if got := putWarnings(t, rr); len(got) != 0 {
		t.Errorf("want no warnings, got %q", got)
	}

	// Only profiles in this request: the profile is checked against the
	// runtimes already saved.
	rr = put(`{"profiles":{"local":{"runtime":"docker","safe_to_evict":false},"gke":{"runtime":"gke","safe_to_evict":true}}}`)
	got := putWarnings(t, rr)
	if len(got) != 1 || !strings.Contains(got[0], "profiles.local.safe_to_evict") {
		t.Errorf("warnings: got %q", got)
	}
}
