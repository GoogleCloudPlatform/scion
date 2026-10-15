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
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// testBuiltins is the shipped default catalog as Linux adjusts it: local on
// docker, remote on the unscoped kubernetes runtime.
func testBuiltins() *builtinCatalog {
	return &builtinCatalog{
		Runtimes: map[string]V1RuntimeConfig{
			"docker":     {Type: "docker"},
			"podman":     {Type: "podman"},
			"container":  {Type: "container"},
			"kubernetes": {Type: "kubernetes"},
		},
		Profiles: map[string]V1ProfileConfig{
			"local":  {Runtime: "docker"},
			"remote": {Runtime: "kubernetes"},
		},
	}
}

// fileSettings is what LoadVersionedSettings yields for a settings file
// that defines gke and local-nfs over the built-in defaults.
func fileSettings(active string) *VersionedSettings {
	return &VersionedSettings{
		ActiveProfile: active,
		Runtimes: map[string]V1RuntimeConfig{
			"docker":        {Type: "docker"},
			"kubernetes":    {Type: "kubernetes"},
			"gke-autopilot": {Type: "kubernetes", Context: "gke"},
		},
		Profiles: map[string]V1ProfileConfig{
			"local":     {Runtime: "docker"},
			"remote":    {Runtime: "kubernetes"},
			"gke":       {Runtime: "gke-autopilot"},
			"local-nfs": {Runtime: "docker"},
		},
	}
}

type overlaySpec struct {
	runtimes map[string]V1RuntimeConfig
	profiles map[string]V1ProfileConfig
}

func newRetainOverlay(spec overlaySpec) *SettingsOverlay {
	o := NewSettingsOverlay()
	o.builtinsOverride = testBuiltins()
	o.Update(spec.runtimes, spec.profiles, nil, "")
	return o
}

var storedGkeNFS = map[string]V1ProfileConfig{
	"gke":       {Runtime: "gke-autopilot"},
	"local-nfs": {Runtime: "docker"},
}

func agentsOn(counts map[string]int) ProfileReferences {
	return ProfileReferences{Agents: counts}
}

func mustResolveType(t *testing.T, vs *VersionedSettings, profile, wantType string) {
	t.Helper()
	_, rtType, err := vs.ResolveRuntime(profile)
	if err != nil {
		t.Fatalf("ResolveRuntime(%q): %v", profile, err)
	}
	if rtType != wantType {
		t.Fatalf("ResolveRuntime(%q) type = %q, want %q", profile, rtType, wantType)
	}
}

func mustNotFound(t *testing.T, vs *VersionedSettings, profile string) *StoredProfileNotFoundError {
	t.Helper()
	_, _, err := vs.ResolveRuntime(profile)
	var nf *StoredProfileNotFoundError
	if !errors.As(err, &nf) {
		t.Fatalf("ResolveRuntime(%q) error = %v, want *StoredProfileNotFoundError", profile, err)
	}
	return nf
}

// Stored {gke, local-nfs}, agents saved on local,
// active_profile local. local resolves to docker; remote (no references) is
// not retained.
func TestRetain_ReferencedBuiltinResolves(t *testing.T) {
	o := newRetainOverlay(overlaySpec{profiles: storedGkeNFS})
	o.SetProfileReferences(agentsOn(map[string]int{"local": 3}))
	vs := fileSettings("local")
	o.Apply(vs)

	mustResolveType(t, vs, "local", "docker")
	mustResolveType(t, vs, "", "docker")
	_ = mustNotFound(t, vs, "remote")
	if got := vs.RetainedProfileNames(); !reflect.DeepEqual(got, []string{"local"}) {
		t.Fatalf("RetainedProfileNames = %v, want [local]", got)
	}
	if !reflect.DeepEqual(vs.storedProfileNames, []string{"gke", "local-nfs"}) {
		t.Fatalf("storedProfileNames = %v, want the stored set only", vs.storedProfileNames)
	}
}

// The built-in definition is used even when the settings file
// defines the same name differently; a file-only profile is never retained.
func TestRetain_BuiltinDefinitionAndNoFileOnly(t *testing.T) {
	o := newRetainOverlay(overlaySpec{profiles: storedGkeNFS})
	o.SetProfileReferences(agentsOn(map[string]int{"local": 1, "fp": 1}))
	vs := fileSettings("gke")
	vs.Profiles["local"] = V1ProfileConfig{Runtime: "gke-autopilot", DefaultTemplate: "file"}
	vs.Profiles["fp"] = V1ProfileConfig{Runtime: "docker"}
	o.Apply(vs)

	if got := vs.Profiles["local"]; got.Runtime != "docker" || got.DefaultTemplate != "" {
		t.Fatalf("retained local = %+v, want the built-in definition", got)
	}
	_ = mustNotFound(t, vs, "fp")
}

// A stored entry always wins over the same-named built-in.
func TestRetain_StoredEntryWins(t *testing.T) {
	stored := map[string]V1ProfileConfig{"gke": {Runtime: "gke-autopilot"}, "local": {Runtime: "gke-autopilot"}}
	o := newRetainOverlay(overlaySpec{profiles: stored})
	o.SetProfileReferences(agentsOn(map[string]int{"local": 2}))
	vs := fileSettings("gke")
	o.Apply(vs)

	if vs.Profiles["local"].Runtime != "gke-autopilot" {
		t.Fatalf("local = %+v, want the stored entry", vs.Profiles["local"])
	}
	if vs.RetainedProfileNames() != nil {
		t.Fatalf("RetainedProfileNames = %v, want nil", vs.RetainedProfileNames())
	}
}

// Upgrade after the manual workaround: stored local and docker, nothing
// retained, no WARN.
func TestRetain_ManualWorkaroundUnchanged(t *testing.T) {
	o := newRetainOverlay(overlaySpec{
		runtimes: map[string]V1RuntimeConfig{"docker": {Type: "docker", Host: "stored"}, "gke-autopilot": {Type: "kubernetes"}},
		profiles: map[string]V1ProfileConfig{"gke": {Runtime: "gke-autopilot"}, "local": {Runtime: "docker", DefaultTemplate: "stored"}},
	})
	o.SetProfileReferences(agentsOn(map[string]int{"local": 2}))
	vs := fileSettings("local")
	logs := captureLogs(t, func() { o.Apply(vs) })

	if vs.RetainedProfileNames() != nil || vs.RetainedRuntimeNames() != nil {
		t.Fatalf("retained = %v / %v, want none", vs.RetainedProfileNames(), vs.RetainedRuntimeNames())
	}
	if vs.Runtimes["docker"].Host != "stored" || vs.Profiles["local"].DefaultTemplate != "stored" {
		t.Fatalf("stored definitions not used: %+v %+v", vs.Runtimes["docker"], vs.Profiles["local"])
	}
	if strings.Contains(logs, "Retaining built-in") {
		t.Fatalf("unexpected WARN: %s", logs)
	}
}

// An empty stored set is unchanged (#5007), and retention sets
// nothing.
func TestRetain_EmptyStoredSetUnchanged(t *testing.T) {
	o := newRetainOverlay(overlaySpec{profiles: map[string]V1ProfileConfig{}})
	o.SetProfileReferences(agentsOn(map[string]int{"local": 2}))
	vs := fileSettings("local")
	o.Apply(vs)

	if vs.storedProfileNames != nil || vs.RetainedProfileNames() != nil || vs.RetainedRuntimeNames() != nil {
		t.Fatalf("stored=%v retained=%v/%v, want all nil", vs.storedProfileNames, vs.RetainedProfileNames(), vs.RetainedRuntimeNames())
	}
	if len(vs.Profiles) != 4 {
		t.Fatalf("file profiles changed: %v", vs.Profiles)
	}
}

// Without SetProfileReferences the overlay behaves exactly as before.
func TestRetain_DisabledUntilEnabled(t *testing.T) {
	o := newRetainOverlay(overlaySpec{profiles: storedGkeNFS})
	vs := fileSettings("local")
	o.Apply(vs)

	_ = mustNotFound(t, vs, "local")
	if _, _, err := vs.ResolveRuntime(""); !errors.As(err, new(*ActiveProfileError)) {
		t.Fatalf("ResolveRuntime(\"\") = %v, want *ActiveProfileError", err)
	}
	o.AddProjectReference("local")
	vs = fileSettings("gke")
	o.Apply(vs)
	_ = mustNotFound(t, vs, "local")
}

// Runtime retention for stored and retained profiles.
func TestRetain_Runtimes(t *testing.T) {
	gkeOnly := map[string]V1RuntimeConfig{"gke-autopilot": {Type: "kubernetes"}}

	t.Run("stored profiles on docker, stored runtimes k8s only", func(t *testing.T) {
		o := newRetainOverlay(overlaySpec{
			runtimes: map[string]V1RuntimeConfig{"k8s": {Type: "kubernetes"}},
			profiles: map[string]V1ProfileConfig{"local": {Runtime: "docker"}, "nfs": {Runtime: "docker"}},
		})
		o.SetProfileReferences(ProfileReferences{})
		vs := fileSettings("local")
		o.Apply(vs)
		mustResolveType(t, vs, "local", "docker")
		mustResolveType(t, vs, "nfs", "docker")
		if got := vs.RetainedRuntimeNames(); !reflect.DeepEqual(got, []string{"docker"}) {
			t.Fatalf("RetainedRuntimeNames = %v, want [docker]", got)
		}
		if _, ok := vs.Runtimes["kubernetes"]; ok {
			t.Fatal("kubernetes runtime retained")
		}
		_, rts := o.RetainedState()
		if len(rts) != 1 || !reflect.DeepEqual(rts[0].NeededBy, []string{"local", "nfs"}) {
			t.Fatalf("RetainedState runtimes = %+v", rts)
		}
	})

	t.Run("stored local on docker, stored runtimes gke only", func(t *testing.T) {
		o := newRetainOverlay(overlaySpec{
			runtimes: gkeOnly,
			profiles: map[string]V1ProfileConfig{"gke": {Runtime: "gke-autopilot"}, "local-nfs": {Runtime: "docker"}, "local": {Runtime: "docker"}},
		})
		o.SetProfileReferences(agentsOn(map[string]int{"local": 5}))
		vs := fileSettings("local")
		o.Apply(vs)
		mustResolveType(t, vs, "local", "docker")
		if vs.RetainedProfileNames() != nil {
			t.Fatalf("RetainedProfileNames = %v, want nil", vs.RetainedProfileNames())
		}
	})

	t.Run("no stored profiles, stored runtimes gke only", func(t *testing.T) {
		o := newRetainOverlay(overlaySpec{runtimes: gkeOnly})
		o.SetProfileReferences(ProfileReferences{})
		vs := fileSettings("gke")
		o.Apply(vs)
		mustResolveType(t, vs, "local", "docker")
		if _, ok := vs.Runtimes["kubernetes"]; ok {
			t.Fatal("kubernetes retained for an unreferenced remote")
		}
	})

	t.Run("no stored profiles, remote referenced", func(t *testing.T) {
		o := newRetainOverlay(overlaySpec{runtimes: gkeOnly})
		o.SetProfileReferences(agentsOn(map[string]int{"remote": 1}))
		vs := fileSettings("gke")
		o.Apply(vs)
		mustResolveType(t, vs, "remote", "kubernetes")
	})

	t.Run("stored remote unreferenced then referenced", func(t *testing.T) {
		spec := overlaySpec{runtimes: gkeOnly, profiles: map[string]V1ProfileConfig{"gke": {Runtime: "gke-autopilot"}, "remote": {Runtime: "kubernetes"}}}
		o := newRetainOverlay(spec)
		o.SetProfileReferences(ProfileReferences{})
		vs := fileSettings("gke")
		o.Apply(vs)
		if _, ok := vs.Runtimes["kubernetes"]; ok {
			t.Fatal("kubernetes retained for an unreferenced stored remote")
		}
		o.SetProfileReferences(agentsOn(map[string]int{"remote": 1}))
		vs = fileSettings("gke")
		o.Apply(vs)
		mustResolveType(t, vs, "remote", "kubernetes")
	})

	t.Run("stored docker wins", func(t *testing.T) {
		o := newRetainOverlay(overlaySpec{
			runtimes: map[string]V1RuntimeConfig{"docker": {Type: "docker", Host: "stored"}},
			profiles: map[string]V1ProfileConfig{"local": {Runtime: "docker"}},
		})
		o.SetProfileReferences(ProfileReferences{})
		vs := fileSettings("local")
		o.Apply(vs)
		if vs.Runtimes["docker"].Host != "stored" || vs.RetainedRuntimeNames() != nil {
			t.Fatalf("docker = %+v retained = %v", vs.Runtimes["docker"], vs.RetainedRuntimeNames())
		}
	})

	t.Run("file-only runtime not retained", func(t *testing.T) {
		o := newRetainOverlay(overlaySpec{runtimes: gkeOnly, profiles: map[string]V1ProfileConfig{"x": {Runtime: "file-rt"}}})
		o.SetProfileReferences(agentsOn(map[string]int{"x": 1}))
		vs := fileSettings("x")
		vs.Runtimes["file-rt"] = V1RuntimeConfig{Type: "docker"}
		o.Apply(vs)
		if _, _, err := vs.ResolveRuntime("x"); err == nil || !strings.Contains(err.Error(), "runtime \"file-rt\" not found") {
			t.Fatalf("ResolveRuntime(x) = %v, want runtime not found", err)
		}
	})

	t.Run("no stored runtimes adds nothing", func(t *testing.T) {
		o := newRetainOverlay(overlaySpec{profiles: storedGkeNFS})
		o.SetProfileReferences(agentsOn(map[string]int{"local": 1}))
		vs := fileSettings("gke")
		o.Apply(vs)
		if vs.RetainedRuntimeNames() != nil {
			t.Fatalf("RetainedRuntimeNames = %v", vs.RetainedRuntimeNames())
		}
	})
}

// The default profile order: stored, single-stored fallback, retained.
func TestRetain_ActiveProfileOrder(t *testing.T) {
	t.Run("two stored, active a referenced-only-by-default built-in", func(t *testing.T) {
		o := newRetainOverlay(overlaySpec{profiles: storedGkeNFS})
		o.SetProfileReferences(ProfileReferences{})
		vs := fileSettings("local")
		o.Apply(vs)
		mustResolveType(t, vs, "", "docker")
		if vs.activeProfileFallbackFrom != nil || vs.activeProfileErr != nil {
			t.Fatal("unexpected fallback or error")
		}
	})
	t.Run("one stored, active local, no refs: fallback, not retained", func(t *testing.T) {
		o := newRetainOverlay(overlaySpec{profiles: map[string]V1ProfileConfig{"gke": {Runtime: "gke-autopilot"}}})
		o.SetProfileReferences(ProfileReferences{})
		vs := fileSettings("local")
		o.Apply(vs)
		if vs.ActiveProfile != "gke" || vs.RetainedProfileNames() != nil {
			t.Fatalf("ActiveProfile=%q retained=%v, want gke and none", vs.ActiveProfile, vs.RetainedProfileNames())
		}
	})
	t.Run("one stored, local referenced: resolvable but not the default", func(t *testing.T) {
		o := newRetainOverlay(overlaySpec{profiles: map[string]V1ProfileConfig{"gke": {Runtime: "gke-autopilot"}}})
		o.SetProfileReferences(agentsOn(map[string]int{"local": 2}))
		vs := fileSettings("local")
		o.Apply(vs)
		mustResolveType(t, vs, "local", "docker")
		if vs.ActiveProfile != "gke" {
			t.Fatalf("ActiveProfile = %q, want gke (fallback before retained)", vs.ActiveProfile)
		}
	})
	t.Run("error names stored and retained", func(t *testing.T) {
		o := newRetainOverlay(overlaySpec{profiles: map[string]V1ProfileConfig{"gke": {Runtime: "gke-autopilot"}, "x": {Runtime: "gke-autopilot"}}})
		o.SetProfileReferences(agentsOn(map[string]int{"remote": 1}))
		vs := fileSettings("typo")
		vs.Runtimes["kubernetes"] = V1RuntimeConfig{Type: "kubernetes"}
		o.Apply(vs)
		_, _, err := vs.ResolveRuntime("")
		var ape *ActiveProfileError
		if !errors.As(err, &ape) {
			t.Fatalf("err = %v, want *ActiveProfileError", err)
		}
		if !reflect.DeepEqual(ape.Available, []string{"gke", "x"}) || !reflect.DeepEqual(ape.Retained, []string{"remote"}) {
			t.Fatalf("Available=%v Retained=%v", ape.Available, ape.Retained)
		}
		if !strings.Contains(err.Error(), "(available profiles: gke, x; retained built-in: remote)") {
			t.Fatalf("message = %q", err.Error())
		}
		nf := mustNotFound(t, vs, "nope")
		if !strings.Contains(nf.Error(), "retained built-in: remote") {
			t.Fatalf("not-found message = %q", nf.Error())
		}
	})
}

// Retention is sticky: a retained name stays retained after its references go away, and a
// project-load active_profile retention is process-wide.
func TestRetain_Sticky(t *testing.T) {
	o := newRetainOverlay(overlaySpec{profiles: storedGkeNFS})
	o.SetProfileReferences(ProfileReferences{})
	vs := fileSettings("local") // retained through active_profile
	o.Apply(vs)

	o.SetProfileReferences(ProfileReferences{}) // a later count with no references
	vs = fileSettings("gke")                    // active_profile edited away
	o.Apply(vs)
	mustResolveType(t, vs, "local", "docker")
}

// An added reference survives a concurrent count that replaces the
// references without it.
func TestRetain_AddedReferenceSurvivesStaleCount(t *testing.T) {
	o := newRetainOverlay(overlaySpec{profiles: storedGkeNFS})
	o.SetProfileReferences(ProfileReferences{})
	stale := ProfileReferences{} // read before the project was linked
	o.AddProjectReference("local")
	o.SetProfileReferences(stale) // lands after the add
	vs := fileSettings("gke")
	o.Apply(vs)
	mustResolveType(t, vs, "local", "docker")
}

// Conservative retention when references could not be counted.
func TestRetain_Conservative(t *testing.T) {
	spec := overlaySpec{
		runtimes: map[string]V1RuntimeConfig{"gke-autopilot": {Type: "kubernetes"}},
		profiles: storedGkeNFS,
	}
	o := newRetainOverlay(spec)
	o.EnableConservativeRetention()
	vs := fileSettings("gke")
	logs := captureLogs(t, func() { o.Apply(vs) })
	mustResolveType(t, vs, "local", "docker")
	_ = mustNotFound(t, vs, "remote")
	if _, ok := vs.Runtimes["kubernetes"]; ok {
		t.Fatal("kubernetes retained in conservative mode")
	}
	if !strings.Contains(logs, "References could not be counted") {
		t.Fatalf("conservative WARN missing: %s", logs)
	}

	// A later successful count with zero references does not remove local,
	// and leaves it marked conservative.
	o.SetProfileReferences(ProfileReferences{})
	vs = fileSettings("gke")
	o.Apply(vs)
	mustResolveType(t, vs, "local", "docker")
	ps, _ := o.RetainedState()
	if len(ps) != 1 || !ps[0].Conservative {
		t.Fatalf("RetainedState = %+v, want local still conservative", ps)
	}
	// A count that sees it referenced clears the mark and re-logs the WARN.
	o.SetProfileReferences(agentsOn(map[string]int{"local": 1}))
	vs = fileSettings("gke")
	logs = captureLogs(t, func() { o.Apply(vs) })
	ps, _ = o.RetainedState()
	if ps[0].Conservative {
		t.Fatal("conservative mark not cleared")
	}
	if !strings.Contains(logs, "Retaining built-in") {
		t.Fatalf("WARN not re-logged after the mark cleared: %s", logs)
	}
}

// In conservative mode the kubernetes exclusion yields to the known
// active_profile.
func TestRetain_ConservativeKnownReferenceWins(t *testing.T) {
	o := newRetainOverlay(overlaySpec{
		runtimes: map[string]V1RuntimeConfig{"gke-autopilot": {Type: "kubernetes"}},
		profiles: storedGkeNFS,
	})
	o.EnableConservativeRetention()
	vs := fileSettings("remote")
	o.Apply(vs)
	mustResolveType(t, vs, "", "kubernetes")
}

// The WARN logs once per retained state; a count change does not re-log, a
// name change does; alternating loads do not flap. The self-perpetuating
// default sentence appears with two or more stored profiles.
func TestRetain_WarnDedup(t *testing.T) {
	o := newRetainOverlay(overlaySpec{profiles: storedGkeNFS})
	o.SetProfileReferences(agentsOn(map[string]int{"local": 1}))
	logs := captureLogs(t, func() {
		o.Apply(fileSettings("local"))
		o.SetProfileReferences(agentsOn(map[string]int{"local": 7}))
		o.Apply(fileSettings("gke"))
		o.Apply(fileSettings("local"))
		o.Apply(fileSettings("gke"))
	})
	if n := strings.Count(logs, "Retaining built-in"); n != 1 {
		t.Fatalf("WARN logged %d times, want 1:\n%s", n, logs)
	}
	if !strings.Contains(logs, `new agents without a profile save \"local\"`) {
		t.Fatalf("self-perpetuating sentence missing:\n%s", logs)
	}
	logs = captureLogs(t, func() {
		o.SetProfileReferences(agentsOn(map[string]int{"remote": 1}))
		o.Apply(fileSettings("gke"))
	})
	if n := strings.Count(logs, "Retaining built-in"); n != 1 {
		t.Fatalf("WARN after a name change logged %d times, want 1:\n%s", n, logs)
	}
}

// Flat views clear the retention markers.
func TestRetain_FlatViewsClearMarkers(t *testing.T) {
	o := newRetainOverlay(overlaySpec{
		runtimes: map[string]V1RuntimeConfig{"gke-autopilot": {Type: "kubernetes"}},
		profiles: storedGkeNFS,
	})
	o.SetProfileReferences(agentsOn(map[string]int{"local": 1}))
	vs := fileSettings("gke")
	o.Apply(vs)
	if vs.RetainedProfileNames() == nil || vs.RetainedRuntimeNames() == nil {
		t.Fatal("fixture retained nothing")
	}
	for name, view := range map[string]*VersionedSettings{
		"ForFlatInstance":      vs.ForFlatInstance(&FlatInstancePolicy{}),
		"ForProfileResolution": vs.ForProfileResolution(ProfileResolutionFlatInstance),
	} {
		if view.RetainedProfileNames() != nil || view.RetainedRuntimeNames() != nil {
			t.Errorf("%s kept retention markers", name)
		}
	}
}

// The real embedded defaults parse and carry local and remote.
func TestRetain_DefaultBuiltinCatalog(t *testing.T) {
	b := defaultBuiltinCatalog()
	if _, ok := b.Profiles["local"]; !ok {
		t.Fatalf("built-in catalog lacks local: %+v", b.Profiles)
	}
	if b.Profiles["remote"].Runtime != "kubernetes" {
		t.Fatalf("built-in remote = %+v", b.Profiles["remote"])
	}
	if _, ok := b.Runtimes[b.Profiles["local"].Runtime]; !ok {
		t.Fatalf("built-in local runtime %q missing from %v", b.Profiles["local"].Runtime, b.Runtimes)
	}
}

// Apply, the reference setters and RetainedState are safe concurrently.
func TestRetain_Concurrent(t *testing.T) {
	o := newRetainOverlay(overlaySpec{
		runtimes: map[string]V1RuntimeConfig{"gke-autopilot": {Type: "kubernetes"}},
		profiles: storedGkeNFS,
	})
	o.SetProfileReferences(ProfileReferences{})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				switch (i + j) % 4 {
				case 0:
					o.Apply(fileSettings("local"))
				case 1:
					o.AddProjectReference("remote")
				case 2:
					o.SetProfileReferences(agentsOn(map[string]int{"local": j}))
				default:
					o.RetainedState()
				}
			}
		}(i)
	}
	wg.Wait()
}

// captureLogs runs fn with the default slog logger writing to a buffer.
func captureLogs(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(prev)
	fn()
	return buf.String()
}

// A stored name is never reported as retained, even after an
// added reference, and a name stored after it was retained drops out of
// the report.
func TestRetain_StoredNameNotReported(t *testing.T) {
	stored := map[string]V1ProfileConfig{"gke": {Runtime: "gke-autopilot"}, "local": {Runtime: "docker"}}
	o := newRetainOverlay(overlaySpec{profiles: stored})
	o.SetProfileReferences(ProfileReferences{})
	o.AddProjectReference("local")
	logs := captureLogs(t, func() { o.Apply(fileSettings("gke")) })
	if strings.Contains(logs, "Retaining built-in") {
		t.Fatalf("WARN for a stored name: %s", logs)
	}
	if ps, rs := o.RetainedState(); ps != nil || rs != nil {
		t.Fatalf("RetainedState = %+v / %+v, want empty", ps, rs)
	}

	o2 := newRetainOverlay(overlaySpec{profiles: storedGkeNFS})
	o2.SetProfileReferences(agentsOn(map[string]int{"local": 1}))
	o2.Apply(fileSettings("gke"))
	if ps, _ := o2.RetainedState(); len(ps) != 1 {
		t.Fatalf("precondition: RetainedState = %+v", ps)
	}
	o2.Update(nil, map[string]V1ProfileConfig{"gke": {Runtime: "gke-autopilot"}, "local": {Runtime: "docker"}}, nil, "")
	o2.Apply(fileSettings("gke"))
	if ps, _ := o2.RetainedState(); ps != nil {
		t.Fatalf("RetainedState after storing local = %+v, want empty", ps)
	}
}

// An added remote reference survives a stale count together with
// its kubernetes runtime.
func TestRetain_AddedRemoteKeepsKubernetesRuntime(t *testing.T) {
	o := newRetainOverlay(overlaySpec{
		runtimes: map[string]V1RuntimeConfig{"gke-autopilot": {Type: "kubernetes"}},
		profiles: storedGkeNFS,
	})
	o.SetProfileReferences(ProfileReferences{})
	o.AddProjectReference("remote")
	o.SetProfileReferences(ProfileReferences{}) // stale count lands first
	vs := fileSettings("gke")
	o.Apply(vs)
	mustResolveType(t, vs, "remote", "kubernetes")
}

// After a conservative start, a count referencing remote adds it
// and the kubernetes runtime.
func TestRetain_ConservativeThenRemoteReferenced(t *testing.T) {
	o := newRetainOverlay(overlaySpec{
		runtimes: map[string]V1RuntimeConfig{"gke-autopilot": {Type: "kubernetes"}},
		profiles: storedGkeNFS,
	})
	o.EnableConservativeRetention()
	o.Apply(fileSettings("gke"))
	o.SetProfileReferences(agentsOn(map[string]int{"remote": 1}))
	vs := fileSettings("gke")
	o.Apply(vs)
	mustResolveType(t, vs, "remote", "kubernetes")
	mustResolveType(t, vs, "local", "docker")
}

// In conservative mode an added reference to remote retains it and its runtime.
func TestRetain_ConservativeAddedRemote(t *testing.T) {
	o := newRetainOverlay(overlaySpec{
		runtimes: map[string]V1RuntimeConfig{"gke-autopilot": {Type: "kubernetes"}},
		profiles: storedGkeNFS,
	})
	o.EnableConservativeRetention()
	o.AddProjectReference("remote")
	vs := fileSettings("gke")
	o.Apply(vs)
	mustResolveType(t, vs, "remote", "kubernetes")
}

// SR1, config side: agents without a profile count toward the configured
// active_profile: with active local, local is retained and the WARN shows
// them; with active gke nothing is retained.
func TestRetain_AgentsWithoutProfile(t *testing.T) {
	o := newRetainOverlay(overlaySpec{profiles: storedGkeNFS})
	o.SetProfileReferences(agentsOn(map[string]int{"": 1}))
	vs := fileSettings("local")
	logs := captureLogs(t, func() { o.Apply(vs) })
	mustResolveType(t, vs, "", "docker")
	if !strings.Contains(logs, "agents_without_profile=1") {
		t.Fatalf("WARN lacks agents_without_profile=1: %s", logs)
	}

	o2 := newRetainOverlay(overlaySpec{profiles: storedGkeNFS})
	o2.SetProfileReferences(agentsOn(map[string]int{"": 1}))
	vs = fileSettings("gke")
	o2.Apply(vs)
	if vs.RetainedProfileNames() != nil {
		t.Fatalf("RetainedProfileNames = %v, want nil", vs.RetainedProfileNames())
	}
}

// The WARN carries what operators act on: per-profile counts and runtime
// type, the remediation, and agent_info_missing when non-zero.
func TestRetain_WarnContent(t *testing.T) {
	o := newRetainOverlay(overlaySpec{
		runtimes: map[string]V1RuntimeConfig{"gke-autopilot": {Type: "kubernetes"}},
		profiles: storedGkeNFS,
	})
	o.SetProfileReferences(ProfileReferences{
		Agents:           map[string]int{"local": 3},
		Projects:         map[string]int{"local": 1},
		AgentInfoMissing: 2,
	})
	logs := captureLogs(t, func() { o.Apply(fileSettings("gke")) })
	for _, want := range []string{
		"agents=3 projects=1 active_profile=false runtime=docker runtime_type=docker",
		"runtime.docker=",
		"needed_by=[local local-nfs]",
		"agent_info_missing=2",
		"remediation=",
		"retired built-ins drop at the next restart",
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("WARN lacks %q:\n%s", want, logs)
		}
	}
}

// With no stored set (an unmanaged hub) nothing is reported as retained,
// even after an added reference made a built-in sticky; and a stored set
// cleared later stops the report.
func TestRetain_UnmanagedHubReportsNothing(t *testing.T) {
	o := newRetainOverlay(overlaySpec{})
	o.SetProfileReferences(ProfileReferences{})
	o.AddProjectReference("local")
	logs := captureLogs(t, func() { o.Apply(fileSettings("local")) })
	if strings.Contains(logs, "Retaining built-in") {
		t.Fatalf("WARN on an unmanaged hub: %s", logs)
	}
	if ps, rs := o.RetainedState(); ps != nil || rs != nil {
		t.Fatalf("RetainedState = %+v / %+v, want empty", ps, rs)
	}

	o2 := newRetainOverlay(overlaySpec{
		runtimes: map[string]V1RuntimeConfig{"gke-autopilot": {Type: "kubernetes"}},
		profiles: storedGkeNFS,
	})
	o2.SetProfileReferences(agentsOn(map[string]int{"local": 1}))
	o2.Apply(fileSettings("gke"))
	if ps, rs := o2.RetainedState(); len(ps) != 1 || len(rs) != 1 {
		t.Fatalf("precondition: RetainedState = %+v / %+v", ps, rs)
	}
	o2.Update(map[string]V1RuntimeConfig{}, map[string]V1ProfileConfig{}, nil, "")
	logs = captureLogs(t, func() { o2.Apply(fileSettings("gke")) })
	if ps, rs := o2.RetainedState(); ps != nil || rs != nil {
		t.Fatalf("RetainedState after clearing the stored sets = %+v / %+v, want empty", ps, rs)
	}
	if strings.Contains(logs, "Retaining built-in") {
		t.Fatalf("WARN after clearing the stored sets: %s", logs)
	}
}

// ConservativeRetention reports conservative mode until a count succeeds.
func TestRetain_ConservativeRetentionAccessor(t *testing.T) {
	o := newRetainOverlay(overlaySpec{profiles: storedGkeNFS})
	if o.ConservativeRetention() || o.RetentionEnabled() {
		t.Fatal("a new overlay is neither conservative nor enabled")
	}
	o.EnableConservativeRetention()
	if !o.ConservativeRetention() || !o.RetentionEnabled() {
		t.Fatal("conservative enable must report conservative and enabled")
	}
	o.SetProfileReferences(ProfileReferences{})
	if o.ConservativeRetention() {
		t.Fatal("a successful count ends conservative mode")
	}
}
