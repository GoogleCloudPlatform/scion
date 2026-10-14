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
// priority_class_name, safe_to_evict, the shared-dir and home storage
// backends (.design/flat-runtime-brokers/contract-appendix-p3.md FC1, FC2,
// FC5, FC7).

const nfsBlockYAML = `  shared_dir_storage:
    backend: local
    nfs:
      mount_root: /mnt/scion-nfs
      shares:
        - id: share-1
          pv_name: pv-1
`

const targetInfraInstancesYAML = `schema_version: "1"
server:
` + nfsBlockYAML + `  broker:
    instances:
      - key: k8s-nfs-1
        name: k8s-nfs-1
        runtime_target:
          type: kubernetes
          namespace: agents
          gke: true
          priority_class_name: agents-high
          safe_to_evict: false
          shared_dir_storage_backend: nfs
          shared_dir_storage_backends:
            scratch: local
          home_storage_backend: nfs
          home_storage_leaf: broker
      - key: docker-1
        name: docker-1
        runtime_target:
          type: docker
          shared_dir_storage_backend: local
          shared_dir_storage_backends:
            cache: nfs
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
	wantK8s := &V1RuntimeTargetConfig{Type: "kubernetes", Namespace: "agents", GKE: true, PriorityClassName: "agents-high",
		SafeToEvict: boolPtr(false), SharedDirStorageBackend: "nfs", SharedDirStorageBackends: map[string]string{"scratch": "local"},
		HomeStorageBackend: "nfs", HomeStorageLeaf: "broker"}
	wantDocker := &V1RuntimeTargetConfig{Type: "docker", SharedDirStorageBackend: "local",
		SharedDirStorageBackends: map[string]string{"cache": "nfs"}}
	if len(strict) != 2 || !reflect.DeepEqual(strict[0].RuntimeTarget, wantK8s) || !reflect.DeepEqual(strict[1].RuntimeTarget, wantDocker) {
		t.Fatalf("strict load: got %+v / %+v", strict[0].RuntimeTarget, strict[1].RuntimeTarget)
	}
	gc, err := LoadGlobalConfig("")
	if err != nil {
		t.Fatal(err)
	}
	wantGlobal := &RuntimeTargetConfig{Type: "kubernetes", Namespace: "agents", GKE: true, PriorityClassName: "agents-high",
		SafeToEvict: boolPtr(false), SharedDirStorageBackend: "nfs", SharedDirStorageBackends: map[string]string{"scratch": "local"},
		HomeStorageBackend: "nfs", HomeStorageLeaf: "broker"}
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
	gc.RuntimeBroker.Instances[0].RuntimeTarget.SharedDirStorageBackends["scratch"] = "nfs"
	if *back.Broker.Instances[0].RuntimeTarget.SafeToEvict || back.Broker.Instances[0].RuntimeTarget.SharedDirStorageBackends["scratch"] != "local" {
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
// safe_to_evict, and accepts the shared-dir backends. Kubernetes validates
// each value, naming its path.
func TestValidateRuntimeBrokerInstances_TargetInfraFields(t *testing.T) {
	docker := dockerInstance("d", "d")
	docker.RuntimeTarget.GKE = true
	docker.RuntimeTarget.PriorityClassName = "high"
	docker.RuntimeTarget.SafeToEvict = boolPtr(false)
	docker.RuntimeTarget.HomeStorageBackend = "local"
	docker.RuntimeTarget.HomeStorageLeaf = "pod"
	docker.RuntimeTarget.SharedDirStorageBackend = "nfs"
	docker.RuntimeTarget.SharedDirStorageBackends = map[string]string{"cache": "local"}
	errs := ValidateRuntimeBrokerInstances([]V1RuntimeBrokerInstanceConfig{docker})
	var paths []string
	for _, e := range errs {
		if e.Message != "field not valid for runtime target type docker" {
			t.Fatalf("unexpected docker error %v", e)
		}
		paths = append(paths, strings.TrimPrefix(e.Path, "server.broker.instances[0].runtime_target."))
	}
	if want := []string{"gke", "home_storage_backend", "home_storage_leaf", "priority_class_name", "safe_to_evict"}; !reflect.DeepEqual(paths, want) {
		t.Fatalf("docker refusals: got %v, want %v", paths, want)
	}

	k8s := func(mut func(*V1RuntimeTargetConfig)) []ValidationError {
		t := &V1RuntimeTargetConfig{Type: "kubernetes"}
		mut(t)
		return ValidateRuntimeBrokerInstances([]V1RuntimeBrokerInstanceConfig{{Key: "k", Name: "k", RuntimeTarget: t}})
	}
	for name, tc := range map[string]struct {
		mut  func(*V1RuntimeTargetConfig)
		path string
	}{
		"priority class": {func(t *V1RuntimeTargetConfig) { t.PriorityClassName = "High_Priority" }, "priority_class_name"},
		"single backend": {func(t *V1RuntimeTargetConfig) { t.SharedDirStorageBackend = "NFS" }, "shared_dir_storage_backend"},
		"per-dir value":  {func(t *V1RuntimeTargetConfig) { t.SharedDirStorageBackends = map[string]string{"data": "gcs"} }, "shared_dir_storage_backends.data"},
		"per-dir name":   {func(t *V1RuntimeTargetConfig) { t.SharedDirStorageBackends = map[string]string{"Bad_Dir": "nfs"} }, "shared_dir_storage_backends.Bad_Dir"},
		"home backend":   {func(t *V1RuntimeTargetConfig) { t.HomeStorageBackend = "gcs" }, "home_storage_backend"},
		"home leaf":      {func(t *V1RuntimeTargetConfig) { t.HomeStorageLeaf = "node" }, "home_storage_leaf"},
	} {
		t.Run(name, func(t *testing.T) {
			errs := k8s(tc.mut)
			if len(errs) != 1 || errs[0].Path != "server.broker.instances[0].runtime_target."+tc.path {
				t.Fatalf("got %v, want one error at %s", errs, tc.path)
			}
		})
	}
	if errs := k8s(func(t *V1RuntimeTargetConfig) {
		t.GKE, t.PriorityClassName, t.SafeToEvict = true, "agents.high", boolPtr(true)
		t.SharedDirStorageBackend, t.SharedDirStorageBackends = "local", map[string]string{"data": "nfs"}
		t.HomeStorageBackend, t.HomeStorageLeaf = "nfs", "pod"
	}); len(errs) != 0 {
		t.Fatalf("valid entry refused: %v", errs)
	}
}

// An instance that selects nfs for shared dirs or home is refused when the
// instances are loaded unless server.shared_dir_storage has a complete nfs
// block; the error names the instance field.
func TestRuntimeBrokerInstances_IncompleteNFSRefusedAtLoad(t *testing.T) {
	entry := func(fields string) string {
		return "  broker:\n    instances:\n      - key: k\n        name: k\n        runtime_target:\n          type: kubernetes\n" + fields
	}
	for name, tc := range map[string]struct {
		yaml, want string
	}{
		"no block": {"schema_version: \"1\"\nserver:\n" + entry("          shared_dir_storage_backend: nfs\n"),
			"server.broker.instances[0].runtime_target.shared_dir_storage_backend: selects \"nfs\", which needs a complete server.shared_dir_storage.nfs block"},
		"no shares": {"schema_version: \"1\"\nserver:\n  shared_dir_storage:\n    nfs:\n      mount_root: /mnt/x\n" + entry("          shared_dir_storage_backends:\n            data: nfs\n"),
			"server.broker.instances[0].runtime_target.shared_dir_storage_backends.data: selects \"nfs\""},
		"home": {"schema_version: \"1\"\nserver:\n" + entry("          home_storage_backend: nfs\n"),
			"server.broker.instances[0].runtime_target.home_storage_backend: selects \"nfs\""},
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
	t.Run("complete block", func(t *testing.T) {
		global := flatTestHome(t)
		writeFlatTestFile(t, filepath.Join(global, "settings.yaml"), "schema_version: \"1\"\nserver:\n"+nfsBlockYAML+
			entry("          shared_dir_storage_backend: nfs\n          home_storage_backend: nfs\n"))
		if _, err := LoadRuntimeBrokerInstances(""); err != nil {
			t.Fatalf("a complete nfs block must be accepted: %v", err)
		}
	})
	t.Run("local needs no block", func(t *testing.T) {
		global := flatTestHome(t)
		writeFlatTestFile(t, filepath.Join(global, "settings.yaml"), "schema_version: \"1\"\nserver:\n"+
			entry("          shared_dir_storage_backend: local\n          home_storage_backend: local\n"))
		if _, err := LoadRuntimeBrokerInstances(""); err != nil {
			t.Fatalf("local backends need no nfs block: %v", err)
		}
	})
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

func TestResolveSharedDirStorage_FlatBranchAndFallThrough(t *testing.T) {
	vs := targetLegacySettings()
	global := &V1SharedDirStorageConfig{Backend: "local", NFS: &V1NFSConfig{MountRoot: "/mnt/x"}}
	vs.Server = &V1ServerConfig{SharedDirStorage: global}

	cfg, src := vs.ForFlatInstance(targetPolicy(V1RuntimeTargetConfig{SharedDirStorageBackend: "nfs"})).ResolveSharedDirStorage("batch")
	if cfg == nil || cfg.Backend != "nfs" || cfg.NFS != global.NFS || src != instPath+"shared_dir_storage_backend" {
		t.Fatalf("instance value: got %+v %q", cfg, src)
	}
	// Unset falls through to server.shared_dir_storage, not the profile's nfs.
	cfg, src = vs.ForFlatInstance(targetPolicy(V1RuntimeTargetConfig{})).ResolveSharedDirStorage("")
	if cfg != global || src != SharedDirStorageGlobalSource {
		t.Fatalf("fall-through: got %+v %q, want the global block", cfg, src)
	}
	vs.Server = nil
	if cfg, src := vs.ForFlatInstance(targetPolicy(V1RuntimeTargetConfig{})).ResolveSharedDirStorage(""); cfg != nil || src != "" {
		t.Fatalf("nothing configured: got %+v %q", cfg, src)
	}
	if cfg, src := vs.ResolveSharedDirStorage(""); cfg == nil || cfg.Backend != "nfs" || src != "profiles.batch.shared_dir_storage_backend" {
		t.Fatalf("legacy resolution changed: %+v %q", cfg, src)
	}
}

func TestResolveSharedDirStorageBackend_FlatBranchAndFallThrough(t *testing.T) {
	vs := targetLegacySettings()
	vs.Server = &V1ServerConfig{SharedDirStorage: &V1SharedDirStorageConfig{Backend: "local"}}
	view := vs.ForFlatInstance(targetPolicy(V1RuntimeTargetConfig{SharedDirStorageBackend: "nfs",
		SharedDirStorageBackends: map[string]string{"cache": "local"}}))
	for dir, want := range map[string]struct {
		backend, source string
		perDir          bool
	}{
		"cache": {"local", instPath + "shared_dir_storage_backends.cache", true},
		"data":  {"nfs", instPath + "shared_dir_storage_backend", false},
	} {
		backend, source, perDir := view.ResolveSharedDirStorageBackend("batch", dir)
		if backend != want.backend || source != want.source || perDir != want.perDir {
			t.Fatalf("dir %s: got (%q, %q, %v), want %+v", dir, backend, source, perDir, want)
		}
	}
	// Unset falls through to the global backend, not the profile's per-dir
	// or single nfs.
	backend, source, perDir := vs.ForFlatInstance(targetPolicy(V1RuntimeTargetConfig{})).ResolveSharedDirStorageBackend("", "data")
	if backend != "local" || source != SharedDirStorageGlobalSource || perDir {
		t.Fatalf("fall-through: got (%q, %q, %v)", backend, source, perDir)
	}
	vs.Server = nil
	if backend, source, _ := vs.ForFlatInstance(targetPolicy(V1RuntimeTargetConfig{})).ResolveSharedDirStorageBackend("", "data"); backend != "" || source != "" {
		t.Fatalf("nothing configured: got (%q, %q)", backend, source)
	}
	if backend, source, _ := vs.ResolveSharedDirStorageBackend("", "data"); backend != "nfs" || source != "profiles.batch.shared_dir_storage_backends.data" {
		t.Fatalf("legacy resolution changed: %q %q", backend, source)
	}
}

func TestResolveHomeStorage_FlatBranchAndFallThrough(t *testing.T) {
	vs := targetLegacySettings()
	vs.Server = &V1ServerConfig{HomeStorage: &V1HomeStorageConfig{Backend: "local", Leaf: "pod"}}

	got := vs.ForFlatInstance(targetPolicy(V1RuntimeTargetConfig{HomeStorageBackend: "nfs", HomeStorageLeaf: "broker"})).ResolveHomeStorage("batch")
	if want := (ResolvedHomeStorage{Backend: "nfs", BackendSource: instPath + "home_storage_backend", Leaf: "broker", LeafSource: instPath + "home_storage_leaf"}); got != want {
		t.Fatalf("instance values: got %+v", got)
	}
	// Unset falls through to server.home_storage, not the profile's nfs.
	got = vs.ForFlatInstance(targetPolicy(V1RuntimeTargetConfig{})).ResolveHomeStorage("")
	if want := (ResolvedHomeStorage{Backend: "local", BackendSource: HomeStorageBackendGlobalSource, Leaf: "pod", LeafSource: HomeStorageLeafGlobalSource}); got != want {
		t.Fatalf("fall-through: got %+v", got)
	}
	// One field set, the other falls through on its own.
	got = vs.ForFlatInstance(targetPolicy(V1RuntimeTargetConfig{HomeStorageBackend: "nfs"})).ResolveHomeStorage("")
	if got.Backend != "nfs" || got.Leaf != "pod" || got.LeafSource != HomeStorageLeafGlobalSource {
		t.Fatalf("backend only: got %+v", got)
	}
	vs.Server = nil
	if got := vs.ForFlatInstance(targetPolicy(V1RuntimeTargetConfig{})).ResolveHomeStorage(""); got != (ResolvedHomeStorage{Backend: "local", Leaf: "pod"}) {
		t.Fatalf("defaults: got %+v", got)
	}
	if got := vs.ResolveHomeStorage(""); got.Backend != "nfs" || got.BackendSource != "profiles.batch.home_storage_backend" {
		t.Fatalf("legacy resolution changed: %+v", got)
	}
}

// Project cleanup finds NFS-backed shared dirs selected only by an
// instance.
func TestSharedDirStorageNFSAnywhere_Instance(t *testing.T) {
	nfs := &V1NFSConfig{MountRoot: "/mnt/x"}
	for name, target := range map[string]*V1RuntimeTargetConfig{
		"single":  {Type: "docker", SharedDirStorageBackend: "nfs"},
		"per dir": {Type: "kubernetes", SharedDirStorageBackends: map[string]string{"data": "nfs"}},
	} {
		vs := &VersionedSettings{Server: &V1ServerConfig{SharedDirStorage: &V1SharedDirStorageConfig{Backend: "local", NFS: nfs},
			Broker: &V1BrokerConfig{Instances: []V1RuntimeBrokerInstanceConfig{{Key: "i", Name: "i", RuntimeTarget: target}}}}}
		cfg, onlyOverrides := vs.SharedDirStorageNFSAnywhere()
		if cfg == nil || cfg.Backend != "nfs" || cfg.NFS != nfs || !onlyOverrides {
			t.Fatalf("%s: got %+v %v", name, cfg, onlyOverrides)
		}
	}
	vs := &VersionedSettings{Server: &V1ServerConfig{Broker: &V1BrokerConfig{Instances: []V1RuntimeBrokerInstanceConfig{
		{Key: "i", Name: "i", RuntimeTarget: &V1RuntimeTargetConfig{Type: "docker", SharedDirStorageBackend: "local"}}}}}}
	if cfg, _ := vs.SharedDirStorageNFSAnywhere(); cfg != nil {
		t.Fatalf("a local instance selects no nfs: %+v", cfg)
	}
}

// The startup report lists each instance's own backend and warns when an
// instance and the plugins' resolution (the global settings, no profile)
// would use different backends for a dir.
func TestFlatInstanceSharedDirStorageReport(t *testing.T) {
	vs := &VersionedSettings{Server: &V1ServerConfig{
		SharedDirStorage: &V1SharedDirStorageConfig{Backend: "local"},
		Broker: &V1BrokerConfig{Instances: []V1RuntimeBrokerInstanceConfig{
			{Key: "same", Name: "same", RuntimeTarget: &V1RuntimeTargetConfig{Type: "docker", SharedDirStorageBackend: "local"}},
			{Key: "plain", Name: "plain", RuntimeTarget: &V1RuntimeTargetConfig{Type: "docker"}},
			{Key: "nfs", Name: "nfs", RuntimeTarget: &V1RuntimeTargetConfig{Type: "kubernetes", SharedDirStorageBackend: "nfs",
				SharedDirStorageBackends: map[string]string{"cache": "local"}}},
		}},
	}}
	lines, warnings := vs.FlatInstanceSharedDirStorageReport()
	wantLines := []string{
		"shared_dir_storage for instance same: backend=local (from server.broker.instances[same].runtime_target.shared_dir_storage_backend)",
		"shared_dir_storage for instance nfs: backend=nfs (from server.broker.instances[nfs].runtime_target.shared_dir_storage_backend)",
		"shared_dir_storage for instance nfs, shared dir cache: backend=local (from server.broker.instances[nfs].runtime_target.shared_dir_storage_backends.cache)",
	}
	if !reflect.DeepEqual(lines, wantLines) {
		t.Fatalf("lines:\n got %q\nwant %q", lines, wantLines)
	}
	if len(warnings) != 1 || !strings.HasPrefix(warnings[0], "server.broker.instances[nfs]: ") ||
		!strings.Contains(warnings[0], "shared dirs without a per-dir entry: nfs here, local for plugins") ||
		strings.Contains(warnings[0], "shared dir cache") {
		t.Fatalf("warnings: %q", warnings)
	}
	if lines, warnings := (&VersionedSettings{}).FlatInstanceSharedDirStorageReport(); lines != nil || warnings != nil {
		t.Fatalf("no instances: got %q %q", lines, warnings)
	}
}
