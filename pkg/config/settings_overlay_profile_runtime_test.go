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

import (
	"errors"
	"strings"
	"testing"
)

// overlayWithDanglingProfile stores a profile whose runtime is not among the
// stored runtimes (ptone/scion#4889).
func overlayWithDanglingProfile() *SettingsOverlay {
	o := NewSettingsOverlay()
	o.Update(
		map[string]V1RuntimeConfig{"docker": {Type: "docker"}},
		map[string]V1ProfileConfig{"local": {Runtime: "docker"}, "cluster": {Runtime: "gke"}},
		nil)
	return o
}

// A stored profile naming a runtime that is not defined yields a typed
// error, for a named profile and for the default profile.
func TestResolveRuntime_StoredProfileRuntimeMissingIsTyped(t *testing.T) {
	for _, tc := range []struct {
		name, active, requested string
	}{
		{name: "named", active: "local", requested: "cluster"},
		{name: "default profile", active: "cluster", requested: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vs := overlayActiveProfileFixture(tc.active)
			overlayWithDanglingProfile().Apply(vs)

			_, _, err := vs.ResolveRuntime(tc.requested)
			var sre *StoredProfileRuntimeNotFoundError
			if !errors.As(err, &sre) {
				t.Fatalf("ResolveRuntime(%q) error = %v, want *StoredProfileRuntimeNotFoundError", tc.requested, err)
			}
			if sre.Profile != "cluster" || sre.Runtime != "gke" {
				t.Errorf("error fields = %+v, want profile cluster, runtime gke", sre)
			}
			if !IsStoredProfileError(err) {
				t.Errorf("IsStoredProfileError(%v) = false, want true", err)
			}
			for _, want := range []string{`runtime "gke" not found for profile "cluster"`, "available runtimes: docker"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err, want)
				}
			}
		})
	}
}

// Without overlaid profiles the plain error is kept, and it is not a
// stored-profile error.
func TestResolveRuntime_ProfileRuntimeMissingUntypedWithoutOverlay(t *testing.T) {
	vs := &VersionedSettings{
		ActiveProfile: "cluster",
		Runtimes:      map[string]V1RuntimeConfig{"docker": {Type: "docker"}},
		Profiles:      map[string]V1ProfileConfig{"cluster": {Runtime: "gke"}},
	}
	_, _, err := vs.ResolveRuntime("")
	var sre *StoredProfileRuntimeNotFoundError
	if errors.As(err, &sre) || IsStoredProfileError(err) {
		t.Fatalf("no overlay: got a stored-profile error: %v", err)
	}
	if err == nil || err.Error() != `runtime "gke" not found for profile "cluster"` {
		t.Errorf("error = %v, want the plain runtime-not-found error", err)
	}
}

// Overlaid profiles over the file's runtimes (no stored runtimes) are still
// authoritative: a profile naming a runtime the file lacks is typed.
func TestResolveRuntime_StoredProfileOverFileRuntimesIsTyped(t *testing.T) {
	vs := overlayActiveProfileFixture("local")
	o := NewSettingsOverlay()
	o.Update(nil, map[string]V1ProfileConfig{"local": {Runtime: "docker"}, "cluster": {Runtime: "gke"}}, nil)
	o.Apply(vs)

	if _, rtType, err := vs.ResolveRuntime("local"); err != nil || rtType != "docker" {
		t.Fatalf("ResolveRuntime(local) = %q, %v; want docker, nil", rtType, err)
	}
	_, _, err := vs.ResolveRuntime("cluster")
	var sre *StoredProfileRuntimeNotFoundError
	if !errors.As(err, &sre) {
		t.Fatalf("ResolveRuntime(cluster) error = %v, want *StoredProfileRuntimeNotFoundError", err)
	}
}

// IsStoredProfileError covers the existing stored-profile errors and not a
// plain error.
func TestIsStoredProfileError(t *testing.T) {
	if !IsStoredProfileError(&ActiveProfileError{}) || !IsStoredProfileError(&StoredProfileNotFoundError{}) {
		t.Error("IsStoredProfileError should match the active-profile and stored-profile errors")
	}
	if IsStoredProfileError(errors.New(`profile "x" not found`)) || IsStoredProfileError(nil) {
		t.Error("IsStoredProfileError should not match a plain error or nil")
	}
}
