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
	"sort"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// LegacyPlacementPolicy is the operator placement of a start on a legacy
// Runtime Broker: the placement of the runtime entries that target the
// agent's runtime (entries, which must agree: SiblingPlacementAgrees), then
// the selected profile's over it (profile nil: no profile rung). Sources
// names every contributing settings key. The result shares no map or slice
// with the settings. It is the one merge of runtime entry and profile
// placement, used by start resolution, the pre-upgrade scan and profile
// conversion.
func LegacyPlacementPolicy(runtimes map[string]V1RuntimeConfig, entries []string, profileName string, profile *V1ProfileConfig) *api.ResolvedPlacement {
	rp := &api.ResolvedPlacement{Sources: map[string][]string{}}
	for _, name := range entries {
		rt := runtimes[name]
		if len(rt.NodeSelector) > 0 {
			rp.NodeSelector = MergeNodeSelector(rt.NodeSelector, nil)
			rp.Sources["node_selector"] = append(rp.Sources["node_selector"], "runtimes."+name)
		}
		if len(rt.Tolerations) > 0 {
			rp.Tolerations = MergeTolerations(rt.Tolerations, nil)
			rp.Sources["tolerations"] = append(rp.Sources["tolerations"], "runtimes."+name)
		}
		if rt.RuntimeClassName != "" {
			rp.RuntimeClassName = rt.RuntimeClassName
			rp.Sources["runtime_class_name"] = append(rp.Sources["runtime_class_name"], "runtimes."+name)
		}
	}
	if profile != nil {
		ApplyProfilePlacement(rp, profileName, *profile)
	}
	return rp
}

// ApplyProfilePlacement applies profile p's placement over rp: its node
// selector keys win, its tolerations are merged first, and its runtime
// class wins when set.
func ApplyProfilePlacement(rp *api.ResolvedPlacement, name string, p V1ProfileConfig) {
	if rp.Sources == nil {
		rp.Sources = map[string][]string{}
	}
	if len(p.NodeSelector) > 0 {
		rp.NodeSelector = MergeNodeSelector(rp.NodeSelector, p.NodeSelector)
		rp.Sources["node_selector"] = append(rp.Sources["node_selector"], "profiles."+name)
	}
	if len(p.Tolerations) > 0 {
		rp.Tolerations = MergeTolerations(rp.Tolerations, p.Tolerations)
		rp.Sources["tolerations"] = append(rp.Sources["tolerations"], "profiles."+name)
	}
	if p.RuntimeClassName != "" {
		rp.RuntimeClassName = p.RuntimeClassName
		rp.Sources["runtime_class_name"] = append(rp.Sources["runtime_class_name"], "profiles."+name)
	}
}

// ProfilePoolConflict reports whether a profile and its runtime entry both
// set the pool node selector key, to different values.
func ProfilePoolConflict(rt V1RuntimeConfig, p V1ProfileConfig) bool {
	rv, rok := rt.NodeSelector[PoolNodeSelectorKey]
	pv, pok := p.NodeSelector[PoolNodeSelectorKey]
	return rok && pok && rv != pv
}

// ResolveLegacyPolicyOffline resolves, without a Runtime Broker or a
// cluster, the operator placement a legacy start of a Kubernetes agent
// would use, for the read-only pre-upgrade scan. agentProfile is the
// agent's recorded profile ("" for none); configured reports whether a
// runtime entry is the operator's (nil: every entry is).
//
// With a recorded profile, the profile and its Kubernetes runtime entry
// must exist, and every configured Kubernetes entry that may target the
// same cluster and namespace (an empty context or namespace may match any)
// must agree with it; the policy is then those entries' placement with the
// profile's over it. A profile with placement on an entry that only the
// built-in defaults define cannot apply. With no recorded profile, the
// policy is known only when no configured Kubernetes entry and no profile
// sets placement (the empty policy): otherwise the Runtime Broker resolves
// it by the live cluster identity, which an offline scan cannot.
//
// ok false means the policy cannot be resolved here: the caller counts the
// agent as incomplete coverage, never as matching nothing. The result is
// operator-owned (api.ResolvedPlacement.OperatorOwned).
func ResolveLegacyPolicyOffline(runtimes map[string]V1RuntimeConfig, profiles map[string]V1ProfileConfig, configured func(string) bool, agentProfile string) (*api.ResolvedPlacement, bool) {
	isConfigured := func(name string) bool { return configured == nil || configured(name) }
	var kube []string
	for name, rt := range runtimes {
		if IsKubernetesRuntimeEntry(name, rt) && isConfigured(name) {
			kube = append(kube, name)
		}
	}
	sort.Strings(kube)

	if agentProfile == "" {
		for _, name := range kube {
			if RuntimeEntryHasPlacement(runtimes[name]) {
				return nil, false
			}
		}
		for _, p := range profiles {
			if ProfileHasPlacement(p) {
				return nil, false
			}
		}
		return &api.ResolvedPlacement{OperatorOwned: true}, true
	}

	p, ok := profiles[agentProfile]
	if !ok || p.Runtime == "" {
		return nil, false
	}
	rt, ok := runtimes[p.Runtime]
	if !ok || !IsKubernetesRuntimeEntry(p.Runtime, rt) {
		return nil, false
	}
	if !isConfigured(p.Runtime) && ProfileHasPlacement(p) {
		return nil, false
	}
	var group []string
	for _, name := range kube {
		if mayTargetSameNamespace(rt, runtimes[name]) {
			group = append(group, name)
		}
	}
	for _, name := range group {
		if !SiblingPlacementAgrees(runtimes[group[0]], runtimes[name]) {
			return nil, false
		}
	}
	rp := LegacyPlacementPolicy(runtimes, group, agentProfile, &p)
	rp.OperatorOwned = true
	return rp, true
}

// mayTargetSameNamespace reports whether two Kubernetes runtime entries may
// target the same cluster and namespace when their contexts and namespaces
// cannot be resolved: each is equal, or empty on either side (the current
// context, the default namespace).
func mayTargetSameNamespace(a, b V1RuntimeConfig) bool {
	ctx := a.Context == b.Context || a.Context == "" || b.Context == ""
	ns := a.Namespace == b.Namespace || a.Namespace == "" || b.Namespace == ""
	return ctx && ns
}
