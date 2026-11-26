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
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/util"
	yamlv3 "gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/api/resource"
)

// V1AgentPolicy is the operator's agent policy: ceilings and allow lists the
// Runtime Broker enforces on the final, fully resolved configuration of
// every agent it starts, whatever set each value (settings, profiles,
// harness configs, templates, the agent's own or inline config, or a Hub
// request). It is read only from the global settings file
// (LoadAgentPolicy): never from a project's settings, the Hub's settings
// overlay or SCION_* environment variables. Absent or empty means no policy.
type V1AgentPolicy struct {
	// AllowedImagePrefixes, when non-empty, lists the image reference
	// prefixes an agent's image must start with. References are compared
	// fully qualified (docker.io/library/ for a short name). A prefix that
	// does not end in "/" matches only at a reference boundary.
	AllowedImagePrefixes []string `json:"allowed_image_prefixes,omitempty" yaml:"allowed_image_prefixes,omitempty" koanf:"allowed_image_prefixes"`
	// MaxResources caps the agent's CPU and memory requests and limits. A
	// missing limit is set to the maximum.
	MaxResources *V1AgentPolicyResources `json:"max_resources,omitempty" yaml:"max_resources,omitempty" koanf:"max_resources"`
}

// V1AgentPolicyResources are the agent policy's resource maximums, in
// Kubernetes quantity notation (cpu: "8" or "500m"; memory: "8Gi").
type V1AgentPolicyResources struct {
	CPU    string `json:"cpu,omitempty" yaml:"cpu,omitempty" koanf:"cpu"`
	Memory string `json:"memory,omitempty" yaml:"memory,omitempty" koanf:"memory"`
}

// IsZero reports whether p sets no rule at all (today's behaviour).
func (p *V1AgentPolicy) IsZero() bool {
	if p == nil {
		return true
	}
	return len(p.AllowedImagePrefixes) == 0 &&
		(p.MaxResources == nil || (p.MaxResources.CPU == "" && p.MaxResources.Memory == ""))
}

// Validate checks the policy. An invalid policy refuses every start
// (LoadAgentPolicy), so a typo never silently disables a rule.
func (p *V1AgentPolicy) Validate() error {
	if p == nil {
		return nil
	}
	var errs []error
	for i, prefix := range p.AllowedImagePrefixes {
		if err := validateImagePrefix(prefix); err != nil {
			errs = append(errs, fmt.Errorf("agent_policy.allowed_image_prefixes[%d]: %w", i, err))
		}
	}
	if r := p.MaxResources; r != nil {
		if r.CPU != "" {
			if _, err := AgentPolicyCPUMax(r.CPU); err != nil {
				errs = append(errs, fmt.Errorf("agent_policy.max_resources.cpu: %w", err))
			}
		}
		if r.Memory != "" {
			if _, err := AgentPolicyMemoryMax(r.Memory); err != nil {
				errs = append(errs, fmt.Errorf("agent_policy.max_resources.memory: %w", err))
			}
		}
	}
	return errors.Join(errs...)
}

// validateImagePrefix rejects a prefix that cannot be a reference prefix.
func validateImagePrefix(prefix string) error {
	switch {
	case prefix == "":
		return errors.New("empty prefix")
	case strings.ContainsAny(prefix, " \t\r\n"):
		return fmt.Errorf("%q contains whitespace", prefix)
	case strings.Contains(prefix, "://"):
		return fmt.Errorf("%q has a URL scheme; give a registry/repository prefix", prefix)
	case strings.HasPrefix(prefix, "/"):
		return fmt.Errorf("%q starts with \"/\"", prefix)
	}
	return nil
}

// cpuInterpretations returns every reading of a CPU quantity by the parsers
// the runtimes use: util.ParseCPU (docker, podman, apple) and the Kubernetes
// quantity parser (kubernetes, cloudrun). It fails when neither parses.
func cpuInterpretations(s string) ([]float64, error) {
	var out []float64
	var firstErr error
	if v, err := util.ParseCPU(s); err == nil {
		out = append(out, v)
	} else {
		firstErr = err
	}
	if q, err := resource.ParseQuantity(strings.TrimSpace(s)); err == nil {
		out = append(out, q.AsApproximateFloat64())
	} else if firstErr == nil {
		firstErr = err
	}
	if len(out) == 0 {
		return nil, firstErr
	}
	return out, nil
}

// memoryInterpretations is cpuInterpretations for a memory quantity
// (util.ParseMemory and the Kubernetes quantity parser), in bytes.
func memoryInterpretations(s string) ([]float64, error) {
	var out []float64
	var firstErr error
	if v, err := util.ParseMemory(s); err == nil {
		out = append(out, float64(v))
	} else {
		firstErr = err
	}
	if q, err := resource.ParseQuantity(strings.TrimSpace(s)); err == nil {
		out = append(out, q.AsApproximateFloat64())
	} else if firstErr == nil {
		firstErr = err
	}
	if len(out) == 0 {
		return nil, firstErr
	}
	return out, nil
}

func minOf(vs []float64) float64 {
	m := math.Inf(1)
	for _, v := range vs {
		m = math.Min(m, v)
	}
	return m
}

func maxOf(vs []float64) float64 {
	m := math.Inf(-1)
	for _, v := range vs {
		m = math.Max(m, v)
	}
	return m
}

// AgentPolicyCPUMax parses a max_resources.cpu value, in cores. Both
// parsers must accept it, and the smaller reading is the maximum.
func AgentPolicyCPUMax(s string) (float64, error) {
	return policyMax(s, cpuInterpretations, 2)
}

// AgentPolicyMemoryMax parses a max_resources.memory value, in bytes. Both
// parsers must accept it, and the smaller reading is the maximum.
func AgentPolicyMemoryMax(s string) (float64, error) {
	return policyMax(s, memoryInterpretations, 2)
}

func policyMax(s string, parse func(string) ([]float64, error), want int) (float64, error) {
	vs, err := parse(s)
	if err != nil {
		return 0, err
	}
	if len(vs) != want {
		return 0, fmt.Errorf("%q is not a quantity every runtime reads the same way (e.g. \"8\", \"500m\", \"8Gi\")", s)
	}
	m := minOf(vs)
	if !(m > 0) || math.IsInf(m, 0) {
		return 0, fmt.Errorf("%q must be a positive quantity", s)
	}
	// "512m" is 512 MB to one parser and half a byte to the other.
	if maxOf(vs) > m*1.001 {
		return 0, fmt.Errorf("%q is read differently by the runtimes; use a Kubernetes quantity such as \"8\", \"500m\" (cpu) or \"8Gi\" (memory)", s)
	}
	return m, nil
}

// AgentPolicyCPUValue parses an agent's CPU request or limit, in cores: the
// largest reading of any runtime parser, so no runtime grants more than
// the policy compared.
func AgentPolicyCPUValue(s string) (float64, error) {
	vs, err := cpuInterpretations(s)
	if err != nil {
		return 0, err
	}
	return maxOf(vs), nil
}

// AgentPolicyCPUMinValue is the smallest reading of an agent's CPU value by
// any runtime parser, in cores.
func AgentPolicyCPUMinValue(s string) (float64, error) {
	vs, err := cpuInterpretations(s)
	if err != nil {
		return 0, err
	}
	return minOf(vs), nil
}

// AgentPolicyMemoryMinValue is AgentPolicyCPUMinValue for memory, in bytes
// (util.ParseMemory's reading is whole bytes, as Docker receives it).
func AgentPolicyMemoryMinValue(s string) (float64, error) {
	vs, err := memoryInterpretations(s)
	if err != nil {
		return 0, err
	}
	return minOf(vs), nil
}

// AgentPolicyMemoryValue is AgentPolicyCPUValue for memory, in bytes.
func AgentPolicyMemoryValue(s string) (float64, error) {
	vs, err := memoryInterpretations(s)
	if err != nil {
		return 0, err
	}
	return maxOf(vs), nil
}

// checkAgentPolicyKnownKeys decodes the raw global settings file's
// agent_policy block strictly, so a misspelled key (max_resource:,
// allowed_image_prefix:) is an error instead of a rule silently left out:
// the settings loader itself ignores unknown keys.
func checkAgentPolicyKnownKeys() error {
	globalDir, err := GetGlobalDir()
	if err != nil {
		return err
	}
	path := GetSettingsPath(globalDir)
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	var doc struct {
		AgentPolicy map[string]any `yaml:"agent_policy"`
	}
	if err := yamlv3.Unmarshal(data, &doc); err != nil {
		return err
	}
	var errs []error
	for k, v := range doc.AgentPolicy {
		sub, known := agentPolicyKnownKeys[k]
		if !known {
			errs = append(errs, fmt.Errorf("unknown key agent_policy.%s", k))
			continue
		}
		if sub == nil {
			continue
		}
		m, ok := v.(map[string]any)
		if !ok {
			if v != nil {
				errs = append(errs, fmt.Errorf("agent_policy.%s must be a mapping", k))
			}
			continue
		}
		for sk := range m {
			if !sub[sk] {
				errs = append(errs, fmt.Errorf("unknown key agent_policy.%s.%s", k, sk))
			}
		}
	}
	return errors.Join(errs...)
}

// agentPolicyKnownKeys are V1AgentPolicy's keys, with the keys of its
// nested blocks (nil for a scalar or list key).
var agentPolicyKnownKeys = map[string]map[string]bool{
	"allowed_image_prefixes": nil,
	"max_resources":          {"cpu": true, "memory": true},
}

// agentPolicyKey is the settings key LoadAgentPolicy looks for in the raw
// global settings file when the file cannot be loaded.
const agentPolicyKey = "agent_policy"

// LoadAgentPolicy returns the agent policy from the global settings file
// only (LoadGlobalSettings: no project settings, no Hub settings overlay, no
// SCION_* environment provider), so nothing a project, a requester or the
// process environment controls can loosen it. It is read on every call: a
// change applies to the next agent start without a restart.
//
// It returns nil when no policy is configured. It fails closed: an invalid
// policy, or a global settings file that cannot be loaded (or loads in the
// legacy format) while mentioning agent_policy, is an error, and the caller
// refuses the start.
func LoadAgentPolicy() (*V1AgentPolicy, error) {
	vs, _, err := LoadGlobalSettings()
	if err != nil {
		if GlobalSettingsMentions(agentPolicyKey) {
			return nil, fmt.Errorf("the global settings file sets %s but cannot be loaded: %w", agentPolicyKey, err)
		}
		return nil, nil
	}
	if GlobalSettingsMentions(agentPolicyKey) {
		if err := checkAgentPolicyKnownKeys(); err != nil {
			return nil, fmt.Errorf("invalid %s in the global settings file: %w", agentPolicyKey, err)
		}
	}
	if vs == nil || vs.AgentPolicy == nil {
		if GlobalSettingsIsLegacyFormat() && GlobalSettingsMentions(agentPolicyKey+":") {
			return nil, fmt.Errorf("the global settings file sets %s but is in the legacy format; %s needs schema_version: \"1\"", agentPolicyKey, agentPolicyKey)
		}
		return nil, nil
	}
	if err := vs.AgentPolicy.Validate(); err != nil {
		return nil, fmt.Errorf("invalid %s in the global settings file: %w", agentPolicyKey, err)
	}
	if vs.AgentPolicy.IsZero() {
		return nil, nil
	}
	p := *vs.AgentPolicy
	return &p, nil
}
