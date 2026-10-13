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

// runtime_target.shared_dir_storage_class / shared_dir_size on a flat
// instance entry, and the strict loader's key rules
// (.design/flat-runtime-brokers/contract-appendix-p3.md FC1, FC2, FC7).

const k8sSharedDirInstanceYAML = `schema_version: "1"
server:
  broker:
    instances:
      - key: k8s-nfs-1
        name: k8s-nfs-1
        runtime_target:
          type: kubernetes
          namespace: agents
          shared_dir_storage_class: standard-rwx
          shared_dir_size: 40Gi
`

// The new fields load strictly, mirror into the server config, convert
// back unchanged and pass the startup deep-equal comparison.
func TestRuntimeBrokerInstances_SharedDirFieldsRoundTrip(t *testing.T) {
	global := flatTestHome(t)
	writeFlatTestFile(t, filepath.Join(global, "settings.yaml"), k8sSharedDirInstanceYAML)
	strict, err := LoadRuntimeBrokerInstances("")
	if err != nil {
		t.Fatal(err)
	}
	wantTarget := &V1RuntimeTargetConfig{Type: "kubernetes", Namespace: "agents",
		SharedDirStorageClass: "standard-rwx", SharedDirSize: "40Gi"}
	if len(strict) != 1 || !reflect.DeepEqual(strict[0].RuntimeTarget, wantTarget) {
		t.Fatalf("strict load: got %+v", strict)
	}
	gc, err := LoadGlobalConfig("")
	if err != nil {
		t.Fatal(err)
	}
	wantGlobal := []RuntimeBrokerInstanceConfig{{Key: "k8s-nfs-1", Name: "k8s-nfs-1", RuntimeTarget: &RuntimeTargetConfig{
		Type: "kubernetes", Namespace: "agents", SharedDirStorageClass: "standard-rwx", SharedDirSize: "40Gi"}}}
	if !reflect.DeepEqual(gc.RuntimeBroker.Instances, wantGlobal) {
		t.Fatalf("server config mirror: got %+v", gc.RuntimeBroker.Instances[0].RuntimeTarget)
	}
	if !reflect.DeepEqual(RuntimeBrokerInstancesToGlobal(strict), gc.RuntimeBroker.Instances) {
		t.Fatal("the startup comparison must find the strict and lenient results equal")
	}
	if back := ConvertGlobalToV1ServerConfig(gc); !reflect.DeepEqual(back.Broker.Instances, strict) {
		t.Fatalf("back-conversion differs: %+v", back.Broker.Instances[0].RuntimeTarget)
	}
}

// Every frozen key that this release does not apply yet is refused with an
// explicit error naming its settings path, never dropped.
func TestRuntimeBrokerInstances_StrictLoaderRefusesReservedKeys(t *testing.T) {
	// Every frozen runtime_target key is wired, so none is reserved.
	cases := map[string]string{}
	if len(cases) != len(reservedRuntimeTargetKeys) {
		t.Fatalf("the test covers %d runtime_target keys, the reserved list has %d", len(cases), len(reservedRuntimeTargetKeys))
	}
	for path, line := range cases {
		t.Run(path, func(t *testing.T) {
			global := flatTestHome(t)
			writeFlatTestFile(t, filepath.Join(global, "settings.yaml"),
				strings.Replace(k8sSharedDirInstanceYAML, "          shared_dir_size: 40Gi\n", "          shared_dir_size: 40Gi\n          "+line+"\n", 1))
			_, err := LoadRuntimeBrokerInstances("")
			want := "server.broker.instances[0]." + path + ": this key is reserved for flat Runtime Broker instances but not supported in this release"
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("got %v, want an error containing %q", err, want)
			}
		})
	}
}

// An unknown key on an entry or under runtime_target fails the strict load
// with its settings path.
func TestRuntimeBrokerInstances_StrictLoaderUnknownKeyNamesPath(t *testing.T) {
	for name, tc := range map[string]struct{ yaml, want string }{
		"runtime_target": {
			strings.Replace(k8sSharedDirInstanceYAML, "          namespace: agents\n", "          namespace: agents\n          shared_dir_sise: 40Gi\n", 1),
			"server.broker.instances[0].runtime_target.shared_dir_sise: unknown key"},
		"entry": {
			k8sSharedDirInstanceYAML + "        profile: batch\n",
			"server.broker.instances[0].profile: unknown key"},
		"second entry": {
			k8sSharedDirInstanceYAML + "      - key: d\n        name: d\n        runtime_target: {type: docker, bogus: x}\n",
			"server.broker.instances[1].runtime_target.bogus: unknown key"},
	} {
		t.Run(name, func(t *testing.T) {
			global := flatTestHome(t)
			writeFlatTestFile(t, filepath.Join(global, "settings.yaml"), tc.yaml)
			_, err := LoadRuntimeBrokerInstances("")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want an error containing %q", err, tc.want)
			}
		})
	}
}

// Docker refuses the Kubernetes-only shared-dir defaults; Kubernetes
// validates the size as a positive quantity, naming the path.
func TestValidateRuntimeBrokerInstances_SharedDirFields(t *testing.T) {
	docker := dockerInstance("d", "d")
	docker.RuntimeTarget.SharedDirStorageClass = "rwx"
	docker.RuntimeTarget.SharedDirSize = "40Gi"
	errs := ValidateRuntimeBrokerInstances([]V1RuntimeBrokerInstanceConfig{docker})
	expectValidationPaths(t, errs, "server.broker.instances[0].runtime_target.shared_dir_storage_class",
		"server.broker.instances[0].runtime_target.shared_dir_size")
	for _, e := range errs {
		if e.Message != "field not valid for runtime target type docker" {
			t.Fatalf("unexpected docker error %v", e)
		}
	}

	k8s := V1RuntimeBrokerInstanceConfig{Key: "k", Name: "k", RuntimeTarget: &V1RuntimeTargetConfig{Type: "kubernetes", SharedDirSize: "0Gi"}}
	errs = ValidateRuntimeBrokerInstances([]V1RuntimeBrokerInstanceConfig{k8s})
	if len(errs) != 1 || errs[0].Path != "server.broker.instances[0].runtime_target.shared_dir_size" || !strings.Contains(errs[0].Message, "positive") {
		t.Fatalf("got %v", errs)
	}
	k8s.RuntimeTarget.SharedDirSize = "40Gi"
	k8s.RuntimeTarget.SharedDirStorageClass = "rwx"
	if errs := ValidateRuntimeBrokerInstances([]V1RuntimeBrokerInstanceConfig{k8s}); len(errs) != 0 {
		t.Fatalf("valid entry refused: %v", errs)
	}
}
