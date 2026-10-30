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

// settingsScanFixture has one entry or pair in every settings category,
// plus clean entries that fall into none.
func settingsScanFixture() (map[string]V1RuntimeConfig, map[string]V1ProfileConfig) {
	pool := map[string]string{PoolNodeSelectorKey: "agents"}
	runtimes := map[string]V1RuntimeConfig{
		"k8s-a":   {Type: "kubernetes", Context: "c1", Namespace: "agents", NodeSelector: pool},
		"k8s-b":   {Type: "kubernetes", Context: "c1", Namespace: "agents"}, // sibling of k8s-a, no placement
		"k8s-bad": {Type: "kubernetes", Context: "c2", Namespace: "x", RuntimeClassName: "Not_Valid"},
		"local":   {Type: "docker", Tolerations: []api.K8sToleration{{Key: "a", Operator: "Exists"}}},
		"clean":   {Type: "kubernetes", Context: "c3", Namespace: "y"},
	}
	profiles := map[string]V1ProfileConfig{
		"pool-other": {Runtime: "k8s-a", NodeSelector: map[string]string{PoolNodeSelectorKey: "batch"}},
		"missing":    {Runtime: "nowhere"},
		"on-docker":  {Runtime: "local", RuntimeClassName: "gvisor"},
		"ok":         {Runtime: "k8s-a", NodeSelector: map[string]string{"disk": "ssd"}},
	}
	return runtimes, profiles
}

// The settings scan counts each category once per entry (or pair) and
// names the sections to repair, without any entry name or value.
func TestScanSettingsPlacementCategories(t *testing.T) {
	runtimes, profiles := settingsScanFixture()
	r := ScanSettingsPlacement(runtimes, profiles)
	want := map[SettingsPlacementCategory]int{
		SettingsInvalidPlacementField:        1,
		SettingsPlacementOnNonKubernetes:     2,
		SettingsProfileRuntimeMissing:        1,
		SettingsSiblingPlacementDisagreement: 1,
		SettingsPoolSelectorConflict:         1,
	}
	if !reflect.DeepEqual(r.Counts, want) {
		t.Fatalf("counts = %v, want %v", r.Counts, want)
	}
	if got := r.Sections(); !reflect.DeepEqual(got, []string{"profiles", "runtimes"}) {
		t.Fatalf("sections = %v", got)
	}
	sum := r.Summary()
	for _, v := range []string{"k8s-a", "agents", "batch", "gvisor", "Not_Valid", "nowhere"} {
		if strings.Contains(sum, v) {
			t.Fatalf("summary carries %q: %s", v, sum)
		}
	}
	if !strings.HasPrefix(sum, "settings_malformed_placement 0; settings_invalid_placement_field 1;") || !r.NeedsAttention() {
		t.Fatalf("summary = %q", sum)
	}

	clean := ScanSettingsPlacement(map[string]V1RuntimeConfig{"k": {Type: "kubernetes", NodeSelector: map[string]string{"disk": "ssd"}}},
		map[string]V1ProfileConfig{"p": {Runtime: "k", Tolerations: []api.K8sToleration{{Key: "d", Operator: "Exists"}}}})
	if clean.NeedsAttention() {
		t.Fatalf("clean settings need attention: %s", clean.Summary())
	}
	unreadable := SettingsPlacementReport{Unreadable: true}
	if !unreadable.NeedsAttention() || !strings.Contains(unreadable.Summary(), "coverage incomplete") {
		t.Fatalf("unreadable: %q", unreadable.Summary())
	}
}

// Raw entries with malformed or misspelt placement keys are categorized
// before a typed decode can drop them; the error names the path only.
func TestValidateRawPlacementEntry(t *testing.T) {
	bad := map[string]string{
		"camel selector":    `{"nodeSelector": {"a": "b"}}`,
		"camel class":       `{"runtimeClassName": "gvisor"}`,
		"selector list":     `{"node_selector": ["a"]}`,
		"selector number":   `{"node_selector": {"a": 1}}`,
		"toleration extra":  `{"tolerations": [{"key": "a", "tolerationSeconds": 30}]}`,
		"toleration snake":  `{"tolerations": [{"key": "a", "toleration_seconds": 30}]}`,
		"tolerations map":   `{"tolerations": {"key": "a"}}`,
		"class number":      `{"runtime_class_name": 3}`,
		"singular spelling": `{"toleration": []}`,
	}
	for name, doc := range bad {
		var entry map[string]interface{}
		if err := json.Unmarshal([]byte(doc), &entry); err != nil {
			t.Fatal(err)
		}
		err := ValidateRawPlacementEntry("runtimes.secret-name", entry)
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		if strings.Contains(err.Error(), "gvisor") || !strings.HasPrefix(err.Error(), "runtimes.secret-name") {
			t.Errorf("%s: error %q", name, err)
		}
	}
	var good map[string]interface{}
	if err := json.Unmarshal([]byte(`{"type": "kubernetes", "node_selector": {"a": "b"}, "tolerations": [{"key": "a", "operator": "Exists", "effect": "NoSchedule"}], "runtime_class_name": "gvisor", "unknown_other": 1}`), &good); err != nil {
		t.Fatal(err)
	}
	if err := ValidateRawPlacementEntry("runtimes.k", good); err != nil {
		t.Fatalf("valid entry refused: %v", err)
	}

	var r SettingsPlacementReport
	r.AddRawSection("profiles", map[string]interface{}{
		"a": map[string]interface{}{"nodeSelector": map[string]interface{}{}},
		"b": good,
		"c": "not a mapping",
	})
	if r.Counts[SettingsMalformedPlacement] != 1 || !reflect.DeepEqual(r.Sections(), []string{"profiles"}) {
		t.Fatalf("raw section: %+v %v", r.Counts, r.Sections())
	}
}

// The legacy merge: entry placement, then the profile's over it (selector
// key by key, tolerations combined with the profile's first, runtime class
// replaced), with the sources of each field.
func TestLegacyPlacementPolicyMergesProfileOverRuntime(t *testing.T) {
	tolA := api.K8sToleration{Key: "a", Operator: "Exists"}
	tolB := api.K8sToleration{Key: "b", Operator: "Exists"}
	runtimes := map[string]V1RuntimeConfig{"k": {Type: "kubernetes",
		NodeSelector: map[string]string{"disk": "ssd", "zone": "1"}, Tolerations: []api.K8sToleration{tolA}, RuntimeClassName: "runc"}}
	prof := V1ProfileConfig{Runtime: "k", NodeSelector: map[string]string{"zone": "2"}, Tolerations: []api.K8sToleration{tolB}, RuntimeClassName: "gvisor"}
	rp := LegacyPlacementPolicy(runtimes, []string{"k"}, "p", &prof)
	if !reflect.DeepEqual(rp.NodeSelector, map[string]string{"disk": "ssd", "zone": "2"}) ||
		!reflect.DeepEqual(rp.Tolerations, []api.K8sToleration{tolB, tolA}) || rp.RuntimeClassName != "gvisor" {
		t.Fatalf("merged = %+v", rp)
	}
	if !reflect.DeepEqual(rp.Sources["node_selector"], []string{"runtimes.k", "profiles.p"}) {
		t.Fatalf("sources = %v", rp.Sources)
	}
	rp.NodeSelector["disk"] = "changed"
	if runtimes["k"].NodeSelector["disk"] != "ssd" {
		t.Fatal("the result shares the runtime entry's map")
	}
	if rp := LegacyPlacementPolicy(runtimes, nil, "", nil); rp.InEffect() {
		t.Fatalf("no entries: %+v", rp)
	}
}

// The offline policy is resolved only when it is certain; otherwise the
// agent is incomplete coverage, never an empty policy that would hide its
// tolerations or runtime class.
func TestResolveLegacyPolicyOfflineUnresolvedIsIncomplete(t *testing.T) {
	tol := api.K8sToleration{Key: "dedicated", Operator: "Exists"}
	runtimes := map[string]V1RuntimeConfig{
		"k8s":    {Type: "kubernetes", Context: "c1", Namespace: "agents", Tolerations: []api.K8sToleration{tol}},
		"alias":  {Type: "kubernetes", Namespace: "agents"}, // empty context: may be the same cluster
		"other":  {Type: "kubernetes", Context: "c2", Namespace: "batch"},
		"local":  {Type: "docker"},
		"plain":  {Type: "kubernetes", Context: "c3", Namespace: "p"},
		"plain2": {Type: "kubernetes", Context: "c3", Namespace: "p"},
	}
	profiles := map[string]V1ProfileConfig{
		"on-k8s":   {Runtime: "k8s", RuntimeClassName: "gvisor"},
		"on-other": {Runtime: "other"},
		"on-plain": {Runtime: "plain"},
		"on-local": {Runtime: "local"},
		"no-rt":    {},
		"dangling": {Runtime: "nowhere"},
	}

	// The alias with no placement disagrees with k8s: cannot tell.
	if _, ok := ResolveLegacyPolicyOffline(runtimes, profiles, nil, "on-k8s"); ok {
		t.Fatal("a possibly aliased disagreeing entry resolved")
	}
	// Without the alias, the entry and the profile combine.
	noAlias := map[string]V1RuntimeConfig{}
	for k, v := range runtimes {
		if k != "alias" {
			noAlias[k] = v
		}
	}
	rp, ok := ResolveLegacyPolicyOffline(noAlias, profiles, nil, "on-k8s")
	if !ok || !rp.OperatorOwned || rp.RuntimeClassName != "gvisor" || !reflect.DeepEqual(rp.Tolerations, []api.K8sToleration{tol}) {
		t.Fatalf("on-k8s = %+v, %v", rp, ok)
	}
	if rp, ok := ResolveLegacyPolicyOffline(noAlias, profiles, nil, "on-plain"); !ok || rp.InEffect() || !rp.OperatorOwned {
		t.Fatalf("on-plain = %+v, %v", rp, ok)
	}
	for _, p := range []string{"missing-profile", "on-local", "no-rt", "dangling"} {
		if _, ok := ResolveLegacyPolicyOffline(noAlias, profiles, nil, p); ok {
			t.Errorf("profile %s resolved", p)
		}
	}
	// No recorded profile: only certain when nothing sets placement.
	if _, ok := ResolveLegacyPolicyOffline(noAlias, profiles, nil, ""); ok {
		t.Fatal("an unrecorded profile resolved while an entry sets placement")
	}
	bare := map[string]V1RuntimeConfig{"plain": runtimes["plain"]}
	if rp, ok := ResolveLegacyPolicyOffline(bare, map[string]V1ProfileConfig{"on-plain": {Runtime: "plain"}}, nil, ""); !ok || rp.InEffect() {
		t.Fatalf("no placement anywhere: %+v, %v", rp, ok)
	}
	// A profile with placement on an entry only the defaults define.
	notConfigured := func(name string) bool { return name != "k8s" }
	if _, ok := ResolveLegacyPolicyOffline(noAlias, profiles, notConfigured, "on-k8s"); ok {
		t.Fatal("a placement profile on a defaults-only entry resolved")
	}
}
