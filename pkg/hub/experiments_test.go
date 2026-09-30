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
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// testExperiment returns a minimally valid experiment for registry
// construction in tests.
func testExperiment(name string, def bool, layers ...experiments.Layer) experiments.Experiment {
	return experiments.Experiment{
		Name:        name,
		Title:       "Test experiment",
		Description: "Used only in pkg/hub tests.",
		Default:     def,
		Layers:      layers,
		Stage:       experiments.StageBeta,
		Issue:       "ptone/scion#2217",
		Owner:       "test",
		ReviewBy:    "2026-12-31",
	}
}

// testRegistry returns a registry with hub.test_gate (server+web, default
// on) and web.only_thing (web-only, default on), per design.md §3.2/§3.6.
func testRegistry(t *testing.T) *experiments.Registry {
	t.Helper()
	reg, err := experiments.NewRegistry([]experiments.Experiment{
		testExperiment("hub.test_gate", true, experiments.LayerServer, experiments.LayerWeb),
		testExperiment("web.only_thing", true, experiments.LayerWeb),
	}, nil)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	return reg
}

// serverWithExperimentsDoc builds a Server wired to a test registry and an
// OperationalSettings instance whose "experiments" row is raw (skip seeding
// when raw == "").
func serverWithExperimentsDoc(t *testing.T, raw string) (*Server, *fakeHubSettingStore) {
	t.Helper()
	fakeStore := newFakeHubSettingStore()
	if raw != "" {
		fakeStore.seed("experiments", json.RawMessage(raw))
	}
	ops := NewOperationalSettings(fakeStore, emptyKoanf(), emptyKoanf())
	if _, err := ops.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	srv := &Server{experiments: testRegistry(t)}
	srv.SetOperationalSettings(ops)
	return srv, fakeStore
}

func TestExperimentRegistry_ZeroValueServerFallsBackToDefault(t *testing.T) {
	srv := &Server{}
	if srv.experimentRegistry() != experiments.Default() {
		t.Error("experimentRegistry() on a zero-value Server must return experiments.Default()")
	}
	// design.md §9: a zero-value &Server{} (no New()) must resolve
	// web.terminal_workspace to its compiled default without panicking.
	if !srv.experimentEnabled("web.terminal_workspace") {
		t.Error("zero-value Server: web.terminal_workspace should resolve to its default (true)")
	}
}

func TestExperimentEnabledIn_Resolution(t *testing.T) {
	srv := &Server{experiments: testRegistry(t)}
	tests := []struct {
		name string
		snap ExperimentsSnapshot
		exp  string
		want bool
	}{
		{"unknown is always off", ExperimentsSnapshot{Overrides: map[string]bool{}}, "hub.does_not_exist", false},
		{"override beats default", ExperimentsSnapshot{Overrides: map[string]bool{"hub.test_gate": false}}, "hub.test_gate", false},
		{"no override uses default", ExperimentsSnapshot{Overrides: map[string]bool{}}, "hub.test_gate", true},
		{"malformed: server-layer resolves off", ExperimentsSnapshot{Malformed: true, Overrides: map[string]bool{}}, "hub.test_gate", false},
		{"malformed: web-only resolves to default", ExperimentsSnapshot{Malformed: true, Overrides: map[string]bool{}}, "web.only_thing", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := srv.experimentEnabledIn(tt.snap, tt.exp); got != tt.want {
				t.Errorf("experimentEnabledIn(%q) = %v, want %v", tt.exp, got, tt.want)
			}
		})
	}
}

func TestResolvedExperiments_WebLayerOnlyFromOneSnapshot(t *testing.T) {
	srv, _ := serverWithExperimentsDoc(t, `{"overrides":{"hub.test_gate":false}}`)

	got := srv.resolvedExperiments()
	want := map[string]bool{"hub.test_gate": false, "web.only_thing": true}
	if len(got) != len(want) {
		t.Fatalf("resolvedExperiments() = %v, want %v", got, want)
	}
	for name, v := range want {
		if got[name] != v {
			t.Errorf("resolvedExperiments()[%q] = %v, want %v", name, got[name], v)
		}
	}
}

// --- requireExperiment ---

func TestRequireExperiment_OffReturns404(t *testing.T) {
	srv, _ := serverWithExperimentsDoc(t, `{"overrides":{"hub.test_gate":false}}`)
	called := false
	handler := srv.requireExperiment("hub.test_gate", func(w http.ResponseWriter, r *http.Request) {
		called = true
	})

	rr := httptest.NewRecorder()
	handler(rr, httptest.NewRequest(http.MethodGet, "/x", nil))

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
	if called {
		t.Error("handler must not run when the experiment is off")
	}
	var body ErrorResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("response body is not JSON: %v", err)
	}
	if body.Error.Code != ErrCodeNotFound {
		t.Errorf("response error.code = %q, want %q", body.Error.Code, ErrCodeNotFound)
	}
}

func TestRequireExperiment_OnRunsHandler(t *testing.T) {
	srv, _ := serverWithExperimentsDoc(t, "")
	called := false
	handler := srv.requireExperiment("hub.test_gate", func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	rr := httptest.NewRecorder()
	handler(rr, httptest.NewRequest(http.MethodGet, "/x", nil))

	if rr.Code != http.StatusOK || !called {
		t.Fatalf("status = %d, called = %v; want 200 and called", rr.Code, called)
	}
}

func TestRequireExperiment_FlipsWithoutReregistering(t *testing.T) {
	srv, fakeStore := serverWithExperimentsDoc(t, "")
	ops := srv.GetOperationalSettings()
	handler := srv.requireExperiment("hub.test_gate", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// On by default (no row yet).
	rr := httptest.NewRecorder()
	handler(rr, httptest.NewRequest(http.MethodGet, "/x", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("before override: status = %d, want 200", rr.Code)
	}

	// Flip to off via the store, without re-registering the route.
	fakeStore.seed("experiments", json.RawMessage(`{"overrides":{"hub.test_gate":false}}`))
	if _, err := ops.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	rr = httptest.NewRecorder()
	handler(rr, httptest.NewRequest(http.MethodGet, "/x", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("after override: status = %d, want 404", rr.Code)
	}
}

func TestRequireExperiment_PanicsAtRegistration(t *testing.T) {
	tests := []struct {
		name string
		exp  string
	}{
		{"unregistered name", "hub.does_not_exist"},
		{"web-only experiment", "web.only_thing"},
	}
	srv := &Server{experiments: testRegistry(t)}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Errorf("expected requireExperiment to panic for %q", tt.exp)
				}
			}()
			srv.requireExperiment(tt.exp, func(w http.ResponseWriter, r *http.Request) {})
		})
	}
}

// --- ExperimentsSnapshot / ReadAuthoritativeExperiments ---

func TestExperimentsSnapshot(t *testing.T) {
	tests := []struct {
		name          string
		raw           string // "" = no row
		wantPresent   bool
		wantMalformed bool
		wantOverride  bool // hub.test_gate present in Overrides
	}{
		{"absent row", "", false, false, false},
		{"valid row", `{"overrides":{"hub.test_gate":false}}`, true, false, true},
		{"malformed row", `not json`, true, true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fakeStore := newFakeHubSettingStore()
			if tt.raw != "" {
				fakeStore.seed("experiments", json.RawMessage(tt.raw))
			}
			ops := NewOperationalSettings(fakeStore, emptyKoanf(), emptyKoanf())
			if _, err := ops.Refresh(context.Background()); err != nil {
				t.Fatalf("Refresh: %v", err)
			}
			snap := ops.ExperimentsSnapshot()
			if snap.Present != tt.wantPresent || snap.Malformed != tt.wantMalformed {
				t.Fatalf("snap = %+v, want Present=%v Malformed=%v", snap, tt.wantPresent, tt.wantMalformed)
			}
			if _, ok := snap.Overrides["hub.test_gate"]; ok != tt.wantOverride {
				t.Errorf("Overrides[hub.test_gate] present = %v, want %v", ok, tt.wantOverride)
			}
		})
	}
}

func TestReadAuthoritativeExperiments(t *testing.T) {
	tests := []struct {
		name          string
		raw           string
		wantRevision  int64
		wantMalformed bool
	}{
		{"absent row", "", 0, false},
		{"valid row", `{"overrides":{"hub.test_gate":true}}`, 1, false},
		{"malformed row", `not json`, 1, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fakeStore := newFakeHubSettingStore()
			if tt.raw != "" {
				fakeStore.seed("experiments", json.RawMessage(tt.raw))
			}
			ops := NewOperationalSettings(fakeStore, emptyKoanf(), emptyKoanf())
			res := ops.ReadAuthoritativeExperiments(context.Background())
			if res.Err != nil || res.Malformed != tt.wantMalformed || res.Revision != tt.wantRevision {
				t.Fatalf("res = %+v, want revision=%d malformed=%v err=nil", res, tt.wantRevision, tt.wantMalformed)
			}
		})
	}
}

// getFailingHubSettingStore stands in for a DB outage on reads.
type getFailingHubSettingStore struct {
	*fakeHubSettingStore
}

func (f *getFailingHubSettingStore) GetHubSetting(context.Context, string) (*store.HubSetting, error) {
	return nil, errors.New("db unavailable")
}

func TestReadAuthoritativeExperiments_StoreError(t *testing.T) {
	failing := &getFailingHubSettingStore{newFakeHubSettingStore()}
	ops := NewOperationalSettings(failing, emptyKoanf(), emptyKoanf())
	res := ops.ReadAuthoritativeExperiments(context.Background())
	if res.Err == nil {
		t.Fatal("expected a store error to be propagated")
	}
}
