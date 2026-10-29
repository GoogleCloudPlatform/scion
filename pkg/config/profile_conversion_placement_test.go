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
	"reflect"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// convPlacementSettings: a Kubernetes runtime entry with pool placement,
// two profiles on it (one adding a selector key, a toleration and a runtime
// class), and a docker entry with placement.
func convPlacementSettings() *VersionedSettings {
	poolTol := api.K8sToleration{Key: PoolNodeSelectorKey, Operator: "Equal", Value: "agents", Effect: "NoSchedule"}
	return &VersionedSettings{
		Runtimes: map[string]V1RuntimeConfig{
			"cluster": {Type: "kubernetes", Namespace: "agents",
				NodeSelector: map[string]string{PoolNodeSelectorKey: "agents"}, Tolerations: []api.K8sToleration{poolTol}},
			"local": {Type: "docker", RuntimeClassName: "gvisor"},
		},
		Profiles: map[string]V1ProfileConfig{
			"fast": {Runtime: "cluster", NodeSelector: map[string]string{"disk": "ssd"},
				Tolerations: []api.K8sToleration{{Key: "dedicated", Operator: "Exists"}}, RuntimeClassName: "gvisor"},
			"plain": {Runtime: "cluster"},
			"desk":  {Runtime: "local"},
		},
	}
}

// The converted flat target carries the profile's effective placement, and
// that target places agents exactly as the legacy start did for the same
// profile: the flat instance's operator placement equals the legacy policy,
// and agent blocks are accepted or refused alike.
func TestProfileConversionCarriesPlacement(t *testing.T) {
	vs := convPlacementSettings()
	got := ConvertProfilesToInstances(ProfileConversionInput{Settings: vs, Profiles: []string{"fast"}})
	if got.ProfileMap["fast"] == "" || len(got.Instances) != 1 {
		t.Fatalf("result = %+v", got)
	}
	target := got.Instances[0].RuntimeTarget
	flat := &api.ResolvedPlacement{NodeSelector: MergeNodeSelector(target.NodeSelector, nil),
		Tolerations: MergeTolerations(target.Tolerations, nil), RuntimeClassName: target.RuntimeClassName, OperatorOwned: true}
	legacy, ok := ResolveLegacyPolicyOffline(vs.Runtimes, vs.Profiles, nil, "fast")
	if !ok {
		t.Fatal("legacy policy unresolved")
	}
	if !reflect.DeepEqual(flat.NodeSelector, legacy.NodeSelector) || !reflect.DeepEqual(tolerationSet(flat.Tolerations), tolerationSet(legacy.Tolerations)) ||
		flat.RuntimeClassName != legacy.RuntimeClassName {
		t.Fatalf("flat %+v, legacy %+v", flat, legacy)
	}
	if !reflect.DeepEqual(flat.NodeSelector, map[string]string{PoolNodeSelectorKey: "agents", "disk": "ssd"}) || flat.RuntimeClassName != "gvisor" || len(flat.Tolerations) != 2 {
		t.Fatalf("flat target placement = %+v", flat)
	}
	for i, block := range []*api.KubernetesConfig{
		{NodeSelector: map[string]string{"extra": "x"}},
		{NodeSelector: map[string]string{PoolNodeSelectorKey: "batch"}},
		{Tolerations: []api.K8sToleration{{Key: "dedicated", Operator: "Exists"}}},
		{Tolerations: []api.K8sToleration{{Key: "other", Operator: "Exists"}}},
		{RuntimeClassName: "kata"},
	} {
		f := CheckAgentPlacement(block, flat, AgentPlacementLayerStored) != nil
		l := CheckAgentPlacement(block, legacy, AgentPlacementLayerStored) != nil
		if f != l {
			t.Errorf("block %d: flat refused %v, legacy refused %v", i, f, l)
		}
	}

	// The placement is not reported as dropped, and a docker profile's
	// placement is reported as not converted.
	desk := ConvertProfilesToInstances(ProfileConversionInput{Settings: vs, Profiles: []string{"desk"}})
	if desk.Instances[0].RuntimeTarget.RuntimeClassName != "" {
		t.Fatal("a docker target carries a runtime class")
	}
	found := false
	for _, f := range desk.Findings {
		if f.Path == "runtimes.local.runtime_class_name" && f.Code == ConversionFieldNotValidForTargetType {
			found = true
		}
	}
	if !found {
		t.Fatalf("docker placement not reported: %+v", desk.Findings)
	}
}

// A profile that sets the pool selector to another value than its runtime
// entry is a blocking finding: the profile is not converted.
func TestProfileConversionPoolConflictBlocks(t *testing.T) {
	vs := convPlacementSettings()
	p := vs.Profiles["fast"]
	p.NodeSelector = map[string]string{PoolNodeSelectorKey: "batch"}
	vs.Profiles["fast"] = p
	got := ConvertProfilesToInstances(ProfileConversionInput{Settings: vs, Profiles: []string{"fast", "plain"}})
	if _, mapped := got.ProfileMap["fast"]; mapped {
		t.Fatalf("the conflicting profile converted: %+v", got.ProfileMap)
	}
	if got.ProfileMap["plain"] == "" {
		t.Fatalf("the other profile did not convert: %+v", got.Findings)
	}
	var f *ConversionFinding
	for i := range got.Findings {
		if got.Findings[i].Path == "profiles.fast.node_selector" {
			f = &got.Findings[i]
		}
	}
	if f == nil || f.Code != ConversionProfileConflict || f.Severity != ConversionSeverityBlocking || !reflect.DeepEqual(f.Profiles, []string{"fast"}) {
		t.Fatalf("finding = %+v", f)
	}
}

// Profiles of one target group with different placement are a blocking
// profile conflict per field, like any other runtime_target field; equal
// toleration sets in another order agree.
func TestProfileConversionGroupPlacementConflict(t *testing.T) {
	vs := convPlacementSettings()
	got := ConvertProfilesToInstances(ProfileConversionInput{Settings: vs, Profiles: []string{"fast", "plain"}})
	paths := map[string]bool{}
	for _, f := range got.Findings {
		if f.Code == ConversionProfileConflict && f.Severity == ConversionSeverityBlocking {
			paths[f.Path] = true
		}
	}
	for _, want := range []string{"profiles.*.node_selector", "profiles.*.tolerations", "profiles.*.runtime_class_name"} {
		if !paths[want] {
			t.Errorf("no blocking conflict at %s: %+v", want, got.Findings)
		}
	}
	if len(got.ProfileMap) != 0 {
		t.Fatalf("conflicting profiles converted: %v", got.ProfileMap)
	}

	a := api.K8sToleration{Key: "a", Operator: "Exists"}
	b := api.K8sToleration{Key: "b", Value: "v"}
	if !conversionValuesEqual("tolerations", []api.K8sToleration{a, b}, []api.K8sToleration{{Key: "b", Operator: "Equal", Value: "v"}, a}) {
		t.Fatal("equal toleration sets differ")
	}
	if conversionValuesEqual("tolerations", []api.K8sToleration{a}, nil) {
		t.Fatal("a set and an unset toleration list agree")
	}
}
