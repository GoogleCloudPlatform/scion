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
	"fmt"
	"os"
	"sort"
	"strings"

	yamlv3 "gopkg.in/yaml.v3"
)

// Read-only pre-upgrade placement scan of the runtimes and profiles
// settings: the categories of entries that placement validation refuses.
// Like the agent scan it reports counts (and the sections to repair), never
// values.

// SettingsPlacementCategory is a stable category name of the settings scan.
type SettingsPlacementCategory string

const (
	// SettingsMalformedPlacement: a placement key has the wrong shape (a
	// node selector that is not a map of strings, a toleration with a key
	// other than key, operator, value and effect, a runtime class that is
	// not a string), or is spelt other than node_selector, tolerations or
	// runtime_class_name (for example nodeSelector).
	SettingsMalformedPlacement SettingsPlacementCategory = "settings_malformed_placement"
	// SettingsInvalidPlacementField: a node selector, toleration list or
	// runtime class is not valid for Kubernetes.
	SettingsInvalidPlacementField SettingsPlacementCategory = "settings_invalid_placement_field"
	// SettingsPlacementOnNonKubernetes: a runtime entry that is not
	// Kubernetes, or a profile whose runtime entry is not, sets placement.
	SettingsPlacementOnNonKubernetes SettingsPlacementCategory = "settings_placement_on_non_kubernetes"
	// SettingsProfileRuntimeMissing: a profile names a runtime entry that
	// does not exist.
	SettingsProfileRuntimeMissing SettingsPlacementCategory = "settings_profile_runtime_missing"
	// SettingsSiblingPlacementDisagreement: two runtime entries with the
	// same context and namespace set different placement.
	SettingsSiblingPlacementDisagreement SettingsPlacementCategory = "settings_sibling_placement_disagreement"
	// SettingsPoolSelectorConflict: a profile sets the pool node selector
	// key to a value other than its runtime entry's.
	SettingsPoolSelectorConflict SettingsPlacementCategory = "settings_pool_selector_conflict"
)

// SettingsPlacementCategories lists every settings category, in report
// order.
var SettingsPlacementCategories = []SettingsPlacementCategory{
	SettingsMalformedPlacement,
	SettingsInvalidPlacementField,
	SettingsPlacementOnNonKubernetes,
	SettingsProfileRuntimeMissing,
	SettingsSiblingPlacementDisagreement,
	SettingsPoolSelectorConflict,
}

// SettingsPlacementReport is the result of a settings scan: the count per
// category (entries, or entry pairs for sibling disagreement), the
// sections ("runtimes", "profiles") that hold a finding, and whether the
// settings could not be read at all (coverage incomplete).
type SettingsPlacementReport struct {
	Unreadable bool
	Counts     map[SettingsPlacementCategory]int
	sections   map[string]bool
}

func (r *SettingsPlacementReport) add(c SettingsPlacementCategory, section string) {
	if r.Counts == nil {
		r.Counts = map[SettingsPlacementCategory]int{}
	}
	if r.sections == nil {
		r.sections = map[string]bool{}
	}
	r.Counts[c]++
	r.sections[section] = true
}

// Sections returns the sections that hold a finding, sorted.
func (r SettingsPlacementReport) Sections() []string {
	out := make([]string, 0, len(r.sections))
	for s := range r.sections {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// NeedsAttention reports whether any entry falls into a category or the
// settings could not be read.
func (r SettingsPlacementReport) NeedsAttention() bool {
	if r.Unreadable {
		return true
	}
	for _, n := range r.Counts {
		if n > 0 {
			return true
		}
	}
	return false
}

// Summary is the report as one line: incomplete coverage first, every
// category with its count, and the sections to repair. It carries no names
// or values.
func (r SettingsPlacementReport) Summary() string {
	var b strings.Builder
	if r.Unreadable {
		b.WriteString("settings could not be read in full (coverage incomplete); ")
	}
	parts := make([]string, 0, len(SettingsPlacementCategories))
	for _, c := range SettingsPlacementCategories {
		parts = append(parts, fmt.Sprintf("%s %d", c, r.Counts[c]))
	}
	b.WriteString(strings.Join(parts, "; "))
	if s := r.Sections(); len(s) > 0 {
		fmt.Fprintf(&b, "; repair sections: %s", strings.Join(s, ", "))
	}
	return b.String()
}

// ScanSettingsPlacement categorizes the typed runtime entries and profiles.
// Sibling runtime entries are grouped by literal context and namespace
// (LiteralSiblingGroups): entries whose cluster identity only a Runtime
// Broker can resolve are compared there, at start.
func ScanSettingsPlacement(runtimes map[string]V1RuntimeConfig, profiles map[string]V1ProfileConfig) SettingsPlacementReport {
	var r SettingsPlacementReport
	for name, rt := range runtimes {
		if len(ValidatePlacementFields("runtimes."+name, rt.NodeSelector, rt.Tolerations, rt.RuntimeClassName)) > 0 {
			r.add(SettingsInvalidPlacementField, "runtimes")
		}
		if RuntimeEntryHasPlacement(rt) && !IsKubernetesRuntimeEntry(name, rt) {
			r.add(SettingsPlacementOnNonKubernetes, "runtimes")
		}
	}
	for name, p := range profiles {
		if len(ValidatePlacementFields("profiles."+name, p.NodeSelector, p.Tolerations, p.RuntimeClassName)) > 0 {
			r.add(SettingsInvalidPlacementField, "profiles")
		}
		if p.Runtime == "" {
			continue
		}
		rt, ok := runtimes[p.Runtime]
		if !ok {
			r.add(SettingsProfileRuntimeMissing, "profiles")
			continue
		}
		if ProfileHasPlacement(p) && !IsKubernetesRuntimeEntry(p.Runtime, rt) {
			r.add(SettingsPlacementOnNonKubernetes, "profiles")
		}
		if ProfilePoolConflict(rt, p) {
			r.add(SettingsPoolSelectorConflict, "profiles")
		}
	}
	for _, e := range ValidatePlacement(runtimes, nil, LiteralSiblingGroups(runtimes)) {
		if strings.HasPrefix(e.Message, "runtime entries ") {
			r.add(SettingsSiblingPlacementDisagreement, "runtimes")
		}
	}
	return r
}

// AddRawSection categorizes the raw entries of one section ("runtimes" or
// "profiles"; the section's map of entry name to raw entry, as decoded from
// JSON or YAML into generic values) for malformed placement
// (ValidateRawPlacementEntry). An entry that is not a mapping is not a
// placement finding.
func (r *SettingsPlacementReport) AddRawSection(section string, entries map[string]interface{}) {
	for _, name := range rawMapKeys(entries) {
		entry, ok := entries[name].(map[string]interface{})
		if !ok {
			continue
		}
		if ValidateRawPlacementEntry(section+"."+name, entry) != nil {
			r.add(SettingsMalformedPlacement, section)
		}
	}
}

// AddGlobalSettingsFileRaw categorizes the raw runtimes and profiles of
// the global settings file (AddRawSection). A file that cannot be found
// adds nothing; one that exists but cannot be read or parsed marks the
// report unreadable.
func (r *SettingsPlacementReport) AddGlobalSettingsFileRaw() {
	globalDir, err := GetGlobalDir()
	if err != nil {
		r.Unreadable = true
		return
	}
	if settingsFileUnstatable(globalDir) {
		r.Unreadable = true
		return
	}
	path := GetSettingsPath(globalDir)
	if path == "" {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		r.Unreadable = true
		return
	}
	var doc map[string]interface{}
	if err := yamlv3.Unmarshal(data, &doc); err != nil {
		r.Unreadable = true
		return
	}
	for _, section := range []string{"runtimes", "profiles"} {
		if m, ok := doc[section].(map[string]interface{}); ok {
			r.AddRawSection(section, m)
		}
	}
}

// placementKeyForms maps the spelling-insensitive form of each placement
// key (lower case, without underscores or hyphens) to its one supported
// spelling.
var placementKeyForms = map[string]string{
	"nodeselector":     "node_selector",
	"tolerations":      "tolerations",
	"toleration":       "tolerations",
	"runtimeclassname": "runtime_class_name",
	"runtimeclass":     "runtime_class_name",
}

// ValidateRawPlacementEntry checks the raw placement keys of one runtime
// entry or profile (decoded into generic values) before a typed decode can
// coerce or drop them: node_selector must be a map of strings, tolerations
// a list of mappings with only key, operator, value and effect, each a
// string, and runtime_class_name a string; and a placement key spelt any
// other way (nodeSelector, runtimeClassName, ...) is refused rather than
// ignored. The error names the path and never a value or a selector key.
func ValidateRawPlacementEntry(path string, entry map[string]interface{}) error {
	for _, k := range rawMapKeys(entry) {
		norm := strings.NewReplacer("_", "", "-", "").Replace(strings.ToLower(k))
		if want, isPlacement := placementKeyForms[norm]; isPlacement && k != want {
			return fmt.Errorf("%s: unsupported placement key; use %s", path, want)
		}
	}
	if err := checkRawPlacement(path, entry); err != nil {
		return err
	}
	if v, present := entry["runtime_class_name"]; present {
		if _, isString := v.(string); !isString {
			return fmt.Errorf("%s.runtime_class_name must be a string", path)
		}
	}
	return nil
}
