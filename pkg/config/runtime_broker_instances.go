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

// MaxRuntimeBrokerInstances is the number of instances one process may host
// in this release (P2 lifts it).
const MaxRuntimeBrokerInstances = 1

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
func (e *RuntimeBrokerHostingError) Code() string { return api.ErrCodeFlatRuntimeBrokerRemoteUnsupported }

func (e *RuntimeBrokerHostingError) Error() string {
	return fmt.Sprintf("%s: server.broker.instances requires the Hub in the same process in this release; "+
		"remote flat Runtime Broker hosting arrives in P2 (ptone/scion#3271). "+
		"Remove server.broker.instances to run this host as a legacy Runtime Broker (instance %q)",
		api.ErrCodeFlatRuntimeBrokerRemoteUnsupported, e.InstanceKey)
}

// ValidateRuntimeBrokerInstances checks server.broker.instances and returns
// one ValidationError per problem, each naming its settings path. Duplicate
// keys are reported even when the count rule also fails.
func ValidateRuntimeBrokerInstances(instances []V1RuntimeBrokerInstanceConfig) []ValidationError {
	var errs []ValidationError
	if len(instances) > MaxRuntimeBrokerInstances {
		errs = append(errs, ValidationError{
			Path:    "server.broker.instances",
			Message: fmt.Sprintf("only %d Runtime Broker instance is supported in this release", MaxRuntimeBrokerInstances),
		})
	}
	firstIndex := map[string]int{}
	for i, inst := range instances {
		p := fmt.Sprintf("server.broker.instances[%d]", i)
		if inst.Key == "" || !runtimeBrokerInstanceKeyRe.MatchString(inst.Key) {
			errs = append(errs, ValidationError{Path: p + ".key",
				Message: fmt.Sprintf("invalid instance key %q (must match %s)", inst.Key, RuntimeBrokerInstanceKeyPattern)})
		} else if j, dup := firstIndex[inst.Key]; dup {
			errs = append(errs, ValidationError{Path: p + ".key",
				Message: fmt.Sprintf("duplicate instance key %q (also at index %d)", inst.Key, j)})
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
			if t.Context != "" {
				errs = append(errs, ValidationError{Path: p + ".runtime_target.context",
					Message: "field not valid for runtime target type docker"})
			}
			if t.Namespace != "" {
				errs = append(errs, ValidationError{Path: p + ".runtime_target.namespace",
					Message: "field not valid for runtime target type docker"})
			}
		case RuntimeTargetTypeKubernetes:
			errs = append(errs, ValidationError{Path: p + ".runtime_target.type",
				Message: "Kubernetes Runtime Broker instances are not implemented yet"})
		default:
			errs = append(errs, ValidationError{Path: p + ".runtime_target.type",
				Message: fmt.Sprintf("unsupported runtime target type %q (supported: docker)", t.Type)})
		}
	}
	sort.SliceStable(errs, func(a, b int) bool { return errs[a].Path < errs[b].Path })
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
	brokerRaw, ok := raw["broker"].(map[string]interface{})
	if !ok {
		return nil, nil
	}
	instRaw, ok := brokerRaw["instances"]
	if !ok || instRaw == nil {
		return nil, nil
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
				Type:        i.RuntimeTarget.Type,
				DisplayName: i.RuntimeTarget.DisplayName,
				Context:     i.RuntimeTarget.Context,
				Namespace:   i.RuntimeTarget.Namespace,
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
				Type:        i.RuntimeTarget.Type,
				DisplayName: i.RuntimeTarget.DisplayName,
				Context:     i.RuntimeTarget.Context,
				Namespace:   i.RuntimeTarget.Namespace,
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
