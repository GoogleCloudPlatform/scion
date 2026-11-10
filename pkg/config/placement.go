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
	"maps"
	"slices"
	"sort"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"k8s.io/apimachinery/pkg/util/validation"
)

// Kubernetes placement defaults: helpers for merging and validating the node
// selector, tolerations and runtime class that an agent runtime's settings
// apply to agent pods. Every function here returns fresh values and never
// mutates its inputs.

// Fixed causes returned by CheckLegacyPlacement. They name the remedies and
// carry no settings paths.
const (
	legacyMatchAllTolerationCause = "kubernetes.tolerations[%d] in the agent config matches all taints, which is not allowed when the agent runtime configures placement; replace the agent's toleration list without that entry, or re-create the agent"
	legacyRuntimeClassCause       = "kubernetes.runtimeClassName in the agent config differs from the agent runtime's runtime_class_name; set runtime_class_name to that value on the runtime entry or a profile, or re-create the agent"
)

// MergeNodeSelector merges two node selectors key by key; upper wins on a
// shared key. It returns a fresh map, or nil when both inputs are empty.
func MergeNodeSelector(lower, upper map[string]string) map[string]string {
	if len(lower) == 0 && len(upper) == 0 {
		return nil
	}
	out := make(map[string]string, len(lower)+len(upper))
	for k, v := range lower {
		out[k] = v
	}
	for k, v := range upper {
		out[k] = v
	}
	return out
}

// tolerationIdentity is the de-duplication key for tolerations. An empty
// operator means Equal, as on the API server.
type tolerationIdentity struct {
	key, operator, value, effect string
}

func identityOfToleration(t api.K8sToleration) tolerationIdentity {
	op := t.Operator
	if op == "" {
		op = "Equal"
	}
	return tolerationIdentity{key: t.Key, operator: op, value: t.Value, effect: t.Effect}
}

// MergeTolerations returns the union of two toleration lists: upper's
// entries first, then lower's, each kept at its first occurrence and
// de-duplicated by (Key, Operator with "" as Equal, Value, Effect). It
// returns a fresh slice, or nil when the union is empty.
func MergeTolerations(lower, upper []api.K8sToleration) []api.K8sToleration {
	if len(lower) == 0 && len(upper) == 0 {
		return nil
	}
	out := make([]api.K8sToleration, 0, len(lower)+len(upper))
	seen := make(map[tolerationIdentity]struct{}, len(lower)+len(upper))
	for _, list := range [][]api.K8sToleration{upper, lower} {
		for _, t := range list {
			id := identityOfToleration(t)
			if _, dup := seen[id]; dup {
				continue
			}
			seen[id] = struct{}{}
			out = append(out, t)
		}
	}
	return out
}

// mergeRuntimeClassName returns upper when it is set, else lower.
func mergeRuntimeClassName(lower, upper string) string {
	if upper != "" {
		return upper
	}
	return lower
}

// cloneKubernetesConfig returns a deep copy of k (nil for nil).
func cloneKubernetesConfig(k *api.KubernetesConfig) *api.KubernetesConfig {
	if k == nil {
		return nil
	}
	out := *k
	out.NodeSelector = maps.Clone(k.NodeSelector)
	out.Tolerations = slices.Clone(k.Tolerations)
	if k.Resources != nil {
		res := api.K8sResources{
			Requests: maps.Clone(k.Resources.Requests),
			Limits:   maps.Clone(k.Resources.Limits),
		}
		out.Resources = &res
	}
	if k.SafeToEvict != nil {
		v := *k.SafeToEvict
		out.SafeToEvict = &v
	}
	return &out
}

// isMatchAllToleration reports whether t tolerates every taint: an empty
// key with operator Exists, whatever the effect.
func isMatchAllToleration(t api.K8sToleration) bool {
	return t.Key == "" && t.Operator == "Exists"
}

// CheckLegacyPlacement checks the agent's own kubernetes block against the
// resolved placement. When placement is not in effect it always returns
// nil. While it is in effect, the block must not carry an empty-key Exists
// toleration (whatever its effect), and its runtimeClassName, when set,
// must equal the resolved runtime class (which must then be set). The error
// is a fixed cause.
func CheckLegacyPlacement(block *api.KubernetesConfig, d *api.ResolvedPlacement) error {
	if !d.InEffect() || block == nil {
		return nil
	}
	for i, t := range block.Tolerations {
		if isMatchAllToleration(t) {
			return fmt.Errorf(legacyMatchAllTolerationCause, i)
		}
	}
	if block.RuntimeClassName != "" && block.RuntimeClassName != d.RuntimeClassName {
		return errors.New(legacyRuntimeClassCause)
	}
	return nil
}

// ApplyPlacement applies the resolved placement d over the agent's own
// kubernetes block base and returns a fresh block; neither input is
// mutated, and the result shares no map or slice with them.
//
// When placement is not in effect, the result is a copy of base (nil for
// nil) with its values unchanged, and there are no warnings. While it is in
// effect, CheckLegacyPlacement must pass; then the placement's selector
// keys win over the agent's (a differing agent value is ignored with a
// warning, and other agent keys are kept), tolerations are merged with the
// placement's first, and the runtime class is the agent's (equal) value or
// else the placement's.
//
// Operator-owned placement (d.OperatorOwned) is applied by
// ApplyOperatorPlacement instead, checked against the agent config.
func ApplyPlacement(base *api.KubernetesConfig, d *api.ResolvedPlacement) (*api.KubernetesConfig, []string, error) {
	if d != nil && d.OperatorOwned {
		out, err := ApplyOperatorPlacement(base, d, AgentPlacementLayerConfig)
		return out, nil, err
	}
	if !d.InEffect() {
		return cloneKubernetesConfig(base), nil, nil
	}
	if err := CheckLegacyPlacement(base, d); err != nil {
		return nil, nil, err
	}
	out := cloneKubernetesConfig(base)
	if out == nil {
		out = &api.KubernetesConfig{}
	}
	var warnings []string
	agentKeys := make([]string, 0, len(out.NodeSelector))
	for k := range out.NodeSelector {
		agentKeys = append(agentKeys, k)
	}
	sort.Strings(agentKeys)
	for _, k := range agentKeys {
		if dv, ok := d.NodeSelector[k]; ok && dv != out.NodeSelector[k] {
			warnings = append(warnings, fmt.Sprintf("kubernetes.nodeSelector key %q in the agent config is ignored; the agent runtime's placement sets this key", k))
		}
	}
	out.NodeSelector = MergeNodeSelector(out.NodeSelector, d.NodeSelector)
	out.Tolerations = MergeTolerations(out.Tolerations, d.Tolerations)
	out.RuntimeClassName = mergeRuntimeClassName(out.RuntimeClassName, d.RuntimeClassName)
	return out, warnings, nil
}

// ValidateNodeSelector checks a node selector as the API server does: each
// key must be a qualified name and each value a valid label value.
func ValidateNodeSelector(selector map[string]string) error {
	keys := make([]string, 0, len(selector))
	for k := range selector {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var errs []error
	for _, k := range keys {
		if msgs := validation.IsQualifiedName(k); len(msgs) > 0 {
			errs = append(errs, fmt.Errorf("node selector key %q: %s", k, strings.Join(msgs, "; ")))
		}
		if msgs := validation.IsValidLabelValue(selector[k]); len(msgs) > 0 {
			errs = append(errs, fmt.Errorf("node selector value for key %q: %s", k, strings.Join(msgs, "; ")))
		}
	}
	return errors.Join(errs...)
}

// ValidateTolerations checks tolerations as the API server does: a
// non-empty key must be a qualified name; an empty key requires operator
// Exists; the operator is "", Equal or Exists; an Equal (or "") value must
// be a valid label value and an Exists value must be empty; the effect is
// "", NoSchedule, PreferNoSchedule or NoExecute.
func ValidateTolerations(tolerations []api.K8sToleration) error {
	var errs []error
	for i, t := range tolerations {
		if t.Key != "" {
			if msgs := validation.IsQualifiedName(t.Key); len(msgs) > 0 {
				errs = append(errs, fmt.Errorf("tolerations[%d].key %q: %s", i, t.Key, strings.Join(msgs, "; ")))
			}
		} else if t.Operator != "Exists" {
			errs = append(errs, fmt.Errorf("tolerations[%d].operator must be Exists when the key is empty", i))
		}
		switch t.Operator {
		case "", "Equal":
			if msgs := validation.IsValidLabelValue(t.Value); len(msgs) > 0 {
				errs = append(errs, fmt.Errorf("tolerations[%d].value %q: %s", i, t.Value, strings.Join(msgs, "; ")))
			}
		case "Exists":
			if t.Value != "" {
				errs = append(errs, fmt.Errorf("tolerations[%d].value must be empty when the operator is Exists", i))
			}
		default:
			errs = append(errs, fmt.Errorf("tolerations[%d].operator %q is not supported (use Equal or Exists)", i, t.Operator))
		}
		switch t.Effect {
		case "", "NoSchedule", "PreferNoSchedule", "NoExecute":
		default:
			errs = append(errs, fmt.Errorf("tolerations[%d].effect %q is not supported (use NoSchedule, PreferNoSchedule or NoExecute)", i, t.Effect))
		}
	}
	return errors.Join(errs...)
}

// ValidateRuntimeClassName checks a runtime class name as the API server
// does (a DNS-1123 subdomain). The empty string means unset and is valid.
func ValidateRuntimeClassName(name string) error {
	if name == "" {
		return nil
	}
	if msgs := validation.IsDNS1123Subdomain(name); len(msgs) > 0 {
		return fmt.Errorf("runtime class name %q: %s", name, strings.Join(msgs, "; "))
	}
	return nil
}

// PoolNodeSelectorKey is the node label key that selects a node pool for
// agent pods. A differing agent value for it is reported as a pool
// conflict.
const PoolNodeSelectorKey = "scion.dev/pool"

// Layers named by CheckAgentPlacement's errors: where the agent's own
// kubernetes block comes from.
const (
	AgentPlacementLayerRequest = "the request"
	AgentPlacementLayerStored  = "the agent's stored config"
	AgentPlacementLayerConfig  = "the agent config"
)

// CheckAgentPlacement checks the agent's own kubernetes block against
// operator-owned placement d (nil means none). layer names where the block
// comes from (AgentPlacementLayerRequest, ...). The block's placement
// fields are validated even when d has none. Then, an empty-key Exists
// toleration is refused while d is in effect; a node selector key that d
// sets must have d's value (other keys are allowed); each toleration must
// be one of d's, compared by (key, operator with "" as Equal, value,
// effect), so any toleration is refused when d sets none (including an
// empty-key Exists toleration with no placement at all); and a set runtime
// class must equal d's, which must then be set.
// Errors name fields by key or index and never carry submitted values.
func CheckAgentPlacement(block *api.KubernetesConfig, d *api.ResolvedPlacement, layer string) error {
	if block == nil {
		return nil
	}
	var opSelector map[string]string
	var opTolerations []api.K8sToleration
	var opRuntimeClass string
	if d != nil {
		opSelector, opTolerations, opRuntimeClass = d.NodeSelector, d.Tolerations, d.RuntimeClassName
	}

	keys := make([]string, 0, len(block.NodeSelector))
	for k := range block.NodeSelector {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for i, k := range keys {
		if ValidateNodeSelector(map[string]string{k: block.NodeSelector[k]}) != nil {
			return fmt.Errorf("kubernetes.nodeSelector entry %d (in key order) in %s is not a valid Kubernetes node label", i, layer)
		}
	}
	for i, t := range block.Tolerations {
		if ValidateTolerations([]api.K8sToleration{t}) != nil {
			return fmt.Errorf("kubernetes.tolerations[%d] in %s is not a valid toleration", i, layer)
		}
	}
	if ValidateRuntimeClassName(block.RuntimeClassName) != nil {
		return fmt.Errorf("kubernetes.runtimeClassName in %s is not a valid RuntimeClass name", layer)
	}

	if d.InEffect() {
		for i, t := range block.Tolerations {
			if isMatchAllToleration(t) {
				return fmt.Errorf("kubernetes.tolerations[%d] in %s matches all taints, which is not allowed when the agent runtime configures placement; remove that entry", i, layer)
			}
		}
	}
	for _, k := range keys {
		ov, ok := opSelector[k]
		if !ok || ov == block.NodeSelector[k] {
			continue
		}
		if k == PoolNodeSelectorKey {
			return fmt.Errorf("pool selector conflicts with inherited placement: kubernetes.nodeSelector key %q in %s differs from the agent runtime's placement; remove it or use the same value", k, layer)
		}
		return fmt.Errorf("kubernetes.nodeSelector key %q in %s conflicts with the agent runtime's placement; remove it or use the same value", k, layer)
	}
	if len(block.Tolerations) > 0 && len(opTolerations) == 0 {
		return fmt.Errorf("kubernetes.tolerations[0] in %s is set, but the agent runtime configures no tolerations; add the toleration to the agent runtime's placement or remove it from the agent config", layer)
	}
	allowed := tolerationSet(opTolerations)
	for i, t := range block.Tolerations {
		if _, ok := allowed[identityOfToleration(t)]; !ok {
			return fmt.Errorf("kubernetes.tolerations[%d] in %s is not one of the agent runtime's tolerations; add it to the agent runtime's placement or remove it from the agent config", i, layer)
		}
	}
	if block.RuntimeClassName != "" && block.RuntimeClassName != opRuntimeClass {
		if opRuntimeClass == "" {
			return fmt.Errorf("kubernetes.runtimeClassName in %s is set, but the agent runtime configures no runtime class; the runtime class comes from the agent runtime's placement, so remove it from the agent config", layer)
		}
		return fmt.Errorf("kubernetes.runtimeClassName in %s differs from the agent runtime's runtime_class_name; remove it from the agent config", layer)
	}
	return nil
}

// ApplyOperatorPlacement applies operator-owned placement d (nil means
// none) over the agent's own kubernetes block base and returns a fresh
// block; neither input is mutated, and the result shares no map or slice
// with them. CheckAgentPlacement must pass first (layer names the block's
// source). The result keeps the agent's other node selector keys and adds
// d's; its tolerations are d's (CheckAgentPlacement has made the agent's
// all members of them); and its runtime class is d's. Unlike ApplyPlacement, nothing is ignored with a warning.
func ApplyOperatorPlacement(base *api.KubernetesConfig, d *api.ResolvedPlacement, layer string) (*api.KubernetesConfig, error) {
	if err := CheckAgentPlacement(base, d, layer); err != nil {
		return nil, err
	}
	out := cloneKubernetesConfig(base)
	if d == nil {
		return out, nil
	}
	if out == nil {
		if !d.InEffect() {
			return nil, nil
		}
		out = &api.KubernetesConfig{}
	}
	out.NodeSelector = MergeNodeSelector(out.NodeSelector, d.NodeSelector)
	out.Tolerations = MergeTolerations(out.Tolerations, d.Tolerations)
	out.RuntimeClassName = mergeRuntimeClassName(out.RuntimeClassName, d.RuntimeClassName)
	return out, nil
}
