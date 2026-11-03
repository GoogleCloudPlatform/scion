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
	"maps"
	"os"
	"sort"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/knadh/koanf/v2"
)

// Settings-level checks for the Kubernetes placement defaults
// (node_selector, tolerations, runtime_class_name) on runtime entries and
// profiles (ptone/scion#5085).

// WorkloadIdentityNodeLabel is the node label the GCP identity "block" mode
// forces into agent pods' node selector. It must equal
// runtime.KubernetesWorkloadIdentityNodeLabel (pkg/config cannot import
// pkg/runtime; a pkg/runtime test pins the two together).
const WorkloadIdentityNodeLabel = "iam.gke.io/gke-metadata-server-enabled"

// pairingExemptPrefixes are node label key prefixes that GKE or Kubernetes
// manage; a selector key under them needs no same-key toleration.
var pairingExemptPrefixes = []string{
	"cloud.google.com/",
	"kubernetes.io/",
	"node.kubernetes.io/",
	"topology.kubernetes.io/",
	"iam.gke.io/",
}

// placementKeySpellings are the placement key names, in the settings
// (snake_case) and agent config (camelCase) spellings, lower-cased.
var placementKeySpellings = []string{"node_selector", "nodeselector", "tolerations", "runtime_class_name", "runtimeclassname"}

// RuntimeEntryHasPlacement reports whether a runtime entry sets any of the
// Kubernetes placement fields (node_selector, tolerations,
// runtime_class_name). Empty values count as unset.
func RuntimeEntryHasPlacement(rt V1RuntimeConfig) bool {
	return len(rt.NodeSelector) > 0 || len(rt.Tolerations) > 0 || rt.RuntimeClassName != ""
}

// ProfileHasPlacement is RuntimeEntryHasPlacement for a profile.
func ProfileHasPlacement(p V1ProfileConfig) bool {
	return len(p.NodeSelector) > 0 || len(p.Tolerations) > 0 || p.RuntimeClassName != ""
}

// SiblingPlacementAgrees reports whether two runtime entries carry the same
// placement: equal node selectors (empty and unset are equal), the same set
// of tolerations (order and duplicates do not matter; an empty operator is
// Equal), and the same runtime class.
func SiblingPlacementAgrees(a, b V1RuntimeConfig) bool {
	if a.RuntimeClassName != b.RuntimeClassName {
		return false
	}
	// maps.Equal treats a nil and an empty map as equal.
	if !maps.Equal(a.NodeSelector, b.NodeSelector) {
		return false
	}
	return maps.Equal(tolerationSet(a.Tolerations), tolerationSet(b.Tolerations))
}

func tolerationSet(ts []api.K8sToleration) map[tolerationIdentity]struct{} {
	out := make(map[tolerationIdentity]struct{}, len(ts))
	for _, t := range ts {
		out[identityOfToleration(t)] = struct{}{}
	}
	return out
}

// ValidatePlacement checks the placement fields of every runtime entry and
// profile: node selector keys and values, tolerations and the runtime class
// are validated as the Kubernetes API server does, each error carrying the
// key path. For each group of sibling runtime entries (entries that target
// the same cluster and namespace, from LiteralSiblingGroups or
// IdentityGroups), entries whose placement differs from the group's first
// entry are an error naming both. Results are sorted by path, then message.
func ValidatePlacement(runtimes map[string]V1RuntimeConfig, profiles map[string]V1ProfileConfig, groups [][]string) []ValidationError {
	var errs []ValidationError
	for name, rt := range runtimes {
		errs = append(errs, ValidatePlacementFields("runtimes."+name, rt.NodeSelector, rt.Tolerations, rt.RuntimeClassName)...)
	}
	for name, p := range profiles {
		errs = append(errs, ValidatePlacementFields("profiles."+name, p.NodeSelector, p.Tolerations, p.RuntimeClassName)...)
	}
	reported := map[[2]string]bool{}
	for _, group := range groups {
		if len(group) < 2 {
			continue
		}
		first := group[0]
		for _, other := range group[1:] {
			if SiblingPlacementAgrees(runtimes[first], runtimes[other]) {
				continue
			}
			pair := [2]string{first, other}
			if pair[1] < pair[0] {
				pair = [2]string{other, first}
			}
			if reported[pair] {
				continue
			}
			reported[pair] = true
			errs = append(errs, ValidationError{
				Path:    "runtimes." + pair[0],
				Message: SiblingDisagreementCause(pair[0], pair[1]),
			})
		}
	}
	sort.Slice(errs, func(i, j int) bool {
		if errs[i].Path != errs[j].Path {
			return errs[i].Path < errs[j].Path
		}
		return errs[i].Message < errs[j].Message
	})
	return errs
}

// ValidatePlacementFields checks one layer's placement fields (node
// selector, tolerations, runtime class) as the Kubernetes API server does
// and returns one error per invalid field, at path + ".node_selector",
// ".tolerations" or ".runtime_class_name". It is the field check shared by
// runtime entries, profiles and flat instance runtime targets, at load and
// per start.
func ValidatePlacementFields(path string, selector map[string]string, tolerations []api.K8sToleration, runtimeClassName string) []ValidationError {
	var errs []ValidationError
	add := func(field string, err error) {
		if err != nil {
			errs = append(errs, ValidationError{Path: path + "." + field, Message: err.Error()})
		}
	}
	add("node_selector", ValidateNodeSelector(selector))
	add("tolerations", ValidateTolerations(tolerations))
	add("runtime_class_name", ValidateRuntimeClassName(runtimeClassName))
	return errs
}

// SiblingDisagreementCause is the fixed message for two runtime entries
// that target the same cluster and namespace with different placement.
func SiblingDisagreementCause(a, b string) string {
	return fmt.Sprintf("runtime entries %s, %s target the same cluster and namespace with different placement; identity-matched entries must agree (put variants in profiles)", a, b)
}

// PlacementIgnoredWarnings returns one warning per runtime entry that sets
// a placement field but does not target Kubernetes, and per profile that
// sets one while its runtime entry does not target Kubernetes. Placement
// has no effect there. Sorted.
func PlacementIgnoredWarnings(runtimes map[string]V1RuntimeConfig, profiles map[string]V1ProfileConfig) []string {
	var warnings []string
	for name, rt := range runtimes {
		if RuntimeEntryHasPlacement(rt) && !isKubernetesRuntimeEntry(name, rt) {
			warnings = append(warnings, fmt.Sprintf("runtimes.%s sets node_selector, tolerations or runtime_class_name but runtime %q is not a Kubernetes runtime; they are ignored", name, name))
		}
	}
	for name, p := range profiles {
		if !ProfileHasPlacement(p) {
			continue
		}
		rt, ok := runtimes[p.Runtime]
		if ok && !isKubernetesRuntimeEntry(p.Runtime, rt) {
			warnings = append(warnings, fmt.Sprintf("profiles.%s sets node_selector, tolerations or runtime_class_name but its runtime %q is not a Kubernetes runtime; they are ignored", name, p.Runtime))
		}
	}
	sort.Strings(warnings)
	return warnings
}

// PlacementSettingsWarnings returns non-fatal findings for the placement
// fields of Kubernetes runtime entries and profiles:
//   - a node selector that sets the Workload Identity node label key, which
//     GCP identity "block" agents override;
//   - a toleration with an empty key and operator Exists, which tolerates
//     every taint;
//   - on an entry with gke: true (and on profiles of such an entry), a
//     selector key with no toleration of the same key, outside the
//     GKE- and Kubernetes-managed label prefixes (GKE Autopilot workload
//     separation needs both).
//
// Sorted.
func PlacementSettingsWarnings(runtimes map[string]V1RuntimeConfig, profiles map[string]V1ProfileConfig) []string {
	var warnings []string
	check := func(path string, selector map[string]string, tolerations []api.K8sToleration) {
		if _, ok := selector[WorkloadIdentityNodeLabel]; ok {
			warnings = append(warnings, fmt.Sprintf("%s.node_selector sets %q; agents in GCP identity \"block\" mode always use their own value for this key", path, WorkloadIdentityNodeLabel))
		}
		for i, t := range tolerations {
			if isMatchAllToleration(t) {
				warnings = append(warnings, fmt.Sprintf("%s.tolerations[%d] has an empty key with operator Exists, which tolerates every taint", path, i))
			}
		}
	}
	for name, rt := range runtimes {
		if !isKubernetesRuntimeEntry(name, rt) {
			continue
		}
		path := "runtimes." + name
		check(path, rt.NodeSelector, rt.Tolerations)
		if rt.GKE {
			for _, k := range MissingTolerationKeys(rt.NodeSelector, rt.Tolerations) {
				warnings = append(warnings, pairingWarning(path, k))
			}
		}
	}
	for name, p := range profiles {
		rt, ok := runtimes[p.Runtime]
		if !ok || !isKubernetesRuntimeEntry(p.Runtime, rt) {
			continue
		}
		path := "profiles." + name
		check(path, p.NodeSelector, p.Tolerations)
		if rt.GKE && len(p.NodeSelector) > 0 {
			merged := MergeTolerations(rt.Tolerations, p.Tolerations)
			for _, k := range MissingTolerationKeys(p.NodeSelector, merged) {
				warnings = append(warnings, pairingWarning(path, k))
			}
		}
	}
	sort.Strings(warnings)
	return warnings
}

func pairingWarning(path, key string) string {
	return fmt.Sprintf("%s.node_selector key %q has no toleration with the same key; on GKE Autopilot, workload separation needs a NoSchedule toleration with the same key and value", path, key)
}

// MissingTolerationKeys returns, sorted, the node selector keys outside the
// GKE- and Kubernetes-managed label prefixes that no toleration names.
func MissingTolerationKeys(selector map[string]string, tolerations []api.K8sToleration) []string {
	tolerated := make(map[string]bool, len(tolerations))
	for _, t := range tolerations {
		tolerated[t.Key] = true
	}
	var missing []string
	for k := range selector {
		if tolerated[k] || hasPairingExemptPrefix(k) {
			continue
		}
		missing = append(missing, k)
	}
	sort.Strings(missing)
	return missing
}

func hasPairingExemptPrefix(key string) bool {
	for _, p := range pairingExemptPrefixes {
		if strings.HasPrefix(key, p) {
			return true
		}
	}
	return false
}

// GlobalSettingsMentionPlacementKeys reports whether the raw bytes of the
// global settings file mention a placement key (node_selector,
// nodeSelector, tolerations, runtime_class_name or runtimeClassName),
// compared case-insensitively. Like GlobalSettingsMentions it is false when
// the file cannot be found or read.
func GlobalSettingsMentionPlacementKeys() bool {
	globalDir, err := GetGlobalDir()
	if err != nil {
		return false
	}
	path := GetSettingsPath(globalDir)
	if path == "" {
		return false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	lower := strings.ToLower(string(data))
	for _, k := range placementKeySpellings {
		if strings.Contains(lower, k) {
			return true
		}
	}
	return false
}

// ConfiguredRuntimeNames returns the names of the runtime entries the
// operator configured and that are in effect. Entries that exist only in
// the embedded default settings, and built-in entries the settings overlay
// retains, are not among them. Kubernetes placement considers only
// configured entries.
//
//   - When the active settings overlay supplies the runtimes section (at
//     least one stored runtime), its stored names only: the settings file's
//     runtimes are then not in effect.
//   - Otherwise the keys of the global settings file's top-level "runtimes"
//     section (exact key), read through the settings loader's own file layer
//     without the embedded defaults (so legacy-format conversion is handled
//     as when the settings load), kept only when the global settings load
//     (the file over the defaults) has them too. A missing file contributes
//     no names.
//
// ok is false when the global directory cannot be resolved, when the
// global settings file exists but cannot be stat'd, read or decoded, when
// its top-level runtimes section is neither a map nor null, or when the
// global settings load fails; callers then treat every entry as configured
// (fail closed). It does not write any file beyond what every settings load
// does (the loader's legacy hub key migration).
func ConfiguredRuntimeNames() (names map[string]bool, ok bool) {
	names = map[string]bool{}
	if o := GetGlobalSettingsOverlay(); o != nil {
		o.mu.RLock()
		supplies := o.active && len(o.runtimes) > 0
		if supplies {
			for name := range o.runtimes {
				names[name] = true
			}
		}
		o.mu.RUnlock()
		if supplies {
			return names, true
		}
	}
	globalDir, err := GetGlobalDir()
	if err != nil {
		return names, false
	}
	if settingsFileUnstatable(globalDir) {
		return names, false
	}
	if GetSettingsPath(globalDir) == "" {
		return names, true
	}
	k := koanf.New(".")
	if _, err := loadSettingsFileConverting(k, globalDir, &legacyLayerState{}); err != nil {
		return names, false
	}
	// The file layer's runtimes section, by the exact top-level key
	// "runtimes". The settings decoder matches a field's exact key first and
	// falls back to a case-insensitive match only when the exact key is
	// absent; the embedded defaults always supply "runtimes", so the settings
	// load never takes a top-level key in another letter case (such as
	// "Runtimes:"). A null section is like an absent one; any other value
	// that is not a map fails closed.
	var fileNames []string
	if section, present := k.Raw()["runtimes"]; present && section != nil {
		m, isMap := section.(map[string]interface{})
		if !isMap {
			return names, false
		}
		for name := range m {
			fileNames = append(fileNames, name)
		}
	}
	// Keep only the names the global settings load itself has (the file over
	// the embedded defaults), so a name counts only when the load uses it.
	loaded, _, err := loadVersionedSettingsFileOnly(globalDir)
	if err != nil {
		return names, false
	}
	for _, name := range fileNames {
		if _, inEffect := loaded.Runtimes[name]; inEffect {
			names[name] = true
		}
	}
	return names, true
}

// GlobalSettingsFileUnstatable reports whether a global settings file name
// (settings.yaml, settings.yml or settings.json) in the global directory
// gives a stat error other than not-exist. The settings loader treats such a
// file as missing, so the settings load without it; Kubernetes placement
// fails closed on it instead.
func GlobalSettingsFileUnstatable() bool {
	globalDir, err := GetGlobalDir()
	if err != nil {
		return false
	}
	return settingsFileUnstatable(globalDir)
}
