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
	"encoding/json"
	"reflect"
	"strings"
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

// conversionFinding returns the finding at path with code, or nil.
func conversionFinding(got ProfileConversionResult, path, code string) *ConversionFinding {
	for i := range got.Findings {
		if got.Findings[i].Path == path && got.Findings[i].Code == code {
			return &got.Findings[i]
		}
	}
	return nil
}

func assertNoBlocking(t *testing.T, got ProfileConversionResult) {
	t.Helper()
	for _, f := range got.Findings {
		if f.Severity == ConversionSeverityBlocking {
			t.Fatalf("blocking finding %+v", f)
		}
	}
}

// instanceByKey returns the instance with key, failing the test when
// there is none.
func instanceByKey(t *testing.T, got ProfileConversionResult, key string) V1RuntimeBrokerInstanceConfig {
	t.Helper()
	for _, inst := range got.Instances {
		if inst.Key == key {
			return inst
		}
	}
	t.Fatalf("no instance %q in %+v", key, got.Instances)
	return V1RuntimeBrokerInstanceConfig{}
}

// A profile that sets the pool selector to another value than its runtime
// entry converts with its own configured pool, on its own instance, and a
// notice names both values so the operator can consolidate.
func TestProfileConversionPoolOverrideSplits(t *testing.T) {
	vs := convPlacementSettings()
	p := vs.Profiles["fast"]
	p.NodeSelector = map[string]string{PoolNodeSelectorKey: "batch"}
	vs.Profiles["fast"] = p
	got := ConvertProfilesToInstances(ProfileConversionInput{Settings: vs, Profiles: []string{"fast", "plain"}})
	assertNoBlocking(t, got)
	if !reflect.DeepEqual(got.ProfileMap, map[string]string{"fast": "k8s-agents-fast", "plain": "k8s-agents-plain"}) {
		t.Fatalf("profile map %v, findings %+v", got.ProfileMap, got.Findings)
	}
	if ns := instanceByKey(t, got, "k8s-agents-fast").RuntimeTarget.NodeSelector; ns[PoolNodeSelectorKey] != "batch" {
		t.Fatalf("fast instance node selector %v, want the profile's pool", ns)
	}
	if ns := instanceByKey(t, got, "k8s-agents-plain").RuntimeTarget.NodeSelector; ns[PoolNodeSelectorKey] != "agents" {
		t.Fatalf("plain instance node selector %v, want the entry's pool", ns)
	}
	f := conversionFinding(got, "profiles.fast.node_selector", ConversionProfileConflict)
	if f == nil || f.Severity != ConversionSeverityNotice || !reflect.DeepEqual(f.Profiles, []string{"fast"}) ||
		!strings.Contains(f.Message, "sets "+PoolNodeSelectorKey+" to batch and its runtime entry cluster sets agents") ||
		!strings.Contains(f.Message, "keeps the profile's configured pool batch") {
		t.Fatalf("pool notice = %+v", f)
	}
}

// Profiles of one target group with different placement are split into one
// instance per distinct placement, each equal to the legacy policy of its
// profile, and one notice names the differing fields and the new instances.
func TestProfileConversionGroupPlacementSplits(t *testing.T) {
	vs := convPlacementSettings()
	got := ConvertProfilesToInstances(ProfileConversionInput{Settings: vs, Profiles: []string{"fast", "plain"}})
	assertNoBlocking(t, got)
	if len(got.Instances) != 2 {
		t.Fatalf("instances %+v", got.Instances)
	}
	for _, name := range []string{"fast", "plain"} {
		target := instanceByKey(t, got, got.ProfileMap[name]).RuntimeTarget
		legacy, ok := ResolveLegacyPolicyOffline(vs.Runtimes, vs.Profiles, nil, name)
		if !ok || !reflect.DeepEqual(target.NodeSelector, legacy.NodeSelector) || target.RuntimeClassName != legacy.RuntimeClassName ||
			!reflect.DeepEqual(tolerationSet(target.Tolerations), tolerationSet(legacy.Tolerations)) {
			t.Fatalf("%s: flat target %+v, legacy %+v (ok %v)", name, target, legacy, ok)
		}
	}
	f := conversionFinding(got, "profiles.*", ConversionProfileConflict)
	if f == nil || f.Severity != ConversionSeverityNotice || !reflect.DeepEqual(f.Profiles, []string{"fast", "plain"}) ||
		!strings.Contains(f.Message, "differ in node_selector, runtime_class_name, tolerations;") ||
		!strings.Contains(f.Message, "k8s-agents-fast (fast), k8s-agents-plain (plain)") {
		t.Fatalf("split notice = %+v", f)
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

// Profiles with the same placement (toleration sets in another order
// agree) merge into one instance with the group's key, with no split.
func TestProfileConversionIdenticalPlacementMerges(t *testing.T) {
	tolA := api.K8sToleration{Key: "a", Operator: "Exists"}
	tolB := api.K8sToleration{Key: "b", Operator: "Equal", Value: "v"}
	vs := &VersionedSettings{
		Runtimes: map[string]V1RuntimeConfig{"cluster": {Type: "kubernetes", Namespace: "agents"}},
		Profiles: map[string]V1ProfileConfig{
			"x": {Runtime: "cluster", NodeSelector: map[string]string{"disk": "ssd"}, Tolerations: []api.K8sToleration{tolA, tolB}},
			"y": {Runtime: "cluster", NodeSelector: map[string]string{"disk": "ssd"}, Tolerations: []api.K8sToleration{{Key: "b", Value: "v"}, tolA}},
		},
	}
	got := ConvertProfilesToInstances(ProfileConversionInput{Settings: vs})
	if len(got.Findings) != 0 || len(got.Instances) != 1 ||
		!reflect.DeepEqual(got.ProfileMap, map[string]string{"x": "k8s-agents", "y": "k8s-agents"}) {
		t.Fatalf("result %+v", got)
	}
}

// Runtime entries that may share a cluster and namespace with different
// placement each keep their own placement: their profiles go to separate
// instances, and a notice names the entries.
func TestProfileConversionSharedNamespaceSplits(t *testing.T) {
	vs := &VersionedSettings{
		Runtimes: map[string]V1RuntimeConfig{
			"a": {Type: "kubernetes", Context: "c1", Namespace: "agents", NodeSelector: map[string]string{PoolNodeSelectorKey: "x"}},
			"b": {Type: "kubernetes", Context: "c1", Namespace: "agents", NodeSelector: map[string]string{PoolNodeSelectorKey: "y"}},
		},
		Profiles: map[string]V1ProfileConfig{"pa": {Runtime: "a"}, "pb": {Runtime: "b"}},
	}
	got := ConvertProfilesToInstances(ProfileConversionInput{Settings: vs})
	assertNoBlocking(t, got)
	for name, pool := range map[string]string{"pa": "x", "pb": "y"} {
		inst := instanceByKey(t, got, "k8s-c1-agents-"+name)
		if got.ProfileMap[name] != inst.Key || !reflect.DeepEqual(inst.RuntimeTarget.NodeSelector, map[string]string{PoolNodeSelectorKey: pool}) {
			t.Fatalf("%s: profile map %v, instance %+v", name, got.ProfileMap, inst.RuntimeTarget)
		}
	}
	for _, path := range []string{"runtimes.a", "runtimes.b"} {
		if f := conversionFinding(got, path, ConversionProfileConflict); f == nil || f.Severity != ConversionSeverityNotice ||
			!strings.Contains(f.Message, "may target the same cluster and namespace") ||
			!strings.Contains(f.Message, "(a legacy start is refused today when both resolve to the same cluster and namespace)") {
			t.Fatalf("no shared namespace notice at %s: %+v", path, got.Findings)
		}
	}
}

// A differing field other than placement splits too: each instance keeps
// its profile's value, and the notice names the field.
func TestProfileConversionNonPlacementSplits(t *testing.T) {
	got := ConvertProfilesToInstances(ProfileConversionInput{Settings: convSizeSettings()})
	assertNoBlocking(t, got)
	if !reflect.DeepEqual(got.ProfileMap, map[string]string{"a": "k8s-agents-a", "b": "k8s-agents-b"}) {
		t.Fatalf("profile map %v", got.ProfileMap)
	}
	if instanceByKey(t, got, "k8s-agents-a").RuntimeTarget.SharedDirSize != "10Gi" || instanceByKey(t, got, "k8s-agents-b").RuntimeTarget.SharedDirSize != "20Gi" {
		t.Fatalf("instances %+v", got.Instances)
	}
	if f := conversionFinding(got, "profiles.*", ConversionProfileConflict); f == nil || !strings.Contains(f.Message, "differ in shared_dir_size;") {
		t.Fatalf("split notice: %+v", got.Findings)
	}
}

// A field choice consolidates the profiles onto the chosen value: one
// instance, and the dropped value is reported. A field choice with no
// difference to consolidate is still reported, as blocking.
func TestProfileConversionChoiceConsolidates(t *testing.T) {
	vs := convSizeSettings()
	choice := ConversionChoice{TargetGroup: "k8s-agents", Field: "shared_dir_size", ChosenProfile: "b"}
	got := ConvertProfilesToInstances(ProfileConversionInput{Settings: vs, Choices: []ConversionChoice{choice}})
	assertNoBlocking(t, got)
	if len(got.Instances) != 1 || got.Instances[0].RuntimeTarget.SharedDirSize != "20Gi" ||
		!reflect.DeepEqual(got.ProfileMap, map[string]string{"a": "k8s-agents", "b": "k8s-agents"}) {
		t.Fatalf("result %+v", got)
	}
	f := conversionFinding(got, "profiles.*.shared_dir_size", ConversionProfileConflictResolved)
	if f == nil || !strings.Contains(f.Message, "a: 10Gi from profiles.a.shared_dir_size") {
		t.Fatalf("the dropped value is not reported: %+v", got.Findings)
	}
	if conversionFinding(got, "profiles.*", ConversionProfileConflict) != nil {
		t.Fatalf("consolidated profiles reported as split: %+v", got.Findings)
	}

	unused := ConversionChoice{TargetGroup: "k8s-agents", Field: "clone_depth", ChosenProfile: "a"}
	got = ConvertProfilesToInstances(ProfileConversionInput{Settings: vs, Choices: []ConversionChoice{choice, unused}})
	if f := conversionFinding(got, "choices[1]", ConversionUnknownSettingsKey); f == nil || f.Severity != ConversionSeverityBlocking ||
		!strings.Contains(f.Message, "has no conflict to resolve") {
		t.Fatalf("unused choice not reported: %+v", got.Findings)
	}
}

func convSizeSettings() *VersionedSettings {
	return &VersionedSettings{
		Runtimes: map[string]V1RuntimeConfig{"cluster": {Type: "kubernetes", Namespace: "agents"}},
		Profiles: map[string]V1ProfileConfig{
			"a": {Runtime: "cluster", SharedDirSize: "10Gi"},
			"b": {Runtime: "cluster", SharedDirSize: "20Gi"},
		},
	}
}

// The result, including split keys and notices, does not depend on the
// order of the profiles in scope: every permutation gives byte-identical
// output.
func TestProfileConversionPermutationDeterministic(t *testing.T) {
	vs := convPlacementSettings()
	for name, p := range map[string]V1ProfileConfig{
		"big":   {Runtime: "cluster", SharedDirSize: "50Gi"},
		"small": {Runtime: "cluster", SharedDirSize: "5Gi", ImageRegistry: "reg/small"},
		"twin":  {Runtime: "cluster", SharedDirSize: "5Gi", ImageRegistry: "reg/small"},
	} {
		vs.Profiles[name] = p
	}
	names := []string{"big", "desk", "fast", "plain", "small", "twin"}
	want, err := json.Marshal(ConvertProfilesToInstances(ProfileConversionInput{Settings: vs, Profiles: names}))
	if err != nil {
		t.Fatal(err)
	}
	var permute func(k int)
	permute = func(k int) {
		if k == len(names) {
			got, err := json.Marshal(ConvertProfilesToInstances(ProfileConversionInput{Settings: vs, Profiles: append([]string(nil), names...)}))
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != string(want) {
				t.Fatalf("order %v: output differs:\n%s\nwant:\n%s", names, got, want)
			}
			return
		}
		for i := k; i < len(names); i++ {
			names[k], names[i] = names[i], names[k]
			permute(k + 1)
			names[k], names[i] = names[i], names[k]
		}
	}
	permute(0)
	all, _ := json.Marshal(ConvertProfilesToInstances(ProfileConversionInput{Settings: vs}))
	if string(all) != string(want) {
		t.Fatal("the default scope (all profiles) differs from the same profiles named")
	}
}

// Conversion matches sibling runtime entries as a legacy start does: with
// an agreeing sibling the converted flat target equals the legacy policy.
// With a disagreeing sibling (the offline legacy policy does not resolve)
// the profile converts with its own entry's placement only, and a notice
// names the entries.
func TestProfileConversionSiblingPlacement(t *testing.T) {
	pool := map[string]string{PoolNodeSelectorKey: "agents"}
	agree := &VersionedSettings{
		Runtimes: map[string]V1RuntimeConfig{
			"a": {Type: "kubernetes", Context: "c1", Namespace: "agents", NodeSelector: pool},
			"b": {Type: "kubernetes", Context: "c1", Namespace: "agents", NodeSelector: pool},
		},
		Profiles: map[string]V1ProfileConfig{"p": {Runtime: "a", RuntimeClassName: "gvisor"}},
	}
	got := ConvertProfilesToInstances(ProfileConversionInput{Settings: agree, Profiles: []string{"p"}})
	if got.ProfileMap["p"] == "" || len(got.Instances) != 1 {
		t.Fatalf("agreeing siblings: %+v", got)
	}
	target := got.Instances[0].RuntimeTarget
	legacy, ok := ResolveLegacyPolicyOffline(agree.Runtimes, agree.Profiles, nil, "p")
	if !ok || !reflect.DeepEqual(target.NodeSelector, legacy.NodeSelector) || target.RuntimeClassName != legacy.RuntimeClassName ||
		!reflect.DeepEqual(tolerationSet(target.Tolerations), tolerationSet(legacy.Tolerations)) {
		t.Fatalf("flat target %+v, legacy %+v (ok %v)", target, legacy, ok)
	}

	disagree := &VersionedSettings{
		Runtimes: map[string]V1RuntimeConfig{
			"a": {Type: "kubernetes", Context: "c1", Namespace: "agents", NodeSelector: pool},
			"b": {Type: "kubernetes", Context: "c1", Namespace: "agents"},
		},
		Profiles: map[string]V1ProfileConfig{"p": {Runtime: "a"}},
	}
	got = ConvertProfilesToInstances(ProfileConversionInput{Settings: disagree, Profiles: []string{"p"}})
	assertNoBlocking(t, got)
	if got.ProfileMap["p"] == "" || !reflect.DeepEqual(got.Instances[0].RuntimeTarget.NodeSelector, pool) {
		t.Fatalf("disagreeing siblings: %+v", got)
	}
	if f := conversionFinding(got, "runtimes.a", ConversionProfileConflict); f == nil || f.Severity != ConversionSeverityNotice {
		t.Fatalf("no sibling notice: %+v", got.Findings)
	}
	if _, ok := ResolveLegacyPolicyOffline(disagree.Runtimes, disagree.Profiles, nil, "p"); ok {
		t.Fatal("the legacy policy resolved for disagreeing siblings")
	}

	// Without configured names every Kubernetes entry is the operator's
	// (fail closed): the built-in entry is a sibling and disagrees. With
	// configured names that leave it out, it is not a sibling.
	withBuiltin := map[string]V1RuntimeConfig{
		"a":          {Type: "kubernetes", NodeSelector: pool},
		"kubernetes": defaultBuiltinCatalog().Runtimes["kubernetes"],
	}
	entries, other := PlacementSiblingEntries(withBuiltin, "a", nil)
	if other != "kubernetes" || !reflect.DeepEqual(entries, []string{"a", "kubernetes"}) {
		t.Fatalf("nil configured: %v %q, want the built-in entry as a disagreeing sibling", entries, other)
	}
	entries, other = PlacementSiblingEntries(withBuiltin, "a", func(name string) bool { return name == "a" })
	if other != "" || !reflect.DeepEqual(entries, []string{"a"}) {
		t.Fatalf("configured without the built-in entry: %v %q", entries, other)
	}
}

// Siblings are the entries that may target the same cluster and namespace,
// as the offline policy resolution groups them: an entry with an empty
// context and namespace may alias one with an explicit context and
// namespace. A profile on disagreeing such entries converts with its own
// entry's placement and a notice; on identical ones the flat target equals
// the legacy policy.
func TestProfileConversionAliasSiblingPlacement(t *testing.T) {
	x := map[string]string{PoolNodeSelectorKey: "x"}
	y := map[string]string{PoolNodeSelectorKey: "y"}
	disagree := &VersionedSettings{
		Runtimes: map[string]V1RuntimeConfig{
			"a": {Type: "kubernetes", NodeSelector: x},
			"b": {Type: "kubernetes", Context: "c1", Namespace: "agents", NodeSelector: y},
		},
		Profiles: map[string]V1ProfileConfig{"p": {Runtime: "a"}},
	}
	got := ConvertProfilesToInstances(ProfileConversionInput{Settings: disagree, Profiles: []string{"p"}})
	assertNoBlocking(t, got)
	if got.ProfileMap["p"] == "" || !reflect.DeepEqual(got.Instances[0].RuntimeTarget.NodeSelector, x) {
		t.Fatalf("possibly aliased disagreeing entries: %+v", got)
	}
	if f := conversionFinding(got, "runtimes.a", ConversionProfileConflict); f == nil || f.Severity != ConversionSeverityNotice {
		t.Fatalf("no sibling notice: %+v", got.Findings)
	}

	// Agreeing entries may target the same namespace and are identical in
	// placement (SiblingPlacementAgrees: equal selectors and runtime class,
	// equal toleration sets), so the merge over them equals either entry
	// up to toleration order; the leg checks that they are not blocked and
	// that the converted target equals the legacy policy.
	tolA := api.K8sToleration{Key: "a", Operator: "Exists"}
	tolB := api.K8sToleration{Key: "b", Operator: "Exists"}
	agree := &VersionedSettings{
		Runtimes: map[string]V1RuntimeConfig{
			"a": {Type: "kubernetes", NodeSelector: x, Tolerations: []api.K8sToleration{tolA, tolB}},
			"b": {Type: "kubernetes", Context: "c1", Namespace: "agents", NodeSelector: x, Tolerations: []api.K8sToleration{tolB, tolA, tolB}},
		},
		Profiles: map[string]V1ProfileConfig{"p": {Runtime: "a", NodeSelector: map[string]string{"disk": "ssd"}}},
	}
	got = ConvertProfilesToInstances(ProfileConversionInput{Settings: agree, Profiles: []string{"p"}})
	if got.ProfileMap["p"] == "" {
		t.Fatalf("agreeing aliased entries blocked: %+v", got.Findings)
	}
	legacy, ok := ResolveLegacyPolicyOffline(agree.Runtimes, agree.Profiles, nil, "p")
	target := got.Instances[0].RuntimeTarget
	if !ok || !reflect.DeepEqual(target.NodeSelector, legacy.NodeSelector) || !reflect.DeepEqual(tolerationSet(target.Tolerations), tolerationSet(legacy.Tolerations)) {
		t.Fatalf("flat target %+v, legacy %+v (ok %v)", target, legacy, ok)
	}
}

// Conversion decides which runtime entries are the operator's with the
// caller's configured names, like the broker: a settings file that
// declares kubernetes: {type: kubernetes} makes that entry the operator's,
// so it is a sibling of an entry with the same empty context and
// namespace, and their different placement gives a notice (the profile
// keeps its own entry's placement). Without configured names every entry
// counts (fail closed); names that leave the entry out give no notice.
func TestProfileConversionConfiguredSiblings(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeFlatTestFile(t, home+"/.scion/settings.yaml", `schema_version: "1"
runtimes:
  kubernetes:
    type: kubernetes
  pooled:
    type: kubernetes
    node_selector:
      pool: agents
profiles:
  p:
    runtime: pooled
`)
	vs, _, err := LoadGlobalSettings()
	if err != nil {
		t.Fatal(err)
	}
	names, known := ConfiguredRuntimeNames()
	if !known || !names["kubernetes"] || !names["pooled"] {
		t.Fatalf("configured names %v (known %v)", names, known)
	}
	configured := func(name string) bool { return names[name] }
	// The loaded built-in profiles are recognized as the shipped defaults;
	// the operator's profile is not.
	for name, p := range vs.Profiles {
		if IsShippedDefaultProfile(name, p) != (name != "p") {
			t.Errorf("profile %s: shipped default = %v", name, IsShippedDefaultProfile(name, p))
		}
	}
	sibling := func(cfg func(string) bool) bool {
		t.Helper()
		got := ConvertProfilesToInstances(ProfileConversionInput{Settings: vs, Profiles: []string{"p"}, ConfiguredRuntimes: cfg})
		assertNoBlocking(t, got)
		if got.ProfileMap["p"] == "" || !reflect.DeepEqual(got.Instances[0].RuntimeTarget.NodeSelector, map[string]string{"pool": "agents"}) {
			t.Fatalf("profile p: %+v", got)
		}
		return conversionFinding(got, "runtimes.pooled", ConversionProfileConflict) != nil
	}
	if !sibling(configured) {
		t.Fatal("the configured kubernetes entry is not a sibling")
	}
	// Without configured names the conversion fails closed: every
	// Kubernetes entry counts, so it is a sibling too.
	if !sibling(nil) {
		t.Fatal("without configured names the kubernetes entry is not a sibling")
	}
	// Configured names that leave the kubernetes entry out (a settings file
	// that does not declare it) make it no sibling.
	if sibling(func(name string) bool { return name != "kubernetes" && names[name] }) {
		t.Fatal("an undeclared built-in entry is a sibling")
	}
}
