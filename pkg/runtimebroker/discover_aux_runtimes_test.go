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
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/k8s"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
)

// fakeKubernetesRuntime builds a *runtime.KubernetesRuntime that carries just
// enough state (a Client with CurrentContext and an empty fake Clientset, and
// DefaultNamespace) for auxiliaryRuntimeIdentity to distinguish it from other
// instances, and for a real rt.List() call to return an empty, error-free
// result instead of panicking on a nil Clientset — without ever dialing a
// real cluster.
func fakeKubernetesRuntime(context, namespace string) *runtime.KubernetesRuntime {
	return &runtime.KubernetesRuntime{
		Client:           &k8s.Client{CurrentContext: context, Clientset: k8sfake.NewClientset()},
		DefaultNamespace: namespace,
	}
}

// stubResolver returns a resolveAuxiliaryRuntime-compatible function that
// maps profile names to pre-built runtimes, so discovery can be exercised
// without live cluster/network access (discoverAuxiliaryRuntimesForProjects
// would otherwise call k8s.Client.Verify() against a real API server for any
// "kubernetes" profile).
func stubResolver(t *testing.T, byProfile map[string]runtime.Runtime) func(string, string, string) runtime.Runtime {
	t.Helper()
	return func(_, _, profileFlag string) runtime.Runtime {
		if rt, ok := byProfile[profileFlag]; ok {
			return rt
		}
		return &runtime.ErrorRuntime{Err: fmt.Errorf("stubResolver: unexpected profile %q", profileFlag)}
	}
}

// writeProjectSettings writes a minimal versioned settings.yaml directly into
// dir (LoadEffectiveSettings expects the directory to contain settings.yaml,
// not a nested .scion/ subdirectory).
func writeProjectSettings(t *testing.T, dir, yaml string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "settings.yaml"), []byte(yaml), 0644); err != nil {
		t.Fatal(err)
	}
}

func newDiscoveryTestServer(t *testing.T, defaultRuntimeName string) *Server {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "")

	cfg := DefaultServerConfig()
	rt := &runtime.MockRuntime{NameFunc: func() string { return defaultRuntimeName }}
	return New(cfg, &filteringMockManager{}, rt)
}

func auxKeys(s *Server) []string {
	s.auxiliaryRuntimesMu.RLock()
	defer s.auxiliaryRuntimesMu.RUnlock()
	keys := make([]string, 0, len(s.auxiliaryRuntimes))
	for k := range s.auxiliaryRuntimes {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// TestDiscoverAuxiliaryRuntimes_DistinctKubernetesRuntimesBothRegister is the
// regression test for ptone/scion#2260: two profiles that resolve to
// different Kubernetes runtimes (different context/namespace here, standing
// in for different clusters) must both be registered, not just one chosen by
// map iteration order.
func TestDiscoverAuxiliaryRuntimes_DistinctKubernetesRuntimesBothRegister(t *testing.T) {
	srv := newDiscoveryTestServer(t, "docker")

	projectDir := t.TempDir()
	writeProjectSettings(t, projectDir, `schema_version: "1"
profiles:
  aux-a:
    runtime: k8s-a
  aux-b:
    runtime: k8s-b
runtimes:
  k8s-a:
    type: kubernetes
    context: cluster-a
    namespace: ns-a
  k8s-b:
    type: kubernetes
    context: cluster-b
    namespace: ns-b
`)

	rtA := fakeKubernetesRuntime("cluster-a", "ns-a")
	rtB := fakeKubernetesRuntime("cluster-b", "ns-b")
	srv.resolveAuxiliaryRuntime = stubResolver(t, map[string]runtime.Runtime{
		"aux-a": rtA,
		"aux-b": rtB,
	})

	srv.discoverAuxiliaryRuntimesForProjects([]string{projectDir})

	keys := auxKeys(srv)
	if len(keys) != 2 {
		t.Fatalf("expected 2 registered auxiliary runtimes, got %d: %v", len(keys), keys)
	}

	srv.auxiliaryRuntimesMu.RLock()
	defer srv.auxiliaryRuntimesMu.RUnlock()
	var gotA, gotB bool
	for _, aux := range srv.auxiliaryRuntimes {
		if aux.Runtime == runtime.Runtime(rtA) {
			gotA = true
		}
		if aux.Runtime == runtime.Runtime(rtB) {
			gotB = true
		}
	}
	if !gotA || !gotB {
		t.Fatalf("expected both cluster-a and cluster-b runtimes registered, gotA=%v gotB=%v (keys=%v)", gotA, gotB, keys)
	}
}

// TestDiscoverAuxiliaryRuntimes_SameRuntimeRegistersOnce ensures two profiles
// that resolve to the SAME underlying Kubernetes runtime (identical
// context+namespace) are still de-duplicated to a single registration, even
// though they come from differently-named runtime entries/profiles.
func TestDiscoverAuxiliaryRuntimes_SameRuntimeRegistersOnce(t *testing.T) {
	srv := newDiscoveryTestServer(t, "docker")

	projectDir := t.TempDir()
	writeProjectSettings(t, projectDir, `schema_version: "1"
profiles:
  aux-a:
    runtime: k8s-a
  aux-a-dup:
    runtime: k8s-a-dup
runtimes:
  k8s-a:
    type: kubernetes
    context: cluster-a
    namespace: ns-a
  k8s-a-dup:
    type: kubernetes
    context: cluster-a
    namespace: ns-a
`)

	srv.resolveAuxiliaryRuntime = stubResolver(t, map[string]runtime.Runtime{
		"aux-a":     fakeKubernetesRuntime("cluster-a", "ns-a"),
		"aux-a-dup": fakeKubernetesRuntime("cluster-a", "ns-a"),
	})

	srv.discoverAuxiliaryRuntimesForProjects([]string{projectDir})

	keys := auxKeys(srv)
	if len(keys) != 1 {
		t.Fatalf("expected exactly 1 registered auxiliary runtime for duplicate resolved runtimes, got %d: %v", len(keys), keys)
	}
}

// TestDiscoverAuxiliaryRuntimes_DefaultRuntimeProfileSkipped guards the
// existing behavior that profiles resolving to the broker's own default
// runtime type are not registered as auxiliary runtimes at all.
func TestDiscoverAuxiliaryRuntimes_DefaultRuntimeProfileSkipped(t *testing.T) {
	srv := newDiscoveryTestServer(t, "docker")

	projectDir := t.TempDir()
	writeProjectSettings(t, projectDir, `schema_version: "1"
profiles:
  default-profile:
    runtime: docker-rt
runtimes:
  docker-rt:
    type: docker
`)

	// The embedded default settings (merged in by LoadEffectiveSettings ahead
	// of the project's own settings.yaml) always contribute a "remote"
	// profile (type kubernetes) regardless of this project's fixture. That
	// profile is irrelevant to what this test checks, so the stub tolerates
	// it (and any other profile) with an ErrorRuntime.
	//
	// "default-profile" resolves to a real, hostless *DockerRuntime — the
	// skip must be recognized by comparing its resolved IDENTITY against the
	// default's, not by re-deriving the type from the settings string.
	// Docker identity is always the bare type name regardless of Host (see
	// auxiliaryRuntimeIdentity), so this identity equals the MockRuntime
	// default's.
	srv.resolveAuxiliaryRuntime = func(_, _, profileFlag string) runtime.Runtime {
		if profileFlag == "default-profile" {
			return &runtime.DockerRuntime{}
		}
		return &runtime.ErrorRuntime{Err: fmt.Errorf("stub: profile %q not under test", profileFlag)}
	}

	srv.discoverAuxiliaryRuntimesForProjects([]string{projectDir})

	if keys := auxKeys(srv); len(keys) != 0 {
		t.Fatalf("expected no auxiliary runtimes registered for a profile matching the default runtime's identity, got %v", keys)
	}
}

// TestDiscoverAuxiliaryRuntimes_AliasedTypeSpellingsCollapse asserts that
// runtime.GetRuntime accepting both "k8s" and "kubernetes" as the
// settings-level type for the same concrete *KubernetesRuntime (whose
// Name() always returns "kubernetes") does not cause duplicate
// registration: two profiles that alias the same cluster this way, one
// declared under each spelling, must still collapse to a single
// registration.
func TestDiscoverAuxiliaryRuntimes_AliasedTypeSpellingsCollapse(t *testing.T) {
	srv := newDiscoveryTestServer(t, "docker")

	projectDir := t.TempDir()
	writeProjectSettings(t, projectDir, `schema_version: "1"
profiles:
  aux-k8s:
    runtime: rt-k8s
  aux-kubernetes:
    runtime: rt-kubernetes
runtimes:
  rt-k8s:
    type: k8s
    context: cluster-a
    namespace: ns-a
  rt-kubernetes:
    type: kubernetes
    context: cluster-a
    namespace: ns-a
`)

	srv.resolveAuxiliaryRuntime = stubResolver(t, map[string]runtime.Runtime{
		"aux-k8s":        fakeKubernetesRuntime("cluster-a", "ns-a"),
		"aux-kubernetes": fakeKubernetesRuntime("cluster-a", "ns-a"),
	})

	srv.discoverAuxiliaryRuntimesForProjects([]string{projectDir})

	if keys := auxKeys(srv); len(keys) != 1 {
		t.Fatalf(`expected the "k8s" and "kubernetes" settings spellings of one cluster to collapse to a single entry, got %d: %v`, len(keys), keys)
	}
}

// TestDiscoverAuxiliaryRuntimes_DifferentInstanceOfDefaultTypeStillRegisters
// asserts that when the broker's own default runtime is itself Kubernetes, a
// profile pointing at a DIFFERENT cluster/context/namespace must still be
// registered as an auxiliary runtime — comparing by type alone would wrongly
// treat it as "the default" (same type) and silently drop it, leaving that
// cluster's agents unreachable after a broker restart. A profile that
// resolves to the same cluster as the default must still be skipped.
func TestDiscoverAuxiliaryRuntimes_DifferentInstanceOfDefaultTypeStillRegisters(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "")
	defaultRT := fakeKubernetesRuntime("cluster-default", "ns-default")
	srv := New(DefaultServerConfig(), &filteringMockManager{}, defaultRT)

	projectDir := t.TempDir()
	writeProjectSettings(t, projectDir, `schema_version: "1"
profiles:
  same-as-default:
    runtime: k8s-default
  different-cluster:
    runtime: k8s-other
runtimes:
  k8s-default:
    type: kubernetes
    context: cluster-default
    namespace: ns-default
  k8s-other:
    type: kubernetes
    context: cluster-other
    namespace: ns-other
`)

	srv.resolveAuxiliaryRuntime = stubResolver(t, map[string]runtime.Runtime{
		"same-as-default":   fakeKubernetesRuntime("cluster-default", "ns-default"),
		"different-cluster": fakeKubernetesRuntime("cluster-other", "ns-other"),
	})

	srv.discoverAuxiliaryRuntimesForProjects([]string{projectDir})

	keys := auxKeys(srv)
	if len(keys) != 1 {
		t.Fatalf("expected exactly 1 registered auxiliary runtime (the non-default cluster), got %d: %v", len(keys), keys)
	}
	wantIdentity := auxiliaryRuntimeIdentity(fakeKubernetesRuntime("cluster-other", "ns-other"))
	if keys[0] != wantIdentity {
		t.Fatalf("registered identity = %q, want %q (the profile pointing at a different cluster than the default, even though both are type kubernetes)", keys[0], wantIdentity)
	}
}

// TestDiscoverAuxiliaryRuntimes_DockerHostDoesNotRegister asserts that on a
// Docker-default broker, a profile that only sets an explicit host targets
// the very same local daemon (DockerRuntime never reads Host back), so it
// must be recognized as the default and register nothing — not a duplicate
// auxiliary entry for "the same daemon under a different host spelling".
func TestDiscoverAuxiliaryRuntimes_DockerHostDoesNotRegister(t *testing.T) {
	srv := newDiscoveryTestServer(t, "docker")

	projectDir := t.TempDir()
	writeProjectSettings(t, projectDir, `schema_version: "1"
profiles:
  explicit-host:
    runtime: docker-explicit
runtimes:
  docker-explicit:
    type: docker
    host: unix:///var/run/docker.sock
`)

	srv.resolveAuxiliaryRuntime = stubResolver(t, map[string]runtime.Runtime{
		"explicit-host": &runtime.DockerRuntime{Host: "unix:///var/run/docker.sock"},
	})

	srv.discoverAuxiliaryRuntimesForProjects([]string{projectDir})

	if keys := auxKeys(srv); len(keys) != 0 {
		t.Fatalf("expected a docker profile that only sets an explicit host to match the default and register nothing, got %v", keys)
	}
}

// TestDiscoverAuxiliaryRuntimes_CollapsedIdentityKeepsSortedFirstProfile
// covers a gap TestDiscoverAuxiliaryRuntimes_DeterministicOrder does not:
// that test uses four profiles with four distinct identities, so it passes
// even without a sort — order only matters when several profiles collapse
// to ONE identity. This test uses two profiles that resolve to the same
// (context, namespace) but to distinguishable *KubernetesRuntime instances
// (different ListAllNamespaces), and asserts the lexicographically-first
// profile's instance is the one kept, every run.
func TestDiscoverAuxiliaryRuntimes_CollapsedIdentityKeepsSortedFirstProfile(t *testing.T) {
	projectDir := t.TempDir()
	writeProjectSettings(t, projectDir, `schema_version: "1"
profiles:
  aux-a:
    runtime: k8s-shared
  aux-b:
    runtime: k8s-shared-2
runtimes:
  k8s-shared:
    type: kubernetes
    context: cluster-x
    namespace: ns-x
  k8s-shared-2:
    type: kubernetes
    context: cluster-x
    namespace: ns-x
`)

	instanceA := fakeKubernetesRuntime("cluster-x", "ns-x")
	instanceA.ListAllNamespaces = true
	instanceB := fakeKubernetesRuntime("cluster-x", "ns-x")
	instanceB.ListAllNamespaces = false

	for i := 0; i < 10; i++ {
		srv := newDiscoveryTestServer(t, "docker")
		srv.resolveAuxiliaryRuntime = stubResolver(t, map[string]runtime.Runtime{
			"aux-a": instanceA,
			"aux-b": instanceB,
		})
		srv.discoverAuxiliaryRuntimesForProjects([]string{projectDir})

		keys := auxKeys(srv)
		if len(keys) != 1 {
			t.Fatalf("run %d: expected the two same-identity profiles to collapse to 1 entry, got %d: %v", i, len(keys), keys)
		}

		srv.auxiliaryRuntimesMu.RLock()
		got := srv.auxiliaryRuntimes[keys[0]].Runtime
		srv.auxiliaryRuntimesMu.RUnlock()

		// "aux-a" sorts before "aux-b", so instanceA (registered from
		// aux-a) must be the one that wins, every run.
		gotK8s, ok := got.(*runtime.KubernetesRuntime)
		if !ok || !gotK8s.ListAllNamespaces {
			t.Fatalf("run %d: expected the lexicographically-first profile's instance (aux-a, ListAllNamespaces=true) to be registered", i)
		}
	}
}

// TestDiscoverAuxiliaryRuntimes_DeterministicOrder runs discovery repeatedly
// against a settings file with many profiles and asserts the same set of
// registered identities results every time, so registration does not depend
// on Go's randomized map iteration order over vs.Profiles.
func TestDiscoverAuxiliaryRuntimes_DeterministicOrder(t *testing.T) {
	projectDir := t.TempDir()
	writeProjectSettings(t, projectDir, `schema_version: "1"
profiles:
  aux-a:
    runtime: k8s-a
  aux-b:
    runtime: k8s-b
  aux-c:
    runtime: k8s-c
  aux-d:
    runtime: k8s-d
runtimes:
  k8s-a:
    type: kubernetes
    context: cluster-a
    namespace: ns-a
  k8s-b:
    type: kubernetes
    context: cluster-b
    namespace: ns-b
  k8s-c:
    type: kubernetes
    context: cluster-c
    namespace: ns-c
  k8s-d:
    type: kubernetes
    context: cluster-d
    namespace: ns-d
`)

	byProfile := map[string]runtime.Runtime{
		"aux-a": fakeKubernetesRuntime("cluster-a", "ns-a"),
		"aux-b": fakeKubernetesRuntime("cluster-b", "ns-b"),
		"aux-c": fakeKubernetesRuntime("cluster-c", "ns-c"),
		"aux-d": fakeKubernetesRuntime("cluster-d", "ns-d"),
	}

	var first []string
	for i := 0; i < 10; i++ {
		srv := newDiscoveryTestServer(t, "docker")
		srv.resolveAuxiliaryRuntime = stubResolver(t, byProfile)
		srv.discoverAuxiliaryRuntimesForProjects([]string{projectDir})

		keys := auxKeys(srv)
		if i == 0 {
			first = keys
			if len(first) != 4 {
				t.Fatalf("expected 4 registered auxiliary runtimes, got %d: %v", len(first), first)
			}
			continue
		}
		if len(keys) != len(first) {
			t.Fatalf("run %d: got %d keys, want %d (%v vs %v)", i, len(keys), len(first), keys, first)
		}
		for j := range keys {
			if keys[j] != first[j] {
				t.Fatalf("run %d: non-deterministic registration, got %v, want %v", i, keys, first)
			}
		}
	}
}

// TestDiscoverAuxiliaryRuntimes_UnresolvedProfileLogLevel: an unresolvable
// non-active profile is reported at Info with a hint; the active profile's
// failure stays a warning (ptone/scion#3605).
func TestDiscoverAuxiliaryRuntimes_UnresolvedProfileLogLevel(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	s := newDiscoveryTestServer(t, "docker")
	s.resolveAuxiliaryRuntime = stubResolver(t, nil) // every profile fails to resolve
	projectDir := filepath.Join(t.TempDir(), ".scion")
	writeProjectSettings(t, projectDir, `schema_version: "1"
active_profile: local
profiles:
  local:
    runtime: k8s
  remote:
    runtime: k8s
runtimes:
  k8s:
    type: kubernetes
`)
	s.discoverAuxiliaryRuntimesForProjects([]string{projectDir})

	out := buf.String()
	var remoteLine, localLine string
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.Contains(line, "profile=remote"):
			remoteLine = line
		case strings.Contains(line, "profile=local"):
			localLine = line
		}
	}
	if !strings.Contains(remoteLine, "level=INFO") || !strings.Contains(remoteLine, "hint=") {
		t.Errorf("non-active profile: want an Info line with a hint, got %q", remoteLine)
	}
	if !strings.Contains(localLine, "level=WARN") {
		t.Errorf("active profile: want a warning, got %q", localLine)
	}
}
