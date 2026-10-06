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
	"fmt"
	"sort"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// TestFindAuxiliaryRuntimeByType_NoMatch covers the ForceRuntime lookup path
// in resolveManagerForOpts finding nothing registered for the requested type.
// A registered entry of a DIFFERENT type is present so the test cannot pass
// by an empty-map accident: it must be the type filter, not just "is the map
// non-empty", that rejects the match.
func TestFindAuxiliaryRuntimeByType_NoMatch(t *testing.T) {
	srv := newDiscoveryTestServer(t, "docker")

	other := &runtime.MockRuntime{NameFunc: func() string { return "cloudrun" }}
	srv.auxiliaryRuntimesMu.Lock()
	srv.auxiliaryRuntimes[auxiliaryRuntimeIdentity(other)] = auxiliaryRuntime{Runtime: other, Manager: agent.NewManager(other)}
	srv.auxiliaryRuntimesMu.Unlock()

	if _, ok := srv.findAuxiliaryRuntimeByType("kubernetes"); ok {
		t.Fatal("expected no match for \"kubernetes\" when only a \"cloudrun\" entry is registered")
	}
}

// TestFindAuxiliaryRuntimeByType_SingleInstance is the common case: exactly
// one registered runtime of the requested type is found via its type,
// regardless of the identity key it is stored under. A second entry of a
// different type is also registered, with an identity that sorts before the
// Kubernetes one, so the test also pins the type filter itself: without it,
// the lexicographically-first key would win and return the wrong runtime.
func TestFindAuxiliaryRuntimeByType_SingleInstance(t *testing.T) {
	srv := newDiscoveryTestServer(t, "docker")

	rt := fakeKubernetesRuntime("cluster-a", "ns-a")
	other := &runtime.MockRuntime{NameFunc: func() string { return "cloudrun" }}

	srv.auxiliaryRuntimesMu.Lock()
	srv.auxiliaryRuntimes[auxiliaryRuntimeIdentity(rt)] = auxiliaryRuntime{Runtime: rt, Manager: agent.NewManager(rt)}
	srv.auxiliaryRuntimes[auxiliaryRuntimeIdentity(other)] = auxiliaryRuntime{Runtime: other, Manager: agent.NewManager(other)}
	srv.auxiliaryRuntimesMu.Unlock()

	aux, ok := srv.findAuxiliaryRuntimeByType("kubernetes")
	if !ok {
		t.Fatal("expected a match for type kubernetes")
	}
	if aux.Runtime != runtime.Runtime(rt) {
		t.Fatalf("expected the registered kubernetes runtime, got %#v", aux.Runtime)
	}
}

// TestFindAuxiliaryRuntimeByType_MultipleInstancesDeterministic documents the
// current, intentionally coarse behavior when more than one auxiliary
// runtime of the same type is registered (e.g. several distinct Kubernetes
// clusters, per ptone/scion#2260): ForceRuntime only names a type, so the
// match is ambiguous. The pick must be deterministic — the identity key that
// sorts first among same-type entries — rather than dependent on map
// iteration order.
//
// Eight distinct Kubernetes identities are registered, rather than two: with
// only two map entries, an unsorted pick is right about half the time by
// chance alone. With eight, it is right only about 1 time in 8, so across
// the ten iterations below a missing sort fails essentially always. An entry
// of a different type, "aaa", whose identity sorts before every "cluster-*"
// one, is also registered on every iteration to pin the type filter
// alongside the sort: without it, that entry — not any Kubernetes one —
// would be the lexicographically-first key in the whole map.
func TestFindAuxiliaryRuntimeByType_MultipleInstancesDeterministic(t *testing.T) {
	srv := newDiscoveryTestServer(t, "docker")

	clusters := []string{"cluster-a", "cluster-b", "cluster-c", "cluster-d", "cluster-e", "cluster-f", "cluster-g", "cluster-h"}
	runtimes := make(map[string]*runtime.KubernetesRuntime, len(clusters))
	identities := make([]string, 0, len(clusters))
	for i, cluster := range clusters {
		rt := fakeKubernetesRuntime(cluster, fmt.Sprintf("ns-%d", i))
		runtimes[cluster] = rt
		identities = append(identities, auxiliaryRuntimeIdentity(rt))
	}
	sort.Strings(identities)
	wantIdentity := identities[0]

	otherType := &runtime.MockRuntime{NameFunc: func() string { return "aaa" }}

	for i := 0; i < 10; i++ {
		// Register entries in reverse-sorted order so a loop that merely
		// preserved insertion order (rather than genuinely sorting) would
		// also be caught.
		srv.auxiliaryRuntimesMu.Lock()
		srv.auxiliaryRuntimes = map[string]auxiliaryRuntime{
			auxiliaryRuntimeIdentity(otherType): {Runtime: otherType, Manager: agent.NewManager(otherType)},
		}
		for j := len(clusters) - 1; j >= 0; j-- {
			rt := runtimes[clusters[j]]
			srv.auxiliaryRuntimes[auxiliaryRuntimeIdentity(rt)] = auxiliaryRuntime{Runtime: rt, Manager: agent.NewManager(rt)}
		}
		srv.auxiliaryRuntimesMu.Unlock()

		aux, ok := srv.findAuxiliaryRuntimeByType("kubernetes")
		if !ok {
			t.Fatal("expected a match for type kubernetes")
		}
		gotIdentity := auxiliaryRuntimeIdentity(aux.Runtime)
		if gotIdentity != wantIdentity {
			t.Fatalf("run %d: expected deterministic pick %q, got %q", i, wantIdentity, gotIdentity)
		}
	}
}
