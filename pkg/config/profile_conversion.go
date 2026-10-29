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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// The conversion finding codes (.design/flat-runtime-brokers/contract-appendix-p3.md
// FC6). The set is frozen.
const (
	ConversionUnsupportedFlatTargetType  = "unsupported_flat_target_type"
	ConversionFlatTargetScopeViolation   = "flat_target_scope_violation"
	ConversionProfileConflict            = "profile_conflict"
	ConversionProfileConflictResolved    = "profile_conflict_resolved"
	ConversionLegacyFieldNoEffect        = "legacy_field_no_effect"
	ConversionLegacyFieldRemoved         = "legacy_field_removed"
	ConversionUnknownSettingsKey         = "unknown_settings_key"
	ConversionFieldNotValidForTargetType = "field_not_valid_for_target_type"
)

// Severities of a ConversionFinding. A profile named by a blocking finding
// has no ProfileMap entry.
const (
	ConversionSeverityBlocking = "blocking"
	ConversionSeverityNotice   = "notice"
)

// ConversionChoiceTemplatePrefix starts a ConversionChoice.ChosenProfile
// that leaves the field unset on the instance because the operator sets it
// in the named template instead. No template is generated.
const ConversionChoiceTemplatePrefix = "template:"

// ProfileConversionInput is the input of ConvertProfilesToInstances.
type ProfileConversionInput struct {
	// Settings are the effective legacy settings (global settings plus the
	// Hub overlay), never a flat view.
	Settings *VersionedSettings
	// RawSettings is the raw YAML or JSON of the same settings, read for
	// unknown and removed keys, which the typed settings cannot hold. When
	// empty, those checks are skipped.
	RawSettings []byte
	// Profiles are the profiles in scope; empty means all of them.
	Profiles []string
	// Choices are explicit operator choices from a reviewed manifest.
	Choices []ConversionChoice
	// ConfiguredRuntimes reports whether a runtime entry is the operator's
	// (ConfiguredRuntimeNames), as a legacy start decides when it matches
	// sibling entries for placement. nil counts every Kubernetes entry as
	// the operator's (fail closed: a built-in entry with different
	// placement is then a sibling that does not merge, with a notice).
	ConfiguredRuntimes func(string) bool
}

// ConversionChoice is one operator choice. A Split choice sets TargetGroup
// and Split only; a field choice sets TargetGroup, Field and ChosenProfile
// and leaves Split empty.
type ConversionChoice struct {
	// TargetGroup is the key of the group the choice applies to: the
	// instance Key of an unsplit group in ProfileConversionResult.Instances,
	// or, for a group whose profiles differ, the group key the split notice
	// names. For a Split choice it is the key of the group before the
	// split. A field choice consolidates the group's profiles on the chosen
	// value instead of splitting them on it.
	TargetGroup string
	// Field is the conflicting field, as the settings path under a profile
	// (for example "shared_dir_size" or "harness_overrides.claude.image");
	// the finding's "profiles.*." prefix may be included.
	Field string
	// ChosenProfile names the profile whose value the instance takes, or
	// "template:<name>" to leave the field unset on the instance.
	ChosenProfile string
	// Split keeps these profiles on their own instance on the same target
	// scope.
	Split []string
}

// ProfileConversionResult is the result of ConvertProfilesToInstances.
type ProfileConversionResult struct {
	// Instances has one proposed instance per target group, or per set of
	// profiles with equal values when a group's profiles differ, sorted by
	// Key. Key and Name are proposals the operator may edit.
	Instances []V1RuntimeBrokerInstanceConfig
	// ProfileMap maps each profile with no blocking finding to its
	// instance key.
	ProfileMap map[string]string
	// Findings are sorted by Path.
	Findings []ConversionFinding
}

// ConversionFinding reports one conversion result that is not a silent
// copy: a conflict, a value that cannot move to a flat instance, or a
// legacy key that has no effect.
type ConversionFinding struct {
	Code     string
	Severity string
	// Path is the legacy settings path, for example
	// "profiles.batch.shared_dir_size". A conflict between profiles uses
	// "profiles.*.<field>", where * stands for the profiles in Profiles.
	Path     string
	Profiles []string
	Message  string
}

var (
	knownProfileKeys      = knownYAMLKeys(V1ProfileConfig{})
	knownRuntimeEntryKeys = knownYAMLKeys(V1RuntimeConfig{})
)

// convProfile is one profile in scope, with its folded instance values.
type convProfile struct {
	name   string
	typ    string // docker or kubernetes once the target is supported
	scope  string
	ctx    string
	ns     string
	fields map[string]convValue
}

// convValue is one folded field value and the legacy key it came from.
type convValue struct {
	v   interface{}
	src string
}

type convGroup struct {
	key string
	// seed identifies the group's scope (plus its members for a Split
	// group); a hash of it makes the key unique.
	seed     string
	scope    string
	typ      string
	ctx      string
	ns       string
	profiles []*convProfile
}

type convState struct {
	findings map[string]*ConversionFinding
}

// add records a finding. Findings with the same code and path are merged,
// so a runtime entry shared by several profiles is reported once with all
// of them.
func (s *convState) add(code, severity, path, message string, profiles ...string) {
	k := code + "\x00" + path + "\x00" + message
	f, ok := s.findings[k]
	if !ok {
		f = &ConversionFinding{Code: code, Severity: severity, Path: path, Message: message}
		s.findings[k] = f
	}
	for _, p := range profiles {
		if !containsString(f.Profiles, p) {
			f.Profiles = append(f.Profiles, p)
		}
	}
	sort.Strings(f.Profiles)
}

// ConvertProfilesToInstances proposes the flat Runtime Broker instances
// that replace the legacy profiles of in.Settings, one per target group
// (contract-appendix-p3.md FC6). It is pure and deterministic: it reads
// only its input, writes nothing, and returns sorted output.
//
// Each profile's values are folded as today's resolvers resolve them (the
// profile's value, else its runtime entry's; maps per key; for the shared
// dir backends a profile's single value shadows the runtime entry's per-dir
// map). Profiles are grouped by resolved target scope. When the profiles of
// a group disagree on a field, every value is preserved: the group is split
// into one instance per set of profiles with equal values, and a notice
// names the differing fields so the operator can consolidate. An operator
// field choice consolidates a field on the chosen value instead, and the
// values it drops are reported. Nothing is merged or chosen silently, and
// no template is generated. Only a detail no flat instance can hold blocks
// a profile. Secret declarations are copied; no secret value is read or
// emitted.
func ConvertProfilesToInstances(in ProfileConversionInput) ProfileConversionResult {
	st := &convState{findings: map[string]*ConversionFinding{}}
	vs := in.Settings
	if vs == nil {
		vs = &VersionedSettings{}
	}

	names := in.Profiles
	if len(names) == 0 {
		for name := range vs.Profiles {
			names = append(names, name)
		}
	}
	names = uniqueSorted(names)

	var profiles []*convProfile
	for _, name := range names {
		prof, ok := vs.Profiles[name]
		if !ok {
			st.add(ConversionUnknownSettingsKey, ConversionSeverityBlocking, "profiles."+name,
				fmt.Sprintf("profile %s is not defined in these settings; remove it from the profiles in scope", name), name)
			continue
		}
		if cp := foldProfile(st, vs, in.ConfiguredRuntimes, name, prof); cp != nil {
			profiles = append(profiles, cp)
		}
	}

	if vs.ActiveProfile != "" && containsString(names, vs.ActiveProfile) {
		st.add(ConversionLegacyFieldNoEffect, ConversionSeverityNotice, "active_profile",
			fmt.Sprintf("active_profile selects profile %s for a create that names no profile on a legacy Runtime Broker; a flat Runtime Broker instance never reads it, and such a create goes to the instance its Runtime Broker is", vs.ActiveProfile),
			vs.ActiveProfile)
	}

	if len(in.RawSettings) > 0 {
		checkRawConversionKeys(st, vs, in.RawSettings, names)
	}

	groups := groupProfiles(profiles)
	groups = applySplitChoices(st, groups, in.Choices)

	result := ProfileConversionResult{ProfileMap: map[string]string{}}
	fieldChoices := fieldChoicesByGroup(st, groups, in.Choices)
	splits := make([]*convSplit, 0, len(groups))
	var parts []*convGroup
	for _, g := range groups {
		sp := splitGroup(g, fieldChoices[g.key])
		splits = append(splits, sp)
		parts = append(parts, sp.parts...)
	}
	uniqueGroupKeys(parts)
	for _, sp := range splits {
		result.Instances = append(result.Instances, buildSplitInstances(st, sp, fieldChoices[sp.key])...)
	}
	sort.Slice(result.Instances, func(i, j int) bool { return result.Instances[i].Key < result.Instances[j].Key })
	reportUnusedFieldChoices(st, splits, fieldChoices)

	blocked := map[string]bool{}
	for _, f := range st.findings {
		if f.Severity == ConversionSeverityBlocking {
			for _, p := range f.Profiles {
				blocked[p] = true
			}
		}
	}
	for _, g := range parts {
		for _, p := range g.profiles {
			if !blocked[p.name] {
				result.ProfileMap[p.name] = g.key
			}
		}
	}

	for _, f := range st.findings {
		result.Findings = append(result.Findings, *f)
	}
	sort.Slice(result.Findings, func(i, j int) bool {
		a, b := result.Findings[i], result.Findings[j]
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		if a.Code != b.Code {
			return a.Code < b.Code
		}
		if pa, pb := strings.Join(a.Profiles, ","), strings.Join(b.Profiles, ","); pa != pb {
			return pa < pb
		}
		return a.Message < b.Message
	})
	return result
}

// flatTargetType maps a legacy runtime type to its flat target type, or ""
// when it has none. "k8s" and "remote" are the legacy names of kubernetes.
func flatTargetType(t string) string {
	switch t {
	case RuntimeTargetTypeDocker:
		return RuntimeTargetTypeDocker
	case RuntimeTargetTypeKubernetes, "k8s", "remote":
		return RuntimeTargetTypeKubernetes
	}
	return ""
}

// foldProfile resolves profile name's target and folds its values per
// field. It returns nil when the profile cannot become a flat instance
// (a blocking finding says why).
func foldProfile(st *convState, vs *VersionedSettings, configured func(string) bool, name string, prof V1ProfileConfig) *convProfile {
	pp := "profiles." + name + "."
	if prof.Runtime == "" {
		st.add(ConversionUnsupportedFlatTargetType, ConversionSeverityBlocking, pp+"runtime",
			fmt.Sprintf("profile %s names no runtime entry, so its runtime type is detected on the host and has no fixed flat Runtime Broker target; set its runtime to a docker or kubernetes runtime entry, or keep profile %s on its legacy Runtime Broker", name, name), name)
		return nil
	}
	rtKey := prof.Runtime
	rt := vs.Runtimes[rtKey]
	rp := "runtimes." + rtKey + "."
	legacyType := rt.Type
	typePath := rp + "type"
	if legacyType == "" {
		// As ProfileRuntimeType: an entry without a type, or no entry at
		// all, takes the entry key as its type.
		legacyType = rtKey
		typePath = pp + "runtime"
	}
	typ := flatTargetType(legacyType)
	ok := true
	if typ == "" {
		st.add(ConversionUnsupportedFlatTargetType, ConversionSeverityBlocking, typePath,
			fmt.Sprintf("runtime type %s has no flat Runtime Broker target; keep the profiles that use it on their legacy Runtime Broker", legacyType), name)
		ok = false
	}
	for _, b := range []struct {
		key string
		set bool
	}{{"cloudrun", rt.CloudRun != nil}, {"cloudrun_instances", rt.CloudRunInstances != nil},
		{"cloudrun_sandbox", rt.CloudRunSandbox != nil}, {"substrate", rt.Substrate != nil}} {
		if b.set {
			st.add(ConversionUnsupportedFlatTargetType, ConversionSeverityBlocking, rp+b.key,
				fmt.Sprintf("runtime entry %s configures %s, which has no flat Runtime Broker target; keep the profiles that use it on their legacy Runtime Broker", rtKey, b.key), name)
			ok = false
		}
	}
	if rt.ListAllNamespaces {
		st.add(ConversionFlatTargetScopeViolation, ConversionSeverityBlocking, rp+"list_all_namespaces",
			fmt.Sprintf("runtime entry %s lists agents in every namespace, but a flat Kubernetes Runtime Broker instance is one cluster and namespace; declare one instance per namespace", rtKey), name)
		ok = false
	}

	// Keys with no effect today are reported, never dropped silently.
	for _, f := range []struct {
		key string
		set bool
	}{{"host", rt.Host != ""}, {"env", len(rt.Env) > 0}, {"sync", rt.Sync != ""}} {
		if f.set {
			st.add(ConversionLegacyFieldNoEffect, ConversionSeverityNotice, rp+f.key,
				fmt.Sprintf("runtimes.%s.%s has no effect today and has no flat Runtime Broker instance field; it is not converted", rtKey, f.key), name)
		}
	}
	// A profile pool that differs from its runtime entry's is preserved:
	// the profile's selector keys are folded over the entry's below.
	if typ == RuntimeTargetTypeKubernetes && ProfilePoolConflict(rt, prof) {
		st.add(ConversionProfileConflict, ConversionSeverityNotice, pp+"node_selector",
			fmt.Sprintf("profiles.%s.node_selector sets %s to %s and its runtime entry %s sets %s; the instance keeps the profile's configured pool %s",
				name, PoolNodeSelectorKey, prof.NodeSelector[PoolNodeSelectorKey], rtKey, rt.NodeSelector[PoolNodeSelectorKey], prof.NodeSelector[PoolNodeSelectorKey]), name)
	}
	// Runtime entries that may share a cluster and namespace merge their
	// placement when they agree. When they disagree, each entry keeps its
	// own placement, and profiles on them are split onto separate
	// instances when their placement differs.
	placementEntries := []string{rtKey}
	if typ == RuntimeTargetTypeKubernetes {
		entries, other := PlacementSiblingEntries(vs.Runtimes, rtKey, configured)
		if other == "" {
			placementEntries = entries
		} else {
			st.add(ConversionProfileConflict, ConversionSeverityNotice, strings.TrimSuffix(rp, "."),
				fmt.Sprintf("runtime entries %s, %s may target the same cluster and namespace (equal, or empty context or namespace) with different placement (a legacy start is refused today when both resolve to the same cluster and namespace); profiles on runtime entry %s keep its own placement, on a separate instance when it differs", rtKey, other, rtKey), name)
		}
	}
	if prof.DefaultTemplate != "" {
		st.add(ConversionLegacyFieldNoEffect, ConversionSeverityNotice, pp+"default_template",
			fmt.Sprintf("profiles.%s.default_template has no effect today (the default template comes from the top-level default_template and the Hub agent_defaults); it is not converted", name), name)
	}
	if !ok {
		return nil
	}

	cp := &convProfile{name: name, typ: typ, fields: map[string]convValue{}}
	if typ == RuntimeTargetTypeKubernetes {
		cp.ctx, cp.ns = strings.TrimSpace(rt.Context), strings.TrimSpace(rt.Namespace)
		cp.scope = "kubernetes\x00" + cp.ctx + "\x00" + cp.ns
	} else {
		cp.scope = RuntimeTargetTypeDocker
	}

	set := func(field string, v interface{}, src string) {
		cp.fields[field] = convValue{v: v, src: src}
	}
	str := func(field, pv, rv string) {
		if pv != "" {
			set(field, pv, pp+field)
		} else if rv != "" {
			set(field, rv, rp+field)
		}
	}

	// runtime_target, rule 2: the profile's value, else the runtime entry's.
	if rt.GKE {
		set("gke", true, rp+"gke")
	}
	str("priority_class_name", "", rt.PriorityClassName)
	str("shared_dir_storage_class", prof.SharedDirStorageClass, rt.SharedDirStorageClass)
	str("shared_dir_size", prof.SharedDirSize, rt.SharedDirSize)
	if prof.SafeToEvict != nil {
		set("safe_to_evict", *prof.SafeToEvict, pp+"safe_to_evict")
	} else if rt.SafeToEvict != nil {
		set("safe_to_evict", *rt.SafeToEvict, rp+"safe_to_evict")
	}
	str("kubernetes_block_service_account", prof.KubernetesBlockServiceAccount, rt.KubernetesBlockServiceAccount)
	// Service-account mappings are a per-key lookup, the profile first,
	// with no single value that shadows the map.
	for gsa, ksa := range rt.KubernetesServiceAccountMappings {
		if ksa != "" {
			set("kubernetes_service_account_mappings."+gsa, ksa, rp+"kubernetes_service_account_mappings."+gsa)
		}
	}
	for gsa, ksa := range prof.KubernetesServiceAccountMappings {
		if ksa != "" {
			set("kubernetes_service_account_mappings."+gsa, ksa, pp+"kubernetes_service_account_mappings."+gsa)
		}
	}
	// Placement: the runtime entry's (and its agreeing siblings', as a
	// legacy start matches them), with the profile's over it, merged
	// exactly as a legacy start merges them (LegacyPlacementPolicy).
	placement := LegacyPlacementPolicy(vs.Runtimes, placementEntries, name, &prof)
	placementSrc := func(field string, inProfile bool) string {
		if inProfile {
			return pp + field
		}
		return rp + field
	}
	if len(placement.NodeSelector) > 0 {
		set("node_selector", placement.NodeSelector, placementSrc("node_selector", len(prof.NodeSelector) > 0))
	}
	if len(placement.Tolerations) > 0 {
		set("tolerations", placement.Tolerations, placementSrc("tolerations", len(prof.Tolerations) > 0))
	}
	if placement.RuntimeClassName != "" {
		set("runtime_class_name", placement.RuntimeClassName, placementSrc("runtime_class_name", prof.RuntimeClassName != ""))
	}
	// The per-profile and per-runtime storage overrides are removed
	// (design OQ2-A): each one set is reported and none is converted.
	reportRemovedStorageOverrides(st, name, rtKey, prof, rt)

	// A docker target has no Kubernetes settings: a legacy docker runtime
	// ignores them, so they are not converted, and each one set is
	// reported.
	if typ == RuntimeTargetTypeDocker {
		for field := range cp.fields {
			if isKubernetesOnlyTargetField(field) {
				delete(cp.fields, field)
			}
		}
		reportDockerKubernetesFields(st, name, rtKey, prof, rt)
	}

	// agent_defaults: copied from the profile (rule 2; these keys exist
	// only on profiles).
	str("default_harness_config", prof.DefaultHarnessConfig, "")
	str("image_registry", prof.ImageRegistry, "")
	if prof.CloneDepth != "" {
		set("clone_depth", prof.CloneDepth, pp+"clone_depth")
	}
	if len(prof.Volumes) > 0 {
		set("volumes", copyVolumeMounts(prof.Volumes), pp+"volumes")
	}
	if prof.Resources != nil {
		set("resources", *prof.Resources, pp+"resources")
	}
	if len(prof.Secrets) > 0 {
		set("secrets", copyRequiredSecrets(prof.Secrets), pp+"secrets")
	}
	for hc, o := range prof.HarnessOverrides {
		hp := "harness_overrides." + hc
		set(hp, true, pp+hp)
		str(hp+".image", o.Image, "")
		str(hp+".image_pull_policy", o.ImagePullPolicy, "")
		str(hp+".user", o.User, "")
		str(hp+".auth_selected_type", o.AuthSelectedType, "")
		if len(o.Env) > 0 {
			set(hp+".env", copyStringMap(o.Env), pp+hp+".env")
		}
		if len(o.Volumes) > 0 {
			set(hp+".volumes", copyVolumeMounts(o.Volumes), pp+hp+".volumes")
		}
		if o.Resources != nil {
			set(hp+".resources", *o.Resources, pp+hp+".resources")
		}
	}
	return cp
}

// isKubernetesOnlyTargetField reports whether a folded runtime_target field
// applies only to a kubernetes target.
func isKubernetesOnlyTargetField(field string) bool {
	switch field {
	case "gke", "priority_class_name", "shared_dir_storage_class", "shared_dir_size", "safe_to_evict",
		"kubernetes_block_service_account", "node_selector", "tolerations", "runtime_class_name":
		return true
	}
	return strings.HasPrefix(field, "kubernetes_service_account_mappings.")
}

// reportDockerKubernetesFields reports every Kubernetes-only key that
// profile name or its docker runtime entry sets.
func reportDockerKubernetesFields(st *convState, name, rtKey string, prof V1ProfileConfig, rt V1RuntimeConfig) {
	report := func(path string) {
		st.add(ConversionFieldNotValidForTargetType, ConversionSeverityNotice, path,
			fmt.Sprintf("%s is not valid for runtime target type docker; a docker runtime ignores it today, so it is not converted", path), name)
	}
	pp, rp := "profiles."+name+".", "runtimes."+rtKey+"."
	for _, f := range []struct {
		key string
		set bool
	}{
		{"shared_dir_storage_class", prof.SharedDirStorageClass != ""}, {"shared_dir_size", prof.SharedDirSize != ""},
		{"safe_to_evict", prof.SafeToEvict != nil},
		{"kubernetes_service_account_mappings", len(prof.KubernetesServiceAccountMappings) > 0},
		{"kubernetes_block_service_account", prof.KubernetesBlockServiceAccount != ""},
		{"node_selector", len(prof.NodeSelector) > 0}, {"tolerations", len(prof.Tolerations) > 0},
		{"runtime_class_name", prof.RuntimeClassName != ""},
	} {
		if f.set {
			report(pp + f.key)
		}
	}
	for _, f := range []struct {
		key string
		set bool
	}{
		{"context", rt.Context != ""}, {"namespace", rt.Namespace != ""}, {"gke", rt.GKE},
		{"priority_class_name", rt.PriorityClassName != ""},
		{"shared_dir_storage_class", rt.SharedDirStorageClass != ""}, {"shared_dir_size", rt.SharedDirSize != ""},
		{"safe_to_evict", rt.SafeToEvict != nil},
		{"kubernetes_service_account_mappings", len(rt.KubernetesServiceAccountMappings) > 0},
		{"kubernetes_block_service_account", rt.KubernetesBlockServiceAccount != ""},
		{"node_selector", len(rt.NodeSelector) > 0}, {"tolerations", len(rt.Tolerations) > 0},
		{"runtime_class_name", rt.RuntimeClassName != ""},
	} {
		if f.set {
			report(rp + f.key)
		}
	}
}

// reportRemovedStorageOverrides reports each removed storage override
// (shared_dir_storage_backend, shared_dir_storage_backends,
// home_storage_backend, home_storage_leaf) that profile name or its runtime
// entry sets, as not converted. The message names the replacement,
// server.project_storage and volumes: (RemovedStorageOverrideErrors).
func reportRemovedStorageOverrides(st *convState, name, rtKey string, prof V1ProfileConfig, rt V1RuntimeConfig) {
	report := func(path string) {
		st.add(ConversionLegacyFieldRemoved, ConversionSeverityNotice, path,
			fmt.Sprintf("%s is no longer supported and is not converted: %s", path, removedOverrideRemedyFn), name)
	}
	type key struct {
		key string
		set bool
	}
	pp, rp := "profiles."+name+".", "runtimes."+rtKey+"."
	for _, f := range []key{
		{"shared_dir_storage_backend", prof.SharedDirStorageBackend != ""},
		{"shared_dir_storage_backends", len(prof.SharedDirStorageBackends) > 0},
		{"home_storage_backend", prof.HomeStorageBackend != ""},
		{"home_storage_leaf", prof.HomeStorageLeaf != ""},
	} {
		if f.set {
			report(pp + f.key)
		}
	}
	for _, f := range []key{
		{"shared_dir_storage_backend", rt.SharedDirStorageBackend != ""},
		{"shared_dir_storage_backends", len(rt.SharedDirStorageBackends) > 0},
		{"home_storage_backend", rt.HomeStorageBackend != ""},
		{"home_storage_leaf", rt.HomeStorageLeaf != ""},
	} {
		if f.set {
			report(rp + f.key)
		}
	}
}

// groupProfiles groups profiles by resolved target scope (rule 1), with the
// proposed instance key derived from the scope. Groups are sorted by key.
func groupProfiles(profiles []*convProfile) []*convGroup {
	byScope := map[string]*convGroup{}
	for _, p := range profiles {
		g, ok := byScope[p.scope]
		if !ok {
			g = &convGroup{seed: p.scope, scope: p.scope, typ: p.typ, ctx: p.ctx, ns: p.ns}
			if p.typ == RuntimeTargetTypeKubernetes {
				g.key = conversionKey(p.scope, "k8s", p.ctx, p.ns)
			} else {
				g.key = conversionKey(p.scope, RuntimeTargetTypeDocker)
			}
			byScope[p.scope] = g
		}
		g.profiles = append(g.profiles, p)
	}
	var out []*convGroup
	for _, g := range byScope {
		out = append(out, g)
	}
	uniqueGroupKeys(out)
	sortGroups(out)
	return out
}

func sortGroups(groups []*convGroup) {
	sort.Slice(groups, func(i, j int) bool { return groups[i].key < groups[j].key })
}

// uniqueGroupKeys gives each group whose key another group shares a key
// with a short hash of its own seed, so that every group has its own key.
// Distinct scopes can build the same text (context "prod" and namespace
// "east" against namespace "prod-east"), and so can a Split group and
// another scope. A group's new key depends only on its key and seed, so the
// result is still a function of the input.
func uniqueGroupKeys(groups []*convGroup) {
	for round := 0; round < 4; round++ {
		count := map[string]int{}
		for _, g := range groups {
			count[g.key]++
		}
		dup := false
		for _, g := range groups {
			if count[g.key] > 1 {
				dup = true
				g.key = hashedConversionKey(g.key, g.seed+strings.Repeat("\x00", round))
			}
		}
		if !dup {
			return
		}
	}
}

// conversionKey builds an instance key from prefix and the non-empty parts,
// each made key-safe (lower-case letters, digits and "-"). When that
// changes a part, or the key would be too long, a short hash of seed is
// appended. The result is a function of its arguments only. Two distinct
// scopes can still build the same key; uniqueGroupKeys resolves that across
// the groups of one conversion.
func conversionKey(seed, prefix string, parts ...string) string {
	key, lossy := prefix, false
	for _, part := range parts {
		if part == "" {
			continue
		}
		s := keySafe(part)
		if s != part {
			lossy = true
		}
		if s != "" {
			key += "-" + s
		}
	}
	if !lossy && len(key) <= 63 {
		return key
	}
	return hashedConversionKey(key, seed)
}

// hashedConversionKey returns key, cut to fit, with "-" and the first 8 hex
// digits of the sha256 of seed appended: at most 63 characters, ending in a
// letter or digit.
func hashedConversionKey(key, seed string) string {
	sum := sha256.Sum256([]byte(seed))
	if len(key) > 54 {
		key = strings.TrimRight(key[:54], "-")
	}
	return key + "-" + hex.EncodeToString(sum[:])[:8]
}

// keySafe lower-cases s and replaces each run of other characters than
// letters and digits with one "-", trimming "-" at both ends.
func keySafe(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			dash = false
		} else if !dash {
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

// applySplitChoices moves the profiles of each Split choice to their own
// group on the same scope. An invalid choice is a blocking finding.
func applySplitChoices(st *convState, groups []*convGroup, choices []ConversionChoice) []*convGroup {
	byKey := map[string]*convGroup{}
	for _, g := range groups {
		byKey[g.key] = g
	}
	split := map[string]bool{}
	var out []*convGroup
	for i, c := range choices {
		if len(c.Split) == 0 {
			continue
		}
		path := fmt.Sprintf("choices[%d]", i)
		g, ok := byKey[c.TargetGroup]
		if c.Field != "" || c.ChosenProfile != "" {
			invalidChoice(st, path, "a Split choice sets only target_group and split", g)
			continue
		}
		if !ok {
			invalidChoice(st, path, fmt.Sprintf("target group %q is not a proposed instance key", c.TargetGroup), nil)
			continue
		}
		members := uniqueSorted(c.Split)
		var moved []*convProfile
		valid := true
		for _, name := range members {
			p := groupProfile(g, name)
			if p == nil || split[name] {
				invalidChoice(st, path, fmt.Sprintf("profile %q is not in target group %s, or is already split", name, g.key), g)
				valid = false
				break
			}
			moved = append(moved, p)
		}
		if !valid {
			continue
		}
		for _, p := range moved {
			split[p.name] = true
		}
		seed := g.scope + "\x00" + strings.Join(members, "\x00")
		out = append(out, &convGroup{
			key:      conversionKey(seed, g.key, strings.Join(members, "-")),
			seed:     seed,
			scope:    g.scope,
			typ:      g.typ,
			ctx:      g.ctx,
			ns:       g.ns,
			profiles: moved,
		})
	}
	for _, g := range groups {
		var rest []*convProfile
		for _, p := range g.profiles {
			if !split[p.name] {
				rest = append(rest, p)
			}
		}
		if len(rest) > 0 {
			g.profiles = rest
			out = append(out, g)
		}
	}
	uniqueGroupKeys(out)
	sortGroups(out)
	return out
}

func groupProfile(g *convGroup, name string) *convProfile {
	for _, p := range g.profiles {
		if p.name == name {
			return p
		}
	}
	return nil
}

func groupProfileNames(g *convGroup) []string {
	if g == nil {
		return nil
	}
	out := make([]string, 0, len(g.profiles))
	for _, p := range g.profiles {
		out = append(out, p.name)
	}
	return out
}

// invalidChoice reports an operator choice that names something the
// conversion does not have. It blocks the profiles of the group it names,
// because the manifest it came from needs fixing.
func invalidChoice(st *convState, path, why string, g *convGroup) {
	st.add(ConversionUnknownSettingsKey, ConversionSeverityBlocking, path,
		"operator choice cannot be applied: "+why, groupProfileNames(g)...)
}

type fieldChoice struct {
	index  int
	choice ConversionChoice
	used   bool
}

// fieldChoicesByGroup indexes the field choices by group key and field.
func fieldChoicesByGroup(st *convState, groups []*convGroup, choices []ConversionChoice) map[string]map[string]*fieldChoice {
	byKey := map[string]*convGroup{}
	for _, g := range groups {
		byKey[g.key] = g
	}
	out := map[string]map[string]*fieldChoice{}
	for i, c := range choices {
		if len(c.Split) > 0 {
			continue
		}
		path := fmt.Sprintf("choices[%d]", i)
		g, ok := byKey[c.TargetGroup]
		if !ok {
			invalidChoice(st, path, fmt.Sprintf("target group %q is not a proposed instance key", c.TargetGroup), nil)
			continue
		}
		field := strings.TrimPrefix(c.Field, "profiles.*.")
		if field == "" || c.ChosenProfile == "" {
			invalidChoice(st, path, "a field choice sets target_group, field and chosen_profile", g)
			continue
		}
		if tmpl, isTemplate := strings.CutPrefix(c.ChosenProfile, ConversionChoiceTemplatePrefix); isTemplate {
			if tmpl == "" {
				invalidChoice(st, path, "the template choice names no template", g)
				continue
			}
			// Only a workload default has a template setting that
			// outranks the instance value; an infrastructure field left
			// unset would silently fall through to the global setting.
			if !isWorkloadField(field) {
				invalidChoice(st, path, fmt.Sprintf("field %s is not a workload default, so no template can set it; choose a profile's value, or split the profiles onto separate instances", field), g)
				continue
			}
		} else if groupProfile(g, c.ChosenProfile) == nil {
			invalidChoice(st, path, fmt.Sprintf("profile %q is not in target group %s", c.ChosenProfile, g.key), g)
			continue
		}
		if out[g.key] == nil {
			out[g.key] = map[string]*fieldChoice{}
		}
		if _, dup := out[g.key][field]; dup {
			invalidChoice(st, path, fmt.Sprintf("field %s of target group %s already has a choice", field, g.key), g)
			continue
		}
		out[g.key][field] = &fieldChoice{index: i, choice: c}
	}
	return out
}

// reportUnusedFieldChoices reports each field choice that matched no
// conflict, so a stale manifest entry is never ignored silently.
func reportUnusedFieldChoices(st *convState, splits []*convSplit, choices map[string]map[string]*fieldChoice) {
	for _, sp := range splits {
		for field, fc := range choices[sp.key] {
			if !fc.used {
				invalidChoice(st, fmt.Sprintf("choices[%d]", fc.index),
					fmt.Sprintf("field %s of target group %s has no conflict to resolve", field, sp.key), sp.parent)
			}
		}
	}
}

// convSplit is one group (a scope group or an operator Split group) and
// the instances it becomes: one when its profiles agree on every field it
// splits on (splitsOn), else one per distinct set of those values.
type convSplit struct {
	parent *convGroup
	// key is the parent's key as operator choices name it, kept even when
	// uniqueGroupKeys later renames an unsplit parent's instance.
	key         string
	fields      []string // every folded field of the parent's profiles, sorted
	conflicting map[string]bool
	splitOn     []string // the conflicting fields the parent splits on, sorted
	parts       []*convGroup
}

// splitsOn reports whether profiles of one group that differ in field go
// to separate instances. It is the one place that decides which details a
// split preserves: every field a flat instance holds, except one an
// operator field choice consolidates, and except a harness override's
// presence when a choice consolidates one of its conflicting sub-fields
// (the chosen sub-field decides whether the override exists).
func splitsOn(field string, choices map[string]*fieldChoice, conflicting map[string]bool) bool {
	if choices[field] != nil {
		return false
	}
	rest, ok := strings.CutPrefix(field, "harness_overrides.")
	if !ok {
		return true
	}
	hc, sub := splitHarnessOverrideField(rest)
	if sub != "" {
		return true
	}
	for f := range choices {
		if r, isOverride := strings.CutPrefix(f, "harness_overrides."); isOverride {
			if fhc, fsub := splitHarnessOverrideField(r); fhc == hc && fsub != "" && conflicting[f] {
				return false
			}
		}
	}
	return true
}

// splitGroup partitions g's profiles by the fields it splits on: each
// profile, in name order, joins the first part whose first profile has
// equal values (conversionValuesEqual) for all of them. With one part, g
// is unchanged. Otherwise each part gets g's key with its profile names
// appended, as an operator Split does.
func splitGroup(g *convGroup, choices map[string]*fieldChoice) *convSplit {
	sp := &convSplit{parent: g, key: g.key, conflicting: map[string]bool{}}
	seen := map[string]bool{}
	for _, p := range g.profiles {
		for f := range p.fields {
			if !seen[f] {
				seen[f] = true
				sp.fields = append(sp.fields, f)
			}
		}
	}
	sort.Strings(sp.fields)
	for _, field := range sp.fields {
		first := g.profiles[0].fields[field]
		for _, p := range g.profiles[1:] {
			if !conversionValuesEqual(field, first.v, p.fields[field].v) {
				sp.conflicting[field] = true
				break
			}
		}
	}
	for _, field := range sp.fields {
		if sp.conflicting[field] && splitsOn(field, choices, sp.conflicting) {
			sp.splitOn = append(sp.splitOn, field)
		}
	}

	var groupsOfParts [][]*convProfile
	for _, p := range g.profiles {
		placed := false
		for i, part := range groupsOfParts {
			if sameSplitValues(sp.splitOn, part[0], p) {
				groupsOfParts[i] = append(part, p)
				placed = true
				break
			}
		}
		if !placed {
			groupsOfParts = append(groupsOfParts, []*convProfile{p})
		}
	}
	if len(groupsOfParts) == 1 {
		sp.parts = []*convGroup{g}
		return sp
	}
	for _, members := range groupsOfParts {
		names := make([]string, 0, len(members))
		for _, p := range members {
			names = append(names, p.name)
		}
		seed := g.seed + "\x00split\x00" + strings.Join(names, "\x00")
		sp.parts = append(sp.parts, &convGroup{
			key:      conversionKey(seed, g.key, strings.Join(names, "-")),
			seed:     seed,
			scope:    g.scope,
			typ:      g.typ,
			ctx:      g.ctx,
			ns:       g.ns,
			profiles: members,
		})
	}
	return sp
}

// sameSplitValues reports whether profiles a and b agree on every field.
func sameSplitValues(fields []string, a, b *convProfile) bool {
	for _, f := range fields {
		if !conversionValuesEqual(f, a.fields[f].v, b.fields[f].v) {
			return false
		}
	}
	return true
}

// buildSplitInstances builds the instances of one split group (rule 3, as
// amended: a difference is preserved on separate instances). A field that
// an operator choice consolidates is reported once for the whole group,
// with every profile's value, and every part takes the chosen value.
func buildSplitInstances(st *convState, sp *convSplit, choices map[string]*fieldChoice) []V1RuntimeBrokerInstanceConfig {
	g := sp.parent
	profiles := groupProfileNames(g)
	if len(sp.parts) > 1 {
		var parts []string
		for _, part := range sp.parts {
			parts = append(parts, fmt.Sprintf("%s (%s)", part.key, strings.Join(groupProfileNames(part), ", ")))
		}
		st.add(ConversionProfileConflict, ConversionSeverityNotice, "profiles.*",
			fmt.Sprintf("profiles %s on target %s differ in %s; they are split into instances %s. To merge them, give the profiles the same values or add a field choice for target %s",
				strings.Join(profiles, ", "), sp.key, strings.Join(sp.splitOn, ", "), strings.Join(parts, ", "), sp.key), profiles...)
	}

	type chosenValue struct {
		v   interface{}
		set bool
	}
	chosen := map[string]chosenValue{}
	splitField := map[string]bool{}
	for _, f := range sp.splitOn {
		splitField[f] = true
	}
	for _, field := range sp.fields {
		if !sp.conflicting[field] || splitField[field] {
			continue
		}
		// A harness override that only some profiles have differs in its
		// presence too. When one of its sub-fields conflicts, that
		// conflict already names the override, so the presence is not
		// reported again; the entry exists once a sub-field is agreed or
		// chosen.
		if coveredHarnessOverrideMarker(field, sp.conflicting) {
			continue
		}
		path := "profiles.*." + field
		fc := choices[field] // not nil: an unchosen difference is split on
		fc.used = true
		if tmpl, isTemplate := strings.CutPrefix(fc.choice.ChosenProfile, ConversionChoiceTemplatePrefix); isTemplate {
			st.add(ConversionProfileConflictResolved, ConversionSeverityNotice, path,
				fmt.Sprintf("operator choice: %s stays unset on instance %s and template %s sets it; no template is generated, so create or update template %s before the profiles are retired (%s)",
					field, sp.key, tmpl, tmpl, conflictValues(g, field)), profiles...)
			continue
		}
		// A chosen profile that leaves the field unset leaves it unset on
		// the instance too.
		cv, set := groupProfile(g, fc.choice.ChosenProfile).fields[field]
		chosen[field] = chosenValue{v: cv.v, set: set}
		st.add(ConversionProfileConflictResolved, ConversionSeverityNotice, path,
			fmt.Sprintf("operator choice: instance %s takes profile %s's %s (%s)", sp.key, fc.choice.ChosenProfile, field, conflictValues(g, field)), profiles...)
	}

	var out []V1RuntimeBrokerInstanceConfig
	for _, part := range sp.parts {
		target := &V1RuntimeTargetConfig{Type: part.typ, Context: part.ctx, Namespace: part.ns}
		defaults := &V1InstanceAgentDefaults{}
		for _, field := range sp.fields {
			switch {
			case !sp.conflicting[field]:
				// Agreed by the group: copied as the group's first profile
				// writes it, so every part carries the same value.
				if cv, set := g.profiles[0].fields[field]; set {
					setInstanceField(target, defaults, field, cv.v)
				}
			case splitField[field]:
				// Agreed by this part, which is split on the field.
				if cv, set := part.profiles[0].fields[field]; set {
					setInstanceField(target, defaults, field, cv.v)
				}
			default:
				if c, ok := chosen[field]; ok && c.set {
					setInstanceField(target, defaults, field, c.v)
				}
			}
		}
		inst := V1RuntimeBrokerInstanceConfig{Key: part.key, Name: part.key, RuntimeTarget: target}
		if !reflect.DeepEqual(*defaults, V1InstanceAgentDefaults{}) {
			inst.AgentDefaults = defaults
		}
		out = append(out, inst)
	}
	return out
}

// conversionValuesEqual is rule 3's deep equality. A missing value (nil)
// differs from every set one, so false and unset safe_to_evict differ.
// Lists are order-sensitive, except secret declarations, which compare as
// a set keyed by key, and tolerations, which compare as a set of (key,
// operator with "" as Equal, value, effect).
func conversionValuesEqual(field string, a, b interface{}) bool {
	if field == "tolerations" {
		at, aok := a.([]api.K8sToleration)
		bt, bok := b.([]api.K8sToleration)
		return aok == bok && reflect.DeepEqual(tolerationSet(at), tolerationSet(bt))
	}
	if field == "secrets" {
		as, _ := a.([]api.RequiredSecret)
		bs, _ := b.([]api.RequiredSecret)
		return reflect.DeepEqual(sortedSecrets(as), sortedSecrets(bs))
	}
	return reflect.DeepEqual(a, b)
}

func sortedSecrets(s []api.RequiredSecret) []api.RequiredSecret {
	out := copyRequiredSecrets(s)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// setInstanceField stores an agreed or chosen value on the instance. It
// panics on a field name the fold does not produce, which is a
// programming error.
func setInstanceField(t *V1RuntimeTargetConfig, d *V1InstanceAgentDefaults, field string, v interface{}) {
	if rest, ok := strings.CutPrefix(field, "kubernetes_service_account_mappings."); ok {
		if t.KubernetesServiceAccountMappings == nil {
			t.KubernetesServiceAccountMappings = map[string]string{}
		}
		t.KubernetesServiceAccountMappings[rest] = v.(string)
		return
	}
	if rest, ok := strings.CutPrefix(field, "harness_overrides."); ok {
		hc, sub := splitHarnessOverrideField(rest)
		if d.HarnessOverrides == nil {
			d.HarnessOverrides = map[string]V1HarnessOverride{}
		}
		o := d.HarnessOverrides[hc]
		switch sub {
		case "":
		case "image":
			o.Image = v.(string)
		case "image_pull_policy":
			o.ImagePullPolicy = v.(string)
		case "user":
			o.User = v.(string)
		case "auth_selected_type":
			o.AuthSelectedType = v.(string)
		case "env":
			o.Env = copyStringMap(v.(map[string]string))
		case "volumes":
			o.Volumes = copyVolumeMounts(v.([]api.VolumeMount))
		case "resources":
			r := v.(api.ResourceSpec)
			o.Resources = &r
		default:
			panic("profile conversion: unknown harness override field " + field)
		}
		d.HarnessOverrides[hc] = o
		return
	}
	switch field {
	case "gke":
		t.GKE = v.(bool)
	case "priority_class_name":
		t.PriorityClassName = v.(string)
	case "shared_dir_storage_class":
		t.SharedDirStorageClass = v.(string)
	case "shared_dir_size":
		t.SharedDirSize = v.(string)
	case "safe_to_evict":
		b := v.(bool)
		t.SafeToEvict = &b
	case "kubernetes_block_service_account":
		t.KubernetesBlockServiceAccount = v.(string)
	case "node_selector":
		t.NodeSelector = copyStringMap(v.(map[string]string))
	case "tolerations":
		t.Tolerations = append([]api.K8sToleration(nil), v.([]api.K8sToleration)...)
	case "runtime_class_name":
		t.RuntimeClassName = v.(string)
	case "default_harness_config":
		d.DefaultHarnessConfig = v.(string)
	case "image_registry":
		d.ImageRegistry = v.(string)
	case "clone_depth":
		d.CloneDepth = v.(api.CloneDepth)
	case "volumes":
		d.Volumes = copyVolumeMounts(v.([]api.VolumeMount))
	case "resources":
		r := v.(api.ResourceSpec)
		d.Resources = &r
	case "secrets":
		d.Secrets = copyRequiredSecrets(v.([]api.RequiredSecret))
	default:
		panic("profile conversion: unknown field " + field)
	}
}

// splitHarnessOverrideField splits a folded field after "harness_overrides."
// into the harness config name and the sub-field, which is "" for the
// override's presence marker. A harness config name may contain dots, so
// the sub-field is the last segment only when it names one.
func splitHarnessOverrideField(rest string) (hc, sub string) {
	if i := strings.LastIndex(rest, "."); i >= 0 && harnessOverrideSubFields[rest[i+1:]] {
		return rest[:i], rest[i+1:]
	}
	return rest, ""
}

// coveredHarnessOverrideMarker reports whether field is a harness override's
// presence marker and one of the same override's sub-fields conflicts.
func coveredHarnessOverrideMarker(field string, conflicting map[string]bool) bool {
	rest, ok := strings.CutPrefix(field, "harness_overrides.")
	if !ok {
		return false
	}
	hc, sub := splitHarnessOverrideField(rest)
	if sub != "" {
		return false
	}
	for f := range conflicting {
		if r, isOverride := strings.CutPrefix(f, "harness_overrides."); isOverride {
			if fhc, fsub := splitHarnessOverrideField(r); fhc == hc && fsub != "" {
				return true
			}
		}
	}
	return false
}

// harnessOverrideSubFields are the folded fields of one harness override.
var harnessOverrideSubFields = map[string]bool{
	"image": true, "image_pull_policy": true, "user": true, "auth_selected_type": true,
	"env": true, "volumes": true, "resources": true,
}

// isWorkloadField reports whether field is an agent_defaults field (W2-W9).
func isWorkloadField(field string) bool {
	switch field {
	case "default_harness_config", "image_registry", "clone_depth", "volumes", "resources", "secrets":
		return true
	}
	return strings.HasPrefix(field, "harness_overrides.")
}

// conflictValues lists each profile's effective value of field and where
// it is set. Environment values are never shown, because they may hold
// credentials; only their keys are.
func conflictValues(g *convGroup, field string) string {
	var parts []string
	for _, p := range g.profiles {
		cv, ok := p.fields[field]
		if !ok {
			parts = append(parts, p.name+": unset")
			continue
		}
		parts = append(parts, fmt.Sprintf("%s: %s from %s", p.name, displayConversionValue(field, cv.v), cv.src))
	}
	return strings.Join(parts, "; ")
}

func displayConversionValue(field string, v interface{}) string {
	if env, ok := v.(map[string]string); ok && strings.HasSuffix(field, ".env") {
		return "env keys " + strings.Join(sortedEnvKeys(env), ",") + " (values not shown)"
	}
	switch s := v.(type) {
	case string:
		return s
	case api.CloneDepth:
		return string(s)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

// checkRawConversionKeys reports, from the raw settings, every key under an
// in-scope profile or its runtime entry that the conversion tables do not
// know (blocking), and the removed profiles.<p>.env (a notice).
func checkRawConversionKeys(st *convState, vs *VersionedSettings, raw []byte, names []string) {
	var doc map[string]interface{}
	if err := yamlv3.Unmarshal(raw, &doc); err != nil {
		st.add(ConversionUnknownSettingsKey, ConversionSeverityBlocking, "",
			"the raw settings cannot be parsed, so their keys cannot be checked: "+err.Error(), names...)
		return
	}
	rawProfiles, _ := doc["profiles"].(map[string]interface{})
	rawRuntimes, _ := doc["runtimes"].(map[string]interface{})
	unknown := func(path string, profiles ...string) {
		st.add(ConversionUnknownSettingsKey, ConversionSeverityBlocking, path,
			fmt.Sprintf("%s is not a known settings key, so it cannot be converted; remove it or correct its name", path), profiles...)
	}
	checkMap := func(path string, v interface{}, known map[string]bool, profiles []string) {
		m, _ := v.(map[string]interface{})
		for _, k := range rawMapKeys(m) {
			if !known[k] {
				unknown(path+"."+k, profiles...)
			}
		}
	}
	checkList := func(path string, v interface{}, known map[string]bool, profiles []string) {
		list, _ := v.([]interface{})
		for i, e := range list {
			checkMap(fmt.Sprintf("%s[%d]", path, i), e, known, profiles)
		}
	}
	checkResources := func(path string, v interface{}, profiles []string) {
		checkMap(path, v, knownResourceSpecKeys, profiles)
		m, _ := v.(map[string]interface{})
		checkMap(path+".requests", m["requests"], knownResourceListKeys, profiles)
		checkMap(path+".limits", m["limits"], knownResourceListKeys, profiles)
	}

	runtimeUsers := map[string][]string{}
	for _, name := range names {
		prof, ok := vs.Profiles[name]
		if ok && prof.Runtime != "" {
			runtimeUsers[prof.Runtime] = append(runtimeUsers[prof.Runtime], name)
		}
		pm, isMap := rawProfiles[name].(map[string]interface{})
		if !isMap {
			continue
		}
		pp := "profiles." + name
		profs := []string{name}
		for _, k := range rawMapKeys(pm) {
			switch {
			case k == "env":
				st.add(ConversionLegacyFieldRemoved, ConversionSeverityNotice, pp+".env",
					fmt.Sprintf("%s.env was removed from the settings and has no effect; it is not converted (use harness_configs.<name>.env)", pp), name)
			case !knownProfileKeys[k]:
				unknown(pp+"."+k, name)
			}
		}
		checkList(pp+".volumes", pm["volumes"], knownVolumeMountKeys, profs)
		checkList(pp+".secrets", pm["secrets"], knownSecretKeys, profs)
		checkResources(pp+".resources", pm["resources"], profs)
		overrides, _ := pm["harness_overrides"].(map[string]interface{})
		for _, hc := range rawMapKeys(overrides) {
			hp := pp + ".harness_overrides." + hc
			checkMap(hp, overrides[hc], knownHarnessOverKeys, profs)
			om, _ := overrides[hc].(map[string]interface{})
			checkList(hp+".volumes", om["volumes"], knownVolumeMountKeys, profs)
			checkResources(hp+".resources", om["resources"], profs)
		}
	}
	for rtKey, users := range runtimeUsers {
		checkMap("runtimes."+rtKey, rawRuntimes[rtKey], knownRuntimeEntryKeys, users)
	}
}

func uniqueSorted(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

func containsString(list []string, s string) bool {
	for _, e := range list {
		if e == s {
			return true
		}
	}
	return false
}
