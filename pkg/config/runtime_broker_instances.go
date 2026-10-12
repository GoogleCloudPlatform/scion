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
	"regexp"
	"sort"
	"strings"

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
		t := inst.RuntimeTarget
		if t == nil || t.Type == "" {
			errs = append(errs, ValidationError{Path: p + ".runtime_target.type", Message: "runtime_target.type is required"})
			continue
		}
		switch t.Type {
		case RuntimeTargetTypeDocker:
			for _, f := range []struct{ field, v string }{{"context", t.Context}, {"namespace", t.Namespace}, {"kubeconfig", t.Kubeconfig},
				{"kubernetes_block_service_account", t.KubernetesBlockServiceAccount}} {
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
			if err := checkStrings(p+".runtime_target", tm, "type", "display_name", "context", "namespace"); err != nil {
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
