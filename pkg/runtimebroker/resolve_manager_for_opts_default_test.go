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
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// newKubernetesDefaultTestServer builds a Server whose default runtime is a
// Kubernetes runtime (cluster-default / ns-default), for testing
// resolveManagerForOpts's cheap default-match pre-check and the
// authoritative post-resolution identity compare that backs it.
func newKubernetesDefaultTestServer(t *testing.T) *Server {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "")

	cfg := DefaultServerConfig()
	return New(cfg, &filteringMockManager{}, fakeKubernetesRuntime("cluster-default", "ns-default"))
}

// TestResolveManagerForOpts_DefaultKubernetesProfileSkipsResolution asserts
// that a profile whose settings resolve to the SAME context and namespace as
// a Kubernetes-default broker's own runtime returns the shared manager
// WITHOUT calling resolveAuxiliaryRuntime (runtime.GetRuntime, which for
// Kubernetes also calls Client.Verify(), a live API round trip) at all. The
// stub here always errors: if resolveManagerForOpts ever invokes it for this
// profile, the test fails.
func TestResolveManagerForOpts_DefaultKubernetesProfileSkipsResolution(t *testing.T) {
	srv := newKubernetesDefaultTestServer(t)
	srv.config.ForceRuntime = ""

	projectDir := t.TempDir()
	writeProjectSettings(t, projectDir, `schema_version: "1"
profiles:
  same:
    runtime: k8s-same
runtimes:
  k8s-same:
    type: kubernetes
    context: cluster-default
    namespace: ns-default
`)

	srv.resolveAuxiliaryRuntime = func(_, _, profileFlag string) runtime.Runtime {
		return &runtime.ErrorRuntime{Err: fmt.Errorf("resolveAuxiliaryRuntime must not be called for profile %q matching the default", profileFlag)}
	}

	mgr, _ := srv.resolveManagerForOpts(api.StartOptions{Name: "agent", Profile: "same", ProjectPath: projectDir})

	if mgr != srv.manager {
		t.Fatalf("expected the shared manager for a profile matching the Kubernetes default (did resolveManagerForOpts call resolveAuxiliaryRuntime instead of short-circuiting?)")
	}
	if keys := auxKeys(srv); len(keys) != 0 {
		t.Fatalf("expected no auxiliary runtime registered for the default-matching profile, got %v", keys)
	}
}

// TestResolveManagerForOpts_DifferentClusterKubernetesProfileResolves is the
// companion case: a profile targeting a cluster/namespace different from a
// Kubernetes-default broker's own runtime must still be fully resolved and
// registered as a distinct auxiliary runtime — the cheap pre-check must not
// short-circuit profiles that are genuinely not the default.
func TestResolveManagerForOpts_DifferentClusterKubernetesProfileResolves(t *testing.T) {
	srv := newKubernetesDefaultTestServer(t)
	srv.config.ForceRuntime = ""

	projectDir := t.TempDir()
	writeProjectSettings(t, projectDir, `schema_version: "1"
profiles:
  other:
    runtime: k8s-other
runtimes:
  k8s-other:
    type: kubernetes
    context: cluster-other
    namespace: ns-other
`)

	other := fakeKubernetesRuntime("cluster-other", "ns-other")
	srv.resolveAuxiliaryRuntime = stubResolver(t, map[string]runtime.Runtime{
		"other": other,
	})

	mgr, _ := srv.resolveManagerForOpts(api.StartOptions{Name: "agent", Profile: "other", ProjectPath: projectDir})

	if mgr == srv.manager {
		t.Fatalf("expected a non-default manager for a profile targeting a different cluster")
	}
	wantIdentity := auxiliaryRuntimeIdentity(other)
	keys := auxKeys(srv)
	if len(keys) != 1 || keys[0] != wantIdentity {
		t.Fatalf("expected exactly the identity %q registered, got %v", wantIdentity, keys)
	}
}

// TestResolveManagerForOpts_DifferentContextSameNamespaceResolves asserts
// that a profile with the same namespace as the Kubernetes-default broker
// but a DIFFERENT context must still be fully resolved and registered, not
// waved through by the pre-check on the strength of the namespace match
// alone.
func TestResolveManagerForOpts_DifferentContextSameNamespaceResolves(t *testing.T) {
	srv := newKubernetesDefaultTestServer(t)
	srv.config.ForceRuntime = ""

	projectDir := t.TempDir()
	writeProjectSettings(t, projectDir, `schema_version: "1"
profiles:
  other-context:
    runtime: k8s-other-context
runtimes:
  k8s-other-context:
    type: kubernetes
    context: cluster-other
    namespace: ns-default
`)

	other := fakeKubernetesRuntime("cluster-other", "ns-default")
	srv.resolveAuxiliaryRuntime = stubResolver(t, map[string]runtime.Runtime{
		"other-context": other,
	})

	mgr, _ := srv.resolveManagerForOpts(api.StartOptions{Name: "agent", Profile: "other-context", ProjectPath: projectDir})

	if mgr == srv.manager {
		t.Fatalf("expected a non-default manager for a profile targeting a different context")
	}
	wantIdentity := auxiliaryRuntimeIdentity(other)
	keys := auxKeys(srv)
	if len(keys) != 1 || keys[0] != wantIdentity {
		t.Fatalf("expected exactly the identity %q registered, got %v", wantIdentity, keys)
	}
}

// TestResolveManagerForOpts_SameContextDifferentNamespaceResolves asserts
// that a profile with the same context as the Kubernetes-default broker but
// a DIFFERENT namespace must still be fully resolved and registered, not
// waved through by the pre-check on the strength of the context match alone.
func TestResolveManagerForOpts_SameContextDifferentNamespaceResolves(t *testing.T) {
	srv := newKubernetesDefaultTestServer(t)
	srv.config.ForceRuntime = ""

	projectDir := t.TempDir()
	writeProjectSettings(t, projectDir, `schema_version: "1"
profiles:
  other-namespace:
    runtime: k8s-other-namespace
runtimes:
  k8s-other-namespace:
    type: kubernetes
    context: cluster-default
    namespace: ns-other
`)

	other := fakeKubernetesRuntime("cluster-default", "ns-other")
	srv.resolveAuxiliaryRuntime = stubResolver(t, map[string]runtime.Runtime{
		"other-namespace": other,
	})

	mgr, _ := srv.resolveManagerForOpts(api.StartOptions{Name: "agent", Profile: "other-namespace", ProjectPath: projectDir})

	if mgr == srv.manager {
		t.Fatalf("expected a non-default manager for a profile targeting a different namespace")
	}
	wantIdentity := auxiliaryRuntimeIdentity(other)
	keys := auxKeys(srv)
	if len(keys) != 1 || keys[0] != wantIdentity {
		t.Fatalf("expected exactly the identity %q registered, got %v", wantIdentity, keys)
	}
}

// TestResolveManagerForOpts_EmptyNamespaceUsesEnvDefault asserts that a
// profile with no namespace set falls back to
// scionrt.DefaultKubernetesNamespace() (SCION_K8S_NAMESPACE), the same chain
// the broker's own default runtime was built from — not a hardcoded
// "default".
func TestResolveManagerForOpts_EmptyNamespaceUsesEnvDefault(t *testing.T) {
	projectDir := t.TempDir()
	writeProjectSettings(t, projectDir, `schema_version: "1"
profiles:
  unset-namespace:
    runtime: k8s-unset-namespace
runtimes:
  k8s-unset-namespace:
    type: kubernetes
    context: cluster-default
`)

	t.Run("matching env default skips resolution", func(t *testing.T) {
		srv := newKubernetesDefaultTestServer(t)
		srv.config.ForceRuntime = ""
		t.Setenv("SCION_K8S_NAMESPACE", "ns-default")

		srv.resolveAuxiliaryRuntime = func(_, _, profileFlag string) runtime.Runtime {
			return &runtime.ErrorRuntime{Err: fmt.Errorf("resolveAuxiliaryRuntime must not be called for profile %q matching the default", profileFlag)}
		}

		mgr, _ := srv.resolveManagerForOpts(api.StartOptions{Name: "agent", Profile: "unset-namespace", ProjectPath: projectDir})

		if mgr != srv.manager {
			t.Fatalf("expected the shared manager when the env default namespace matches the broker's own")
		}
	})

	t.Run("different env default resolves", func(t *testing.T) {
		srv := newKubernetesDefaultTestServer(t)
		srv.config.ForceRuntime = ""
		t.Setenv("SCION_K8S_NAMESPACE", "ns-other")

		other := fakeKubernetesRuntime("cluster-default", "ns-other")
		srv.resolveAuxiliaryRuntime = stubResolver(t, map[string]runtime.Runtime{
			"unset-namespace": other,
		})

		mgr, _ := srv.resolveManagerForOpts(api.StartOptions{Name: "agent", Profile: "unset-namespace", ProjectPath: projectDir})

		if mgr == srv.manager {
			t.Fatalf("expected a non-default manager when the env default namespace differs from the broker's own")
		}
		wantIdentity := auxiliaryRuntimeIdentity(other)
		keys := auxKeys(srv)
		if len(keys) != 1 || keys[0] != wantIdentity {
			t.Fatalf("expected exactly the identity %q registered, got %v", wantIdentity, keys)
		}
	})
}

// TestResolveManagerForOpts_K8sAliasTypeMatchesDefault asserts that a
// profile declared with the "k8s" alias (rather than "kubernetes") must
// still be recognized as matching the Kubernetes-default broker's own
// runtime, via canonicalRuntimeTypeName.
func TestResolveManagerForOpts_K8sAliasTypeMatchesDefault(t *testing.T) {
	srv := newKubernetesDefaultTestServer(t)
	srv.config.ForceRuntime = ""

	projectDir := t.TempDir()
	writeProjectSettings(t, projectDir, `schema_version: "1"
profiles:
  k8s-alias:
    runtime: k8s-alias-runtime
runtimes:
  k8s-alias-runtime:
    type: k8s
    context: cluster-default
    namespace: ns-default
`)

	srv.resolveAuxiliaryRuntime = func(_, _, profileFlag string) runtime.Runtime {
		return &runtime.ErrorRuntime{Err: fmt.Errorf("resolveAuxiliaryRuntime must not be called for profile %q matching the default", profileFlag)}
	}

	mgr, _ := srv.resolveManagerForOpts(api.StartOptions{Name: "agent", Profile: "k8s-alias", ProjectPath: projectDir})

	if mgr != srv.manager {
		t.Fatalf("expected the shared manager for a profile using the \"k8s\" alias for the Kubernetes-default broker's own type")
	}
	if keys := auxKeys(srv); len(keys) != 0 {
		t.Fatalf("expected no auxiliary runtime registered for the default-matching profile, got %v", keys)
	}
}

// TestResolveManagerForOpts_EmptyContextUsesKubeconfigCurrentContext asserts
// that a profile with no context set targets the kubeconfig's current
// context, resolved the same way runtime.GetRuntime does
// (k8s.NewClientWithContext), not a hardcoded fallback.
func TestResolveManagerForOpts_EmptyContextUsesKubeconfigCurrentContext(t *testing.T) {
	projectDir := t.TempDir()
	writeProjectSettings(t, projectDir, `schema_version: "1"
profiles:
  unset-context:
    runtime: k8s-unset-context
runtimes:
  k8s-unset-context:
    type: kubernetes
    namespace: ns-default
`)

	writeKubeconfig := func(t *testing.T, currentContext string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "kubeconfig")
		contents := fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: %[1]s
  cluster:
    server: https://%[1]s.example.invalid
contexts:
- name: %[1]s
  context:
    cluster: %[1]s
    user: %[1]s-user
current-context: %[1]s
users:
- name: %[1]s-user
  user: {}
`, currentContext)
		if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
		return path
	}

	t.Run("matching current context skips resolution", func(t *testing.T) {
		srv := newKubernetesDefaultTestServer(t)
		srv.config.ForceRuntime = ""
		t.Setenv("KUBECONFIG", writeKubeconfig(t, "cluster-default"))

		srv.resolveAuxiliaryRuntime = func(_, _, profileFlag string) runtime.Runtime {
			return &runtime.ErrorRuntime{Err: fmt.Errorf("resolveAuxiliaryRuntime must not be called for profile %q matching the default", profileFlag)}
		}

		mgr, _ := srv.resolveManagerForOpts(api.StartOptions{Name: "agent", Profile: "unset-context", ProjectPath: projectDir})

		if mgr != srv.manager {
			t.Fatalf("expected the shared manager when the kubeconfig's current context matches the broker's own")
		}
	})

	t.Run("different current context resolves", func(t *testing.T) {
		srv := newKubernetesDefaultTestServer(t)
		srv.config.ForceRuntime = ""
		t.Setenv("KUBECONFIG", writeKubeconfig(t, "cluster-other"))

		other := fakeKubernetesRuntime("cluster-other", "ns-default")
		srv.resolveAuxiliaryRuntime = stubResolver(t, map[string]runtime.Runtime{
			"unset-context": other,
		})

		mgr, _ := srv.resolveManagerForOpts(api.StartOptions{Name: "agent", Profile: "unset-context", ProjectPath: projectDir})

		if mgr == srv.manager {
			t.Fatalf("expected a non-default manager when the kubeconfig's current context differs from the broker's own")
		}
		wantIdentity := auxiliaryRuntimeIdentity(other)
		keys := auxKeys(srv)
		if len(keys) != 1 || keys[0] != wantIdentity {
			t.Fatalf("expected exactly the identity %q registered, got %v", wantIdentity, keys)
		}
	})
}

// TestResolveManagerForOpts_PreCheckUnprovenMatchResolvesToDefaultIdentity
// pins the authoritative post-resolution identity compare
// (auxiliaryRuntimeIdentity(resolved) == auxiliaryRuntimeIdentity(s.runtime)):
// even when the cheap pre-check cannot prove a profile matches the broker's
// own default — here, because its own kubeconfig read fails on a
// nonexistent KUBECONFIG path — full resolution landing on the SAME identity
// as the default must still return the shared manager and register nothing,
// not create a duplicate auxiliary entry for the broker's own runtime.
func TestResolveManagerForOpts_PreCheckUnprovenMatchResolvesToDefaultIdentity(t *testing.T) {
	srv := newKubernetesDefaultTestServer(t)
	srv.config.ForceRuntime = ""
	// Ensure the pre-check's in-process kubeconfig read cannot succeed via
	// an in-cluster fallback either, so it is forced to return false (not
	// a conclusive non-match) and the caller falls through to resolution.
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "does-not-exist"))

	projectDir := t.TempDir()
	writeProjectSettings(t, projectDir, `schema_version: "1"
profiles:
  unset-context:
    runtime: k8s-unset-context
runtimes:
  k8s-unset-context:
    type: kubernetes
    namespace: ns-default
`)

	srv.resolveAuxiliaryRuntime = stubResolver(t, map[string]runtime.Runtime{
		"unset-context": fakeKubernetesRuntime("cluster-default", "ns-default"),
	})

	mgr, _ := srv.resolveManagerForOpts(api.StartOptions{Name: "agent", Profile: "unset-context", ProjectPath: projectDir})

	if mgr != srv.manager {
		t.Fatalf("expected the shared manager when full resolution lands on the default's own identity, even though the pre-check could not prove it")
	}
	if keys := auxKeys(srv); len(keys) != 0 {
		t.Fatalf("expected no auxiliary runtime registered when resolution matches the default's identity, got %v", keys)
	}
}

// TestResolveManagerForOpts_FailedResolutionReturnsErrorManagerWithoutRegistering
// pins the "error" early return: when resolution fails outright (e.g. an
// unreachable cluster), resolveManagerForOpts must wrap the error runtime in
// its own manager and return immediately, rather than falling through to the
// identity compare and registering a manager under the identity "error".
// That would make every later LookupAgent/LookupContainerID/listAgents pass
// call List on a manager that always errors, turning a lookup for a
// genuinely missing agent into a listing-unavailable failure instead of a
// clean "not found".
func TestResolveManagerForOpts_FailedResolutionReturnsErrorManagerWithoutRegistering(t *testing.T) {
	srv := newKubernetesDefaultTestServer(t)
	srv.config.ForceRuntime = ""

	projectDir := t.TempDir()
	writeProjectSettings(t, projectDir, `schema_version: "1"
profiles:
  unreachable:
    runtime: k8s-unreachable
runtimes:
  k8s-unreachable:
    type: kubernetes
    context: cluster-other
    namespace: ns-other
`)

	srv.resolveAuxiliaryRuntime = func(_, _, profileFlag string) runtime.Runtime {
		return &runtime.ErrorRuntime{Err: fmt.Errorf("cluster unreachable for profile %q", profileFlag)}
	}

	mgr, runtimeType := srv.resolveManagerForOpts(api.StartOptions{Name: "agent", Profile: "unreachable", ProjectPath: projectDir})

	if runtimeType != "error" {
		t.Fatalf("expected the resolved runtime type to be \"error\", got %q", runtimeType)
	}
	if mgr == srv.manager {
		t.Fatal("expected a manager wrapping the resolution failure, not the shared manager")
	}
	if keys := auxKeys(srv); len(keys) != 0 {
		t.Fatalf("expected no auxiliary runtime registered for a failed resolution, got %v", keys)
	}
}

// TestResolveManagerForOpts_NonKubernetesDefaultSkipsResolution pins
// defaultRuntimeMatchesProfile's non-Kubernetes branch: on a Docker-default
// broker, a profile whose type matches is an identity match without further
// checks (every non-Kubernetes type uses the bare type-name identity), so it
// must return the shared manager WITHOUT calling resolveAuxiliaryRuntime.
func TestResolveManagerForOpts_NonKubernetesDefaultSkipsResolution(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "")
	cfg := DefaultServerConfig()
	srv := New(cfg, &filteringMockManager{}, &runtime.MockRuntime{NameFunc: func() string { return "docker" }})
	srv.config.ForceRuntime = ""

	projectDir := t.TempDir()
	writeProjectSettings(t, projectDir, `schema_version: "1"
profiles:
  same:
    runtime: docker-same
runtimes:
  docker-same:
    type: docker
`)

	srv.resolveAuxiliaryRuntime = func(_, _, profileFlag string) runtime.Runtime {
		return &runtime.ErrorRuntime{Err: fmt.Errorf("resolveAuxiliaryRuntime must not be called for profile %q matching the default", profileFlag)}
	}

	mgr, _ := srv.resolveManagerForOpts(api.StartOptions{Name: "agent", Profile: "same", ProjectPath: projectDir})

	if mgr != srv.manager {
		t.Fatalf("expected the shared manager for a profile matching a non-Kubernetes default's type")
	}
	if keys := auxKeys(srv); len(keys) != 0 {
		t.Fatalf("expected no auxiliary runtime registered for the default-matching profile, got %v", keys)
	}
}

// TestResolveManagerForOpts_NilClientKubernetesDefaultResolves pins
// defaultRuntimeMatchesProfile's nil-Client guard: a Kubernetes-default
// broker whose runtime has no Client (for example, one under construction,
// or stubbed without one in a test) cannot have its context/namespace
// compared, so the pre-check must not claim a match — a profile for a
// genuinely different cluster must still be fully resolved and registered.
func TestResolveManagerForOpts_NilClientKubernetesDefaultResolves(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "")
	cfg := DefaultServerConfig()
	srv := New(cfg, &filteringMockManager{}, &runtime.KubernetesRuntime{Client: nil, DefaultNamespace: "ns-default"})
	srv.config.ForceRuntime = ""

	projectDir := t.TempDir()
	writeProjectSettings(t, projectDir, `schema_version: "1"
profiles:
  other:
    runtime: k8s-other
runtimes:
  k8s-other:
    type: kubernetes
    context: cluster-other
    namespace: ns-default
`)

	other := fakeKubernetesRuntime("cluster-other", "ns-default")
	srv.resolveAuxiliaryRuntime = stubResolver(t, map[string]runtime.Runtime{
		"other": other,
	})

	mgr, _ := srv.resolveManagerForOpts(api.StartOptions{Name: "agent", Profile: "other", ProjectPath: projectDir})

	if mgr == srv.manager {
		t.Fatalf("expected a non-default manager when the default's Client is nil")
	}
	wantIdentity := auxiliaryRuntimeIdentity(other)
	keys := auxKeys(srv)
	if len(keys) != 1 || keys[0] != wantIdentity {
		t.Fatalf("expected exactly the identity %q registered, got %v", wantIdentity, keys)
	}
}
