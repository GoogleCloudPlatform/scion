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

package runtime

import (
	"errors"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// installDanglingProfileOverlay installs an overlay whose stored profile
// "cluster" names a runtime ("gke") the stored runtimes lack, and whose
// "main" profile resolves (ptone/scion#4889).
func installDanglingProfileOverlay(t *testing.T) {
	t.Helper()
	o := config.NewSettingsOverlay()
	o.Update(
		map[string]config.V1RuntimeConfig{"main-rt": {Type: "docker"}},
		map[string]config.V1ProfileConfig{"main": {Runtime: "main-rt"}, "cluster": {Runtime: "gke"}},
		nil)
	config.SetGlobalSettingsOverlay(o)
}

// A named stored profile whose runtime is not defined builds an
// ErrorRuntime carrying the typed error, not an auto-detected runtime.
func TestGetRuntime_NamedStoredProfileRuntimeMissingIsError(t *testing.T) {
	projectDir := br4Setup(t)
	installDanglingProfileOverlay(t)

	rt, rec := GetRuntimeWithRecord(projectDir, "cluster")
	er, ok := rt.(*ErrorRuntime)
	if !ok {
		t.Fatalf("GetRuntime(cluster) = %T (%s), want *ErrorRuntime", rt, rt.Name())
	}
	var sre *config.StoredProfileRuntimeNotFoundError
	if !errors.As(er.Err, &sre) || sre.Profile != "cluster" || sre.Runtime != "gke" {
		t.Errorf("ErrorRuntime.Err = %v, want *StoredProfileRuntimeNotFoundError for cluster/gke", er.Err)
	}
	if rec.Present {
		t.Errorf("record = %+v, want the zero record for an ErrorRuntime", rec)
	}
}

// The default profile (empty name) keeps the existing fallback, so a
// broker's startup default runtime is still built.
func TestGetRuntime_DefaultStoredProfileRuntimeMissingKeepsFallback(t *testing.T) {
	projectDir := br4Setup(t)
	o := config.NewSettingsOverlay()
	o.Update(
		map[string]config.V1RuntimeConfig{"main-rt": {Type: "docker"}},
		map[string]config.V1ProfileConfig{"main": {Runtime: "gke"}},
		nil)
	config.SetGlobalSettingsOverlay(o)

	if rt := GetRuntime(projectDir, ""); rt == nil {
		t.Fatal("GetRuntime(\"\") = nil")
	} else if er, isErr := rt.(*ErrorRuntime); isErr {
		var sre *config.StoredProfileRuntimeNotFoundError
		if errors.As(er.Err, &sre) {
			t.Errorf("GetRuntime(\"\") returned the stored-profile ErrorRuntime: %v", er.Err)
		}
	}
}

// A named stored profile that resolves is unaffected.
func TestGetRuntime_NamedStoredProfileResolves(t *testing.T) {
	projectDir := br4Setup(t)
	installDanglingProfileOverlay(t)

	if rt := GetRuntime(projectDir, "main"); rt.Name() != "docker" {
		t.Errorf("GetRuntime(main) = %s, want docker", rt.Name())
	}
}
