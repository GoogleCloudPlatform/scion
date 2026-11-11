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
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// The runtime_target infrastructure fields of a flat instance entry: gke,
// priority_class_name, safe_to_evict
// (.design/flat-runtime-brokers/contract-appendix-p3.md FC1, FC2, FC5,
// FC7). The shared-dir and home storage overrides are refused:
// server.project_storage decides placement.

const targetInfraInstancesYAML = `schema_version: "1"
server:
  broker:
    instances:
      - key: k8s-nfs-1
        name: k8s-nfs-1
        runtime_target:
          type: kubernetes
          namespace: agents
          gke: true
          priority_class_name: agents-high
          safe_to_evict: false
          kubernetes_service_account_mappings:
            agent@proj.iam.gserviceaccount.com: ksa-agent
      - key: docker-1
        name: docker-1
        runtime_target:
          type: docker
`

// Every field loads strictly, mirrors into the server config, converts back
// unchanged and passes the startup deep-equal comparison. An explicit
// safe_to_evict false stays false (not unset) through every form.
func TestRuntimeBrokerInstances_TargetInfraFieldsRoundTrip(t *testing.T) {
	global := flatTestHome(t)
	writeFlatTestFile(t, filepath.Join(global, "settings.yaml"), targetInfraInstancesYAML)
	strict, err := LoadRuntimeBrokerInstances("")
	if err != nil {
		t.Fatal(err)
	}
	mappings := map[string]string{"agent@proj.iam.gserviceaccount.com": "ksa-agent"}
	wantK8s := &V1RuntimeTargetConfig{Type: "kubernetes", Namespace: "agents", GKE: true, PriorityClassName: "agents-high",
		SafeToEvict: boolPtr(false), KubernetesServiceAccountMappings: mappings}
	wantDocker := &V1RuntimeTargetConfig{Type: "docker"}
	if len(strict) != 2 || !reflect.DeepEqual(strict[0].RuntimeTarget, wantK8s) || !reflect.DeepEqual(strict[1].RuntimeTarget, wantDocker) {
		t.Fatalf("strict load: got %+v / %+v", strict[0].RuntimeTarget, strict[1].RuntimeTarget)
	}
	gc, err := LoadGlobalConfig("")
	if err != nil {
		t.Fatal(err)
	}
	wantGlobal := &RuntimeTargetConfig{Type: "kubernetes", Namespace: "agents", GKE: true, PriorityClassName: "agents-high",
		SafeToEvict: boolPtr(false), KubernetesServiceAccountMappings: mappings}
	if len(gc.RuntimeBroker.Instances) != 2 || !reflect.DeepEqual(gc.RuntimeBroker.Instances[0].RuntimeTarget, wantGlobal) {
		t.Fatalf("server config mirror: got %+v", gc.RuntimeBroker.Instances)
	}
	if !reflect.DeepEqual(RuntimeBrokerInstancesToGlobal(strict), gc.RuntimeBroker.Instances) {
		t.Fatal("the startup comparison must find the strict and lenient results equal")
	}
	back := ConvertGlobalToV1ServerConfig(gc)
	if !reflect.DeepEqual(back.Broker.Instances, strict) {
		t.Fatalf("back-conversion differs: %+v", back.Broker.Instances[0].RuntimeTarget)
	}
	// The conversions copy the reference fields.
	*gc.RuntimeBroker.Instances[0].RuntimeTarget.SafeToEvict = true
	gc.RuntimeBroker.Instances[0].RuntimeTarget.KubernetesServiceAccountMappings["agent@proj.iam.gserviceaccount.com"] = "other"
	if *back.Broker.Instances[0].RuntimeTarget.SafeToEvict ||
		back.Broker.Instances[0].RuntimeTarget.KubernetesServiceAccountMappings["agent@proj.iam.gserviceaccount.com"] != "ksa-agent" {
		t.Fatal("the settings and server-config forms share a pointer or map")
	}
}

// safe_to_evict is tri-state at load: absent and null are unset, false and
// true are kept as written.
func TestRuntimeBrokerInstances_SafeToEvictTriState(t *testing.T) {
	for line, want := range map[string]*bool{"": nil, "safe_to_evict: null": nil, "safe_to_evict: false": boolPtr(false), "safe_to_evict: true": boolPtr(true)} {
		t.Run(line, func(t *testing.T) {
			global := flatTestHome(t)
			writeFlatTestFile(t, filepath.Join(global, "settings.yaml"), instancesYAML("- key: k\n        name: k\n        runtime_target:\n          type: kubernetes\n          "+line))
			got, err := LoadRuntimeBrokerInstances("")
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got[0].RuntimeTarget.SafeToEvict, want) {
				t.Fatalf("safe_to_evict: got %v, want %v", got[0].RuntimeTarget.SafeToEvict, want)
			}
		})
	}
}

// Docker refuses the Kubernetes-only fields, including an explicit false
// safe_to_evict. Kubernetes validates each value, naming its path. Both
// refuse the removed storage overrides, pointing at a volumes: entry.
func TestValidateRuntimeBrokerInstances_TargetInfraFields(t *testing.T) {
	docker := dockerInstance("d", "d")
	docker.RuntimeTarget.GKE = true
	docker.RuntimeTarget.PriorityClassName = "high"
	docker.RuntimeTarget.SafeToEvict = boolPtr(false)
	errs := ValidateRuntimeBrokerInstances([]V1RuntimeBrokerInstanceConfig{docker})
	var paths []string
	for _, e := range errs {
		if e.Message != "field not valid for runtime target type docker" {
			t.Fatalf("unexpected docker error %v", e)
		}
		paths = append(paths, strings.TrimPrefix(e.Path, "server.broker.instances[0].runtime_target."))
	}
	if want := []string{"gke", "priority_class_name", "safe_to_evict"}; !reflect.DeepEqual(paths, want) {
		t.Fatalf("docker refusals: got %v, want %v", paths, want)
	}

	for _, typ := range []string{RuntimeTargetTypeDocker, RuntimeTargetTypeKubernetes} {
		t.Run("removed overrides "+typ, func(t *testing.T) {
			target := &V1RuntimeTargetConfig{Type: typ, SharedDirStorageBackend: "local",
				SharedDirStorageBackends: map[string]string{"cache": "local"}, HomeStorageBackend: "local", HomeStorageLeaf: "pod"}
			errs := ValidateRuntimeBrokerInstances([]V1RuntimeBrokerInstanceConfig{{Key: "k", Name: "k", RuntimeTarget: target}})
			var paths []string
			for _, e := range errs {
				if !strings.Contains(e.Message, "is no longer supported") || !strings.Contains(e.Message, "volumes:") {
					t.Fatalf("unexpected error %v", e)
				}
				paths = append(paths, strings.TrimPrefix(e.Path, "server.broker.instances[0].runtime_target."))
			}
			want := []string{"home_storage_backend", "home_storage_leaf", "shared_dir_storage_backend", "shared_dir_storage_backends"}
			if !reflect.DeepEqual(paths, want) {
				t.Fatalf("removed override refusals: got %v, want %v", paths, want)
			}
		})
	}

	k8s := func(mut func(*V1RuntimeTargetConfig)) []ValidationError {
		t := &V1RuntimeTargetConfig{Type: "kubernetes"}
		mut(t)
		return ValidateRuntimeBrokerInstances([]V1RuntimeBrokerInstanceConfig{{Key: "k", Name: "k", RuntimeTarget: t}})
	}
	if errs := k8s(func(t *V1RuntimeTargetConfig) { t.PriorityClassName = "High_Priority" }); len(errs) != 1 ||
		errs[0].Path != "server.broker.instances[0].runtime_target.priority_class_name" {
		t.Fatalf("got %v, want one error at priority_class_name", errs)
	}
	if errs := k8s(func(t *V1RuntimeTargetConfig) {
		t.GKE, t.PriorityClassName, t.SafeToEvict = true, "agents.high", boolPtr(true)
	}); len(errs) != 0 {
		t.Fatalf("valid entry refused: %v", errs)
	}
}

func TestRuntimeBrokerTarget_KubernetesRuntimeConfig(t *testing.T) {
	target := &V1RuntimeTargetConfig{Type: "kubernetes", Context: "ctx", Namespace: "agents", Kubeconfig: "/etc/kc",
		GKE: true, PriorityClassName: "agents-high", SharedDirSize: "40Gi"}
	want := V1RuntimeConfig{Type: "kubernetes", Context: "ctx", Namespace: "agents", GKE: true, PriorityClassName: "agents-high"}
	if got := target.KubernetesRuntimeConfig(); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

// --- Flat branches and fall-through (FC5) ---

// targetLegacySettings has an active profile and a runtime entry that set
// every target infrastructure field; none of it may reach a flat view.
func targetLegacySettings() *VersionedSettings {
	return &VersionedSettings{
		ActiveProfile: "batch",
		Runtimes: map[string]V1RuntimeConfig{"cluster": {Type: "kubernetes", SafeToEvict: boolPtr(true),
			SharedDirStorageBackend: "nfs", SharedDirStorageBackends: map[string]string{"data": "nfs"},
			HomeStorageBackend: "nfs", HomeStorageLeaf: "broker"}},
		Profiles: map[string]V1ProfileConfig{"batch": {Runtime: "cluster", SafeToEvict: boolPtr(true),
			SharedDirStorageBackend: "nfs", SharedDirStorageBackends: map[string]string{"data": "nfs"},
			HomeStorageBackend: "nfs", HomeStorageLeaf: "broker"}},
	}
}

func targetPolicy(t V1RuntimeTargetConfig) *FlatInstancePolicy {
	t.Type = RuntimeTargetTypeKubernetes
	return NewFlatInstancePolicy(V1RuntimeBrokerInstanceConfig{Key: "k8s-1", Name: "k8s-1", RuntimeTarget: &t})
}

const instPath = "server.broker.instances[k8s-1].runtime_target."

func TestResolveSafeToEvict_FlatBranch(t *testing.T) {
	vs := targetLegacySettings()
	for _, profile := range []string{"", "batch"} {
		v, src := vs.ForFlatInstance(targetPolicy(V1RuntimeTargetConfig{SafeToEvict: boolPtr(false)})).ResolveSafeToEvictWithSource(profile)
		if v == nil || *v || src != instPath+"safe_to_evict" {
			t.Fatalf("profile %q: an explicit false must be kept: got %v %q", profile, v, src)
		}
	}
	p := targetPolicy(V1RuntimeTargetConfig{SafeToEvict: boolPtr(false)})
	v, _ := vs.ForFlatInstance(p).ResolveSafeToEvictWithSource("")
	*v = true
	if *p.Target.SafeToEvict {
		t.Fatal("the resolver must return a copy, never the policy's pointer")
	}
	if v, _ := vs.ForFlatInstance(targetPolicy(V1RuntimeTargetConfig{SafeToEvict: boolPtr(true)})).ResolveSafeToEvictWithSource(""); v == nil || !*v {
		t.Fatalf("an explicit true must be kept: got %v", v)
	}
	// Fall-through: there is no settings rung below the profile and
	// runtime entry, so unset is nil (not false, not the profile's true).
	if v, src := vs.ForFlatInstance(targetPolicy(V1RuntimeTargetConfig{})).ResolveSafeToEvictWithSource("batch"); v != nil || src != "" {
		t.Fatalf("unset: got %v %q, want nil", v, src)
	}
	if v, src := vs.ResolveSafeToEvictWithSource(""); v == nil || !*v || src != "profiles.batch.safe_to_evict" {
		t.Fatalf("legacy resolution changed: %v %q", v, src)
	}
}
