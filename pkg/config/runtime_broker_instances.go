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
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"

	yamlv3 "gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// Flat Runtime Broker instance settings (.design/flat-runtime-brokers-contract.md
// section 2).

// RuntimeBrokerInstanceKeyPattern is the frozen shape of an instance key.
const RuntimeBrokerInstanceKeyPattern = `^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`

var runtimeBrokerInstanceKeyRe = regexp.MustCompile(RuntimeBrokerInstanceKeyPattern)

// Runtime target types accepted in server.broker.instances.
const (
	RuntimeTargetTypeDocker     = "docker"
	RuntimeTargetTypeKubernetes = "kubernetes"
)

// ErrRuntimeBrokerInstancesInServerYAML is returned by LoadGlobalConfig when a
// legacy server.yaml configures runtimeBroker.instances.
var ErrRuntimeBrokerInstancesInServerYAML = errors.New(
	"runtimeBroker.instances is only supported under server.broker.instances in settings.yaml")

// RuntimeBrokerHostingError is the P1 fail-closed refusal of a flat instance
// without the Hub in the same process.
type RuntimeBrokerHostingError struct {
	InstanceKey string
}

// Code returns the frozen error code.
func (e *RuntimeBrokerHostingError) Code() string {
	return api.ErrCodeFlatRuntimeBrokerRemoteUnsupported
}

func (e *RuntimeBrokerHostingError) Error() string {
	return "server.broker.instances requires the Hub in the same process in this release; " +
		"remote flat Runtime Broker hosting arrives in P2 (ptone/scion#3271). " +
		"Remove server.broker.instances to run this host as a legacy Runtime Broker"
}

// ValidateRuntimeBrokerInstances checks server.broker.instances and returns
// one ValidationError per problem, each naming its settings path. There is
// no policy limit on the number of instances (P2.1); each key must be
// unique.
func ValidateRuntimeBrokerInstances(instances []V1RuntimeBrokerInstanceConfig) []ValidationError {
	var errs []ValidationError
	firstIndex := map[string]int{}
	for i, inst := range instances {
		p := fmt.Sprintf("server.broker.instances[%d]", i)
		if inst.Key == "" || !runtimeBrokerInstanceKeyRe.MatchString(inst.Key) {
			errs = append(errs, ValidationError{Path: p + ".key",
				Message: fmt.Sprintf("invalid instance key %q (must match %s)", inst.Key, RuntimeBrokerInstanceKeyPattern)})
		} else if j, dup := firstIndex[inst.Key]; dup {
			errs = append(errs, duplicateInstanceKeyError(i, j, inst.Key))
		} else {
			firstIndex[inst.Key] = i
		}
		if inst.Name == "" {
			errs = append(errs, ValidationError{Path: p + ".name", Message: "name is required"})
		}
		t := inst.RuntimeTarget
		if t == nil || t.Type == "" {
			errs = append(errs, ValidationError{Path: p + ".runtime_target.type", Message: "runtime_target.type is required"})
			continue
		}
		switch t.Type {
		case RuntimeTargetTypeDocker:
			for _, f := range []struct{ field, v string }{{"context", t.Context}, {"namespace", t.Namespace}, {"kubeconfig", t.Kubeconfig},
				{"kubernetes_block_service_account", t.KubernetesBlockServiceAccount},
				{"shared_dir_storage_class", t.SharedDirStorageClass}, {"shared_dir_size", t.SharedDirSize},
				{"priority_class_name", t.PriorityClassName},
				{"home_storage_backend", t.HomeStorageBackend}, {"home_storage_leaf", t.HomeStorageLeaf}} {
				if field, v := f.field, f.v; v != "" {
					errs = append(errs, ValidationError{Path: p + ".runtime_target." + field,
						Message: "field not valid for runtime target type docker"})
				}
			}
			// gke false and an absent safe_to_evict are unset; any other
			// value of them is refused, as for the string fields above.
			for _, f := range []struct {
				field string
				set   bool
			}{{"kubernetes_service_account_mappings", len(t.KubernetesServiceAccountMappings) > 0},
				{"gke", t.GKE}, {"safe_to_evict", t.SafeToEvict != nil}} {
				if f.set {
					errs = append(errs, ValidationError{Path: p + ".runtime_target." + f.field,
						Message: "field not valid for runtime target type docker"})
				}
			}
			errs = append(errs, validateInstanceSharedDirBackends(p, t)...)
		case RuntimeTargetTypeKubernetes:
			if t.Kubeconfig != "" && !validInstanceKubeconfigPath(t.Kubeconfig) {
				errs = append(errs, ValidationError{Path: p + ".runtime_target.kubeconfig",
					Message: fmt.Sprintf("kubeconfig must be one absolute local file path (no ~, environment variables or path list): %q", t.Kubeconfig)})
			}
			if t.KubernetesBlockServiceAccount != "" {
				if err := ValidateKubernetesBlockServiceAccount(t.KubernetesBlockServiceAccount); err != nil {
					errs = append(errs, ValidationError{Path: p + ".runtime_target.kubernetes_block_service_account", Message: err.Error()})
				}
			}
			if err := ValidateKubernetesServiceAccountMappings(t.KubernetesServiceAccountMappings); err != nil {
				errs = append(errs, ValidationError{Path: p + ".runtime_target.kubernetes_service_account_mappings", Message: err.Error()})
			}
			if err := ValidateSharedDirSize(t.SharedDirSize); err != nil {
				errs = append(errs, ValidationError{Path: p + ".runtime_target.shared_dir_size", Message: err.Error()})
			}
			if t.PriorityClassName != "" {
				if verrs := validation.IsDNS1123Subdomain(t.PriorityClassName); len(verrs) > 0 {
					errs = append(errs, ValidationError{Path: p + ".runtime_target.priority_class_name",
						Message: fmt.Sprintf("%q is not a valid Kubernetes PriorityClass name: %s", t.PriorityClassName, strings.Join(verrs, "; "))})
				}
			}
			if err := checkHomeStorageBackend(t.HomeStorageBackend); err != nil {
				errs = append(errs, ValidationError{Path: p + ".runtime_target.home_storage_backend", Message: err.Error()})
			}
			if err := checkHomeStorageLeaf(t.HomeStorageLeaf); err != nil {
				errs = append(errs, ValidationError{Path: p + ".runtime_target.home_storage_leaf", Message: err.Error()})
			}
			errs = append(errs, validateInstanceSharedDirBackends(p, t)...)
		default:
			errs = append(errs, ValidationError{Path: p + ".runtime_target.type",
				Message: fmt.Sprintf("unsupported runtime target type %q (supported: docker, kubernetes)", t.Type)})
		}
	}
	sort.SliceStable(errs, func(a, b int) bool { return errs[a].Path < errs[b].Path })
	return errs
}

// validateInstanceSharedDirBackends checks an instance's
// shared_dir_storage_backend and shared_dir_storage_backends values with
// the rules for runtime and profile entries, by checking them as one
// unnamed runtime entry and renaming the error paths. Whether "nfs" has a
// complete server.shared_dir_storage.nfs block is checked where that block
// is read (RuntimeBrokerInstanceNFSErrors).
func validateInstanceSharedDirBackends(p string, t *V1RuntimeTargetConfig) []ValidationError {
	errs := ValidateSharedDirStorageBackendValues(map[string]V1RuntimeConfig{"": {
		SharedDirStorageBackend: t.SharedDirStorageBackend, SharedDirStorageBackends: t.SharedDirStorageBackends}}, nil)
	for i := range errs {
		errs[i].Path = p + ".runtime_target" + strings.TrimPrefix(errs[i].Path, "runtimes.")
	}
	return errs
}

// validInstanceKubeconfigPath reports whether p is one absolute local file
// path. It is never expanded (~, environment variables) and never a path
// list; the file itself is checked when the instance's runtime is built.
func validInstanceKubeconfigPath(p string) bool {
	return strings.HasPrefix(p, "/") && !strings.ContainsRune(p, filepath.ListSeparator) && strings.Trim(p, "/") != ""
}

func duplicateInstanceKeyError(i, j int, key string) ValidationError {
	return ValidationError{Path: fmt.Sprintf("server.broker.instances[%d].key", i),
		Message: fmt.Sprintf("duplicate instance key %q (also at index %d)", key, j)}
}

// RuntimeBrokerInstanceDuplicateKeys reports every repeated instance key.
// The JSON schema cannot express key uniqueness, so scion config validate
// (ValidateSettings) runs this after the schema pass, keeping the schema and
// ValidateRuntimeBrokerInstances in agreement.
func RuntimeBrokerInstanceDuplicateKeys(instances []V1RuntimeBrokerInstanceConfig) []ValidationError {
	var errs []ValidationError
	firstIndex := map[string]int{}
	for i, inst := range instances {
		if j, dup := firstIndex[inst.Key]; dup {
			errs = append(errs, duplicateInstanceKeyError(i, j, inst.Key))
		} else {
			firstIndex[inst.Key] = i
		}
	}
	return errs
}

// RuntimeBrokerInstanceSharedDirSizes reports every runtime_target
// shared_dir_size that is not a positive Kubernetes quantity, which the JSON
// schema cannot express; scion config validate (ValidateSettings) runs it
// after the schema pass, as ValidateRuntimeBrokerInstances does at load.
func RuntimeBrokerInstanceSharedDirSizes(instances []V1RuntimeBrokerInstanceConfig) []ValidationError {
	var errs []ValidationError
	for i, inst := range instances {
		if inst.RuntimeTarget == nil {
			continue
		}
		if err := ValidateSharedDirSize(inst.RuntimeTarget.SharedDirSize); err != nil {
			errs = append(errs, ValidationError{Path: fmt.Sprintf("server.broker.instances[%d].runtime_target.shared_dir_size", i), Message: err.Error()})
		}
	}
	return errs
}

// RuntimeBrokerInstanceNFSErrors reports every instance runtime_target
// value that selects "nfs" (shared_dir_storage_backend, a
// shared_dir_storage_backends entry, or home_storage_backend) while
// sharedDir, the server.shared_dir_storage block of the same settings
// file, has no complete nfs block. Both NFS shared dirs and an NFS home
// use that block, so an incomplete one refuses the instance when it is
// loaded rather than failing its agents' first starts. Results are sorted
// by path.
func RuntimeBrokerInstanceNFSErrors(instances []V1RuntimeBrokerInstanceConfig, sharedDir *V1SharedDirStorageConfig) []ValidationError {
	nfs := &V1SharedDirStorageConfig{Backend: "nfs"}
	if sharedDir != nil {
		nfs.NFS = sharedDir.NFS
	}
	blockErr := nfs.Validate()
	if blockErr == nil {
		return nil
	}
	var errs []ValidationError
	add := func(path string) {
		errs = append(errs, ValidationError{Path: path,
			Message: "selects \"nfs\", which needs a complete server.shared_dir_storage.nfs block (" + blockErr.Error() + ")"})
	}
	for i, inst := range instances {
		t := inst.RuntimeTarget
		if t == nil {
			continue
		}
		p := fmt.Sprintf("server.broker.instances[%d].runtime_target", i)
		if t.SharedDirStorageBackend == "nfs" {
			add(p + ".shared_dir_storage_backend")
		}
		for dir, backend := range t.SharedDirStorageBackends {
			if backend == "nfs" {
				add(p + ".shared_dir_storage_backends." + dir)
			}
		}
		if t.HomeStorageBackend == HomeStorageBackendNFS {
			add(p + ".home_storage_backend")
		}
	}
	sort.Slice(errs, func(a, b int) bool { return errs[a].Path < errs[b].Path })
	return errs
}

// CheckRuntimeBrokerInstanceHosting is the P1 fail-closed gate: a non-empty
// instances list requires the Hub in the same process. hubInProcess must be
// exactly the predicate that admits the embedded registration
// (colocatedBrokerRegisters at startup), so a remote or simulated-remote
// Runtime Broker with instances is refused. Saved credentials of any kind
// never change the answer. Legacy hosting (no instances) is always allowed.
func CheckRuntimeBrokerInstanceHosting(instances []V1RuntimeBrokerInstanceConfig, hubInProcess bool) error {
	if len(instances) == 0 || hubInProcess {
		return nil
	}
	return &RuntimeBrokerHostingError{InstanceKey: instances[0].Key}
}

// LoadRuntimeBrokerInstances strictly loads and validates
// server.broker.instances from the settings.yaml the server reads: the global
// settings.yaml if it exists and has a raw "server" key (whether or not that
// section unmarshals), otherwise the --config directory's settings.yaml under
// the same rule, otherwise none. A global settings.yaml that exists but is
// not valid YAML is an error. Unknown keys and type errors are errors, unlike
// the lenient settings loaders.
func LoadRuntimeBrokerInstances(configPath string) ([]V1RuntimeBrokerInstanceConfig, error) {
	globalDir, err := GetGlobalDir()
	if err != nil {
		return nil, err
	}
	raw, found, err := readServerSectionStrict(globalDir)
	if err != nil {
		return nil, err
	}
	if !found && configPath != "" {
		if info, statErr := os.Stat(configPath); statErr == nil {
			dir := configPath
			if !info.IsDir() {
				dir = filepath.Dir(configPath)
			}
			if dir != globalDir {
				raw, found, err = readServerSectionStrict(dir)
				if err != nil {
					return nil, err
				}
			}
		}
	}
	if !found {
		return nil, nil
	}
	brokerVal, present := raw["broker"]
	if !present || brokerVal == nil {
		return nil, nil
	}
	brokerRaw, ok := brokerVal.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("server.broker must be a mapping")
	}
	instRaw, ok := brokerRaw["instances"]
	if !ok || instRaw == nil {
		return nil, nil
	}
	if err := checkInstanceScalarsAreStrings(instRaw); err != nil {
		return nil, err
	}
	if err := checkInstanceKeys(instRaw); err != nil {
		return nil, err
	}
	data, err := yamlv3.Marshal(instRaw)
	if err != nil {
		return nil, fmt.Errorf("server.broker.instances: %w", err)
	}
	dec := yamlv3.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var instances []V1RuntimeBrokerInstanceConfig
	if err := dec.Decode(&instances); err != nil {
		return nil, fmt.Errorf("server.broker.instances: %w", err)
	}
	if errs := ValidateRuntimeBrokerInstances(instances); len(errs) > 0 {
		return nil, joinValidationErrors(errs)
	}
	if err := checkInstanceNFS(instances, raw["shared_dir_storage"]); err != nil {
		return nil, err
	}
	return instances, nil
}

// checkInstanceNFS refuses instances that select nfs while sdRaw, the raw
// server.shared_dir_storage section of the same file, has no complete nfs
// block (RuntimeBrokerInstanceNFSErrors).
func checkInstanceNFS(instances []V1RuntimeBrokerInstanceConfig, sdRaw interface{}) error {
	var sd *V1SharedDirStorageConfig
	var decodeErr error
	if sdRaw != nil {
		data, err := yamlv3.Marshal(sdRaw)
		if err == nil {
			sd = &V1SharedDirStorageConfig{}
			err = yamlv3.Unmarshal(data, sd)
		}
		if err != nil {
			sd, decodeErr = nil, err
		}
	}
	errs := RuntimeBrokerInstanceNFSErrors(instances, sd)
	if len(errs) == 0 {
		return nil
	}
	if decodeErr != nil {
		return fmt.Errorf("server.shared_dir_storage: %w; %w", decodeErr, joinValidationErrors(errs))
	}
	return joinValidationErrors(errs)
}

// checkInstanceScalarsAreStrings rejects non-string scalars where the schema
// requires strings (for example key: 123), which the YAML decoder would
// otherwise coerce into strings.
func checkInstanceScalarsAreStrings(instRaw interface{}) error {
	list, ok := instRaw.([]interface{})
	if !ok {
		return fmt.Errorf("server.broker.instances must be a list")
	}
	checkStrings := func(path string, m map[string]interface{}, keys ...string) error {
		for _, k := range keys {
			if v, present := m[k]; present && v != nil {
				if _, isString := v.(string); !isString {
					return fmt.Errorf("%s.%s must be a string", path, k)
				}
			}
		}
		return nil
	}
	for i, e := range list {
		p := fmt.Sprintf("server.broker.instances[%d]", i)
		m, ok := e.(map[string]interface{})
		if !ok {
			return fmt.Errorf("%s must be a mapping", p)
		}
		if err := checkStrings(p, m, "key", "name"); err != nil {
			return err
		}
		if t, present := m["runtime_target"]; present && t != nil {
			tm, ok := t.(map[string]interface{})
			if !ok {
				return fmt.Errorf("%s.runtime_target must be a mapping", p)
			}
			if err := checkStrings(p+".runtime_target", tm, "type", "display_name", "context", "namespace",
				"shared_dir_storage_class", "shared_dir_size", "priority_class_name",
				"shared_dir_storage_backend", "home_storage_backend", "home_storage_leaf"); err != nil {
				return err
			}
		}
	}
	return nil
}

// reservedInstanceKeys and reservedRuntimeTargetKeys are keys of the frozen
// instance schema (.design/flat-runtime-brokers/contract-appendix-p3.md
// FC1) that this release does not apply yet. The strict loader refuses each
// with an explicit error rather than reporting it as unknown or dropping
// it, so a key is never accepted without its effect. A change that wires a
// key removes it here, adds the Go field and adds it to the JSON schema.
var (
	reservedInstanceKeys = []string{"agent_defaults"}
	// Every frozen runtime_target key is wired.
	reservedRuntimeTargetKeys []string
)

// knownYAMLKeys returns the yaml keys of struct type v's fields.
func knownYAMLKeys(v interface{}) map[string]bool {
	keys := map[string]bool{}
	t := reflect.TypeOf(v)
	for i := 0; i < t.NumField(); i++ {
		if name, _, _ := strings.Cut(t.Field(i).Tag.Get("yaml"), ","); name != "" && name != "-" {
			keys[name] = true
		}
	}
	return keys
}

var (
	knownInstanceKeys      = knownYAMLKeys(V1RuntimeBrokerInstanceConfig{})
	knownRuntimeTargetKeys = knownYAMLKeys(V1RuntimeTargetConfig{})
)

// checkInstanceKeys refuses every key of an instance entry or its
// runtime_target that the strict decode would not apply, naming its
// settings path: a reserved key (frozen, not yet supported) or an unknown
// one. Keys are checked in sorted order so the first error is stable.
func checkInstanceKeys(instRaw interface{}) error {
	list, _ := instRaw.([]interface{})
	check := func(path string, m map[string]interface{}, known map[string]bool, reserved []string) error {
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if known[k] {
				continue
			}
			for _, r := range reserved {
				if k == r {
					return fmt.Errorf("%s.%s: this key is reserved for flat Runtime Broker instances but not supported in this release; remove it", path, k)
				}
			}
			return fmt.Errorf("%s.%s: unknown key", path, k)
		}
		return nil
	}
	for i, e := range list {
		p := fmt.Sprintf("server.broker.instances[%d]", i)
		m, _ := e.(map[string]interface{})
		if err := check(p, m, knownInstanceKeys, reservedInstanceKeys); err != nil {
			return err
		}
		if tm, ok := m["runtime_target"].(map[string]interface{}); ok {
			if err := check(p+".runtime_target", tm, knownRuntimeTargetKeys, reservedRuntimeTargetKeys); err != nil {
				return err
			}
		}
	}
	return nil
}

// readServerSectionStrict reads settings.yaml in dir. It reports found only
// when the file has a raw "server" key; a missing file is not an error, an
// unparseable one is.
func readServerSectionStrict(dir string) (map[string]interface{}, bool, error) {
	path := filepath.Join(dir, "settings.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("read %s: %w", path, err)
	}
	var doc map[string]interface{}
	if err := yamlv3.Unmarshal(data, &doc); err != nil {
		return nil, false, fmt.Errorf("parse %s: %w", path, err)
	}
	serverRaw, ok := doc["server"]
	if !ok || serverRaw == nil {
		return nil, false, nil
	}
	server, ok := serverRaw.(map[string]interface{})
	if !ok {
		return nil, true, fmt.Errorf("%s: server must be a mapping", path)
	}
	return server, true, nil
}

func joinValidationErrors(errs []ValidationError) error {
	all := make([]error, 0, len(errs))
	for _, e := range errs {
		all = append(all, e)
	}
	return errors.Join(all...)
}

// v1InstancesToGlobal deep-copies settings instances into server-config form.
func v1InstancesToGlobal(in []V1RuntimeBrokerInstanceConfig) []RuntimeBrokerInstanceConfig {
	if len(in) == 0 {
		return nil
	}
	out := make([]RuntimeBrokerInstanceConfig, 0, len(in))
	for _, i := range in {
		o := RuntimeBrokerInstanceConfig{Key: i.Key, Name: i.Name}
		if i.RuntimeTarget != nil {
			o.RuntimeTarget = &RuntimeTargetConfig{
				Type:                             i.RuntimeTarget.Type,
				DisplayName:                      i.RuntimeTarget.DisplayName,
				Context:                          i.RuntimeTarget.Context,
				Namespace:                        i.RuntimeTarget.Namespace,
				Kubeconfig:                       i.RuntimeTarget.Kubeconfig,
				KubernetesBlockServiceAccount:    i.RuntimeTarget.KubernetesBlockServiceAccount,
				KubernetesServiceAccountMappings: copyStringMap(i.RuntimeTarget.KubernetesServiceAccountMappings),
				SharedDirStorageClass:            i.RuntimeTarget.SharedDirStorageClass,
				SharedDirSize:                    i.RuntimeTarget.SharedDirSize,
				GKE:                              i.RuntimeTarget.GKE,
				PriorityClassName:                i.RuntimeTarget.PriorityClassName,
				SafeToEvict:                      copyBoolPtr(i.RuntimeTarget.SafeToEvict),
				SharedDirStorageBackend:          i.RuntimeTarget.SharedDirStorageBackend,
				SharedDirStorageBackends:         copyStringMap(i.RuntimeTarget.SharedDirStorageBackends),
				HomeStorageBackend:               i.RuntimeTarget.HomeStorageBackend,
				HomeStorageLeaf:                  i.RuntimeTarget.HomeStorageLeaf,
			}
		}
		out = append(out, o)
	}
	return out
}

// globalInstancesToV1 deep-copies server-config instances into settings form.
func globalInstancesToV1(in []RuntimeBrokerInstanceConfig) []V1RuntimeBrokerInstanceConfig {
	if len(in) == 0 {
		return nil
	}
	out := make([]V1RuntimeBrokerInstanceConfig, 0, len(in))
	for _, i := range in {
		o := V1RuntimeBrokerInstanceConfig{Key: i.Key, Name: i.Name}
		if i.RuntimeTarget != nil {
			o.RuntimeTarget = &V1RuntimeTargetConfig{
				Type:                             i.RuntimeTarget.Type,
				DisplayName:                      i.RuntimeTarget.DisplayName,
				Context:                          i.RuntimeTarget.Context,
				Namespace:                        i.RuntimeTarget.Namespace,
				Kubeconfig:                       i.RuntimeTarget.Kubeconfig,
				KubernetesBlockServiceAccount:    i.RuntimeTarget.KubernetesBlockServiceAccount,
				KubernetesServiceAccountMappings: copyStringMap(i.RuntimeTarget.KubernetesServiceAccountMappings),
				SharedDirStorageClass:            i.RuntimeTarget.SharedDirStorageClass,
				SharedDirSize:                    i.RuntimeTarget.SharedDirSize,
				GKE:                              i.RuntimeTarget.GKE,
				PriorityClassName:                i.RuntimeTarget.PriorityClassName,
				SafeToEvict:                      copyBoolPtr(i.RuntimeTarget.SafeToEvict),
				SharedDirStorageBackend:          i.RuntimeTarget.SharedDirStorageBackend,
				SharedDirStorageBackends:         copyStringMap(i.RuntimeTarget.SharedDirStorageBackends),
				HomeStorageBackend:               i.RuntimeTarget.HomeStorageBackend,
				HomeStorageLeaf:                  i.RuntimeTarget.HomeStorageLeaf,
			}
		}
		out = append(out, o)
	}
	return out
}

// RuntimeBrokerInstancesToGlobal maps settings instances to the server-config
// form exactly as ConvertV1ServerToGlobalConfig does, for the startup
// comparison against GlobalConfig.RuntimeBroker.Instances.
func RuntimeBrokerInstancesToGlobal(in []V1RuntimeBrokerInstanceConfig) []RuntimeBrokerInstanceConfig {
	return v1InstancesToGlobal(in)
}

// copyBoolPtr returns a new pointer to *b, or nil, so a tri-state value is
// never shared between two copies of a config.
func copyBoolPtr(b *bool) *bool {
	if b == nil {
		return nil
	}
	v := *b
	return &v
}

// copyStringMap returns a copy of m (nil for an empty map).
func copyStringMap(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// FlatInstanceSharedDirStorageReport describes the shared-dir storage
// backend policy of each flat Runtime Broker instance in vs
// (server.broker.instances), for the startup log. lines has one entry per
// instance whose backend comes from its runtime_target and one per shared
// dir its shared_dir_storage_backends names. warnings has one entry per
// instance whose backend for some dir differs from what the global
// settings resolve with no profile, which is how plugins resolve a shared
// dir's host path (runtime.ResolveSharedDirHostPath): a plugin would then
// use a different directory from the one the instance's agents mount.
// Call it on global settings.
func (vs *VersionedSettings) FlatInstanceSharedDirStorageReport() (lines, warnings []string) {
	if vs == nil || vs.Server == nil || vs.Server.Broker == nil {
		return nil, nil
	}
	label := func(backend string) string {
		if backend == "nfs" {
			return "nfs"
		}
		return "local"
	}
	globalBackend, _, _ := vs.ResolveSharedDirStorageBackend("", "")
	for _, inst := range vs.Server.Broker.Instances {
		t := inst.RuntimeTarget
		if t == nil {
			continue
		}
		view := vs.ForFlatInstance(NewFlatInstancePolicy(inst))
		var differs []string
		if t.SharedDirStorageBackend != "" {
			lines = append(lines, fmt.Sprintf("shared_dir_storage for instance %s: backend=%s (from %s)",
				inst.Key, label(t.SharedDirStorageBackend), view.flat.path("runtime_target.shared_dir_storage_backend")))
		}
		// Dirs no per-dir entry names: the instance's single value, else
		// the global backend, against the plugins' resolution.
		if single, _, _ := view.ResolveSharedDirStorageBackend("", ""); label(single) != label(globalBackend) {
			differs = append(differs, fmt.Sprintf("shared dirs without a per-dir entry: %s here, %s for plugins", label(single), label(globalBackend)))
		}
		dirs := make([]string, 0, len(t.SharedDirStorageBackends))
		for dir := range t.SharedDirStorageBackends {
			dirs = append(dirs, dir)
		}
		sort.Strings(dirs)
		for _, dir := range dirs {
			backend, source, _ := view.ResolveSharedDirStorageBackend("", dir)
			lines = append(lines, fmt.Sprintf("shared_dir_storage for instance %s, shared dir %s: backend=%s (from %s)",
				inst.Key, dir, label(backend), source))
			if plugin, _, _ := vs.ResolveSharedDirStorageBackend("", dir); label(plugin) != label(backend) {
				differs = append(differs, fmt.Sprintf("shared dir %s: %s here, %s for plugins", dir, label(backend), label(plugin)))
			}
		}
		if len(differs) > 0 {
			warnings = append(warnings, fmt.Sprintf("server.broker.instances[%s]: the instance's shared-dir storage backend differs from the one plugins resolve from the global settings (%s); a plugin would use a different directory from the one this instance's agents mount. Set the same backend in the global settings file to keep them in agreement",
				inst.Key, strings.Join(differs, "; ")))
		}
	}
	return lines, warnings
}
