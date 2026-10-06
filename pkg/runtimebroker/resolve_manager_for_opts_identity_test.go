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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// TestResolveManagerForOpts_DistinctKubernetesProfilesBothTracked asserts
// that resolveManagerForOpts, which populates the same Server.auxiliaryRuntimes
// map that discoverAuxiliaryRuntimesForProjects does, registers two profiles
// resolving to different Kubernetes clusters as two separate entries rather
// than one overwriting the other. This calls resolveManagerForOpts directly
// (as the agent-start path does) once per profile and asserts both
// identities remain registered afterward.
func TestResolveManagerForOpts_DistinctKubernetesProfilesBothTracked(t *testing.T) {
	srv := newDiscoveryTestServer(t, "docker")
	srv.config.ForceRuntime = ""

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

	mgrA, _ := srv.resolveManagerForOpts(api.StartOptions{Name: "agent-a", Profile: "aux-a", ProjectPath: projectDir})
	mgrB, _ := srv.resolveManagerForOpts(api.StartOptions{Name: "agent-b", Profile: "aux-b", ProjectPath: projectDir})

	if mgrA == srv.manager || mgrB == srv.manager {
		t.Fatalf("expected both profiles to resolve to non-default managers")
	}
	if mgrA == mgrB {
		t.Fatalf("expected distinct managers for distinct Kubernetes clusters")
	}

	keys := auxKeys(srv)
	if len(keys) != 2 {
		t.Fatalf("expected both Kubernetes clusters to remain registered (not overwrite each other), got %d: %v", len(keys), keys)
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
		t.Fatalf("expected both cluster-a and cluster-b runtimes registered, gotA=%v gotB=%v", gotA, gotB)
	}
}

// TestResolveManagerForOpts_SameProfileTwiceReusesIdentitySlot ensures that
// resolving the SAME profile twice (e.g. two agent starts against one
// cluster) keeps exactly one entry — the identity key must be stable across
// calls, not accumulate duplicates.
func TestResolveManagerForOpts_SameProfileTwiceReusesIdentitySlot(t *testing.T) {
	srv := newDiscoveryTestServer(t, "docker")
	srv.config.ForceRuntime = ""

	projectDir := t.TempDir()
	writeProjectSettings(t, projectDir, `schema_version: "1"
profiles:
  aux-a:
    runtime: k8s-a
runtimes:
  k8s-a:
    type: kubernetes
    context: cluster-a
    namespace: ns-a
`)

	srv.resolveAuxiliaryRuntime = stubResolver(t, map[string]runtime.Runtime{
		"aux-a": fakeKubernetesRuntime("cluster-a", "ns-a"),
	})

	_, _ = srv.resolveManagerForOpts(api.StartOptions{Name: "agent-1", Profile: "aux-a", ProjectPath: projectDir})
	_, _ = srv.resolveManagerForOpts(api.StartOptions{Name: "agent-2", Profile: "aux-a", ProjectPath: projectDir})

	if keys := auxKeys(srv); len(keys) != 1 {
		t.Fatalf("expected exactly 1 registered auxiliary runtime after resolving the same profile twice, got %d: %v", len(keys), keys)
	}
}

// TestResolveManagerForOpts_ForceRuntimeSelectsIdentityKeyedAux covers the
// ForceRuntime call site in resolveManagerForOpts (handlers.go), which looks
// up findAuxiliaryRuntimeByType rather than indexing s.auxiliaryRuntimes
// directly: that map is keyed by auxiliaryRuntimeIdentity, not by type, so a
// Kubernetes entry is never found under the bare type name "kubernetes". A
// registered Kubernetes auxiliary runtime must still be selected when
// ForceRuntime names its type, without ever calling resolveAuxiliaryRuntime
// (settings resolution is not consulted when ForceRuntime short-circuits).
func TestResolveManagerForOpts_ForceRuntimeSelectsIdentityKeyedAux(t *testing.T) {
	srv := newDiscoveryTestServer(t, "docker")
	srv.config.ForceRuntime = "kubernetes"

	rt := fakeKubernetesRuntime("cluster-a", "ns-a")
	wantManager := agent.NewManager(rt)
	srv.auxiliaryRuntimesMu.Lock()
	srv.auxiliaryRuntimes[auxiliaryRuntimeIdentity(rt)] = auxiliaryRuntime{Runtime: rt, Manager: wantManager}
	srv.auxiliaryRuntimesMu.Unlock()

	srv.resolveAuxiliaryRuntime = func(_, _, _ string) runtime.Runtime {
		t.Fatal("resolveAuxiliaryRuntime must not be called when ForceRuntime matches a registered auxiliary runtime")
		return nil
	}

	gotManager, gotName := srv.resolveManagerForOpts(api.StartOptions{Name: "agent-a"})

	if gotManager != wantManager {
		t.Fatalf("expected the registered auxiliary manager, got %#v", gotManager)
	}
	if gotName != "kubernetes" {
		t.Fatalf("expected runtime name %q, got %q", "kubernetes", gotName)
	}
	if keys := auxKeys(srv); len(keys) != 1 {
		t.Fatalf("expected the auxiliary registry to remain unchanged (1 entry), got %d: %v", len(keys), keys)
	}
}

// TestResolveRuntimeNameForOpts_ForceRuntimeMatchesDispatch pins
// resolveRuntimeNameForOpts — the auth preflight's cheap, client-free
// runtime-name lookup — to resolveManagerForOpts's own ForceRuntime
// resolution. The two are required to stay in sync by hand (see
// resolveRuntimeNameForOpts's doc comment): both must agree on the runtime
// name for a ForceRuntime that names a registered identity-keyed auxiliary
// runtime, or the preflight could compute credential requirements for a
// different runtime than the one that actually dispatches.
func TestResolveRuntimeNameForOpts_ForceRuntimeMatchesDispatch(t *testing.T) {
	srv := newDiscoveryTestServer(t, "docker")
	srv.config.ForceRuntime = "kubernetes"

	rt := fakeKubernetesRuntime("cluster-a", "ns-a")
	srv.auxiliaryRuntimesMu.Lock()
	srv.auxiliaryRuntimes[auxiliaryRuntimeIdentity(rt)] = auxiliaryRuntime{Runtime: rt, Manager: agent.NewManager(rt)}
	srv.auxiliaryRuntimesMu.Unlock()

	srv.resolveAuxiliaryRuntime = func(_, _, _ string) runtime.Runtime {
		t.Fatal("resolveAuxiliaryRuntime must not be called when ForceRuntime matches a registered auxiliary runtime")
		return nil
	}

	_, dispatchName := srv.resolveManagerForOpts(api.StartOptions{Name: "agent-a"})
	preflightName := srv.resolveRuntimeNameForOpts(api.StartOptions{Name: "agent-a"})

	if preflightName != "kubernetes" {
		t.Fatalf("expected preflight name %q, got %q", "kubernetes", preflightName)
	}
	if preflightName != dispatchName {
		t.Fatalf("preflight name %q disagrees with dispatch name %q", preflightName, dispatchName)
	}
}
