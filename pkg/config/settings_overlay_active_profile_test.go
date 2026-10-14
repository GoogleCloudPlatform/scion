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
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
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

// captureSlog routes the process-wide default slog logger into a buffer for
// the test. It swaps global state, so a test that calls it must not use
// t.Parallel(), and no parallel test in this package may log while it runs.
func captureSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// When active_profile is not one of the overlaid profiles and they hold
// exactly one profile, that profile is the default; the choice is logged
// once, naming the profile and why, however often settings are loaded.
func TestSettingsOverlay_ActiveProfile_SingleStoredProfileUsed(t *testing.T) {
	for _, active := range []string{"gone", ""} {
		t.Run("active="+active, func(t *testing.T) {
			logs := captureSlog(t)
			o := NewSettingsOverlay()
			o.Update(
				map[string]V1RuntimeConfig{"k8s": {Type: "kubernetes"}},
				map[string]V1ProfileConfig{"cluster": {Runtime: "k8s"}},
				nil, "")

			for i := 0; i < 3; i++ {
				vs := overlayActiveProfileFixture(active)
				o.Apply(vs)
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
				if got := vs.ConfiguredActiveProfile(); got != active {
					t.Errorf("ConfiguredActiveProfile() = %q, want the file value %q", got, active)
				}
				// The fallback is only for the default profile: a named
				// profile the stored profiles lack is still refused.
				_, _, err = vs.ResolveRuntime("vanished")
				var spe *StoredProfileNotFoundError
				if !errors.As(err, &spe) {
					t.Fatalf("ResolveRuntime(vanished) error = %v, want *StoredProfileNotFoundError", err)
				}
				if !strings.Contains(err.Error(), `profile "vanished" not found`) || !strings.Contains(err.Error(), "available profiles: cluster") {
					t.Errorf("error %q should name vanished and the one stored profile", err)
				}
			}

			out := logs.String()
			if n := strings.Count(out, "Default profile set to the only stored profile"); n != 1 {
				t.Fatalf("fallback logged %d times, want 1:\n%s", n, out)
			}
			wantActive := "active_profile=" + active
			if active == "" {
				wantActive = "active_profile=(unset)"
			}
			for _, want := range []string{"profile=cluster", wantActive, "does not name one of the hub's stored profiles"} {
				if !strings.Contains(out, want) {
					t.Errorf("log %q does not contain %q", out, want)
				}
			}
		})
	}
}

// The fallback line is logged again after an Apply that resolves without
// the fallback, and loads alternating between two unresolvable
// active_profile values log each pair once instead of on every load.
func TestSettingsOverlay_ActiveProfile_FallbackLogDedup(t *testing.T) {
	logs := captureSlog(t)
	o := NewSettingsOverlay()
	o.Update(
		map[string]V1RuntimeConfig{"k8s": {Type: "kubernetes"}},
		map[string]V1ProfileConfig{"cluster": {Runtime: "k8s"}},
		nil, "")
	count := func() int { return strings.Count(logs.String(), "Default profile set to the only stored profile") }

	for i := 0; i < 4; i++ {
		o.Apply(overlayActiveProfileFixture("gone"))
		o.Apply(overlayActiveProfileFixture("other"))
	}
	if n := count(); n != 2 {
		t.Fatalf("alternating loads logged %d times, want 2 (one per pair):\n%s", n, logs.String())
	}

	// A load whose active_profile resolves resets the set; the next
	// fallback is logged again.
	o.Apply(overlayActiveProfileFixture("cluster"))
	o.Apply(overlayActiveProfileFixture("gone"))
	if n := count(); n != 3 {
		t.Fatalf("after recovery logged %d times in total, want 3:\n%s", n, logs.String())
	}

	// The set is capped: more distinct pairs than the cap still log each
	// new pair and keep the set bounded.
	for i := 0; i < maxFallbackLogKeys+4; i++ {
		o.Apply(overlayActiveProfileFixture(fmt.Sprintf("p%d", i)))
	}
	o.fallbackLogMu.Lock()
	size := len(o.fallbackLogged)
	o.fallbackLogMu.Unlock()
	if size > maxFallbackLogKeys {
		t.Errorf("logged set size = %d, want <= %d", size, maxFallbackLogKeys)
	}
	if n := count(); n != 3+maxFallbackLogKeys+4 {
		t.Errorf("distinct pairs logged %d times in total, want %d", n, 3+maxFallbackLogKeys+4)
	}
}

// With several overlaid profiles, an active_profile that is not one of
// them is refused with an error naming every stored profile, and nothing is
// chosen in its place. Zero stored profiles (an empty stored map) is not
// refused: it leaves the file's profiles in place (see
// settings_overlay_empty_catalog_test.go).
func TestSettingsOverlay_ActiveProfile_SeveralStoredProfilesRefused(t *testing.T) {
	for name, tc := range map[string]struct {
		profiles map[string]V1ProfileConfig
		want     string
	}{
		"two": {
			profiles: map[string]V1ProfileConfig{"cluster": {Runtime: "k8s"}, "batch": {Runtime: "k8s"}},
			want:     "available profiles: batch, cluster",
		},
	} {
		t.Run(name, func(t *testing.T) {
			logs := captureSlog(t)
			o := NewSettingsOverlay()
			o.Update(map[string]V1RuntimeConfig{"k8s": {Type: "kubernetes"}}, tc.profiles, nil, "")
			vs := overlayActiveProfileFixture("gone")
			o.Apply(vs)

			if vs.ActiveProfile != "gone" {
				t.Errorf("ActiveProfile = %q, want the file value kept", vs.ActiveProfile)
			}
			_, _, err := vs.ResolveRuntime("")
			var ape *ActiveProfileError
			if !errors.As(err, &ape) {
				t.Fatalf("ResolveRuntime(\"\") error = %v, want *ActiveProfileError", err)
			}
			if !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), `"gone"`) {
				t.Errorf("error %q should name \"gone\" and contain %q", err, tc.want)
			}
			if strings.Contains(logs.String(), "only stored profile") {
				t.Errorf("fallback logged with %s stored profiles: %s", name, logs.String())
			}
		})
	}
}

// Flat co-located instance: after the hub settings overlay is applied, the
// flat instance view (ForFlatInstance) sees none of the overlay's profiles,
// active_profile resolution or stored-profile errors, and its runtime_target
// values win over the overlaid runtimes'; the overlay's image_registry and
// harness_configs stay on their lower, non-profile rungs. Both a single
// stored profile (which would replace active_profile on a legacy broker) and
// several (which would make the default profile an error) are covered.
func TestSettingsOverlay_FlatInstanceViewIsolatedFromOverlay(t *testing.T) {
	for name, profiles := range map[string]map[string]V1ProfileConfig{
		"single stored profile": {"stored": {Runtime: "cluster", SharedDirStorageClass: "prof-rwx", SharedDirSize: "30Gi"}},
		"several stored profiles": {
			"stored":  {Runtime: "cluster", SharedDirStorageClass: "prof-rwx", SharedDirSize: "30Gi"},
			"stored2": {Runtime: "cluster"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("SCION_IMAGE_REGISTRY", "")
			vs := legacySharedDirSettings() // file active_profile "batch"
			o := NewSettingsOverlay()
			o.Update(
				map[string]V1RuntimeConfig{"cluster": {Type: "kubernetes", Namespace: "stored-ns", SharedDirStorageClass: "rt-stored", SharedDirSize: "50Gi"}},
				profiles,
				map[string]HarnessConfigEntry{"claude": {}},
				"registry.example/stored")
			o.Apply(vs)

			policy := k8sPolicy("k8s-nfs-1", "inst-rwx", "40Gi")
			wantTarget := policy.Target
			view := vs.ForFlatInstance(policy)

			// (a) No profile tier reaches the flat view.
			if view.ActiveProfile != "" || view.Profiles != nil || view.ConfiguredActiveProfile() != "" {
				t.Fatalf("flat view has a profile tier: active=%q configured=%q profiles=%v", view.ActiveProfile, view.ConfiguredActiveProfile(), view.Profiles)
			}
			_, _, err := view.ResolveRuntime("")
			var ape *ActiveProfileError
			if errors.As(err, &ape) {
				t.Fatalf("flat view ResolveRuntime(\"\") returned *ActiveProfileError: %v", err)
			}
			if err == nil || err.Error() != `profile "" not found` {
				t.Fatalf("flat view ResolveRuntime(\"\") error = %v, want the plain not-found error", err)
			}
			var spe *StoredProfileNotFoundError
			if _, _, err := view.ResolveRuntime("stored"); errors.As(err, &spe) || err == nil {
				t.Fatalf("flat view ResolveRuntime(stored) = %v, want the plain not-found error", err)
			}

			// (b) runtime_target comes from the instance, not the overlaid
			// runtimes or profiles.
			for _, p := range []string{"", "stored", "batch"} {
				class, size, key := view.ResolveSharedDirDefaultsWithSource(p)
				if class != "inst-rwx" || size != "40Gi" || key != "server.broker.instances[k8s-nfs-1].runtime_target.shared_dir_size" {
					t.Fatalf("profile %q: got (%q, %q, %q), want the instance values", p, class, size, key)
				}
			}
			if view.flat != policy || !reflect.DeepEqual(view.flat.Target, wantTarget) {
				t.Fatalf("flat policy changed: %+v", view.flat.Target)
			}

			// Non-profile overlay values stay shared, on their own rungs.
			if view.ImageRegistry != "registry.example/stored" {
				t.Errorf("ImageRegistry = %q, want the overlay's top-level value", view.ImageRegistry)
			}
			if _, ok := view.HarnessConfigs["claude"]; !ok {
				t.Errorf("HarnessConfigs = %v, want the overlay's entries", view.HarnessConfigs)
			}
		})
	}
}
