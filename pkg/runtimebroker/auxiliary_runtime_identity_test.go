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

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/k8s"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// TestAuxiliaryRuntimeIdentity_KubernetesContextAndNamespace covers the core
// disambiguating fields for Kubernetes: two different (context, namespace)
// pairs must produce different identities, and the same pair must produce
// the same identity even from two distinct *KubernetesRuntime values.
func TestAuxiliaryRuntimeIdentity_KubernetesContextAndNamespace(t *testing.T) {
	a1 := fakeKubernetesRuntime("cluster-a", "ns-a")
	a2 := fakeKubernetesRuntime("cluster-a", "ns-a")
	b := fakeKubernetesRuntime("cluster-b", "ns-a")
	c := fakeKubernetesRuntime("cluster-a", "ns-c")

	if got := auxiliaryRuntimeIdentity(a1); got != auxiliaryRuntimeIdentity(a2) {
		t.Fatalf("expected identical (context,namespace) to produce the same identity, got %q vs %q", got, auxiliaryRuntimeIdentity(a2))
	}
	if auxiliaryRuntimeIdentity(a1) == auxiliaryRuntimeIdentity(b) {
		t.Fatal("expected different contexts to produce different identities")
	}
	if auxiliaryRuntimeIdentity(a1) == auxiliaryRuntimeIdentity(c) {
		t.Fatal("expected different namespaces to produce different identities")
	}
}

// TestAuxiliaryRuntimeIdentity_CanonicalNameNotSettingsSpelling asserts that
// the identity's type component is exactly the resolved runtime's own
// Name() ("kubernetes"), not a settings-level spelling such as "k8s".
// runtime.GetRuntime accepts both spellings for the same concrete
// *KubernetesRuntime type, so a caller that (incorrectly) built the identity
// from the settings string instead of Name() would produce two different
// identities for one cluster; auxiliaryRuntimeIdentity takes only the
// resolved runtime, so there is no settings string to read in the first
// place. The exact expected string pins the type component to Name()
// specifically, not just to "whatever this function happens to return
// deterministically".
func TestAuxiliaryRuntimeIdentity_CanonicalNameNotSettingsSpelling(t *testing.T) {
	rt := fakeKubernetesRuntime("cluster-a", "ns-a")
	if got, want := rt.Name(), "kubernetes"; got != want {
		t.Fatalf("sanity check failed: KubernetesRuntime.Name() = %q, want %q", got, want)
	}
	want := "kubernetes|context=cluster-a|namespace=ns-a"
	if got := auxiliaryRuntimeIdentity(rt); got != want {
		t.Fatalf("identity = %q, want %q", got, want)
	}
}

// TestAuxiliaryRuntimeIdentity_DockerHost asserts that DockerRuntime.Host,
// which is assigned from settings but never read back by DockerRuntime (no
// -H/--host flag, no DOCKER_HOST wiring), cannot distinguish one reachable
// Docker daemon from another — every instance below must collapse to the
// same bare-type identity as a generic Runtime double that doesn't model a
// host at all (e.g. a mock standing in for the broker's own default
// runtime), including two different, unnormalized spellings of the same
// local socket.
func TestAuxiliaryRuntimeIdentity_DockerHost(t *testing.T) {
	local := &runtime.DockerRuntime{}
	mockDefault := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	explicitSocket := &runtime.DockerRuntime{Host: "unix:///var/run/docker.sock"}
	differentSpelling := &runtime.DockerRuntime{Host: "/var/run/docker.sock"}
	remote := &runtime.DockerRuntime{Host: "tcp://remote-host:2375"}

	want := auxiliaryRuntimeIdentity(mockDefault)
	for _, rt := range []*runtime.DockerRuntime{local, explicitSocket, differentSpelling, remote} {
		if got := auxiliaryRuntimeIdentity(rt); got != want {
			t.Fatalf("expected Host to be ignored for Docker identity (host=%q), got %q, want %q", rt.Host, got, want)
		}
	}
}

// TestAuxiliaryRuntimeIdentity_PodmanHost mirrors the Docker case for Podman:
// Host is likewise never read back by PodmanRuntime, so it cannot distinguish
// instances either.
func TestAuxiliaryRuntimeIdentity_PodmanHost(t *testing.T) {
	local := &runtime.PodmanRuntime{}
	remoteA := &runtime.PodmanRuntime{Host: "ssh://host-a"}
	remoteB := &runtime.PodmanRuntime{Host: "ssh://host-b"}

	if auxiliaryRuntimeIdentity(local) != "podman" {
		t.Fatalf("expected bare 'podman' identity for a hostless runtime, got %q", auxiliaryRuntimeIdentity(local))
	}
	if got, want := auxiliaryRuntimeIdentity(remoteA), "podman"; got != want {
		t.Fatalf("expected Host to be ignored for Podman identity, got %q, want %q", got, want)
	}
	if auxiliaryRuntimeIdentity(remoteA) != auxiliaryRuntimeIdentity(remoteB) {
		t.Fatal("expected different Podman hosts to collapse to the same identity")
	}
}

// TestAuxiliaryRuntimeIdentity_CloudRunProjectAndLocation asserts that Cloud
// Run keeps the bare type identity like any other runtime with no stable
// per-instance config today: an unresolved (auto-detected) instance and
// instances with different explicit project/location all match a generic
// type-only double. ProjectID/Location are resolved lazily from GCE metadata
// on an auto-detected instance's first API call, under a mutex that
// auxiliaryRuntimeIdentity does not hold, so an identity built from those
// fields would be unsynchronized with that resolution and would also change
// value out from under an already-registered entry once it completed.
func TestAuxiliaryRuntimeIdentity_CloudRunProjectAndLocation(t *testing.T) {
	autoDetected, err := runtime.NewCloudRunRuntime(&config.CloudRunConfig{})
	if err != nil {
		t.Fatal(err)
	}
	mockDefault := &runtime.MockRuntime{NameFunc: func() string { return "cloudrun" }}

	projA, err := runtime.NewCloudRunRuntime(&config.CloudRunConfig{ProjectID: "proj-a", Location: "us-central1"})
	if err != nil {
		t.Fatal(err)
	}
	projB, err := runtime.NewCloudRunRuntime(&config.CloudRunConfig{ProjectID: "proj-b", Location: "us-central1"})
	if err != nil {
		t.Fatal(err)
	}

	want := auxiliaryRuntimeIdentity(mockDefault)
	for _, rt := range []runtime.Runtime{autoDetected, projA, projB} {
		if got := auxiliaryRuntimeIdentity(rt); got != want {
			t.Fatalf("expected Cloud Run identity to stay type-only regardless of project/location, got %q, want %q", got, want)
		}
	}
}

// TestAuxiliaryRuntimeIdentity_DefaultFallback covers a runtime type with no
// per-instance identity fields modeled (e.g. Apple container, Cloud Run
// Sandbox, or any generic double): identity is the bare type name, and two
// different types never collide.
func TestAuxiliaryRuntimeIdentity_DefaultFallback(t *testing.T) {
	a := &runtime.MockRuntime{NameFunc: func() string { return "container" }}
	b := &runtime.MockRuntime{NameFunc: func() string { return "cloudrun-sandbox" }}

	if got, want := auxiliaryRuntimeIdentity(a), "container"; got != want {
		t.Fatalf("identity = %q, want %q", got, want)
	}
	if auxiliaryRuntimeIdentity(a) == auxiliaryRuntimeIdentity(b) {
		t.Fatal("expected different types to produce different identities")
	}
}

// TestAuxiliaryRuntimeIdentity_InClusterContextIsOpaque asserts that
// auxiliaryRuntimeIdentity reads k8s.Client.CurrentContext through
// unmodified, including the in-cluster sentinel value "in-cluster". Context
// resolution (in-cluster detection setting it to "in-cluster";
// loadClientConfig resolving an empty configured context to the
// kubeconfig's current-context name) happens before auxiliaryRuntimeIdentity
// ever sees the value, so this function only needs to trust whatever
// CurrentContext already holds, not interpret it.
func TestAuxiliaryRuntimeIdentity_InClusterContextIsOpaque(t *testing.T) {
	rt := &runtime.KubernetesRuntime{
		Client:           &k8s.Client{CurrentContext: "in-cluster"},
		DefaultNamespace: "scion",
	}
	want := "kubernetes|context=in-cluster|namespace=scion"
	if got := auxiliaryRuntimeIdentity(rt); got != want {
		t.Fatalf("identity = %q, want %q", got, want)
	}
}
