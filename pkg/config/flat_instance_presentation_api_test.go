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
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// presentationFieldRenames maps a FlatInstancePresentation field (by its Go
// path) to the name the wire type gives it. Every other field keeps its
// name.
var presentationFieldRenames = map[string]string{
	"RuntimeTarget": "Target",
}

// presentationAPIOnlyFields are wire fields that are not FC4-a fields.
var presentationAPIOnlyFields = map[string]bool{
	"SchemaVersion": true,
}

// fieldShapes records, for every field at every depth, its path (renamed
// through renames, top-level fields in skip left out) and its shape for the
// reflection comparison:
// the kind chain down to a leaf (pointer, slice and map element kinds,
// and the leaf type for non-struct leaves).
func fieldShapes(t reflect.Type, prefix string, renames map[string]string, skip map[string]bool, out map[string]string) {
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		path := prefix + f.Name
		if prefix == "" && skip[f.Name] {
			continue
		}
		if r, ok := renames[path]; ok {
			path = r
		}
		ft := f.Type
		shape := ""
		for {
			switch ft.Kind() {
			case reflect.Pointer:
				shape += "*"
				ft = ft.Elem()
				continue
			case reflect.Slice:
				shape += "[]"
				ft = ft.Elem()
				continue
			case reflect.Map:
				shape += "map[" + ft.Key().String() + "]"
				ft = ft.Elem()
				continue
			}
			break
		}
		// A struct of this package or of the wire type is compared field by
		// field; any other leaf (string, bool, api.CloneDepth,
		// api.ResourceSpec) must be the same type on both sides.
		if ft.Kind() == reflect.Struct && (ft.PkgPath() == reflect.TypeOf(FlatInstancePresentation{}).PkgPath() ||
			strings.HasPrefix(ft.Name(), "Presented")) {
			out[path] = shape + "struct"
			fieldShapes(ft, path+".", renames, skip, out)
			continue
		}
		out[path] = shape + ft.String()
	}
}

// The wire type's field set equals FC4-a's published set, which is
// FlatInstancePresentation's field set, at every depth and with the same
// shapes (pointer, slice, map, leaf type). SchemaVersion is the only wire
// field that is not an FC4-a field.
func TestRuntimeTargetPresentation_FieldSetEqualsFC4a(t *testing.T) {
	cfg := map[string]string{}
	fieldShapes(reflect.TypeOf(FlatInstancePresentation{}), "", presentationFieldRenames, nil, cfg)
	wire := map[string]string{}
	fieldShapes(reflect.TypeOf(api.RuntimeTargetPresentation{}), "", nil, presentationAPIOnlyFields, wire)
	if !reflect.DeepEqual(cfg, wire) {
		t.Fatalf("field sets differ:\nconfig: %v\nwire:   %v", sortedKeys(cfg), sortedKeys(wire))
	}
	if len(cfg) < 30 {
		t.Fatalf("only %d fields compared; the walk did not descend: %v", len(cfg), sortedKeys(cfg))
	}
	if _, ok := reflect.TypeOf(api.RuntimeTargetPresentation{}).FieldByName("SchemaVersion"); !ok {
		t.Fatal("the wire type has no SchemaVersion")
	}
}

// presentationNeverKeys are the FC4-a "never" fields, under both their
// settings (snake_case) and wire (camelCase) spellings. env is published
// only as envKeys.
var presentationNeverKeys = []string{
	"context", "kubeconfig",
	"kubernetes_service_account_mappings", "kubernetesServiceAccountMappings",
	"kubernetes_block_service_account", "kubernetesBlockServiceAccount",
	"home_storage_leaf", "homeStorageLeaf",
	"source", "bucket", "prefix", "server", "mode", "volume_name", "volumeName",
	"alternative_env_keys", "alternativeEnvKeys", "env",
}

// collectKeys returns every object key at every depth of a decoded JSON
// value, and every secrets[] element's keys separately (a secret's target
// is never published, while a volume's target is).
func collectKeys(v interface{}, parent string, keys map[string]bool, secretKeys map[string]bool) {
	switch x := v.(type) {
	case map[string]interface{}:
		for k, e := range x {
			keys[k] = true
			collectKeys(e, k, keys, secretKeys)
		}
	case []interface{}:
		for _, e := range x {
			if m, ok := e.(map[string]interface{}); ok && parent == "secrets" {
				for k := range m {
					secretKeys[k] = true
				}
			}
			collectKeys(e, parent, keys, secretKeys)
		}
	}
}

// The wire JSON of a fully populated instance holds every published value
// and none of the "never" keys or values: no value FC4-a withholds appears
// anywhere, under any key (so a withheld value copied into a published
// field is caught too).
func TestPresentation_NeverFieldsAbsent(t *testing.T) {
	inst := fullyPopulatedInstance()
	got := PresentInstance(inst).API()
	evict := false
	want := api.RuntimeTargetPresentation{
		SchemaVersion: 1, Key: "k8s-nfs-1", Name: "k8s nfs one",
		Target: &api.PresentedTarget{
			Type: "kubernetes", DisplayName: "Cluster A", Namespace: "agents", GKE: true,
			PriorityClassName: "agents-high", SharedDirStorageClass: "standard-rwx", SharedDirSize: "40Gi",
			SafeToEvict: &evict, SharedDirStorageBackend: "nfs", SharedDirStorageBackends: map[string]string{"data": "local"},
			HomeStorageBackend: "nfs",
		},
		AgentDefaults: &api.PresentedAgentDefaults{
			DefaultHarnessConfig: "claude", ImageRegistry: "registry.example/team", CloneDepth: "full",
			Resources: &api.ResourceSpec{Requests: api.ResourceList{CPU: "1", Memory: "2Gi"},
				Limits: api.ResourceList{CPU: "2", Memory: "4Gi"}, Disk: "20Gi"},
			Volumes: []api.PresentedVolume{{Target: "/mnt/a", ReadOnly: true, Type: "nfs"}},
			Secrets: []api.PresentedSecretDecl{{Key: "API_KEY", Type: "file", Description: "the API key"}},
			HarnessOverrides: map[string]api.PresentedHarnessOverride{"claude": {
				Image: "img:1", ImagePullPolicy: "Never", AuthSelectedType: "vertex-ai", User: "agent",
				EnvKeys:   []string{"EMPTY", "TOKEN"},
				Volumes:   []api.PresentedVolume{{Target: "/mnt/b", ReadOnly: true, Type: "nfs"}},
				Resources: &api.ResourceSpec{Limits: api.ResourceList{Memory: "8Gi"}},
			}},
		},
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "NEVER") {
		t.Fatalf("a never-published value is in the wire presentation: %s", raw)
	}
	// Checked after the values, so a withheld value copied into a published
	// field fails on the value check above first.
	if !reflect.DeepEqual(got, want) {
		gj, _ := json.MarshalIndent(got, "", "  ")
		t.Fatalf("wire presentation:\n%s", gj)
	}
	var decoded interface{}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	keys, secretKeys := map[string]bool{}, map[string]bool{}
	collectKeys(decoded, "", keys, secretKeys)
	for _, k := range presentationNeverKeys {
		if keys[k] {
			t.Errorf("never-published key %q is in the wire presentation: %s", k, raw)
		}
	}
	if secretKeys["target"] {
		t.Errorf("a secret's target is in the wire presentation: %s", raw)
	}
	if !secretKeys["key"] {
		t.Fatalf("the secrets walk saw no secret: %s", raw)
	}
	// The source holds every withheld value (TestPresentInstance_RedactsEveryFC4aRow
	// counts them), so the checks above are not vacuous.
	if src, _ := json.Marshal(inst); strings.Count(string(src), "NEVER-") != 21 {
		t.Fatalf("the source does not hold the 21 never-published values: %s", src)
	}
}

// safe_to_evict keeps unset, false and true distinct through the wire type
// and its JSON (the transport).
func TestPresentation_SafeToEvictTriStateOnTheWire(t *testing.T) {
	tr, fa := true, false
	for name, v := range map[string]*bool{"unset": nil, "true": &tr, "false": &fa} {
		p := PresentInstance(V1RuntimeBrokerInstanceConfig{Key: "a", Name: "a",
			RuntimeTarget: &V1RuntimeTargetConfig{Type: "kubernetes", SafeToEvict: v}}).API()
		raw, err := json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		var back api.RuntimeTargetPresentation
		if err := json.Unmarshal(raw, &back); err != nil {
			t.Fatal(err)
		}
		got := back.Target.SafeToEvict
		if (got == nil) != (v == nil) || (got != nil && *got != *v) {
			t.Errorf("%s: got %v after the round trip (%s)", name, got, raw)
		}
		if v == nil && strings.Contains(string(raw), "safeToEvict") {
			t.Errorf("unset: safeToEvict is on the wire: %s", raw)
		}
	}
}

// API shares no map, slice or pointer with the presentation it copies, and
// a presentation without blocks has none on the wire.
func TestPresentation_APISharesNothing(t *testing.T) {
	p := PresentInstance(fullyPopulatedInstance())
	got := p.API()
	before, _ := json.Marshal(got)
	p.RuntimeTarget.SharedDirStorageBackends["data"] = "nfs"
	*p.RuntimeTarget.SafeToEvict = true
	p.AgentDefaults.Resources.Requests.CPU = "9"
	p.AgentDefaults.Volumes[0].Target = "/changed"
	p.AgentDefaults.Secrets[0].Key = "CHANGED"
	ho := p.AgentDefaults.HarnessOverrides["claude"]
	ho.EnvKeys[0] = "CHANGED"
	ho.Resources.Limits.Memory = "1Mi"
	ho.Volumes[0].Target = "/changed"
	if after, _ := json.Marshal(got); string(after) != string(before) {
		t.Fatalf("changing the presentation changed the wire copy:\n%s\n%s", before, after)
	}
	empty := FlatInstancePresentation{Key: "a", Name: "b"}.API()
	if empty.Target != nil || empty.AgentDefaults != nil || empty.SchemaVersion != api.RuntimeTargetPresentationSchemaVersion {
		t.Fatalf("got %+v", empty)
	}
}

// TestPresentInstance_ImageRegistryRedactedForDisplay: presented agent
// defaults use the same image-registry display redaction as the broker
// record (RedactImageRegistry), in the published form and on the wire; the
// configuration keeps the configured value, and a registry without userinfo
// is presented unchanged.
func TestPresentInstance_ImageRegistryRedactedForDisplay(t *testing.T) {
	const configured = "https://user:s3cret@registry.example/team"
	const want = "https://***@registry.example/team"
	inst := V1RuntimeBrokerInstanceConfig{
		Key: "k", Name: "n",
		RuntimeTarget: &V1RuntimeTargetConfig{Type: "docker"},
		AgentDefaults: &V1InstanceAgentDefaults{ImageRegistry: configured},
	}
	p := PresentInstance(inst)
	if p.AgentDefaults == nil {
		t.Fatalf("no presented agent defaults")
	}
	if got := p.AgentDefaults.ImageRegistry; got != want {
		t.Errorf("presented image registry = %q, want %q (the broker record's display redaction)", got, want)
	}
	raw, err := json.Marshal(p.API())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), "s3cret") {
		t.Errorf("the wire presentation carries the registry's userinfo: %s", raw)
	}
	if inst.AgentDefaults.ImageRegistry != configured {
		t.Errorf("configured image registry = %q, want it unchanged (%q)", inst.AgentDefaults.ImageRegistry, configured)
	}
	inst.AgentDefaults.ImageRegistry = "registry.example/team"
	if got := PresentInstance(inst).AgentDefaults.ImageRegistry; got != "registry.example/team" {
		t.Errorf("presented image registry without userinfo = %q, want it unchanged", got)
	}
}
