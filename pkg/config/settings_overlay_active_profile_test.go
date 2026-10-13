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

func overlayActiveProfileFixture(active string) *VersionedSettings {
	return &VersionedSettings{
		ActiveProfile: active,
		Runtimes: map[string]V1RuntimeConfig{
			"docker": {Type: "docker"},
		},
		Profiles: map[string]V1ProfileConfig{
			"local": {Runtime: "docker"},
		},
	}
}

func overlayWithProfiles() *SettingsOverlay {
	o := NewSettingsOverlay()
	o.Update(
		map[string]V1RuntimeConfig{"docker": {Type: "docker"}, "k8s": {Type: "kubernetes"}},
		map[string]V1ProfileConfig{"local": {Runtime: "docker"}, "cluster": {Runtime: "k8s"}},
		nil, "")
	return o
}

// The file's active_profile is kept when the overlaid profiles define it.
func TestSettingsOverlay_ActiveProfile_FileValueKeptWhenPresent(t *testing.T) {
	vs := overlayActiveProfileFixture("cluster")
	overlayWithProfiles().Apply(vs)

	if vs.ActiveProfile != "cluster" {
		t.Fatalf("ActiveProfile = %q, want cluster", vs.ActiveProfile)
	}
	_, rtType, err := vs.ResolveRuntime("")
	if err != nil {
		t.Fatalf("ResolveRuntime(\"\"): %v", err)
	}
	if rtType != "kubernetes" {
		t.Errorf("runtime type = %q, want kubernetes", rtType)
	}
}

// A file active_profile the overlaid profiles lack yields an error naming
// the configured profile and every available one, only when the default
// profile is needed.
func TestSettingsOverlay_ActiveProfile_MissingNamesConfiguredAndAvailable(t *testing.T) {
	vs := overlayActiveProfileFixture("gone")
	overlayWithProfiles().Apply(vs)

	if vs.ActiveProfile != "gone" {
		t.Errorf("ActiveProfile = %q, want the file value kept", vs.ActiveProfile)
	}
	_, _, err := vs.ResolveRuntime("")
	var ape *ActiveProfileError
	if !errors.As(err, &ape) {
		t.Fatalf("ResolveRuntime(\"\") error = %v, want *ActiveProfileError", err)
	}
	msg := err.Error()
	for _, want := range []string{`"gone"`, "cluster, local"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not contain %q", msg, want)
		}
	}
	if strings.HasPrefix(msg, "profile ") {
		t.Errorf("error %q is the bare profile-not-found form", msg)
	}

	// An explicit profile still resolves; the error is only for the default.
	if _, rtType, err := vs.ResolveRuntime("local"); err != nil || rtType != "docker" {
		t.Errorf("ResolveRuntime(local) = %q, %v; want docker, nil", rtType, err)
	}
}

// An unset file active_profile with overlaid profiles gets the same error,
// saying it is unset.
func TestSettingsOverlay_ActiveProfile_UnsetNamesUnsetAndAvailable(t *testing.T) {
	vs := overlayActiveProfileFixture("")
	overlayWithProfiles().Apply(vs)

	_, _, err := vs.ResolveRuntime("")
	var ape *ActiveProfileError
	if !errors.As(err, &ape) {
		t.Fatalf("ResolveRuntime(\"\") error = %v, want *ActiveProfileError", err)
	}
	if !strings.Contains(err.Error(), "(unset)") || !strings.Contains(err.Error(), "cluster, local") {
		t.Errorf("error %q should say unset and list the available profiles", err)
	}
}

// An empty overlaid profiles map lists no available profiles.
func TestSettingsOverlay_ActiveProfile_EmptyOverlaidMap(t *testing.T) {
	vs := overlayActiveProfileFixture("local")
	o := NewSettingsOverlay()
	o.Update(nil, map[string]V1ProfileConfig{}, nil, "")
	o.Apply(vs)

	_, _, err := vs.ResolveRuntime("")
	if err == nil || !strings.Contains(err.Error(), "available profiles: none") {
		t.Errorf("ResolveRuntime(\"\") error = %v, want one listing no available profiles", err)
	}
}

// When the overlay does not replace profiles, active_profile handling is
// unchanged (bare not-found error, no ActiveProfileError).
func TestSettingsOverlay_ActiveProfile_UnchangedWithoutOverlaidProfiles(t *testing.T) {
	vs := overlayActiveProfileFixture("gone")
	o := NewSettingsOverlay()
	o.Update(map[string]V1RuntimeConfig{"docker": {Type: "docker"}}, nil, nil, "")
	o.Apply(vs)

	_, _, err := vs.ResolveRuntime("")
	var ape *ActiveProfileError
	if errors.As(err, &ape) {
		t.Fatalf("got *ActiveProfileError without overlaid profiles: %v", err)
	}
	if err == nil || !strings.Contains(err.Error(), `profile "gone" not found`) {
		t.Errorf("error = %v, want the existing not-found error", err)
	}
}

// The flat-instance view skips the profile tier, including this error.
func TestSettingsOverlay_ActiveProfile_FlatViewDropsError(t *testing.T) {
	vs := overlayActiveProfileFixture("gone")
	overlayWithProfiles().Apply(vs)

	view := vs.ForProfileResolution(ProfileResolutionFlatInstance)
	_, _, err := view.ResolveRuntime("")
	var ape *ActiveProfileError
	if errors.As(err, &ape) {
		t.Errorf("flat view returned *ActiveProfileError: %v", err)
	}
}

// A named profile the overlaid profiles lack yields an error naming it and
// the stored profiles, keeping the "profile %q not found" form.
func TestSettingsOverlay_RequestedProfileMissingNamesRequestedAndAvailable(t *testing.T) {
	vs := overlayActiveProfileFixture("local")
	overlayWithProfiles().Apply(vs)

	_, _, err := vs.ResolveRuntime("vanished")
	var spe *StoredProfileNotFoundError
	if !errors.As(err, &spe) {
		t.Fatalf("ResolveRuntime(vanished) error = %v, want *StoredProfileNotFoundError", err)
	}
	for _, want := range []string{`profile "vanished" not found`, "cluster, local"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
}

// Without overlaid profiles, or in the flat view, a missing named profile
// keeps the bare not-found error.
func TestSettingsOverlay_RequestedProfileMissingUnchangedWithoutOverlaidProfiles(t *testing.T) {
	vs := overlayActiveProfileFixture("local")
	_, _, err := vs.ResolveRuntime("vanished")
	var spe *StoredProfileNotFoundError
	if errors.As(err, &spe) || err == nil || err.Error() != `profile "vanished" not found` {
		t.Errorf("no overlay: error = %v, want the bare not-found error", err)
	}

	overlayWithProfiles().Apply(vs)
	view := vs.ForProfileResolution(ProfileResolutionFlatInstance)
	if _, _, err := view.ResolveRuntime("vanished"); errors.As(err, &spe) {
		t.Errorf("flat view returned *StoredProfileNotFoundError: %v", err)
	}
}
