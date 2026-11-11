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
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// TestProfileResolutionMiddleware_FlatOnly: every request a flat instance
// serves carries flat profile resolution; a legacy Runtime Broker's requests
// carry none (legacy resolution).
func TestProfileResolutionMiddleware_FlatOnly(t *testing.T) {
	f := newFlatInstanceTestServer(t, flatInstanceOpts{hubInProcess: true})
	var got config.ProfileResolutionMode
	probe := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = config.ProfileResolutionFrom(r.Context())
	})
	f.srv.applyMiddleware(probe).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if got != config.ProfileResolutionFlatInstance {
		t.Fatalf("flat instance request mode = %v, want flat", got)
	}

	legacy := newTestServer(t)
	got = config.ProfileResolutionFlatInstance
	legacy.applyMiddleware(probe).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if got != config.ProfileResolutionLegacy {
		t.Fatalf("legacy request mode = %v, want legacy", got)
	}
}

// TestFlatSettingsView_SkipsActiveProfile: the Runtime Broker's own settings
// readers (env-gather extraction, harness policy) see no profile tier on a
// flat instance and the unchanged settings on a legacy one.
func TestFlatSettingsView_SkipsActiveProfile(t *testing.T) {
	vs := &config.VersionedSettings{ActiveProfile: "batch", Profiles: map[string]config.V1ProfileConfig{"batch": {Runtime: "docker"}}}
	f := newFlatInstanceTestServer(t, flatInstanceOpts{hubInProcess: true})
	if v := f.srv.settingsView(vs); v.ActiveProfile != "" || len(v.Profiles) != 0 {
		t.Fatalf("flat view keeps the profile tier: %+v", v)
	}
	if vs.ActiveProfile != "batch" {
		t.Fatal("the loaded settings were modified")
	}
	if v := newTestServer(t).settingsView(vs); v != vs {
		t.Fatal("a legacy Runtime Broker must read the settings unchanged")
	}
}
