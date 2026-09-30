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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
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
// on), web.only_thing (web-only, default on), and hub.server_only
// (server-only, default on, so a test asserting web-layer filtering has a
// name that must NOT appear in a web-facing result), per ptone/scion#2217.
func testRegistry(t *testing.T) *experiments.Registry {
	t.Helper()
	reg, err := experiments.NewRegistry([]experiments.Experiment{
		testExperiment("hub.test_gate", true, experiments.LayerServer, experiments.LayerWeb),
		testExperiment("web.only_thing", true, experiments.LayerWeb),
		testExperiment("hub.server_only", true, experiments.LayerServer),
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
	// ptone/scion#2217: a zero-value &Server{} (no New()) must resolve
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
	// hub.server_only has no LayerWeb: it must never appear in a map meant
	// for the web-facing /api/v1/experiments response (1a-ii).
	if _, present := got["hub.server_only"]; present {
		t.Error("resolvedExperiments() must exclude server-only experiments")
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

// TestRefreshMalformedPredicateMatchesParseExperimentsDoc proves that the
// real Refresh ingest path -- not a second, in-test copy of the predicate --
// agrees with opsettings.ParseExperimentsDoc on the shared document table
// ("ParseExperimentsDoc applies exactly the Refresh/Update predicate";
// ptone/scion#2217). It reads the cached sectionState.Malformed directly
// (not through ExperimentsSnapshot, which re-parses and would hide a
// disagreement between Refresh's ingest-time check and ParseExperimentsDoc).
//
// Only the Refresh side needs this table: Update rejects a schema-invalid
// document (including a non-boolean override value) in opsettings.Validate
// before writing, so its post-write sec.New() check never sees one.
func TestRefreshMalformedPredicateMatchesParseExperimentsDoc(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{"valid with overrides", `{"overrides":{"hub.test_gate":false}}`},
		{"valid empty object", `{}`},
		{"invalid json", `not json`},
		{"non-boolean override value", `{"overrides":{"hub.test_gate":"nope"}}`},
		{"parseable doc with extra top-level key", `{"overrides":{},"unexpected":true}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fakeStore := newFakeHubSettingStore()
			fakeStore.seed("experiments", json.RawMessage(tt.raw))
			ops := NewOperationalSettings(fakeStore, emptyKoanf(), emptyKoanf())
			if _, err := ops.Refresh(context.Background()); err != nil {
				t.Fatalf("Refresh: %v", err)
			}

			ops.mu.RLock()
			cachedMalformed := ops.cache["experiments"].Malformed
			ops.mu.RUnlock()

			_, wantMalformed := opsettings.ParseExperimentsDoc(json.RawMessage(tt.raw))
			if cachedMalformed != wantMalformed {
				t.Errorf("Refresh cached Malformed=%v, opsettings.ParseExperimentsDoc malformed=%v", cachedMalformed, wantMalformed)
			}
		})
	}
}

// TestExperimentEnabled_NonBooleanOverrideValueResolvesOff exercises the real
// Refresh path (not a synthetic ExperimentsSnapshot) with a row whose bytes
// are valid JSON but whose override value is not a boolean. Refresh's
// sec.New() type check catches this and caches the section as malformed;
// hub.test_gate has LayerServer, so the malformed-row policy resolves it OFF.
func TestExperimentEnabled_NonBooleanOverrideValueResolvesOff(t *testing.T) {
	srv, _ := serverWithExperimentsDoc(t, `{"overrides":{"hub.test_gate":"nope"}}`)
	if srv.experimentEnabled("hub.test_gate") {
		t.Error("hub.test_gate must resolve OFF when the stored row has a non-boolean override value")
	}
}

// --- ExperimentsSnapshot / ReadAuthoritativeExperiments ---

// testUpdatedAt is a fixed, non-zero timestamp used to prove that
// ExperimentsSnapshot/ReadAuthoritativeExperiments propagate every metadata
// field from a single seeded row, not just Present/Malformed.
var testUpdatedAt = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

func TestExperimentsSnapshot(t *testing.T) {
	tests := []struct {
		name          string
		seed          bool
		raw           string
		wantPresent   bool
		wantMalformed bool
		wantRevision  int64
		wantUpdatedAt time.Time
		wantUpdatedBy string
		wantOverrides map[string]bool
	}{
		{
			name: "absent row", seed: false,
			wantPresent: false, wantMalformed: false, wantRevision: 0,
			wantUpdatedAt: time.Time{}, wantUpdatedBy: "", wantOverrides: map[string]bool{},
		},
		{
			name: "valid row", seed: true, raw: `{"overrides":{"hub.test_gate":false}}`,
			wantPresent: true, wantMalformed: false, wantRevision: 7,
			wantUpdatedAt: testUpdatedAt, wantUpdatedBy: "admin@example.com",
			wantOverrides: map[string]bool{"hub.test_gate": false},
		},
		{
			name: "malformed row", seed: true, raw: `not json`,
			wantPresent: true, wantMalformed: true, wantRevision: 7,
			wantUpdatedAt: testUpdatedAt, wantUpdatedBy: "admin@example.com",
			wantOverrides: map[string]bool{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fakeStore := newFakeHubSettingStore()
			if tt.seed {
				fakeStore.settings["experiments"] = &store.HubSetting{
					ID: "experiments", Section: "experiments", Value: json.RawMessage(tt.raw),
					Revision: 7, UpdatedBy: "admin@example.com", UpdatedAt: testUpdatedAt,
				}
			}
			ops := NewOperationalSettings(fakeStore, emptyKoanf(), emptyKoanf())
			if _, err := ops.Refresh(context.Background()); err != nil {
				t.Fatalf("Refresh: %v", err)
			}
			snap := ops.ExperimentsSnapshot()
			if snap.Present != tt.wantPresent || snap.Malformed != tt.wantMalformed ||
				snap.Revision != tt.wantRevision || !snap.UpdatedAt.Equal(tt.wantUpdatedAt) ||
				snap.UpdatedBy != tt.wantUpdatedBy {
				t.Fatalf("snap = %+v, want Present=%v Malformed=%v Revision=%d UpdatedAt=%v UpdatedBy=%q",
					snap, tt.wantPresent, tt.wantMalformed, tt.wantRevision, tt.wantUpdatedAt, tt.wantUpdatedBy)
			}
			assertOverrides(t, snap.Overrides, tt.wantOverrides)
		})
	}
}

func TestReadAuthoritativeExperiments(t *testing.T) {
	tests := []struct {
		name          string
		seed          bool
		raw           string
		wantRevision  int64
		wantMalformed bool
		wantOverrides map[string]bool
	}{
		{"absent row", false, "", 0, false, map[string]bool{}},
		{"valid row", true, `{"overrides":{"hub.test_gate":true}}`, 1, false, map[string]bool{"hub.test_gate": true}},
		{"malformed row", true, `not json`, 1, true, map[string]bool{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fakeStore := newFakeHubSettingStore()
			if tt.seed {
				fakeStore.seed("experiments", json.RawMessage(tt.raw))
			}
			ops := NewOperationalSettings(fakeStore, emptyKoanf(), emptyKoanf())
			res := ops.ReadAuthoritativeExperiments(context.Background())
			if res.Err != nil || res.Malformed != tt.wantMalformed || res.Revision != tt.wantRevision {
				t.Fatalf("res = %+v, want revision=%d malformed=%v err=nil", res, tt.wantRevision, tt.wantMalformed)
			}
			assertOverrides(t, res.Overrides, tt.wantOverrides)
		})
	}
}

// assertOverrides checks that got is non-nil and holds exactly the entries
// of want. Every caller in this file expects a non-nil map (absent or
// malformed still means "{}", not nil; ptone/scion#2217), because 1a-ii's
// PUT merges the request onto ReadAuthoritativeExperiments().Overrides and a
// nil map there panics on the first write to a hub with no row.
func assertOverrides(t *testing.T, got, want map[string]bool) {
	t.Helper()
	if got == nil {
		t.Fatalf("Overrides is nil, want non-nil map %v", want)
	}
	if len(got) != len(want) {
		t.Fatalf("Overrides = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("Overrides[%q] = %v, want %v", k, got[k], v)
		}
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
