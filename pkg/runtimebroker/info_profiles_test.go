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
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

func writeHomeSettings(t *testing.T, settingsYAML string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	scionDir := filepath.Join(home, ".scion")
	if err := os.MkdirAll(scionDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scionDir, "settings.yaml"), []byte(settingsYAML), 0644); err != nil {
		t.Fatal(err)
	}
}

func infoProfilesByName(profiles []BrokerProfile) map[string]BrokerProfile {
	m := make(map[string]BrokerProfile, len(profiles))
	for _, p := range profiles {
		m[p.Name] = p
	}
	return m
}

// A profile whose runtime entry is keyed differently from its type is
// advertised as the resolved type, with that entry's context/namespace;
// the local-only filter also applies to the resolved type, not the key.
func TestBuildInfoProfiles_RuntimeKeyDiffersFromType_AdvertisesResolvedType(t *testing.T) {
	writeHomeSettings(t, `schema_version: "1"
active_profile: staging
runtimes:
  noattach-nip:
    type: noattach
  k8s-staging:
    type: kubernetes
    context: staging-ctx
    namespace: staging-ns
  workstation-docker:
    type: docker
profiles:
  noattach-nip:
    runtime: noattach-nip
  staging:
    runtime: k8s-staging
  workstation:
    runtime: workstation-docker
`)

	byName := infoProfilesByName((&Server{}).buildInfoProfiles("kubernetes"))

	if p, ok := byName["noattach-nip"]; !ok || p.Type != "noattach" {
		t.Errorf("noattach-nip = %+v (present=%v), want Type %q", p, ok, "noattach")
	}
	p, ok := byName["staging"]
	if !ok || p.Type != "kubernetes" || p.Context != "staging-ctx" || p.Namespace != "staging-ns" {
		t.Errorf("staging = %+v (present=%v), want Type kubernetes, Context staging-ctx, Namespace staging-ns", p, ok)
	}
	if p, ok := byName["workstation"]; ok {
		t.Errorf("workstation (type docker, key workstation-docker) = %+v, want it filtered out on a non-local broker", p)
	}
}

// Settings whose runtime keys equal their types advertise exactly what
// they did before: the key is the type.
func TestBuildInfoProfiles_RuntimeKeyEqualsType_Unchanged(t *testing.T) {
	writeHomeSettings(t, `schema_version: "1"
active_profile: local
runtimes:
  docker:
    type: docker
  kubernetes:
    type: kubernetes
    context: kctx
    namespace: kns
profiles:
  local:
    runtime: docker
  remote:
    runtime: kubernetes
  unnamed: {}
  dangling:
    runtime: no-such-runtime
`)

	byName := infoProfilesByName((&Server{}).buildInfoProfiles("docker"))

	// s.runtime is nil on this zero-value Server, and the auxiliary-runtime
	// map is empty, so resolveLiveRuntimeInstance finds no live instance for
	// any profile here — Attach is unknown (nil) for all of them, not a
	// guessed true. See TestBuildInfoProfiles_DefaultTypeProfile_AsksLiveInstance
	// below for the case where a live instance actually answers.
	want := map[string]BrokerProfile{
		"local":    {Name: "local", Type: "docker", Available: true},
		"remote":   {Name: "remote", Type: "kubernetes", Available: true, Context: "kctx", Namespace: "kns"},
		"unnamed":  {Name: "unnamed", Type: "docker", Available: true},
		"dangling": {Name: "dangling", Type: "no-such-runtime", Available: true},
	}
	for name, w := range want {
		if got, ok := byName[name]; !ok || got != w {
			t.Errorf("%s = %+v (present=%v), want %+v", name, got, ok, w)
		}
	}
}

// TestBuildInfoProfiles_DefaultTypeProfile_AsksLiveInstance proves the
// resolver actually asks a live instance rather than just reporting
// unknown: a default runtime that opts out of attach makes the matching
// profile's Attach explicitly &false, while a differently-typed profile —
// no live instance backs it on this Server — stays nil (unknown, per
// resolveLiveRuntimeInstance).
func TestBuildInfoProfiles_DefaultTypeProfile_AsksLiveInstance(t *testing.T) {
	writeHomeSettings(t, `schema_version: "1"
active_profile: local
runtimes:
  docker:
    type: docker
  kubernetes:
    type: kubernetes
profiles:
  local:
    runtime: docker
  remote:
    runtime: kubernetes
`)

	rt := &attachCapableTestRuntime{
		MockRuntime:    &runtime.MockRuntime{NameFunc: func() string { return "docker" }},
		supportsAttach: false,
	}
	srv := &Server{runtime: rt}
	byName := infoProfilesByName(srv.buildInfoProfiles("docker"))

	local, ok := byName["local"]
	if !ok || local.Attach == nil || *local.Attach {
		t.Errorf("local (default type, opted out) = %+v, want Attach=&false", local)
	}
	remote, ok := byName["remote"]
	if !ok || remote.Attach != nil {
		t.Errorf("remote (different type, no live instance on this Server) = %+v, want Attach=nil", remote)
	}
}
