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
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// The FC4-a rows (.design/flat-runtime-brokers/contract-appendix-p3.md),
// one entry per field of each source struct, by its settings (yaml) name:
// "as is" fields are published unchanged, "never" fields are never
// published, and the other forms are partial.
const (
	fc4AsIs     = "as is"
	fc4Never    = "never"
	fc4Partial  = "partial"   // a list or map whose elements are presented field by field
	fc4KeysOnly = "keys only" // a map published as its keys only
)

var fc4aRows = map[string]map[string]string{
	"instance": {
		"key": fc4AsIs, "name": fc4AsIs, "runtime_target": fc4Partial, "agent_defaults": fc4Partial,
	},
	"runtime_target": {
		"type": fc4AsIs, "display_name": fc4AsIs, "namespace": fc4AsIs,
		"context": fc4Never, "kubeconfig": fc4Never,
		"kubernetes_service_account_mappings": fc4Never, "kubernetes_block_service_account": fc4Never,
		"gke": fc4AsIs, "priority_class_name": fc4AsIs, "shared_dir_storage_class": fc4AsIs,
		"shared_dir_size": fc4AsIs, "safe_to_evict": fc4AsIs,
		"shared_dir_storage_backend": fc4AsIs, "shared_dir_storage_backends": fc4AsIs,
		"home_storage_backend": fc4AsIs, "home_storage_leaf": fc4Never,
	},
	"agent_defaults": {
		"default_harness_config": fc4AsIs, "image_registry": fc4AsIs, "clone_depth": fc4AsIs,
		"resources": fc4AsIs, "volumes": fc4Partial, "secrets": fc4Partial, "harness_overrides": fc4Partial,
	},
	"volume": {
		"target": fc4AsIs, "read_only": fc4AsIs, "type": fc4AsIs,
		"source": fc4Never, "bucket": fc4Never, "prefix": fc4Never, "server": fc4Never,
		"mode": fc4Never, "volume_name": fc4Never,
	},
	"secret": {
		"key": fc4AsIs, "type": fc4AsIs, "description": fc4AsIs,
		"target": fc4Never, "alternative_env_keys": fc4Never,
	},
	"harness_override": {
		"image": fc4AsIs, "image_pull_policy": fc4AsIs, "auth_selected_type": fc4AsIs, "user": fc4AsIs,
		"env": fc4KeysOnly, "volumes": fc4Partial, "resources": fc4AsIs,
	},
}

// fc4aSources and fc4aPresentations name the source struct of each row
// group and the presentation struct that publishes it.
var (
	fc4aSources = map[string]reflect.Type{
		"instance":         reflect.TypeOf(V1RuntimeBrokerInstanceConfig{}),
		"runtime_target":   reflect.TypeOf(V1RuntimeTargetConfig{}),
		"agent_defaults":   reflect.TypeOf(V1InstanceAgentDefaults{}),
		"volume":           reflect.TypeOf(api.VolumeMount{}),
		"secret":           reflect.TypeOf(api.RequiredSecret{}),
		"harness_override": reflect.TypeOf(V1HarnessOverride{}),
	}
	fc4aPresentations = map[string]reflect.Type{
		"instance":         reflect.TypeOf(FlatInstancePresentation{}),
		"runtime_target":   reflect.TypeOf(FlatRuntimeTargetPresentation{}),
		"agent_defaults":   reflect.TypeOf(FlatAgentDefaultsPresentation{}),
		"volume":           reflect.TypeOf(FlatVolumePresentation{}),
		"secret":           reflect.TypeOf(FlatSecretPresentation{}),
		"harness_override": reflect.TypeOf(FlatHarnessOverridePresentation{}),
	}
)

func tagNames(t reflect.Type, tag string) []string {
	var out []string
	for i := 0; i < t.NumField(); i++ {
		name := strings.Split(t.Field(i).Tag.Get(tag), ",")[0]
		if name != "" && name != "-" {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// Every field of every source struct has an FC4-a row, so a field added to
// the instance schema later fails this test until the table classifies it.
// And the presentation types carry exactly the published fields: no "never"
// field has a place to go.
func TestPresentInstance_EveryFieldHasAnFC4aRow(t *testing.T) {
	for group, src := range fc4aSources {
		rows := fc4aRows[group]
		var classified []string
		for name := range rows {
			classified = append(classified, name)
		}
		sort.Strings(classified)
		if got := tagNames(src, "yaml"); !reflect.DeepEqual(got, classified) {
			t.Errorf("%s: source fields %v, FC4-a rows %v", group, got, classified)
		}
		var published []string
		for name, form := range rows {
			switch form {
			case fc4Never:
			case fc4KeysOnly:
				published = append(published, name+"_keys")
			default:
				published = append(published, name)
			}
		}
		sort.Strings(published)
		if got := tagNames(fc4aPresentations[group], "json"); !reflect.DeepEqual(got, published) {
			t.Errorf("%s: presentation fields %v, published rows %v", group, got, published)
		}
	}
}

// fullyPopulatedInstance sets every field of an instance entry. Each value
// FC4-a never publishes contains "NEVER-", and so does every env value.
func fullyPopulatedInstance() V1RuntimeBrokerInstanceConfig {
	evict := false
	vol := func(tag string) api.VolumeMount {
		return api.VolumeMount{Source: "NEVER-source-" + tag, Target: "/mnt/" + tag, ReadOnly: true, Type: "nfs",
			Bucket: "NEVER-bucket-" + tag, Prefix: "NEVER-prefix-" + tag, Mode: "NEVER-mode-" + tag,
			Server: "NEVER-server-" + tag, VolumeName: "NEVER-volume-name-" + tag}
	}
	return V1RuntimeBrokerInstanceConfig{
		Key:  "k8s-nfs-1",
		Name: "k8s nfs one",
		RuntimeTarget: &V1RuntimeTargetConfig{
			Type: "kubernetes", DisplayName: "Cluster A", Namespace: "agents",
			Context: "NEVER-context", Kubeconfig: "/NEVER-kubeconfig",
			KubernetesBlockServiceAccount:    "NEVER-block-sa",
			KubernetesServiceAccountMappings: map[string]string{"NEVER-gsa@example.iam.gserviceaccount.com": "NEVER-ksa"},
			GKE:                              true, PriorityClassName: "agents-high",
			SharedDirStorageClass: "standard-rwx", SharedDirSize: "40Gi", SafeToEvict: &evict,
			SharedDirStorageBackend: "nfs", SharedDirStorageBackends: map[string]string{"data": "local"},
			HomeStorageBackend: "nfs", HomeStorageLeaf: "NEVER-leaf",
		},
		AgentDefaults: &V1InstanceAgentDefaults{
			DefaultHarnessConfig: "claude", ImageRegistry: "registry.example/team", CloneDepth: "full",
			Resources: &api.ResourceSpec{Requests: api.ResourceList{CPU: "1", Memory: "2Gi"},
				Limits: api.ResourceList{CPU: "2", Memory: "4Gi"}, Disk: "20Gi"},
			Volumes: []api.VolumeMount{vol("a")},
			Secrets: []api.RequiredSecret{{Key: "API_KEY", Type: "file", Description: "the API key",
				Target: "NEVER-target", AlternativeEnvKeys: []string{"NEVER-ALT"}}},
			HarnessOverrides: map[string]V1HarnessOverride{"claude": {
				Image: "img:1", ImagePullPolicy: "Never", AuthSelectedType: "vertex-ai", User: "agent",
				Env:       map[string]string{"TOKEN": "NEVER-env-value", "EMPTY": ""},
				Volumes:   []api.VolumeMount{vol("b")},
				Resources: &api.ResourceSpec{Limits: api.ResourceList{Memory: "8Gi"}},
			}},
		},
	}
}

// The redaction test covers every FC4-a row: each "as is" value is
// published unchanged, each partial form keeps exactly its listed parts,
// and no "never" value appears anywhere in the result.
func TestPresentInstance_RedactsEveryFC4aRow(t *testing.T) {
	inst := fullyPopulatedInstance()
	got := PresentInstance(inst)
	evict := false
	want := FlatInstancePresentation{
		Key: "k8s-nfs-1", Name: "k8s nfs one",
		RuntimeTarget: &FlatRuntimeTargetPresentation{
			Type: "kubernetes", DisplayName: "Cluster A", Namespace: "agents", GKE: true,
			PriorityClassName: "agents-high", SharedDirStorageClass: "standard-rwx", SharedDirSize: "40Gi",
			SafeToEvict: &evict, SharedDirStorageBackend: "nfs", SharedDirStorageBackends: map[string]string{"data": "local"},
			HomeStorageBackend: "nfs",
		},
		AgentDefaults: &FlatAgentDefaultsPresentation{
			DefaultHarnessConfig: "claude", ImageRegistry: "registry.example/team", CloneDepth: "full",
			Resources: &api.ResourceSpec{Requests: api.ResourceList{CPU: "1", Memory: "2Gi"},
				Limits: api.ResourceList{CPU: "2", Memory: "4Gi"}, Disk: "20Gi"},
			Volumes: []FlatVolumePresentation{{Target: "/mnt/a", ReadOnly: true, Type: "nfs"}},
			Secrets: []FlatSecretPresentation{{Key: "API_KEY", Type: "file", Description: "the API key"}},
			HarnessOverrides: map[string]FlatHarnessOverridePresentation{"claude": {
				Image: "img:1", ImagePullPolicy: "Never", AuthSelectedType: "vertex-ai", User: "agent",
				EnvKeys:   []string{"EMPTY", "TOKEN"},
				Volumes:   []FlatVolumePresentation{{Target: "/mnt/b", ReadOnly: true, Type: "nfs"}},
				Resources: &api.ResourceSpec{Limits: api.ResourceList{Memory: "8Gi"}},
			}},
		},
	}
	if !reflect.DeepEqual(got, want) {
		gj, _ := json.MarshalIndent(got, "", "  ")
		t.Fatalf("presentation:\n%s", gj)
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "NEVER") {
		t.Fatalf("a never-published value is in the presentation: %s", raw)
	}
	// The source really holds every never-published value, so the check
	// above is not vacuous.
	src, _ := json.Marshal(inst)
	if n := strings.Count(string(src), "NEVER-"); n != 21 {
		t.Fatalf("the source holds %d never-published values, want 21: %s", n, src)
	}
}

// safe_to_evict is published as a tri-state: unset, true and false stay
// distinct.
func TestPresentInstance_SafeToEvictTriState(t *testing.T) {
	tr, fa := true, false
	for name, v := range map[string]*bool{"unset": nil, "true": &tr, "false": &fa} {
		got := PresentInstance(V1RuntimeBrokerInstanceConfig{Key: "a", Name: "a",
			RuntimeTarget: &V1RuntimeTargetConfig{Type: "kubernetes", SafeToEvict: v}}).RuntimeTarget.SafeToEvict
		if (got == nil) != (v == nil) || (got != nil && *got != *v) {
			t.Errorf("%s: got %v", name, got)
		}
		if v != nil && got == v {
			t.Errorf("%s: the pointer is shared with the source", name)
		}
	}
}

// The presentation shares no map, slice or pointer with the source, and an
// entry without blocks presents none.
func TestPresentInstance_SharesNothingWithSource(t *testing.T) {
	inst := fullyPopulatedInstance()
	got := PresentInstance(inst)
	before, _ := json.Marshal(got)
	inst.RuntimeTarget.SharedDirStorageBackends["data"] = "nfs"
	*inst.RuntimeTarget.SafeToEvict = true
	inst.AgentDefaults.Resources.Requests.CPU = "9"
	inst.AgentDefaults.Volumes[0].Target = "/changed"
	ho := inst.AgentDefaults.HarnessOverrides["claude"]
	ho.Resources.Limits.Memory = "1Mi"
	ho.Volumes[0].Target = "/changed"
	if after, _ := json.Marshal(got); string(after) != string(before) {
		t.Fatalf("changing the source changed the presentation:\n%s\n%s", before, after)
	}
	if p := PresentInstance(V1RuntimeBrokerInstanceConfig{Key: "a", Name: "b"}); p.RuntimeTarget != nil || p.AgentDefaults != nil {
		t.Fatalf("got %+v", p)
	}
}
