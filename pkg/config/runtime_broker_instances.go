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
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
	yamlv3 "gopkg.in/yaml.v3"

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
		if d := inst.AgentDefaults; d != nil {
			errs = append(errs, validateInstanceAgentDefaults(p+".agent_defaults", d)...)
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
				{"shared_dir_storage_class", t.SharedDirStorageClass}, {"shared_dir_size", t.SharedDirSize}} {
				if field, v := f.field, f.v; v != "" {
					errs = append(errs, ValidationError{Path: p + ".runtime_target." + field,
						Message: "field not valid for runtime target type docker"})
				}
			}
			if len(t.KubernetesServiceAccountMappings) > 0 {
				errs = append(errs, ValidationError{Path: p + ".runtime_target.kubernetes_service_account_mappings",
					Message: "field not valid for runtime target type docker"})
			}
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
		default:
			errs = append(errs, ValidationError{Path: p + ".runtime_target.type",
				Message: fmt.Sprintf("unsupported runtime target type %q (supported: docker, kubernetes)", t.Type)})
		}
	}
	sort.SliceStable(errs, func(a, b int) bool { return errs[a].Path < errs[b].Path })
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
	return instances, nil
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
				"shared_dir_storage_class", "shared_dir_size"); err != nil {
				return err
			}
		}
		if d, present := m["agent_defaults"]; present && d != nil {
			dm, ok := d.(map[string]interface{})
			if !ok {
				return fmt.Errorf("%s.agent_defaults must be a mapping", p)
			}
			if err := checkStrings(p+".agent_defaults", dm, "default_harness_config", "image_registry"); err != nil {
				return err
			}
			overrides, _ := dm["harness_overrides"].(map[string]interface{})
			names := make([]string, 0, len(overrides))
			for name := range overrides {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				om, _ := overrides[name].(map[string]interface{})
				op := p + ".agent_defaults.harness_overrides." + name
				if err := checkStrings(op, om, "image", "image_pull_policy", "user", "auth_selected_type"); err != nil {
					return err
				}
				env, _ := om["env"].(map[string]interface{})
				envKeys := make([]string, 0, len(env))
				for k := range env {
					envKeys = append(envKeys, k)
				}
				sort.Strings(envKeys)
				if err := checkStrings(op+".env", env, envKeys...); err != nil {
					return err
				}
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
	reservedInstanceKeys      []string
	reservedRuntimeTargetKeys = []string{
		"gke", "priority_class_name", "safe_to_evict",
		"shared_dir_storage_backend", "shared_dir_storage_backends",
		"home_storage_backend", "home_storage_leaf",
	}
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
	knownAgentDefaultsKeys = knownYAMLKeys(V1InstanceAgentDefaults{})
	knownHarnessOverKeys   = knownYAMLKeys(V1HarnessOverride{})
	knownVolumeMountKeys   = knownYAMLKeys(api.VolumeMount{})
	knownSecretKeys        = knownYAMLKeys(api.RequiredSecret{})
	knownResourceSpecKeys  = knownYAMLKeys(api.ResourceSpec{})
	knownResourceListKeys  = knownYAMLKeys(api.ResourceList{})
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
		if dm, ok := m["agent_defaults"].(map[string]interface{}); ok {
			if err := checkAgentDefaultsKeys(p+".agent_defaults", dm, check); err != nil {
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

var (
	agentDefaultsSchemaOnce sync.Once
	agentDefaultsSchema     *jsonschema.Schema
	agentDefaultsSchemaErr  error
)

// validateInstanceAgentDefaults checks d against the settings schema's
// $defs/instanceAgentDefaults, which reuses the definitions profiles are
// validated with (scion config validate): harnessOverride, volumeMount,
// requiredSecret, resourceSpec and the clone_depth pattern. For example an
// out-of-range image_pull_policy, auth_selected_type, volume type or secret
// type is refused. A volume target and a secret key, which the schema
// requires but a marshalled struct always carries, must be non-empty. Each
// problem is one ValidationError whose path is under path.
func validateInstanceAgentDefaults(path string, d *V1InstanceAgentDefaults) []ValidationError {
	var errs []ValidationError
	for i, v := range d.Volumes {
		if v.Target == "" {
			errs = append(errs, ValidationError{Path: fmt.Sprintf("%s.volumes[%d].target", path, i), Message: "target is required"})
		}
	}
	for i, sec := range d.Secrets {
		if sec.Key == "" {
			errs = append(errs, ValidationError{Path: fmt.Sprintf("%s.secrets[%d].key", path, i), Message: "key is required"})
		}
	}
	names := make([]string, 0, len(d.HarnessOverrides))
	for name := range d.HarnessOverrides {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		for i, v := range d.HarnessOverrides[name].Volumes {
			if v.Target == "" {
				errs = append(errs, ValidationError{Path: fmt.Sprintf("%s.harness_overrides.%s.volumes[%d].target", path, name, i), Message: "target is required"})
			}
		}
	}

	agentDefaultsSchemaOnce.Do(func() {
		schemaPath := settingsSchemaFiles["1"]
		data, err := schemasFS.ReadFile(schemaPath)
		if err != nil {
			agentDefaultsSchemaErr = err
			return
		}
		var doc interface{}
		if err := json.Unmarshal(data, &doc); err != nil {
			agentDefaultsSchemaErr = err
			return
		}
		compiler := jsonschema.NewCompiler()
		if err := compiler.AddResource(schemaPath, doc); err != nil {
			agentDefaultsSchemaErr = err
			return
		}
		agentDefaultsSchema, agentDefaultsSchemaErr = compiler.Compile(schemaPath + "#/$defs/instanceAgentDefaults")
	})
	if agentDefaultsSchemaErr != nil {
		return append(errs, ValidationError{Path: path, Message: "cannot load the agent_defaults schema: " + agentDefaultsSchemaErr.Error()})
	}
	raw, err := json.Marshal(d)
	if err != nil {
		return append(errs, ValidationError{Path: path, Message: err.Error()})
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return append(errs, ValidationError{Path: path, Message: err.Error()})
	}
	if err := agentDefaultsSchema.Validate(inst); err != nil {
		for _, e := range extractValidationErrors(err) {
			e.Path = path + settingsPathSuffix(e.Path)
			errs = append(errs, e)
		}
	}
	return errs
}

// settingsPathSuffix turns a JSON pointer without its leading "/" (for
// example "harness_overrides/claude/volumes/0/type") into the settings path
// style the loader's other errors use (".harness_overrides.claude.volumes[0].type").
func settingsPathSuffix(pointer string) string {
	if pointer == "" {
		return ""
	}
	var b strings.Builder
	for _, seg := range strings.Split(pointer, "/") {
		seg = strings.ReplaceAll(strings.ReplaceAll(seg, "~1", "/"), "~0", "~")
		if _, err := strconv.Atoi(seg); err == nil {
			b.WriteString("[" + seg + "]")
		} else {
			b.WriteString("." + seg)
		}
	}
	return b.String()
}

// checkAgentDefaultsKeys refuses every unknown key of an instance's
// agent_defaults, of each of its harness overrides, and of each resources
// block, volume and secret declaration in them, naming its settings path.
func checkAgentDefaultsKeys(path string, dm map[string]interface{}, check func(string, map[string]interface{}, map[string]bool, []string) error) error {
	if err := check(path, dm, knownAgentDefaultsKeys, nil); err != nil {
		return err
	}
	checkList := func(path string, v interface{}, known map[string]bool) error {
		list, _ := v.([]interface{})
		for i, e := range list {
			if em, ok := e.(map[string]interface{}); ok {
				if err := check(fmt.Sprintf("%s[%d]", path, i), em, known, nil); err != nil {
					return err
				}
			}
		}
		return nil
	}
	checkResources := func(path string, v interface{}) error {
		rm, ok := v.(map[string]interface{})
		if !ok {
			return nil
		}
		if err := check(path, rm, knownResourceSpecKeys, nil); err != nil {
			return err
		}
		for _, list := range []string{"limits", "requests"} {
			if lm, ok := rm[list].(map[string]interface{}); ok {
				if err := check(path+"."+list, lm, knownResourceListKeys, nil); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := checkResources(path+".resources", dm["resources"]); err != nil {
		return err
	}
	if err := checkList(path+".volumes", dm["volumes"], knownVolumeMountKeys); err != nil {
		return err
	}
	if err := checkList(path+".secrets", dm["secrets"], knownSecretKeys); err != nil {
		return err
	}
	overrides, _ := dm["harness_overrides"].(map[string]interface{})
	names := make([]string, 0, len(overrides))
	for name := range overrides {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		om, ok := overrides[name].(map[string]interface{})
		if !ok {
			continue
		}
		op := path + ".harness_overrides." + name
		if err := check(op, om, knownHarnessOverKeys, nil); err != nil {
			return err
		}
		if err := checkResources(op+".resources", om["resources"]); err != nil {
			return err
		}
		if err := checkList(op+".volumes", om["volumes"], knownVolumeMountKeys); err != nil {
			return err
		}
	}
	return nil
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
			}
		}
		if d := copyInstanceAgentDefaults(i.AgentDefaults); d != nil {
			o.AgentDefaults = &InstanceAgentDefaultsConfig{
				DefaultHarnessConfig: d.DefaultHarnessConfig,
				ImageRegistry:        d.ImageRegistry,
				Volumes:              d.Volumes,
				Resources:            d.Resources,
				HarnessOverrides:     d.HarnessOverrides,
				Secrets:              d.Secrets,
				CloneDepth:           d.CloneDepth,
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
			}
		}
		if i.AgentDefaults != nil {
			o.AgentDefaults = copyInstanceAgentDefaults(&V1InstanceAgentDefaults{
				DefaultHarnessConfig: i.AgentDefaults.DefaultHarnessConfig,
				ImageRegistry:        i.AgentDefaults.ImageRegistry,
				Volumes:              i.AgentDefaults.Volumes,
				Resources:            i.AgentDefaults.Resources,
				HarnessOverrides:     i.AgentDefaults.HarnessOverrides,
				Secrets:              i.AgentDefaults.Secrets,
				CloneDepth:           i.AgentDefaults.CloneDepth,
			})
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

// copyInstanceAgentDefaults returns a deep copy of d (nil for nil): no map,
// slice or pointer is shared with d. Empty maps and slices copy as nil.
func copyInstanceAgentDefaults(d *V1InstanceAgentDefaults) *V1InstanceAgentDefaults {
	if d == nil {
		return nil
	}
	out := *d
	out.Volumes = copyVolumeMounts(d.Volumes)
	out.Resources = copyResourceSpec(d.Resources)
	out.Secrets = copyRequiredSecrets(d.Secrets)
	out.HarnessOverrides = nil
	if len(d.HarnessOverrides) > 0 {
		out.HarnessOverrides = make(map[string]V1HarnessOverride, len(d.HarnessOverrides))
		for name, o := range d.HarnessOverrides {
			o.Env = copyStringMap(o.Env)
			o.Volumes = copyVolumeMounts(o.Volumes)
			o.Resources = copyResourceSpec(o.Resources)
			out.HarnessOverrides[name] = o
		}
	}
	return &out
}

func copyVolumeMounts(v []api.VolumeMount) []api.VolumeMount {
	if len(v) == 0 {
		return nil
	}
	return append([]api.VolumeMount(nil), v...)
}

func copyResourceSpec(r *api.ResourceSpec) *api.ResourceSpec {
	if r == nil {
		return nil
	}
	cpy := *r
	return &cpy
}

func copyRequiredSecrets(s []api.RequiredSecret) []api.RequiredSecret {
	if len(s) == 0 {
		return nil
	}
	out := make([]api.RequiredSecret, len(s))
	for i, sec := range s {
		if len(sec.AlternativeEnvKeys) > 0 {
			sec.AlternativeEnvKeys = append([]string(nil), sec.AlternativeEnvKeys...)
		} else {
			sec.AlternativeEnvKeys = nil
		}
		out[i] = sec
	}
	return out
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
