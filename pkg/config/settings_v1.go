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
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/confmap"
	"github.com/knadh/koanf/providers/env"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/providers/rawbytes"
	"github.com/knadh/koanf/v2"
	yamlv3 "gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/validation"
)

// ResolveHarnessConfig looks up a named harness config and merges profile-level overrides.
// If profileName is empty, ActiveProfile is used. If the config name is not found in the
// settings map, an empty HarnessConfigEntry is used as the base (profile overrides still apply).
func (vs *VersionedSettings) ResolveHarnessConfig(profileName, harnessConfigName string) (HarnessConfigEntry, error) {
	if profileName == "" {
		profileName = vs.ActiveProfile
	}

	baseConfig := vs.HarnessConfigs[harnessConfigName]

	profile, ok := vs.Profiles[profileName]
	if !ok {
		return baseConfig, nil
	}

	result := baseConfig

	// profiles.<name>.env is deliberately NOT merged here. It was removed in
	// Gap 3 ("G3-full") as a breaking change, on this rationale from the
	// product owner:
	//
	//	"settings schema has gotten pretty rich, need to pare down the number
	//	 of control and injection points, profiles are already a bit
	//	 problematic in other ways in the architecture."
	//
	// This is the ONLY statement of intent for the removal, which is why it
	// is recorded here and not only in the CHANGELOG. Do not restore the
	// merge because a caller appears to want profile env: the removal is the
	// feature, not an oversight.
	//
	// Note what this does NOT remove. profiles.<name>.harness_overrides.<hc>.env
	// below is a different key with a different blast radius and is still
	// merged. Volumes are also untouched.
	//
	// Pinned by TestResolveHarnessConfig_ProfileEnvNotMerged (pkg/config) and
	// TestResolveAuthEnvOverlay_ProfileEnvNoLongerArrivesViaHarnessConfig
	// (pkg/agent). Both leave harness_overrides UNSET for the key under test:
	// this removes a rank that is not the top of its ladder, and a middle-rank
	// removal is invisible whenever a higher rank is populated.

	// Merge profile-level volumes
	if profile.Volumes != nil {
		result.Volumes = append(result.Volumes, profile.Volumes...)
	}

	// Apply harness overrides from the profile
	if profile.HarnessOverrides != nil {
		if override, ok := profile.HarnessOverrides[harnessConfigName]; ok {
			if override.Image != "" {
				result.Image = override.Image
			}
			if override.ImagePullPolicy != "" {
				result.ImagePullPolicy = override.ImagePullPolicy
			}
			if override.User != "" {
				result.User = override.User
			}
			if override.AuthSelectedType != "" {
				result.AuthSelectedType = override.AuthSelectedType
			}
			// profiles.<name>.harness_overrides.<hc>.env SURVIVES G3-full, which
			// removed profiles.<name>.env above but deliberately stopped here.
			//
			// This is now the only profiles-scoped env injection point left, and it
			// is the TOP-RANKED env source in this function — merged after the base,
			// so it outranks harness_configs.<hc>.env. Removing it too was ruled
			// against on reversibility: retiring this rank later is additive, while
			// restoring it after shipping would be a revert of a documented breaking
			// change.
			//
			// It is NOT the migration path for the removed profiles.<name>.env.
			// That is harness_configs.<hc>.env, a different, top-level key, which
			// survives independently of this one.
			if override.Env != nil {
				result.Env = mergeMaps(result.Env, override.Env)
			}
			if override.Volumes != nil {
				result.Volumes = append(result.Volumes, override.Volumes...)
			}
		}
	}

	return result, nil
}

// ResolveRuntime resolves the runtime config for a profile.
// Returns the runtime config, the effective runtime type, and any error.
// If profileName is empty, ActiveProfile is used.
// The runtime type is V1RuntimeConfig.Type if set, otherwise the map key name.
func (vs *VersionedSettings) ResolveRuntime(profileName string) (V1RuntimeConfig, string, error) {
	if profileName == "" {
		profileName = vs.ActiveProfile
	}
	profile, ok := vs.Profiles[profileName]
	if !ok {
		return V1RuntimeConfig{}, "", fmt.Errorf("profile %q not found", profileName)
	}
	rtConfig, ok := vs.Runtimes[profile.Runtime]
	if !ok {
		return V1RuntimeConfig{}, "", fmt.Errorf("runtime %q not found for profile %q", profile.Runtime, profileName)
	}

	// Resolve the effective runtime type: explicit Type field, or map key name
	runtimeType := rtConfig.Type
	if runtimeType == "" {
		runtimeType = profile.Runtime
	}

	return rtConfig, runtimeType, nil
}

// missingSchemaVersionWarning is emitted when a settings file is loaded as v1
// only because its runtime entries use v1-only keys (v1RuntimeIndicatorKeys).
var missingSchemaVersionWarning = `settings.yaml contains v1 runtime fields (` + strings.Join(v1RuntimeIndicatorKeys, ", ") + `) but is missing 'schema_version: "1"'; add it as the first line to silence this warning`

// ResolveSharedDirDefaults returns the settings-level Kubernetes shared-dir
// PVC defaults (storage class and size) for a profile. Each field is
// resolved independently: the profile's value wins, otherwise the value on
// the profile's runtime entry is used. Empty means "not set in settings".
// If profileName is empty, ActiveProfile is used. An unknown profile
// yields empty values; a profile naming a missing runtime entry yields the
// profile's own values only.
//
// These are defaults only. A template's or agent's kubernetes block
// (api.KubernetesConfig.SharedDirStorageClass / SharedDirSize) wins over
// both; see ApplySharedDirDefaults.
func (vs *VersionedSettings) ResolveSharedDirDefaults(profileName string) (storageClass, size string) {
	storageClass, size, _ = vs.ResolveSharedDirDefaultsWithSource(profileName)
	return storageClass, size
}

// ResolveSharedDirDefaultsWithSource is ResolveSharedDirDefaults that also
// returns the settings key the size came from
// ("profiles.NAME.shared_dir_size" or "runtimes.NAME.shared_dir_size"), so
// an error about the value can name where it is set. sizeKey is empty when
// size is empty.
func (vs *VersionedSettings) ResolveSharedDirDefaultsWithSource(profileName string) (storageClass, size, sizeKey string) {
	if vs == nil {
		return "", "", ""
	}
	if profileName == "" {
		profileName = vs.ActiveProfile
	}
	profile, ok := vs.Profiles[profileName]
	if !ok {
		return "", "", ""
	}
	storageClass, size = profile.SharedDirStorageClass, profile.SharedDirSize
	if size != "" {
		sizeKey = "profiles." + profileName + ".shared_dir_size"
	}
	if rt, ok := vs.Runtimes[profile.Runtime]; ok {
		if storageClass == "" {
			storageClass = rt.SharedDirStorageClass
		}
		if size == "" && rt.SharedDirSize != "" {
			size = rt.SharedDirSize
			sizeKey = "runtimes." + profile.Runtime + ".shared_dir_size"
		}
	}
	return storageClass, size, sizeKey
}

// ResolveProfileValue returns a per-profile setting with the standard
// precedence used by per-profile overrides: the profile's own value, else
// the value on the profile's runtime entry. The zero value of T means
// "not set" at that level. key is the settings key name used to build the
// returned source ("profiles.NAME.KEY" or "runtimes.NAME.KEY"). If
// profileName is empty, vs.ActiveProfile is used. A nil vs, an unknown
// profile, or a profile and runtime entry that both leave the value unset
// yield the zero value and an empty source, meaning the caller's global
// value applies. Because the zero value means "not set", a bool or numeric
// key cannot express an explicit false or 0 override with T = bool or
// int; for such keys use a pointer type (for example T = *bool), so nil
// means unset.
//
// Example, for a string key:
//
//	v, src := ResolveProfileValue(vs, profile, "shared_dir_storage_backend",
//		func(p V1ProfileConfig) string { return p.SharedDirStorageBackend },
//		func(r V1RuntimeConfig) string { return r.SharedDirStorageBackend })
//
// Call it on settings whose source matches the key's scope: keys that a
// project must not set should be read from LoadGlobalSettings or
// LoadGlobalSettingsWithOverlay, not from project-merged settings.
func ResolveProfileValue[T comparable](vs *VersionedSettings, profileName, key string,
	fromProfile func(V1ProfileConfig) T, fromRuntime func(V1RuntimeConfig) T) (value T, source string) {
	var zero T
	if vs == nil {
		return zero, ""
	}
	if profileName == "" {
		profileName = vs.ActiveProfile
	}
	profile, ok := vs.Profiles[profileName]
	if !ok {
		return zero, ""
	}
	if v := fromProfile(profile); v != zero {
		return v, "profiles." + profileName + "." + key
	}
	if rt, ok := vs.Runtimes[profile.Runtime]; ok {
		if v := fromRuntime(rt); v != zero {
			return v, "runtimes." + profile.Runtime + "." + key
		}
	}
	return zero, ""
}

// ResolveProfileSetting is ResolveProfileValue for string settings, where
// "" means not set.
func (vs *VersionedSettings) ResolveProfileSetting(profileName, key string,
	fromProfile func(V1ProfileConfig) string, fromRuntime func(V1RuntimeConfig) string) (value, source string) {
	return ResolveProfileValue(vs, profileName, key, fromProfile, fromRuntime)
}

// SharedDirStorageGlobalSource is the source key ResolveSharedDirStorage
// returns when no profile or runtime entry overrides the backend.
const SharedDirStorageGlobalSource = "server.shared_dir_storage.backend"

// ResolveSharedDirStorage returns the shared-dir storage config that
// applies to agents using profileName: the backend comes from the
// profile's shared_dir_storage_backend, else its runtime entry's, else
// server.shared_dir_storage.backend. The nfs block always comes from
// server.shared_dir_storage.nfs. source names the key the backend came
// from. The result is nil when nothing is configured (the local layout).
// The returned config is a copy when an override applies; the global
// block is never modified.
//
// Call this only on settings from LoadGlobalSettings or
// LoadGlobalSettingsWithOverlay: like server.shared_dir_storage itself,
// the overrides must not be settable from a project's own settings.
//
// A dispatch-time NFS mount check should choose the backend through this
// method too, so it agrees with the start path.
func (vs *VersionedSettings) ResolveSharedDirStorage(profileName string) (cfg *V1SharedDirStorageConfig, source string) {
	if vs == nil {
		return nil, ""
	}
	var global *V1SharedDirStorageConfig
	if vs.Server != nil {
		global = vs.Server.SharedDirStorage
	}
	backend, source := vs.ResolveProfileSetting(profileName, "shared_dir_storage_backend",
		func(p V1ProfileConfig) string { return p.SharedDirStorageBackend },
		func(r V1RuntimeConfig) string { return r.SharedDirStorageBackend })
	if backend == "" {
		if global == nil {
			return nil, ""
		}
		return global, SharedDirStorageGlobalSource
	}
	out := &V1SharedDirStorageConfig{Backend: backend}
	if global != nil {
		out.NFS = global.NFS
	}
	return out, source
}

// SharedDirStorageNFSAnywhere reports whether the global backend or any
// runtime or profile override selects nfs, and returns the nfs-backed
// config to use for project-wide operations such as cleanup. It is nil
// when no setting selects nfs. Like ResolveSharedDirStorage, call it only
// on global settings.
func (vs *VersionedSettings) SharedDirStorageNFSAnywhere() (cfg *V1SharedDirStorageConfig, onlyOverrides bool) {
	if vs == nil {
		return nil, false
	}
	var global *V1SharedDirStorageConfig
	if vs.Server != nil {
		global = vs.Server.SharedDirStorage
	}
	if global != nil && global.Backend == "nfs" {
		return global, false
	}
	found := false
	for _, rt := range vs.Runtimes {
		if rt.SharedDirStorageBackend == "nfs" {
			found = true
		}
	}
	for _, p := range vs.Profiles {
		if p.SharedDirStorageBackend == "nfs" {
			found = true
		}
	}
	if !found {
		return nil, false
	}
	out := &V1SharedDirStorageConfig{Backend: "nfs"}
	if global != nil {
		out.NFS = global.NFS
	}
	return out, true
}

// ValidateSharedDirStorageBackends checks shared_dir_storage_backend on
// every runtime and profile entry: the value must be empty, "local" or
// "nfs", and "nfs" needs a complete server.shared_dir_storage.nfs block
// (global may be nil). This checks configuration only; it never looks at
// the filesystem. Each error's Path names the settings key. Results are
// sorted by path.
func ValidateSharedDirStorageBackends(runtimes map[string]V1RuntimeConfig, profiles map[string]V1ProfileConfig, global *V1SharedDirStorageConfig) []ValidationError {
	var errs []ValidationError
	check := func(path, backend string) {
		switch backend {
		case "", "local":
			return
		case "nfs":
			cfg := &V1SharedDirStorageConfig{Backend: "nfs"}
			if global != nil {
				cfg.NFS = global.NFS
			}
			if err := cfg.Validate(); err != nil {
				errs = append(errs, ValidationError{Path: path, Message: "selects \"nfs\", which needs a complete server.shared_dir_storage.nfs block (" + err.Error() + ")"})
			}
		default:
			errs = append(errs, ValidationError{Path: path, Message: fmt.Sprintf("must be \"local\" or \"nfs\" (got %q)", backend)})
		}
	}
	for name, rt := range runtimes {
		check("runtimes."+name+".shared_dir_storage_backend", rt.SharedDirStorageBackend)
	}
	for name, p := range profiles {
		check("profiles."+name+".shared_dir_storage_backend", p.SharedDirStorageBackend)
	}
	sort.Slice(errs, func(i, j int) bool { return errs[i].Path < errs[j].Path })
	return errs
}

// ValidateSharedDirSize checks that a shared_dir_size value parses as a
// positive Kubernetes resource quantity (for example 10Gi or 1Ti). Empty is
// valid and means "not set".
func ValidateSharedDirSize(size string) error {
	if size == "" {
		return nil
	}
	q, err := resource.ParseQuantity(size)
	if err != nil {
		return fmt.Errorf("invalid shared_dir_size %q: must be a Kubernetes quantity such as 10Gi or 1Ti", size)
	}
	if q.Sign() <= 0 {
		return fmt.Errorf("invalid shared_dir_size %q: must be a positive Kubernetes quantity such as 10Gi or 1Ti", size)
	}
	return nil
}

// ResolveKubernetesServiceAccountMapping returns the Kubernetes ServiceAccount
// (KSA) name mapped to gsaEmail for profileName's runtime, and whether a
// mapping was found. gsaEmail is lower-cased before lookup — GCP service
// account emails are lowercase, and ValidateKubernetesServiceAccountMappings
// rejects any settings entry whose key is not, so both sides of the lookup
// are guaranteed lowercase and a caller does not need to normalize first.
//
// A profile-level entry for gsaEmail takes precedence over a runtime-level
// entry for the same gsaEmail — the same profile-overrides precedence
// ResolveImageRegistry already applies, here evaluated per map key instead of
// as a whole-value override, since this setting is a map rather than a
// scalar. A profile with no entry for gsaEmail (including an empty-string
// value, treated as unset) falls through to the runtime-level entry, rather
// than that GSA having no mapping at all. profileName "" resolves to
// vs.ActiveProfile, matching ResolveRuntime and ResolveImageRegistry. If
// ResolveRuntime cannot resolve profileName's runtime (unknown profile or
// runtime), this returns false, the same as no mapping being found — callers
// that need to distinguish "no mapping" from "settings error" should call
// ResolveRuntime themselves first.
//
// Scion never creates, annotates, or binds the returned KSA: it must already
// exist and already be bound to gsaEmail via Workload Identity, out of band,
// by the operator (see docs-site/.../ha/setup-gcp.md).
//
// This is the profileName-only convenience form: it resolves profileName's
// own runtime entry name internally (via ResolveRuntime) before delegating
// to ResolveKubernetesServiceAccountMappingForSelection. A caller that
// already knows both the effective profile name AND the runtime entry name a
// dispatch resolved to — e.g. the broker, which gets both from one shared
// resolver so this lookup cannot use a different profile/runtime than the
// rest of the dispatch did — should call
// ResolveKubernetesServiceAccountMappingForSelection directly instead,
// because a ForceRuntime-selected dispatch has a runtime entry but no
// profile at all, which this form cannot express.
func (vs *VersionedSettings) ResolveKubernetesServiceAccountMapping(profileName, gsaEmail string) (string, bool) {
	if profileName == "" {
		profileName = vs.ActiveProfile
	}
	runtimeEntryName := ""
	if profile, ok := vs.Profiles[profileName]; ok {
		runtimeEntryName = profile.Runtime
	}
	return vs.ResolveKubernetesServiceAccountMappingForSelection(profileName, runtimeEntryName, gsaEmail)
}

// ResolveKubernetesServiceAccountMappingForSelection resolves the KSA mapped
// to gsaEmail for an explicit runtime selection: profileName (if non-empty)
// is checked first, at the profile level; runtimeEntryName (if non-empty) is
// checked next, at the runtime level (the `runtimes:` map key — not the
// runtime type). Either may be empty independently: a ForceRuntime-selected
// dispatch has a runtime entry but no profile at all, and this function
// still resolves correctly from the runtime entry alone in that case.
// Returns false if neither argument is non-empty, or if neither lookup finds
// a non-empty entry for gsaEmail (which is lower-cased before matching, the
// same as ResolveKubernetesServiceAccountMapping).
//
// An empty-string value at the profile level is treated as no entry and
// falls through to the runtime level, rather than as a mapping to an empty
// KSA name: the JSON schema (settings-v1.schema.json) already rejects an
// empty value for any mapping written through a schema-validated path (the
// Admin server-config API, `scion config validate`), so this fall-through
// only matters for a hand-edited file written outside that path. Treating
// it as "unset" there — instead of returning ("", true) and letting an
// empty KSA name reach dispatch — keeps a stray blank entry from silently
// becoming a different failure mode (an empty ServiceAccountName on the pod
// spec) than the one an operator gets from every other malformed entry
// (ValidateKubernetesServiceAccountMappings's actionable error at the point
// of use).
func (vs *VersionedSettings) ResolveKubernetesServiceAccountMappingForSelection(profileName, runtimeEntryName, gsaEmail string) (string, bool) {
	gsaEmail = strings.ToLower(gsaEmail)
	if profileName != "" {
		if profile, ok := vs.Profiles[profileName]; ok {
			if ksa, ok := profile.KubernetesServiceAccountMappings[gsaEmail]; ok && ksa != "" {
				return ksa, true
			}
		}
	}
	if runtimeEntryName != "" {
		if rtConfig, ok := vs.Runtimes[runtimeEntryName]; ok {
			if ksa, ok := rtConfig.KubernetesServiceAccountMappings[gsaEmail]; ok && ksa != "" {
				return ksa, true
			}
		}
	}
	return "", false
}

// ResolveKubernetesNamespace returns the namespace configured on the
// runtimeEntryName entry of the runtimes: map, and whether one is set.
// Profiles carry no namespace of their own: a profile that needs a
// different namespace selects a runtime entry that sets it. When this
// returns false, the Kubernetes runtime's own default namespace applies.
//
// GCP identity mode "assign" on Kubernetes resolves the Workload Identity
// principal as the (namespace, KSA) pair, so the namespace is read from the
// same operator settings as the KSA mapping rather than from a request- or
// template-supplied field.
func (vs *VersionedSettings) ResolveKubernetesNamespace(runtimeEntryName string) (string, bool) {
	if runtimeEntryName != "" {
		if rtConfig, ok := vs.Runtimes[runtimeEntryName]; ok && rtConfig.Namespace != "" {
			return rtConfig.Namespace, true
		}
	}
	return "", false
}

// ProjectSettingsHasKubernetesServiceAccountMappings reports whether a
// project's OWN settings.yaml — read directly, never merged with the
// broker's global settings the way LoadEffectiveSettings/LoadVersionedSettings
// merge them for every other lookup — sets
// kubernetes_service_account_mappings on the named profile or runtime entry.
// This exists only to let a caller warn an operator that the setting there
// has no effect: ResolveKubernetesServiceAccountMappingForSelection reads
// this mapping only from the broker's own global settings, by design, and a
// project-level entry for the same runtime/profile name is invisible to it —
// not merely overridden. A plain LoadEffectiveSettings(projectPath) call
// cannot detect this on its own: koanf layers the project file on top of the
// already-loaded global one, so a key the project file never mentions still
// reads back from the global layer underneath it, indistinguishable from the
// project having set the same value itself.
//
// Best-effort: a missing or unreadable project settings file, or one in a
// split-storage external location this does not resolve, returns false, not
// an error — this exists only to produce an operator-facing warning, not to
// change any dispatch outcome.
func ProjectSettingsHasKubernetesServiceAccountMappings(projectPath, runtimeEntryName, profileName string) bool {
	// projectPath may be a project root (not yet resolved to its .scion
	// directory) — GetResolvedProjectDir is the same first step every other
	// caller in this codebase takes before LoadEffectiveSettings/
	// LoadVersionedSettings (e.g. resolveDispatchProfileSelection in
	// pkg/runtimebroker). resolveEffectiveProjectPath on its own does not do
	// this: called with a non-empty, non-"global"/"home" path, it only
	// applies the external-split-storage redirect (GetProjectConfigDir) on
	// top of whatever was passed in, assuming it already points at a .scion
	// directory.
	resolvedProjectDir, err := GetResolvedProjectDir(projectPath)
	if err != nil || resolvedProjectDir == "" {
		return false
	}
	globalDir, _ := GetGlobalDir()
	if resolvedProjectDir == globalDir {
		return false
	}
	effectiveProjectPath := resolveEffectiveProjectPath(resolvedProjectDir)
	if effectiveProjectPath == "" || effectiveProjectPath == globalDir {
		return false
	}
	settingsPath := GetSettingsPath(effectiveProjectPath)
	if settingsPath == "" {
		return false
	}
	raw, err := os.ReadFile(settingsPath)
	if err != nil {
		return false
	}
	var probe struct {
		Profiles map[string]struct {
			KubernetesServiceAccountMappings map[string]string `yaml:"kubernetes_service_account_mappings" json:"kubernetes_service_account_mappings"`
		} `yaml:"profiles" json:"profiles"`
		Runtimes map[string]struct {
			KubernetesServiceAccountMappings map[string]string `yaml:"kubernetes_service_account_mappings" json:"kubernetes_service_account_mappings"`
		} `yaml:"runtimes" json:"runtimes"`
	}
	if err := yamlv3.Unmarshal(raw, &probe); err != nil {
		return false
	}
	if profileName != "" {
		if p, ok := probe.Profiles[profileName]; ok && len(p.KubernetesServiceAccountMappings) > 0 {
			return true
		}
	}
	if runtimeEntryName != "" {
		if rt, ok := probe.Runtimes[runtimeEntryName]; ok && len(rt.KubernetesServiceAccountMappings) > 0 {
			return true
		}
	}
	return false
}

// gsaEmailPattern matches a syntactically well-formed, lowercase GCP service
// account email: a non-empty local part and one or more non-empty
// hyphen-delimited domain labels ending in "gserviceaccount.com". GCP issues
// service account emails in lowercase only, so uppercase is rejected here
// rather than silently accepted and then never matching a lookup (which
// lower-cases gsaEmail — see ResolveKubernetesServiceAccountMapping). This
// deliberately accepts every lowercase GSA shape GCP issues, not only
// user-managed accounts:
//
//	name@project.iam.gserviceaccount.com   (user-managed)
//	project@appspot.gserviceaccount.com    (App Engine default)
//	number-compute@developer.gserviceaccount.com (Compute Engine default)
var gsaEmailPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?@([a-z0-9]([a-z0-9-]*[a-z0-9])?\.)+gserviceaccount\.com$`)

// ValidateKubernetesServiceAccountMappings checks that every key in mappings
// is a well-formed, lowercase GCP service account email and every value is a
// valid Kubernetes ServiceAccount name (a DNS-1123 subdomain, per the
// Kubernetes ServiceAccount object's own name validation — up to 253
// characters, dot-separated labels — not the shorter DNS-1123 label format
// some other Kubernetes names use).
//
// This is called at the point each mapping is actually used, in
// pkg/runtimebroker/gcp_identity_assign.go, on the single resolved (gsaEmail,
// ksaName) pair for that dispatch — never on an empty-string ksaName,
// because ResolveKubernetesServiceAccountMappingForSelection already treats
// an empty value as no mapping before returning one. It is not called by the
// JSON schema validator (ValidateSettings): the schema enforces the same two
// patterns independently (see settings-v1.schema.json's
// kubernetes_service_account_mappings propertyNames/additionalProperties),
// so settings.yaml written through a schema-validated path (the Admin
// server-config API, `scion config validate`) is already checked before
// this function ever runs. This function exists as a second, narrower
// check for settings loaded without going through that validator — e.g. a
// hand-edited settings.yaml in file-only mode — so a malformed mapping is
// still caught before it reaches dispatch, with an error naming the
// specific mapping at fault rather than a whole-document schema failure.
func ValidateKubernetesServiceAccountMappings(mappings map[string]string) error {
	for gsaEmail, ksaName := range mappings {
		if !gsaEmailPattern.MatchString(gsaEmail) {
			return fmt.Errorf("kubernetes_service_account_mappings: %q is not a well-formed, lowercase GCP service account email", gsaEmail)
		}
		if errs := validation.IsDNS1123Subdomain(ksaName); len(errs) > 0 {
			return fmt.Errorf("kubernetes_service_account_mappings[%s]: %q is not a valid Kubernetes ServiceAccount name: %s", gsaEmail, ksaName, strings.Join(errs, "; "))
		}
	}
	return nil
}

// ValidateSharedDirSizes checks shared_dir_size on every runtime and
// profile entry. Each error's Path names the settings key
// ("runtimes.NAME.shared_dir_size" / "profiles.NAME.shared_dir_size").
// Results are sorted by path.
func ValidateSharedDirSizes(runtimes map[string]V1RuntimeConfig, profiles map[string]V1ProfileConfig) []ValidationError {
	var errs []ValidationError
	for name, rt := range runtimes {
		if err := ValidateSharedDirSize(rt.SharedDirSize); err != nil {
			errs = append(errs, ValidationError{Path: "runtimes." + name + ".shared_dir_size", Message: err.Error()})
		}
	}
	for name, p := range profiles {
		if err := ValidateSharedDirSize(p.SharedDirSize); err != nil {
			errs = append(errs, ValidationError{Path: "profiles." + name + ".shared_dir_size", Message: err.Error()})
		}
	}
	sort.Slice(errs, func(i, j int) bool { return errs[i].Path < errs[j].Path })
	return errs
}

// ApplySharedDirDefaults returns base with SharedDirStorageClass and
// SharedDirSize filled from the given settings defaults where base leaves
// them empty, so a template's or agent's explicit value always wins. base
// is never modified; a copy is returned. When both defaults are empty,
// base is returned unchanged (including nil).
func ApplySharedDirDefaults(base *api.KubernetesConfig, storageClass, size string) *api.KubernetesConfig {
	if storageClass == "" && size == "" {
		return base
	}
	out := &api.KubernetesConfig{}
	if base != nil {
		cpy := *base
		out = &cpy
	}
	if out.SharedDirStorageClass == "" {
		out.SharedDirStorageClass = storageClass
	}
	if out.SharedDirSize == "" {
		out.SharedDirSize = size
	}
	return out
}

// ResolveSafeToEvict returns the settings-level safe_to_evict default for a
// profile: the profile's value if set, otherwise the value on the profile's
// runtime entry. Nil means "not set in settings". If profileName is empty,
// ActiveProfile is used; an unknown profile yields nil.
//
// This is a default only. A template's or agent's kubernetes.safeToEvict
// wins over it; see ApplySafeToEvictDefault.
//
// This follows the ResolveSharedDirDefaults pattern; it can move onto a
// generic per-profile resolver once one exists.
func (vs *VersionedSettings) ResolveSafeToEvict(profileName string) *bool {
	v, _ := vs.ResolveSafeToEvictWithSource(profileName)
	return v
}

// ResolveSafeToEvictWithSource is ResolveSafeToEvict that also returns the
// settings key the value came from ("profiles.NAME.safe_to_evict" or
// "runtimes.NAME.safe_to_evict"). source is empty when the value is nil.
// The returned pointer is a fresh copy, never one held by vs.
func (vs *VersionedSettings) ResolveSafeToEvictWithSource(profileName string) (value *bool, source string) {
	if vs == nil {
		return nil, ""
	}
	if profileName == "" {
		profileName = vs.ActiveProfile
	}
	profile, ok := vs.Profiles[profileName]
	if !ok {
		return nil, ""
	}
	if profile.SafeToEvict != nil {
		v := *profile.SafeToEvict
		return &v, "profiles." + profileName + ".safe_to_evict"
	}
	if rt, ok := vs.Runtimes[profile.Runtime]; ok && rt.SafeToEvict != nil {
		v := *rt.SafeToEvict
		return &v, "runtimes." + profile.Runtime + ".safe_to_evict"
	}
	return nil, ""
}

// ApplySafeToEvictDefault returns base with SafeToEvict filled from the
// settings default when base leaves it unset, so a template's or agent's
// explicit value (true or false) always wins. base is never modified; a
// copy is returned. When def is nil, base is returned unchanged (including
// nil).
func ApplySafeToEvictDefault(base *api.KubernetesConfig, def *bool) *api.KubernetesConfig {
	if def == nil {
		return base
	}
	out := &api.KubernetesConfig{}
	if base != nil {
		cpy := *base
		out = &cpy
	}
	if out.SafeToEvict == nil {
		v := *def
		out.SafeToEvict = &v
	}
	return out
}

// SafeToEvictIgnoredWarnings returns a warning for every runtime entry that
// sets safe_to_evict but is not a Kubernetes runtime, and for every profile
// that sets it while pointing at such an entry. The setting is accepted and
// ignored there. Results are sorted.
func SafeToEvictIgnoredWarnings(runtimes map[string]V1RuntimeConfig, profiles map[string]V1ProfileConfig) []string {
	var warnings []string
	for name, rt := range runtimes {
		if rt.SafeToEvict != nil && !isKubernetesRuntimeEntry(name, rt) {
			warnings = append(warnings, fmt.Sprintf("runtimes.%s.safe_to_evict is set but runtime %q is not a Kubernetes runtime; it is ignored", name, name))
		}
	}
	for name, p := range profiles {
		if p.SafeToEvict == nil {
			continue
		}
		rt, ok := runtimes[p.Runtime]
		if ok && !isKubernetesRuntimeEntry(p.Runtime, rt) {
			warnings = append(warnings, fmt.Sprintf("profiles.%s.safe_to_evict is set but its runtime %q is not a Kubernetes runtime; it is ignored", name, p.Runtime))
		}
	}
	sort.Strings(warnings)
	return warnings
}

// isKubernetesRuntimeEntry reports whether a runtime entry targets
// Kubernetes: an explicit type of "kubernetes" (or "k8s"), or no type and
// the entry name itself is one of those.
func isKubernetesRuntimeEntry(name string, rt V1RuntimeConfig) bool {
	t := rt.Type
	if t == "" {
		t = name
	}
	// "remote" is normalised to the Kubernetes runtime by the runtime factory.
	return t == "kubernetes" || t == "k8s" || t == "remote"
}

// GetHubEndpoint returns the Hub endpoint from settings, or empty string if not configured.
func (vs *VersionedSettings) GetHubEndpoint() string {
	if vs.Hub != nil {
		return vs.Hub.Endpoint
	}
	return ""
}

// IsHubConfigured returns true if Hub settings are configured.
func (vs *VersionedSettings) IsHubConfigured() bool {
	return vs.Hub != nil && vs.Hub.Endpoint != ""
}

// IsHubEnabled returns true if Hub integration is explicitly enabled.
func (vs *VersionedSettings) IsHubEnabled() bool {
	return vs.Hub != nil && vs.Hub.Enabled != nil && *vs.Hub.Enabled
}

// IsHubLinked returns true if this project has been explicitly linked to the Hub.
func (vs *VersionedSettings) IsHubLinked() bool {
	return vs.Hub != nil && vs.Hub.Linked != nil && *vs.Hub.Linked
}

// IsHubExplicitlyDisabled returns true if Hub integration is explicitly disabled.
func (vs *VersionedSettings) IsHubExplicitlyDisabled() bool {
	return vs.Hub != nil && vs.Hub.Enabled != nil && !*vs.Hub.Enabled
}

// IsHubLocalOnly returns true if the project is configured for local-only mode.
func (vs *VersionedSettings) IsHubLocalOnly() bool {
	return vs.Hub != nil && vs.Hub.LocalOnly != nil && *vs.Hub.LocalOnly
}

// IsImageRegistryConfigured returns true if image_registry is set at any level
// (top-level or in any profile).
func (vs *VersionedSettings) IsImageRegistryConfigured(profileName string) bool {
	return vs.ResolveImageRegistry(profileName) != ""
}

// ResolveImageRegistry returns the effective image_registry for the given profile.
// Profile-level image_registry takes precedence over the top-level setting.
func (vs *VersionedSettings) ResolveImageRegistry(profileName string) string {
	if profileName == "" {
		profileName = vs.ActiveProfile
	}
	if profile, ok := vs.Profiles[profileName]; ok && profile.ImageRegistry != "" {
		return profile.ImageRegistry
	}
	return vs.ImageRegistry
}

// RequireImageRegistry checks that image_registry is configured and returns an
// actionable error if not. This is called early in command execution to fail fast.
func RequireImageRegistry(projectPath, profileName string) error {
	vs, _, err := LoadEffectiveSettings(projectPath)
	if err != nil {
		// Can't load settings — let other code handle this error
		return nil
	}
	if vs == nil {
		return nil
	}
	if vs.IsImageRegistryConfigured(profileName) {
		return nil
	}
	return fmt.Errorf("image_registry is not configured.\n\n" +
		"Scion requires container images to run agents. To get started:\n\n" +
		"  1. Build your images:  image-build/scripts/build-images.sh --registry <your-registry> --push\n" +
		"     See image-build/README.md for detailed instructions.\n\n" +
		"  2. Configure scion:   scion config set --global image_registry <your-registry>\n" +
		"     Example:           scion config set --global image_registry ghcr.io/myorg")
}

// RewriteImageRegistry replaces the registry prefix of a container image reference
// with newRegistry for any short-form (non-fully-qualified) image.
// If newRegistry is empty, the original image is returned unchanged.
func RewriteImageRegistry(fullImage, newRegistry string) string {
	if newRegistry == "" || fullImage == "" {
		return fullImage
	}

	// Extract the basename (last path component, e.g. "scion-claude:latest")
	lastSlash := strings.LastIndex(fullImage, "/")
	var basename string
	if lastSlash >= 0 {
		basename = fullImage[lastSlash+1:]
	} else {
		basename = fullImage
	}

	// If the image already has an explicit registry hostname
	// (first path component contains "." or ":"), don't rewrite it.
	// A fully qualified reference like ghcr.io/org/scion-foo:v1
	// means the author chose that registry deliberately.
	if firstSlash := strings.Index(fullImage, "/"); firstSlash >= 0 {
		firstComponent := fullImage[:firstSlash]
		if strings.ContainsAny(firstComponent, ".:") {
			return fullImage
		}
	}

	// Strip trailing slash from registry
	registry := strings.TrimRight(newRegistry, "/")
	return registry + "/" + basename
}

// PrintDeprecationWarnings prints deprecation warnings to stderr.
func PrintDeprecationWarnings(warnings []string) {
	for _, w := range warnings {
		fmt.Fprintf(os.Stderr, "Warning: %s\n", w)
	}
}

// VersionedSettings is the root configuration struct for versioned settings (v1+).
type VersionedSettings struct {
	SchemaVersion        string                        `json:"schema_version" yaml:"schema_version" koanf:"schema_version"`
	ProjectType          string                        `json:"project_type,omitempty" yaml:"project_type,omitempty" koanf:"project_type"`
	ActiveProfile        string                        `json:"active_profile,omitempty" yaml:"active_profile,omitempty" koanf:"active_profile"`
	DefaultTemplate      string                        `json:"default_template,omitempty" yaml:"default_template,omitempty" koanf:"default_template"`
	DefaultHarnessConfig string                        `json:"default_harness_config,omitempty" yaml:"default_harness_config,omitempty" koanf:"default_harness_config"`
	Server               *V1ServerConfig               `json:"server,omitempty" yaml:"server,omitempty" koanf:"server"`
	Hub                  *V1HubClientConfig            `json:"hub,omitempty" yaml:"hub,omitempty" koanf:"hub"`
	CLI                  *V1CLIConfig                  `json:"cli,omitempty" yaml:"cli,omitempty" koanf:"cli"`
	Telemetry            *V1TelemetryConfig            `json:"telemetry,omitempty" yaml:"telemetry,omitempty" koanf:"telemetry"`
	Runtimes             map[string]V1RuntimeConfig    `json:"runtimes,omitempty" yaml:"runtimes,omitempty" koanf:"runtimes"`
	Runtime              *V1RuntimeDefaultsConfig      `json:"runtime,omitempty" yaml:"runtime,omitempty" koanf:"runtime"`
	ImageRegistry        string                        `json:"image_registry,omitempty" yaml:"image_registry,omitempty" koanf:"image_registry"`
	WorkspacePath        string                        `json:"workspace_path,omitempty" yaml:"workspace_path,omitempty" koanf:"workspace_path"`
	HarnessConfigs       map[string]HarnessConfigEntry `json:"harness_configs,omitempty" yaml:"harness_configs,omitempty" koanf:"harness_configs"`
	Profiles             map[string]V1ProfileConfig    `json:"profiles,omitempty" yaml:"profiles,omitempty" koanf:"profiles"`
	SharedDirs           []api.SharedDir               `json:"shared_dirs,omitempty" yaml:"shared_dirs,omitempty" koanf:"shared_dirs"`
	ManagedAgents        *V1ManagedAgentsConfig        `json:"managed_agents,omitempty" yaml:"managed_agents,omitempty" koanf:"managed_agents"`

	// Default agent limits (applied when no explicit value is set)
	DefaultMaxTurns      int               `json:"default_max_turns,omitempty" yaml:"default_max_turns,omitempty" koanf:"default_max_turns"`
	DefaultMaxModelCalls int               `json:"default_max_model_calls,omitempty" yaml:"default_max_model_calls,omitempty" koanf:"default_max_model_calls"`
	DefaultMaxDuration   string            `json:"default_max_duration,omitempty" yaml:"default_max_duration,omitempty" koanf:"default_max_duration"`
	DefaultResources     *api.ResourceSpec `json:"default_resources,omitempty" yaml:"default_resources,omitempty" koanf:"default_resources"`

	// Default agent model settings
	DefaultModel         string `json:"default_model,omitempty" yaml:"default_model,omitempty" koanf:"default_model"`
	DefaultThinkingLevel *int   `json:"default_thinking_level,omitempty" yaml:"default_thinking_level,omitempty" koanf:"default_thinking_level"`

	// Default agent authorization
	DefaultMaxAgentRole string `json:"default_max_agent_role,omitempty" yaml:"default_max_agent_role,omitempty" koanf:"default_max_agent_role"`
	DefaultAgentRole    string `json:"default_agent_role,omitempty" yaml:"default_agent_role,omitempty" koanf:"default_agent_role"`

	// Default runtime broker (hub-level)
	DefaultRuntimeBroker string `json:"default_runtime_broker,omitempty" yaml:"default_runtime_broker,omitempty" koanf:"default_runtime_broker"`

	// DefaultTimezone is the hub-level IANA timezone fallback (e.g.
	// "America/Los_Angeles") for agent containers: applied as TZ when the
	// agent has no pinned timezone and no storage-scope TZ environment
	// variable applies. This is the settings.yaml form of
	// agent_defaults.default_timezone.
	DefaultTimezone string `json:"default_timezone,omitempty" yaml:"default_timezone,omitempty" koanf:"default_timezone"`

	// DefaultGCPIdentityMode is the hub-level default GCP metadata mode
	// ("block", "passthrough", "assign") applied to new agents whose project
	// has no default of its own. Empty means no hub default (block).
	DefaultGCPIdentityMode string `json:"default_gcp_identity_mode,omitempty" yaml:"default_gcp_identity_mode,omitempty" koanf:"default_gcp_identity_mode"`

	// DefaultGCPIdentityServiceAccountID is the hub-scoped service account
	// assigned when DefaultGCPIdentityMode is "assign".
	DefaultGCPIdentityServiceAccountID string `json:"default_gcp_identity_service_account_id,omitempty" yaml:"default_gcp_identity_service_account_id,omitempty" koanf:"default_gcp_identity_service_account_id"`

	// AutoInjectGcloudADC controls whether the host's gcloud Application Default
	// Credentials file is automatically injected into agent containers in
	// co-located (workstation) mode.
	AutoInjectGcloudADC bool `json:"auto_inject_gcloud_adc,omitempty" yaml:"auto_inject_gcloud_adc,omitempty" koanf:"auto_inject_gcloud_adc"`

	// AutoExposePorts controls whether ports are automatically exposed in agent containers.
	AutoExposePorts *AutoExposePortsSettings `json:"auto_expose_ports,omitempty" yaml:"auto_expose_ports,omitempty" koanf:"auto_expose_ports"`

	// ProjectDefaults controls hub-level defaults applied at project creation time.
	ProjectDefaults *ProjectDefaultsSettings `json:"project_defaults,omitempty" yaml:"project_defaults,omitempty" koanf:"project_defaults"`

	// Quotas controls hub-level quota enforcement toggles.
	Quotas *QuotaSettings `json:"quotas,omitempty" yaml:"quotas,omitempty" koanf:"quotas"`

	// AgentSecrets controls hub-level policy for secrets written by agents.
	AgentSecrets *AgentSecretsSettings `json:"agent_secrets,omitempty" yaml:"agent_secrets,omitempty" koanf:"agent_secrets"`
}

// AutoExposePortsSettings holds the auto-expose ports configuration.
type AutoExposePortsSettings struct {
	Enabled *bool `json:"enabled,omitempty" yaml:"enabled,omitempty" koanf:"enabled"`
}

// QuotaSettings holds the quota enforcement configuration in settings.yaml.
type QuotaSettings struct {
	// EnforceBrokerQuotas controls whether max_agents_per_broker is enforced
	// on create. Default true (fail-safe) when absent.
	EnforceBrokerQuotas *bool `json:"enforce_broker_quotas,omitempty" yaml:"enforce_broker_quotas,omitempty" koanf:"enforce_broker_quotas"`
}

// AgentSecretsSettings holds the hub-level policy for secrets written by
// agents in settings.yaml.
type AgentSecretsSettings struct {
	// UserScopeOnly, when true, restricts agents to writing user (profile)
	// scope secrets only. Default false (nil is false): agents may write
	// project scope as they do today.
	UserScopeOnly *bool `json:"user_scope_only,omitempty" yaml:"user_scope_only,omitempty" koanf:"user_scope_only"`
}

// ProjectDefaultsSettings holds project creation defaults in settings.yaml.
type ProjectDefaultsSettings struct {
	DefaultScratchpad *bool `json:"default_scratchpad,omitempty" yaml:"default_scratchpad,omitempty" koanf:"default_scratchpad"`
}

// V1MaintenanceConfig holds maintenance/update configuration under server.maintenance
// in settings.yaml. Controls the deployment tier, release channel, update policy,
// and related settings for the binary auto-update feature.
type V1MaintenanceConfig struct {
	DeploymentTier     string `json:"deployment_tier,omitempty" yaml:"deployment_tier,omitempty" koanf:"deployment_tier"`
	ReleaseChannel     string `json:"release_channel,omitempty" yaml:"release_channel,omitempty" koanf:"release_channel"`
	UpdatePolicy       string `json:"update_policy,omitempty" yaml:"update_policy,omitempty" koanf:"update_policy"`
	CheckIntervalHours int    `json:"check_interval_hours,omitempty" yaml:"check_interval_hours,omitempty" koanf:"check_interval_hours"`
	GitHubRepo         string `json:"github_repo,omitempty" yaml:"github_repo,omitempty" koanf:"github_repo"`
}

// V1ServerConfig holds server-side configuration in the versioned settings format.
// This mirrors GlobalConfig but uses snake_case koanf/yaml tags.
// Only valid at the global level (~/.scion/settings.yaml), never in project-level settings.
type V1ServerConfig struct {
	// Mode selects the server operating mode: "workstation" (default) or "hosted".
	// When set to "hosted", the server behaves as if --hosted were passed.
	// The legacy value "production" is also accepted for backward compatibility.
	Mode             string                    `json:"mode,omitempty" yaml:"mode,omitempty" koanf:"mode"`
	Env              string                    `json:"env,omitempty" yaml:"env,omitempty" koanf:"env"`
	Hub              *V1ServerHubConfig        `json:"hub,omitempty" yaml:"hub,omitempty" koanf:"hub"`
	Broker           *V1BrokerConfig           `json:"broker,omitempty" yaml:"broker,omitempty" koanf:"broker"`
	Database         *V1DatabaseConfig         `json:"database,omitempty" yaml:"database,omitempty" koanf:"database"`
	Auth             *V1AuthConfig             `json:"auth,omitempty" yaml:"auth,omitempty" koanf:"auth"`
	OAuth            *V1OAuthConfig            `json:"oauth,omitempty" yaml:"oauth,omitempty" koanf:"oauth"`
	Storage          *V1StorageConfig          `json:"storage,omitempty" yaml:"storage,omitempty" koanf:"storage"`
	WorkspaceStorage *V1WorkspaceStorageConfig `json:"workspace_storage,omitempty" yaml:"workspace_storage,omitempty" koanf:"workspace_storage"`
	// SharedDirStorage selects the storage backend for project shared
	// directories, independent of WorkspaceStorage (design
	// deploy-config-explore §3.2). See V1SharedDirStorageConfig.
	SharedDirStorage *V1SharedDirStorageConfig `json:"shared_dir_storage,omitempty" yaml:"shared_dir_storage,omitempty" koanf:"shared_dir_storage"`
	Secrets          *V1SecretsConfig          `json:"secrets,omitempty" yaml:"secrets,omitempty" koanf:"secrets"`
	LogLevel         string                    `json:"log_level,omitempty" yaml:"log_level,omitempty" koanf:"log_level"`
	LogFormat        string                    `json:"log_format,omitempty" yaml:"log_format,omitempty" koanf:"log_format"`

	// Maintenance holds binary auto-update and deployment tier settings.
	Maintenance *V1MaintenanceConfig `json:"maintenance,omitempty" yaml:"maintenance,omitempty" koanf:"maintenance"`

	// NotificationChannels configures external notification delivery channels.
	// Secrets (webhook URLs, API tokens) are held in memory only — never persisted to a database.
	NotificationChannels []V1NotificationChannelConfig `json:"notification_channels,omitempty" yaml:"notification_channels,omitempty" koanf:"notification_channels"`

	// MessageBroker configures the message broker for pub/sub message routing.
	// When enabled, messages are routed through the broker instead of direct dispatch,
	// enabling topic-based subscriptions and broadcast fan-out at the Hub level.
	MessageBroker *V1MessageBrokerConfig `json:"message_broker,omitempty" yaml:"message_broker,omitempty" koanf:"message_broker"`

	// NativeChat controls the built-in chat feature (web chat UI and the
	// /api/v1/chat/* endpoints). Absent means enabled — chat shipped default-on.
	NativeChat *V1NativeChatConfig `json:"native_chat,omitempty" yaml:"native_chat,omitempty" koanf:"native_chat"`

	// Plugins configures external plugin loading for message brokers.
	// Plugins run as separate processes using hashicorp/go-plugin.
	Plugins *V1PluginsConfig `json:"plugins,omitempty" yaml:"plugins,omitempty" koanf:"plugins"`

	// GitHubApp configures the Hub's GitHub App integration for agent git authentication.
	GitHubApp *V1GitHubAppConfig `json:"github_app,omitempty" yaml:"github_app,omitempty" koanf:"github_app"`

	// Scheduler configures the Hub background task scheduler.
	Scheduler *V1SchedulerConfig `json:"scheduler,omitempty" yaml:"scheduler,omitempty" koanf:"scheduler"`

	// OIDCLogin configures an external OIDC provider for web login
	// (the Hub as Relying Party, not as IdP).
	OIDCLogin *V1OIDCLoginConfig `json:"oidc_login,omitempty" yaml:"oidc_login,omitempty" koanf:"oidc_login"`

	// OIDC configures the OIDC Identity Provider feature.
	OIDC *OIDCProviderConfig `json:"oidc,omitempty" yaml:"oidc,omitempty" koanf:"oidc"`

	// Federation configures hub-hub federation authentication.
	Federation *V1FederationConfig `json:"federation,omitempty" yaml:"federation,omitempty" koanf:"federation"`
}

// V1GitHubAppConfig holds the GitHub App configuration in settings.yaml format.
type V1GitHubAppConfig struct {
	AppID           int64  `json:"app_id,omitempty" yaml:"app_id,omitempty" koanf:"app_id"`
	PrivateKeyPath  string `json:"private_key_path,omitempty" yaml:"private_key_path,omitempty" koanf:"private_key_path"`
	PrivateKey      string `json:"private_key,omitempty" yaml:"private_key,omitempty" koanf:"private_key"`
	WebhookSecret   string `json:"webhook_secret,omitempty" yaml:"webhook_secret,omitempty" koanf:"webhook_secret"`
	APIBaseURL      string `json:"api_base_url,omitempty" yaml:"api_base_url,omitempty" koanf:"api_base_url"`
	WebhooksEnabled bool   `json:"webhooks_enabled,omitempty" yaml:"webhooks_enabled,omitempty" koanf:"webhooks_enabled"`
	InstallationURL string `json:"installation_url,omitempty" yaml:"installation_url,omitempty" koanf:"installation_url"`
}

// V1SchedulerConfig holds configuration for the Hub background task scheduler.
// Controls the tick interval and concurrency of recurring maintenance tasks
// to allow operators to tune scheduler load to match DB capacity.
type V1SchedulerConfig struct {
	// IntervalSeconds is the root ticker interval in seconds. All recurring
	// handlers fire at multiples of this interval. Default: 60 (1 minute).
	// Increasing this value reduces DB connection pressure on small deployments.
	IntervalSeconds int `json:"interval_seconds,omitempty" yaml:"interval_seconds,omitempty" koanf:"interval_seconds"`
	// MaxConcurrency limits the number of recurring handlers that may run
	// simultaneously in a single tick. When nil (unset), the scheduler uses
	// its built-in default of 2, so the fix for issue #367 (DB connection
	// pool saturation) is active out-of-the-box. Set to 0 for unlimited
	// (pre-fix behavior), or a higher value for larger deployments.
	MaxConcurrency *int `json:"max_concurrency,omitempty" yaml:"max_concurrency,omitempty" koanf:"max_concurrency"`
}

// V1OIDCLoginConfig holds configuration for an external OIDC login provider
// in the versioned settings format (snake_case).
type V1OIDCLoginConfig struct {
	Enabled      *bool    `json:"enabled,omitempty" yaml:"enabled,omitempty" koanf:"enabled"`
	DisplayName  string   `json:"display_name,omitempty" yaml:"display_name,omitempty" koanf:"display_name"`
	IssuerURL    string   `json:"issuer_url,omitempty" yaml:"issuer_url,omitempty" koanf:"issuer_url"`
	ClientID     string   `json:"client_id,omitempty" yaml:"client_id,omitempty" koanf:"client_id"`
	ClientSecret string   `json:"client_secret,omitempty" yaml:"client_secret,omitempty" koanf:"client_secret"`
	Scopes       []string `json:"scopes,omitempty" yaml:"scopes,omitempty" koanf:"scopes"`
}

// V1FederationConfig is the admin API wire format for federation settings.
type V1FederationConfig struct {
	Enabled          *bool                   `json:"enabled,omitempty" yaml:"enabled,omitempty" koanf:"enabled"`
	TrustedIssuers   []V1TrustedIssuerConfig `json:"trusted_issuers,omitempty" yaml:"trusted_issuers,omitempty" koanf:"trusted_issuers"`
	Algorithms       []string                `json:"algorithms,omitempty" yaml:"algorithms,omitempty" koanf:"algorithms"`
	RefreshInterval  string                  `json:"refresh_interval,omitempty" yaml:"refresh_interval,omitempty" koanf:"refresh_interval"`
	DebounceInterval string                  `json:"debounce_interval,omitempty" yaml:"debounce_interval,omitempty" koanf:"debounce_interval"`
}

// V1TrustedIssuerConfig is the admin API wire format for a single trusted issuer.
type V1TrustedIssuerConfig struct {
	IssuerURL        string   `json:"issuer_url" yaml:"issuer_url" koanf:"issuer_url"`
	JWKSURL          string   `json:"jwks_url,omitempty" yaml:"jwks_url,omitempty" koanf:"jwks_url"`
	ExpectedAudience string   `json:"expected_audience,omitempty" yaml:"expected_audience,omitempty" koanf:"expected_audience"`
	AllowedProjects  []string `json:"allowed_projects,omitempty" yaml:"allowed_projects,omitempty" koanf:"allowed_projects"`
	AllowedRootUsers []string `json:"allowed_root_users,omitempty" yaml:"allowed_root_users,omitempty" koanf:"allowed_root_users"`
	DefaultScopes    []string `json:"default_scopes,omitempty" yaml:"default_scopes,omitempty" koanf:"default_scopes"`
	IssuerType       string   `json:"issuer_type,omitempty" yaml:"issuer_type,omitempty" koanf:"issuer_type"`
	DefaultRole      string   `json:"default_role,omitempty" yaml:"default_role,omitempty" koanf:"default_role"`
	AllowedEmails    []string `json:"allowed_emails,omitempty" yaml:"allowed_emails,omitempty" koanf:"allowed_emails"`
	// AllowedGCPProjects and AllowedDomains must stay the last two fields, in
	// the same position and order as on TrustedIssuerConfig
	// (federation_config.go): this file's own GlobalConfig<->V1Settings
	// conversions do a direct struct type conversion between the two
	// (TrustedIssuerConfig(vi) and V1TrustedIssuerConfig(ti), below), which
	// requires identical field name/type/order (struct tags aside).
	AllowedGCPProjects []string `json:"allowed_gcp_projects,omitempty" yaml:"allowed_gcp_projects,omitempty" koanf:"allowed_gcp_projects"`
	AllowedDomains     []string `json:"allowed_domains,omitempty" yaml:"allowed_domains,omitempty" koanf:"allowed_domains"`
}

// V1NotificationChannelConfig holds configuration for an external notification channel.
type V1NotificationChannelConfig struct {
	Type             string            `json:"type" yaml:"type" koanf:"type"`
	Params           map[string]string `json:"params,omitempty" yaml:"params,omitempty" koanf:"params"`
	FilterTypes      []string          `json:"filter_types,omitempty" yaml:"filter_types,omitempty" koanf:"filter_types"`
	FilterUrgentOnly bool              `json:"filter_urgent_only,omitempty" yaml:"filter_urgent_only,omitempty" koanf:"filter_urgent_only"`
}

// V1MessageBrokerConfig configures the message broker adapter.
type V1MessageBrokerConfig struct {
	// Enabled controls whether the message broker is active. Default false.
	Enabled bool `json:"enabled,omitempty" yaml:"enabled,omitempty" koanf:"enabled"`
	// Type selects the broker adapter: "inprocess" (default). Future: "nats", "redis".
	Type string `json:"type,omitempty" yaml:"type,omitempty" koanf:"type"`
	// Types lists multiple broker plugin names for fan-out mode.
	// When non-empty, all listed plugins run concurrently via FanOutBroker.
	// Falls back to Type (singular) for backward compatibility.
	Types []string `json:"types,omitempty" yaml:"types,omitempty" koanf:"types"`
}

// V1NativeChatConfig configures the built-in native chat feature.
type V1NativeChatConfig struct {
	// Enabled controls whether the chat UI and the /api/v1/chat/* endpoints
	// are available. Nil (absent) means enabled: chat shipped default-on, so
	// an operator must opt out explicitly. A pointer is required to tell
	// "absent" apart from an explicit false — see EnabledSetting.
	Enabled *bool `json:"enabled,omitempty" yaml:"enabled,omitempty" koanf:"enabled"`
}

// EnabledSetting returns the tri-state toggle: nil when the operator expressed
// no preference (callers must read that as enabled — chat is default-on),
// otherwise the explicit setting. Safe to call on a nil config.
func (nc *V1NativeChatConfig) EnabledSetting() *bool {
	if nc == nil {
		return nil
	}
	return nc.Enabled
}

// V1PluginsConfig configures external plugin loading for message brokers.
type V1PluginsConfig struct {
	// Broker maps plugin names to their configuration for message broker plugins.
	Broker map[string]V1PluginEntry `json:"broker,omitempty" yaml:"broker,omitempty" koanf:"broker"`
}

// V1PluginEntry holds configuration for a single plugin.
type V1PluginEntry struct {
	// Path is the explicit filesystem path to the plugin binary.
	// If empty, discovery will attempt to find it automatically.
	// Ignored when SelfManaged is true.
	Path string `json:"path,omitempty" yaml:"path,omitempty" koanf:"path"`
	// Config is an opaque key-value map passed to the plugin via Configure().
	Config map[string]string `json:"config,omitempty" yaml:"config,omitempty" koanf:"config"`
	// ConfigFile is the path to a standalone YAML config file for this plugin.
	// When set, the hub reads non-sensitive settings from this file and merges
	// them into the Config map. Secrets are loaded separately via SecretBackend.
	// If empty, the inline Config map is used as-is (backward compatible).
	ConfigFile string `json:"config_file,omitempty" yaml:"config_file,omitempty" koanf:"config_file"`
	// SelfManaged indicates the plugin manages its own process lifecycle.
	// The Hub connects to the plugin's RPC server rather than starting it.
	SelfManaged bool `json:"self_managed,omitempty" yaml:"self_managed,omitempty" koanf:"self_managed"`
	// Address is the network address for self-managed or gRPC plugins.
	// Required when SelfManaged is true or Mode is "grpc".
	Address string `json:"address,omitempty" yaml:"address,omitempty" koanf:"address"`
	// Mode selects the plugin communication mode: "" or "plugin" (default go-plugin
	// subprocess), "grpc" (standalone gRPC broker), "self-managed" (go-plugin RPC to
	// an externally-managed process). When empty, falls back to SelfManaged for
	// backward compatibility.
	Mode string `json:"mode,omitempty" yaml:"mode,omitempty" koanf:"mode"`
	// TLSCertFile is the path to the client TLS certificate for mTLS.
	TLSCertFile string `json:"tls_cert_file,omitempty" yaml:"tls_cert_file,omitempty" koanf:"tls_cert_file"`
	// TLSKeyFile is the path to the client TLS private key for mTLS.
	TLSKeyFile string `json:"tls_key_file,omitempty" yaml:"tls_key_file,omitempty" koanf:"tls_key_file"`
	// TLSCAFile is the path to the CA certificate for verifying the server.
	TLSCAFile string `json:"tls_ca_file,omitempty" yaml:"tls_ca_file,omitempty" koanf:"tls_ca_file"`
	// TLSSkipVerify disables TLS certificate verification (for development).
	TLSSkipVerify bool `json:"tls_skip_verify,omitempty" yaml:"tls_skip_verify,omitempty" koanf:"tls_skip_verify"`

	// AuthType selects the per-RPC authentication mode for gRPC connections.
	// Supported values: "" or "none" (no auth), "google_id_token" (Google OIDC
	// ID token via metadata server or ADC). Required for authenticated remote
	// gRPC transport (Cloud Run, Kubernetes).
	AuthType string `json:"auth_type,omitempty" yaml:"auth_type,omitempty" koanf:"auth_type"`

	// AuthAudience is the target audience for ID tokens when AuthType is
	// "google_id_token". For Cloud Run, this is the service URL. For
	// Kubernetes, this is the bridge's expected audience claim.
	AuthAudience string `json:"auth_audience,omitempty" yaml:"auth_audience,omitempty" koanf:"auth_audience"`
}

// V1ServerHubConfig holds the Hub API server settings (when running scion-server).
type V1ServerHubConfig struct {
	Port      int    `json:"port,omitempty" yaml:"port,omitempty" koanf:"port"`
	Host      string `json:"host,omitempty" yaml:"host,omitempty" koanf:"host"`
	HubID     string `json:"hub_id,omitempty" yaml:"hub_id,omitempty" koanf:"hub_id"`
	HubName   string `json:"hub_name,omitempty" yaml:"hub_name,omitempty" koanf:"hub_name"`
	PublicURL string `json:"public_url,omitempty" yaml:"public_url,omitempty" koanf:"public_url"`
	// AgentEndpoint optionally overrides the Hub URL injected into agents as
	// SCION_HUB_ENDPOINT, without changing PublicURL's other uses (invite
	// links, chat-bridge links, the OIDC issuer default, the cloudrun_invoker
	// audience default). Must be scheme://host[:port] only when set — see
	// config.ValidateAgentEndpoint for the exact rules and the normalized
	// form this field should hold.
	AgentEndpoint string        `json:"agent_endpoint,omitempty" yaml:"agent_endpoint,omitempty" koanf:"agent_endpoint"`
	ReadTimeout   string        `json:"read_timeout,omitempty" yaml:"read_timeout,omitempty" koanf:"read_timeout"`
	WriteTimeout  string        `json:"write_timeout,omitempty" yaml:"write_timeout,omitempty" koanf:"write_timeout"`
	CORS          *V1CORSConfig `json:"cors,omitempty" yaml:"cors,omitempty" koanf:"cors"`
	AdminEmails   []string      `json:"admin_emails,omitempty" yaml:"admin_emails,omitempty" koanf:"admin_emails"`

	// SoftDeleteRetention is how long soft-deleted agents are retained (e.g., "72h").
	SoftDeleteRetention string `json:"soft_delete_retention,omitempty" yaml:"soft_delete_retention,omitempty" koanf:"soft_delete_retention"`
	// SoftDeleteRetainFiles controls whether workspace files are preserved during soft-delete.
	SoftDeleteRetainFiles *bool `json:"soft_delete_retain_files,omitempty" yaml:"soft_delete_retain_files,omitempty" koanf:"soft_delete_retain_files"`
	// GCPIAMCheckMode controls whether IAM actAs permission is checked when
	// binding a GCP service account to an agent.
	// "off" (default) or "enforce".
	GCPIAMCheckMode string `json:"gcp_iam_check_mode,omitempty" yaml:"gcp_iam_check_mode,omitempty" koanf:"gcp_iam_check_mode"`
	// GCPIAMDenyUnknownPolicy controls behavior when deny policies cannot be
	// evaluated. "fail-open" (default) or "fail-closed".
	GCPIAMDenyUnknownPolicy string `json:"gcp_iam_deny_unknown_policy,omitempty" yaml:"gcp_iam_deny_unknown_policy,omitempty" koanf:"gcp_iam_deny_unknown_policy"`
	// AutoSuspendStalled controls whether stalled agents are automatically suspended.
	AutoSuspendStalled *bool `json:"auto_suspend_stalled,omitempty" yaml:"auto_suspend_stalled,omitempty" koanf:"auto_suspend_stalled"`
	// StalledThreshold is how long before an agent is marked stalled (e.g., "5m", "10m").
	StalledThreshold string `json:"stalled_threshold,omitempty" yaml:"stalled_threshold,omitempty" koanf:"stalled_threshold"`
	// MissingAgentGrace is how long a running agent must be absent from its
	// runtime broker's complete heartbeat inventory before the Hub marks it
	// as having no container (e.g., "3m"; minimum "1m").
	MissingAgentGrace string `json:"missing_agent_grace,omitempty" yaml:"missing_agent_grace,omitempty" koanf:"missing_agent_grace"`
	// StartClaimLeaseTTL is the lease of a start claim; the holder renews it
	// every third of this (e.g., "90s"; 30s to 5m).
	StartClaimLeaseTTL string `json:"start_claim_lease_ttl,omitempty" yaml:"start_claim_lease_ttl,omitempty" koanf:"start_claim_lease_ttl"`
	// StartMaxDuration is the hard deadline on any agent start, including a
	// wait for another hub node (e.g., "12m"; minimum 11m).
	StartMaxDuration string `json:"start_max_duration,omitempty" yaml:"start_max_duration,omitempty" koanf:"start_max_duration"`
	// StartUnconfirmedHold is the longest a start whose outcome is unknown
	// blocks other starts of the agent (e.g., "13m"; minimum 12m40s).
	StartUnconfirmedHold string `json:"start_unconfirmed_hold,omitempty" yaml:"start_unconfirmed_hold,omitempty" koanf:"start_unconfirmed_hold"`
	// StartCreateUnconfirmedHold is StartUnconfirmedHold for a new agent's
	// create-and-start (e.g., "5m"; 3m up to StartUnconfirmedHold).
	StartCreateUnconfirmedHold string `json:"start_create_unconfirmed_hold,omitempty" yaml:"start_create_unconfirmed_hold,omitempty" koanf:"start_create_unconfirmed_hold"`
	// DisableLegacyStorageFallback disables legacy un-namespaced storage path fallback.
	DisableLegacyStorageFallback *bool `json:"disable_legacy_storage_fallback,omitempty" yaml:"disable_legacy_storage_fallback,omitempty" koanf:"disable_legacy_storage_fallback"`
	// AsyncAgentLaunch is the non-blocking agent create kill switch.
	AsyncAgentLaunch *bool `json:"async_agent_launch,omitempty" yaml:"async_agent_launch,omitempty" koanf:"async_agent_launch"`
	// LaunchTimeout is the whole-launch budget for an opted-in launch (e.g., "5m").
	LaunchTimeout string `json:"launch_timeout,omitempty" yaml:"launch_timeout,omitempty" koanf:"launch_timeout"`
	// LaunchKeepaliveSeconds is the broker keepalive interval, in seconds.
	// Today it only sets the reaper's staleness window (8x this value); it
	// will also be sent to the broker once the async dispatch path lands.
	LaunchKeepaliveSeconds *int `json:"launch_keepalive_seconds,omitempty" yaml:"launch_keepalive_seconds,omitempty" koanf:"launch_keepalive_seconds"`
}

// V1BrokerConfig holds Runtime Broker configuration.
// Includes broker identity fields previously in hub client config.
type V1BrokerConfig struct {
	Enabled              bool          `json:"enabled,omitempty" yaml:"enabled,omitempty" koanf:"enabled"`
	Port                 int           `json:"port,omitempty" yaml:"port,omitempty" koanf:"port"`
	Host                 string        `json:"host,omitempty" yaml:"host,omitempty" koanf:"host"`
	ReadTimeout          string        `json:"read_timeout,omitempty" yaml:"read_timeout,omitempty" koanf:"read_timeout"`
	WriteTimeout         string        `json:"write_timeout,omitempty" yaml:"write_timeout,omitempty" koanf:"write_timeout"`
	HubEndpoint          string        `json:"hub_endpoint,omitempty" yaml:"hub_endpoint,omitempty" koanf:"hub_endpoint"`
	ContainerHubEndpoint string        `json:"container_hub_endpoint,omitempty" yaml:"container_hub_endpoint,omitempty" koanf:"container_hub_endpoint"`
	BrokerID             string        `json:"broker_id,omitempty" yaml:"broker_id,omitempty" koanf:"broker_id"`
	BrokerName           string        `json:"broker_name,omitempty" yaml:"broker_name,omitempty" koanf:"broker_name"`
	BrokerNickname       string        `json:"broker_nickname,omitempty" yaml:"broker_nickname,omitempty" koanf:"broker_nickname"`
	BrokerToken          string        `json:"broker_token,omitempty" yaml:"broker_token,omitempty" koanf:"broker_token"`
	AutoProvide          *bool         `json:"auto_provide,omitempty" yaml:"auto_provide,omitempty" koanf:"auto_provide"`
	CORS                 *V1CORSConfig `json:"cors,omitempty" yaml:"cors,omitempty" koanf:"cors"`
	// AllowContainerScriptHarnesses controls whether this broker will
	// dispatch agents whose harness-config declares container-script
	// provisioning. Defaults to true; set false to block container-script dispatches.
	AllowContainerScriptHarnesses *bool `json:"allow_container_script_harnesses,omitempty" yaml:"allow_container_script_harnesses,omitempty" koanf:"allow_container_script_harnesses"`
}

// V1DatabaseConfig holds database settings.
type V1DatabaseConfig struct {
	Driver          string `json:"driver,omitempty" yaml:"driver,omitempty" koanf:"driver"`
	URL             string `json:"url,omitempty" yaml:"url,omitempty" koanf:"url"`
	MaxOpenConns    int    `json:"max_open_conns,omitempty" yaml:"max_open_conns,omitempty" koanf:"max_open_conns"`
	MaxIdleConns    int    `json:"max_idle_conns,omitempty" yaml:"max_idle_conns,omitempty" koanf:"max_idle_conns"`
	ConnMaxLifetime string `json:"conn_max_lifetime,omitempty" yaml:"conn_max_lifetime,omitempty" koanf:"conn_max_lifetime"`
	ConnMaxIdleTime string `json:"conn_max_idle_time,omitempty" yaml:"conn_max_idle_time,omitempty" koanf:"conn_max_idle_time"`
}

// V1AuthConfig holds authentication settings.
type V1AuthConfig struct {
	// Mode selects the exclusive human auth mode: "oauth" (default), "proxy", or "dev".
	// In proxy mode, OAuth handlers are disabled; in dev mode, dev token auth is used.
	Mode              string             `json:"mode,omitempty" yaml:"mode,omitempty" koanf:"mode"`
	DevMode           bool               `json:"dev_mode,omitempty" yaml:"dev_mode,omitempty" koanf:"dev_mode"`
	DevToken          string             `json:"dev_token,omitempty" yaml:"dev_token,omitempty" koanf:"dev_token"`
	DevTokenFile      string             `json:"dev_token_file,omitempty" yaml:"dev_token_file,omitempty" koanf:"dev_token_file"`
	AuthorizedDomains []string           `json:"authorized_domains,omitempty" yaml:"authorized_domains,omitempty" koanf:"authorized_domains"`
	UserAccessMode    string             `json:"user_access_mode,omitempty" yaml:"user_access_mode,omitempty" koanf:"user_access_mode"`
	DefaultUserRole   string             `json:"default_user_role,omitempty" yaml:"default_user_role,omitempty" koanf:"default_user_role"`
	Proxy             *V1ProxyConfig     `json:"proxy,omitempty" yaml:"proxy,omitempty" koanf:"proxy"`
	Transport         *V1TransportConfig `json:"transport,omitempty" yaml:"transport,omitempty" koanf:"transport"`
	Username          string             `json:"username,omitempty" yaml:"username,omitempty" koanf:"username"`
	DisplayName       string             `json:"display_name,omitempty" yaml:"display_name,omitempty" koanf:"display_name"`
	Email             string             `json:"email,omitempty" yaml:"email,omitempty" koanf:"email"`
}

// V1TransportConfig holds transport-layer auth settings for agent outbound requests.
type V1TransportConfig struct {
	// Mode selects the transport auth mode: "none" (default), "cloudrun_invoker", or "iap".
	Mode string `json:"mode,omitempty" yaml:"mode,omitempty" koanf:"mode"`
	// OIDCAudience is the OIDC audience for transport tokens.
	OIDCAudience string `json:"oidc_audience,omitempty" yaml:"oidc_audience,omitempty" koanf:"oidc_audience"`
	// PlatformAuthSA is the dedicated SA email used for transport-layer auth.
	PlatformAuthSA string `json:"platform_auth_sa,omitempty" yaml:"platform_auth_sa,omitempty" koanf:"platform_auth_sa"`
}

// V1ProxyConfig holds proxy authentication settings (consulted when auth.mode == "proxy").
type V1ProxyConfig struct {
	// Provider selects the proxy auth provider: "iap", "jwt", or "header".
	Provider string `json:"provider,omitempty" yaml:"provider,omitempty" koanf:"provider"`
	// IAP holds Google IAP-specific settings.
	IAP *V1IAPConfig `json:"iap,omitempty" yaml:"iap,omitempty" koanf:"iap"`
	// JWT holds settings for the generic JWT proxy auth provider.
	JWT *V1JWTAuthConfig `json:"jwt,omitempty" yaml:"jwt,omitempty" koanf:"jwt"`
	// RequireTrustedProxyIP enables defense-in-depth IP allowlisting.
	RequireTrustedProxyIP bool `json:"require_trusted_proxy_ip,omitempty" yaml:"require_trusted_proxy_ip,omitempty" koanf:"require_trusted_proxy_ip"`
}

// V1IAPConfig holds Google IAP-specific settings.
type V1IAPConfig struct {
	// Audience is the expected audience claim — MANDATORY for IAP.
	Audience string `json:"audience,omitempty" yaml:"audience,omitempty" koanf:"audience"`
	// Issuer overrides the default IAP issuer (for testing).
	Issuer string `json:"issuer,omitempty" yaml:"issuer,omitempty" koanf:"issuer"`
	// JWKSURL overrides the default IAP JWKS URL (for testing).
	JWKSURL string `json:"jwks_url,omitempty" yaml:"jwks_url,omitempty" koanf:"jwks_url"`
}

// V1JWTAuthConfig holds settings for the generic JWT proxy auth provider
// (V1/snake_case settings.yaml equivalent of JWTAuthConfig).
type V1JWTAuthConfig struct {
	Header        string             `json:"header,omitempty" yaml:"header,omitempty" koanf:"header"`
	Algorithm     string             `json:"algorithm,omitempty" yaml:"algorithm,omitempty" koanf:"algorithm"`
	Issuer        string             `json:"issuer,omitempty" yaml:"issuer,omitempty" koanf:"issuer"`
	Audience      string             `json:"audience,omitempty" yaml:"audience,omitempty" koanf:"audience"`
	JWKSURL       string             `json:"jwks_url,omitempty" yaml:"jwks_url,omitempty" koanf:"jwks_url"`
	JWKSFile      string             `json:"jwks_file,omitempty" yaml:"jwks_file,omitempty" koanf:"jwks_file"`
	PublicKeyFile string             `json:"public_key_file,omitempty" yaml:"public_key_file,omitempty" koanf:"public_key_file"`
	Claims        *V1JWTClaimsConfig `json:"claims,omitempty" yaml:"claims,omitempty" koanf:"claims"`
}

// V1JWTClaimsConfig maps JWT claim names to identity fields (V1/snake_case
// settings.yaml equivalent of JWTClaimsConfig).
type V1JWTClaimsConfig struct {
	Email       string `json:"email,omitempty" yaml:"email,omitempty" koanf:"email"`
	Subject     string `json:"subject,omitempty" yaml:"subject,omitempty" koanf:"subject"`
	DisplayName string `json:"display_name,omitempty" yaml:"display_name,omitempty" koanf:"display_name"`
	Domain      string `json:"domain,omitempty" yaml:"domain,omitempty" koanf:"domain"`
}

// V1OAuthConfig holds OAuth provider configurations.
type V1OAuthConfig struct {
	Web    *V1OAuthClientConfig `json:"web,omitempty" yaml:"web,omitempty" koanf:"web"`
	CLI    *V1OAuthClientConfig `json:"cli,omitempty" yaml:"cli,omitempty" koanf:"cli"`
	Device *V1OAuthClientConfig `json:"device,omitempty" yaml:"device,omitempty" koanf:"device"`
}

// V1OAuthClientConfig holds OAuth provider settings for a specific client type.
type V1OAuthClientConfig struct {
	Google *V1OAuthProviderConfig `json:"google,omitempty" yaml:"google,omitempty" koanf:"google"`
	GitHub *V1OAuthProviderConfig `json:"github,omitempty" yaml:"github,omitempty" koanf:"github"`
}

// V1OAuthProviderConfig holds OAuth credentials for a single provider.
type V1OAuthProviderConfig struct {
	ClientID     string `json:"client_id,omitempty" yaml:"client_id,omitempty" koanf:"client_id"`
	ClientSecret string `json:"client_secret,omitempty" yaml:"client_secret,omitempty" koanf:"client_secret"`
}

// V1StorageConfig holds storage settings.
type V1StorageConfig struct {
	Provider  string `json:"provider,omitempty" yaml:"provider,omitempty" koanf:"provider"`
	Bucket    string `json:"bucket,omitempty" yaml:"bucket,omitempty" koanf:"bucket"`
	LocalPath string `json:"local_path,omitempty" yaml:"local_path,omitempty" koanf:"local_path"`
}

// V1WorkspaceStorageConfig selects the workspace storage backend.
// Backend defaults to "local" (today's node-local behavior). When set to "nfs",
// the NFS sub-block configures shared network-attached workspace storage.
// "cloudrun-volume" and "gke-shared-volume" select vendor-managed volume backends.
type V1WorkspaceStorageConfig struct {
	Backend         string                   `json:"backend,omitempty" yaml:"backend,omitempty" koanf:"backend"` // "local" | "nfs" | "cloudrun-volume" | "gke-shared-volume"
	NFS             *V1NFSConfig             `json:"nfs,omitempty" yaml:"nfs,omitempty" koanf:"nfs"`
	CloudRunVolume  *V1CloudRunVolumeConfig  `json:"cloudrun_volume,omitempty" yaml:"cloudrun_volume,omitempty" koanf:"cloudrun_volume"`
	GKESharedVolume *V1GKESharedVolumeConfig `json:"gke_shared_volume,omitempty" yaml:"gke_shared_volume,omitempty" koanf:"gke_shared_volume"`
}

// V1NFSConfig holds NFS workspace storage settings.
type V1NFSConfig struct {
	// MountRoot is the local base under which each share is mounted at <MountRoot>/<share.ID>.
	MountRoot string `json:"mount_root,omitempty" yaml:"mount_root,omitempty" koanf:"mount_root"`
	// MountOptions are passed to mount.nfs. Default "vers=3,hard,nconnect=4,_netdev".
	// NFSv4.1 requires Filestore Enterprise/zonal or self-hosted NFS; basic/HDD
	// (BASIC_HDD) supports NFSv3 only. We use Postgres advisory locks, not NFS
	// flock, so v3 is fine for correctness.
	MountOptions string       `json:"mount_options,omitempty" yaml:"mount_options,omitempty" koanf:"mount_options"`
	Shares       []V1NFSShare `json:"shares,omitempty" yaml:"shares,omitempty" koanf:"shares"`

	// AutoMount lets a Runtime Broker mount each share itself, at
	// <MountRoot>/<share.ID>, in the background at startup and again before
	// each NFS-backed dispatch to a local-container runtime (dispatches to
	// Kubernetes or Cloud Run never mount). A broker whose default runtime
	// is Kubernetes or Cloud Run never mounts, even with AutoMount on; it
	// only verifies. Default false: the operator (or the kubelet, on
	// Kubernetes) provides the mounts, and the broker only checks them
	// read-only for /healthz and scion doctor. Mounting requires the broker
	// to run as root (mount.nfs checks uid 0). Only server.workspace_storage
	// reads this field; shared_dir_storage ignores it.
	AutoMount bool `json:"auto_mount,omitempty" yaml:"auto_mount,omitempty" koanf:"auto_mount"`

	// Stable, node-independent ownership for NFS-backed trees.
	// Default 1000:1000 to converge with the K8s pod UID/GID.
	UID int `json:"uid,omitempty" yaml:"uid,omitempty" koanf:"uid"` // default 1000
	GID int `json:"gid,omitempty" yaml:"gid,omitempty" koanf:"gid"` // default 1000

	// Kubernetes realization
	StorageClass string `json:"storage_class,omitempty" yaml:"storage_class,omitempty" koanf:"storage_class"`
	SubPathRoot  string `json:"subpath_root,omitempty" yaml:"subpath_root,omitempty" koanf:"subpath_root"` // default "projects"
}

// V1NFSShare identifies a single NFS export that may be mounted by a Runtime Broker.
type V1NFSShare struct {
	ID     string `json:"id,omitempty" yaml:"id,omitempty" koanf:"id"`                // stable share id → mount dir + (K8s) PV name
	Server string `json:"server,omitempty" yaml:"server,omitempty" koanf:"server"`    // e.g. 10.0.0.2 or Filestore IP
	Export string `json:"export,omitempty" yaml:"export,omitempty" koanf:"export"`    // server export path, e.g. /scion-workspaces
	PVName string `json:"pv_name,omitempty" yaml:"pv_name,omitempty" koanf:"pv_name"` // K8s static PV+subPath strategy
}

// V1CloudRunVolumeConfig holds Cloud Run managed volume settings.
// Cloud Run volumes are declared in the service spec and mounted by the
// platform — no host path or NFS server is needed.
type V1CloudRunVolumeConfig struct {
	// VolumeName is the Cloud Run volume resource name declared in the service YAML.
	VolumeName string `json:"volume_name,omitempty" yaml:"volume_name,omitempty" koanf:"volume_name"`
	// SubPathRoot is the sub-directory prefix within the volume. Default "projects".
	SubPathRoot string `json:"subpath_root,omitempty" yaml:"subpath_root,omitempty" koanf:"subpath_root"`
}

// V1GKESharedVolumeConfig holds GKE-provided shared volume settings
// (e.g. a Filestore CSI-backed PVC that GKE manages).
type V1GKESharedVolumeConfig struct {
	// VolumeName is the K8s volume name referencing the PVC.
	VolumeName string `json:"volume_name,omitempty" yaml:"volume_name,omitempty" koanf:"volume_name"`
	// PVClaimName is the PVC name bound to the GKE-managed shared storage.
	PVClaimName string `json:"pv_claim_name,omitempty" yaml:"pv_claim_name,omitempty" koanf:"pv_claim_name"`
	// SubPathRoot is the sub-directory prefix within the volume. Default "projects".
	SubPathRoot string `json:"subpath_root,omitempty" yaml:"subpath_root,omitempty" koanf:"subpath_root"`
}

// ApplyNFSDefaults fills default values for NFS sub-fields when Backend is "nfs".
// When Backend is empty or "local", the NFS block is left as-is (no materialization).
// This is idempotent and safe to call multiple times.
func (ws *V1WorkspaceStorageConfig) ApplyNFSDefaults() {
	if ws == nil || strings.ToLower(ws.Backend) != "nfs" {
		return
	}
	if ws.NFS == nil {
		ws.NFS = &V1NFSConfig{}
	}
	if ws.NFS.MountOptions == "" {
		ws.NFS.MountOptions = "vers=3,hard,nconnect=4,_netdev"
	}
	if ws.NFS.UID == 0 {
		ws.NFS.UID = 1000
	}
	if ws.NFS.GID == 0 {
		ws.NFS.GID = 1000
	}
	if ws.NFS.SubPathRoot == "" {
		ws.NFS.SubPathRoot = "projects"
	}
}

// ValidateNFS returns an error if Backend is "nfs" but the NFS block is
// misconfigured (e.g. no shares defined). Call after ApplyNFSDefaults.
func (ws *V1WorkspaceStorageConfig) ValidateNFS() error {
	if ws == nil || strings.ToLower(ws.Backend) != "nfs" {
		return nil
	}
	if ws.NFS == nil || len(ws.NFS.Shares) == 0 {
		return fmt.Errorf("workspace_storage.backend is \"nfs\" but no NFS shares are defined; " +
			"add at least one entry under workspace_storage.nfs.shares")
	}
	return nil
}

// V1SharedDirStorageConfig selects the storage backend for a project's
// shared directories (design deploy-config-explore §3.2), decoupled from
// V1WorkspaceStorageConfig. Backend defaults to "local" (today's behavior:
// shared dirs live next to the project's local config, resolved by
// GetSharedDirsBasePath). When set to "nfs", the reused V1NFSConfig block
// configures the shared NFS export that both Docker/Podman/Apple and
// Kubernetes runtimes resolve into the same layout:
//
//	<nfs.mount_root>/<nfs.shares[0].id>/<nfs.subpath_root>/<projectID>/shared-dirs/<name>
//
// shared_dir_storage is Layer-0 (see opsettings/koanf.go layer0Prefixes):
// never written to the DB, requires a restart to take effect, and — per
// design §3.2.1 "global-only" and AC5 — read from each broker process's
// GLOBAL settings.yaml only. Callers must source this block via
// LoadGlobalSettings(), not from the project-merged VersionedSettings that
// LoadEffectiveSettings(dir) returns for a project directory, AND NOT via
// LoadEffectiveSettings("") — an empty path resolves a project from the
// current working directory (FindProjectRoot) and merges it on top of
// global, so it is not global-only either (round 2 review findings
// C1/T1/S-F2). A project's settings must not be able to redirect Docker
// bind-mount sources to an operator-unapproved host path.
//
// The workspace-storage-only fields on V1NFSConfig (UID, GID, MountOptions,
// StorageClass) are not used by shared_dir_storage. Warning about them being
// set-but-ignored is phase 2 (design §3.2.1), not implemented here.
type V1SharedDirStorageConfig struct {
	Backend string       `json:"backend,omitempty" yaml:"backend,omitempty" koanf:"backend"` // "" | "local" | "nfs"
	NFS     *V1NFSConfig `json:"nfs,omitempty" yaml:"nfs,omitempty" koanf:"nfs"`
}

// Validate returns an error unless Backend is exactly one of "", "local", or
// "nfs" — no trimming or case folding, so a typo such as "NFS " or "Nfs"
// fails closed rather than silently taking the local-layout branch (design
// G5) — and, when Backend is "nfs", unless the block is fully configured.
// Unlike V1WorkspaceStorageConfig.ValidateNFS, this requires MountRoot and
// the first share's ID to be non-empty: shared_dir_storage has no other
// source for the host mount point used by local-container bind mounts
// (design deploy-config-explore §3.2.1).
func (s *V1SharedDirStorageConfig) Validate() error {
	if s == nil {
		return nil
	}
	switch s.Backend {
	case "", "local":
		return nil
	case "nfs":
		// fall through to the nfs-specific checks below.
	default:
		return fmt.Errorf(
			"server.shared_dir_storage.backend must be \"\", \"local\", or \"nfs\" (got %q)", s.Backend)
	}
	if s.NFS == nil {
		return fmt.Errorf(
			"server.shared_dir_storage.backend is \"nfs\" but no nfs block is configured; add server.shared_dir_storage.nfs")
	}
	if len(s.NFS.Shares) == 0 {
		return fmt.Errorf(
			"server.shared_dir_storage.backend is \"nfs\" but no NFS shares are defined; " +
				"add at least one entry under server.shared_dir_storage.nfs.shares")
	}
	if s.NFS.MountRoot == "" {
		return fmt.Errorf("server.shared_dir_storage.backend is \"nfs\" but nfs.mount_root is empty")
	}
	if s.NFS.Shares[0].ID == "" {
		return fmt.Errorf("server.shared_dir_storage.backend is \"nfs\" but nfs.shares[0].id is empty")
	}
	if s.NFS.SubPathRoot != "" {
		if err := validateSubPathRoot(s.NFS.SubPathRoot); err != nil {
			return fmt.Errorf("server.shared_dir_storage.nfs.subpath_root %w", err)
		}
	}
	return nil
}

// validateSubPathRoot rejects a subpath_root that would produce a
// confusing error or an unsafe path-component chain once joined with the
// project ID and shared-dir name (round 6 review nit #6). An absolute
// value (e.g. "/projects") produces a leading empty path component when
// the resulting relative path is split on "/", which the component walk in
// pkg/agent/shared_dir_storage_unix.go then reports as a confusing
// low-level "mkdir path component \"\": no such file or directory" instead
// of a clear configuration error — it still fails closed either way, but
// this catches the operator mistake at config-validation time with an
// actionable message instead. "." and ".." components have no meaningful
// interpretation here either: subpath_root exists to name one literal,
// fixed subdirectory of the export, not to navigate the tree.
func validateSubPathRoot(subPathRoot string) error {
	if filepath.IsAbs(subPathRoot) {
		return fmt.Errorf("must be relative, not absolute (got %q)", subPathRoot)
	}
	for _, comp := range strings.Split(filepath.ToSlash(subPathRoot), "/") {
		switch comp {
		case "":
			return fmt.Errorf("must not contain an empty path component (got %q)", subPathRoot)
		case ".":
			return fmt.Errorf("must not contain a \".\" path component (got %q)", subPathRoot)
		case "..":
			return fmt.Errorf("must not contain a \"..\" path component (got %q)", subPathRoot)
		}
	}
	return nil
}

// sharedDirStorageIgnoredNFSFields lists the V1NFSConfig fields that are
// meaningful for workspace_storage but never consulted by shared_dir_storage
// (design deploy-config-explore §3.2.1's "warned and ignored if set").
var sharedDirStorageIgnoredNFSFields = []struct {
	name string
	set  func(nfs *V1NFSConfig) bool
}{
	{"uid", func(nfs *V1NFSConfig) bool { return nfs.UID != 0 }},
	{"gid", func(nfs *V1NFSConfig) bool { return nfs.GID != 0 }},
	{"mount_options", func(nfs *V1NFSConfig) bool { return nfs.MountOptions != "" }},
	{"storage_class", func(nfs *V1NFSConfig) bool { return nfs.StorageClass != "" }},
	{"auto_mount", func(nfs *V1NFSConfig) bool { return nfs.AutoMount }},
}

// IgnoredNFSFields returns the names of the workspace-storage-only NFS
// fields (uid, gid, mount_options, storage_class, auto_mount) that are set on s but
// never used by shared_dir_storage, for a one-time startup warning (Phase 2
// item 5, design §7 Phase 2: "startup validation warns about ignored
// fields"). Returns nil if s is nil, s.NFS is nil, or backend isn't "nfs" —
// there is nothing to warn about in any of those cases.
func (s *V1SharedDirStorageConfig) IgnoredNFSFields() []string {
	if s == nil || s.NFS == nil || s.Backend != "nfs" {
		return nil
	}
	var ignored []string
	for _, f := range sharedDirStorageIgnoredNFSFields {
		if f.set(s.NFS) {
			ignored = append(ignored, f.name)
		}
	}
	return ignored
}

// ResolvedLayoutSummary returns a one-line, human-readable summary of the
// resolved shared_dir_storage layout — backend, host base, subpath_root,
// pv_name — for the single startup log line design §7 Phase 2 calls for.
// Returns "" if s is nil, s.NFS is nil, or backend isn't "nfs" (nothing to
// log: the unset/local case is unchanged Phase 1 behavior, not a new
// resolved layout).
func (s *V1SharedDirStorageConfig) ResolvedLayoutSummary() string {
	if s == nil || s.NFS == nil || s.Backend != "nfs" {
		return ""
	}
	hostBase := ""
	pvName := ""
	if len(s.NFS.Shares) > 0 {
		hostBase = filepath.Join(s.NFS.MountRoot, s.NFS.Shares[0].ID)
		pvName = s.NFS.Shares[0].PVName
	}
	subPathRoot := s.NFS.SubPathRoot
	if subPathRoot == "" {
		subPathRoot = "projects"
	}
	return fmt.Sprintf("backend=nfs host_base=%s subpath_root=%s pv_name=%s", hostBase, subPathRoot, pvName)
}

// V1SecretsConfig holds secrets backend settings.
type V1SecretsConfig struct {
	Backend                 string   `json:"backend,omitempty" yaml:"backend,omitempty" koanf:"backend"`
	GCPProjectID            string   `json:"gcp_project_id,omitempty" yaml:"gcp_project_id,omitempty" koanf:"gcp_project_id"`
	GCPCredentials          string   `json:"gcp_credentials,omitempty" yaml:"gcp_credentials,omitempty" koanf:"gcp_credentials"`
	GCPReplicationLocations []string `json:"gcp_replication_locations,omitempty" yaml:"gcp_replication_locations,omitempty" koanf:"gcp_replication_locations"`
}

// V1CORSConfig holds CORS settings for server endpoints.
type V1CORSConfig struct {
	Enabled        bool     `json:"enabled,omitempty" yaml:"enabled,omitempty" koanf:"enabled"`
	AllowedOrigins []string `json:"allowed_origins,omitempty" yaml:"allowed_origins,omitempty" koanf:"allowed_origins"`
	AllowedMethods []string `json:"allowed_methods,omitempty" yaml:"allowed_methods,omitempty" koanf:"allowed_methods"`
	AllowedHeaders []string `json:"allowed_headers,omitempty" yaml:"allowed_headers,omitempty" koanf:"allowed_headers"`
	MaxAge         int      `json:"max_age,omitempty" yaml:"max_age,omitempty" koanf:"max_age"`
}

// V1HubClientConfig defines hub client connection settings for versioned config.
// Legacy fields (Token, APIKey, BrokerID, BrokerToken, LastSyncedAt) are removed.
type V1HubClientConfig struct {
	Enabled   *bool  `json:"enabled,omitempty" yaml:"enabled,omitempty" koanf:"enabled"`
	Linked    *bool  `json:"linked,omitempty" yaml:"linked,omitempty" koanf:"linked"`
	Endpoint  string `json:"endpoint,omitempty" yaml:"endpoint,omitempty" koanf:"endpoint"`
	ProjectID string `json:"project_id,omitempty" yaml:"project_id,omitempty" koanf:"project_id"`
	LocalOnly *bool  `json:"local_only,omitempty" yaml:"local_only,omitempty" koanf:"local_only"`
}

// V1CLIConfig defines CLI behavior settings for versioned config.
type V1CLIConfig struct {
	AutoHelp            *bool `json:"autohelp,omitempty" yaml:"autohelp,omitempty" koanf:"autohelp"`
	InteractiveDisabled *bool `json:"interactive_disabled,omitempty" yaml:"interactive_disabled,omitempty" koanf:"interactive_disabled"`
}

// V1TelemetryConfig holds telemetry/observability settings.
// Configurable at global or project scope in settings.yaml, and overridable per-template/agent
// in scion-agent.yaml. See design doc section 10.2 for the full reference.
type V1TelemetryConfig struct {
	Enabled  *bool                    `json:"enabled,omitempty" yaml:"enabled,omitempty" koanf:"enabled"`
	Cloud    *V1TelemetryCloudConfig  `json:"cloud,omitempty" yaml:"cloud,omitempty" koanf:"cloud"`
	Hub      *V1TelemetryHubConfig    `json:"hub,omitempty" yaml:"hub,omitempty" koanf:"hub"`
	Local    *V1TelemetryLocalConfig  `json:"local,omitempty" yaml:"local,omitempty" koanf:"local"`
	Filter   *V1TelemetryFilterConfig `json:"filter,omitempty" yaml:"filter,omitempty" koanf:"filter"`
	Resource map[string]string        `json:"resource,omitempty" yaml:"resource,omitempty" koanf:"resource"`
}

// V1TelemetryCloudConfig holds cloud OTLP forwarding settings.
type V1TelemetryCloudConfig struct {
	Enabled      *bool                   `json:"enabled,omitempty" yaml:"enabled,omitempty" koanf:"enabled"`
	Endpoint     string                  `json:"endpoint,omitempty" yaml:"endpoint,omitempty" koanf:"endpoint"`
	Protocol     string                  `json:"protocol,omitempty" yaml:"protocol,omitempty" koanf:"protocol"`
	Headers      map[string]string       `json:"headers,omitempty" yaml:"headers,omitempty" koanf:"headers"`
	TLS          *V1TelemetryTLSConfig   `json:"tls,omitempty" yaml:"tls,omitempty" koanf:"tls"`
	Batch        *V1TelemetryBatchConfig `json:"batch,omitempty" yaml:"batch,omitempty" koanf:"batch"`
	Provider     string                  `json:"provider,omitempty" yaml:"provider,omitempty" koanf:"provider"`
	GCPProjectID *string                 `json:"gcp_project_id,omitempty" yaml:"gcp_project_id,omitempty" koanf:"gcp_project_id"`
	CloudLogging *bool                   `json:"cloud_logging,omitempty" yaml:"cloud_logging,omitempty" koanf:"cloud_logging"`
}

// V1TelemetryTLSConfig holds TLS settings for cloud OTLP export.
type V1TelemetryTLSConfig struct {
	Enabled            *bool  `json:"enabled,omitempty" yaml:"enabled,omitempty" koanf:"enabled"`
	InsecureSkipVerify *bool  `json:"insecure_skip_verify,omitempty" yaml:"insecure_skip_verify,omitempty" koanf:"insecure_skip_verify"`
	CAFile             string `json:"ca_file,omitempty" yaml:"ca_file,omitempty" koanf:"ca_file"`
}

// V1TelemetryBatchConfig holds batch export settings.
type V1TelemetryBatchConfig struct {
	MaxSize int    `json:"max_size,omitempty" yaml:"max_size,omitempty" koanf:"max_size"`
	Timeout string `json:"timeout,omitempty" yaml:"timeout,omitempty" koanf:"timeout"`
}

// V1TelemetryHubConfig holds Hub telemetry reporting settings.
type V1TelemetryHubConfig struct {
	Enabled        *bool  `json:"enabled,omitempty" yaml:"enabled,omitempty" koanf:"enabled"`
	ReportInterval string `json:"report_interval,omitempty" yaml:"report_interval,omitempty" koanf:"report_interval"`
}

// V1TelemetryLocalConfig holds local debug telemetry output settings.
type V1TelemetryLocalConfig struct {
	Enabled *bool  `json:"enabled,omitempty" yaml:"enabled,omitempty" koanf:"enabled"`
	File    string `json:"file,omitempty" yaml:"file,omitempty" koanf:"file"`
	Console *bool  `json:"console,omitempty" yaml:"console,omitempty" koanf:"console"`
}

// V1TelemetryFilterConfig holds event filtering and sampling settings.
type V1TelemetryFilterConfig struct {
	Enabled          *bool                        `json:"enabled,omitempty" yaml:"enabled,omitempty" koanf:"enabled"`
	RespectDebugMode *bool                        `json:"respect_debug_mode,omitempty" yaml:"respect_debug_mode,omitempty" koanf:"respect_debug_mode"`
	Events           *V1TelemetryEventsConfig     `json:"events,omitempty" yaml:"events,omitempty" koanf:"events"`
	Attributes       *V1TelemetryAttributesConfig `json:"attributes,omitempty" yaml:"attributes,omitempty" koanf:"attributes"`
	Sampling         *V1TelemetrySamplingConfig   `json:"sampling,omitempty" yaml:"sampling,omitempty" koanf:"sampling"`
}

// V1TelemetryEventsConfig holds event include/exclude lists.
type V1TelemetryEventsConfig struct {
	Include []string `json:"include,omitempty" yaml:"include,omitempty" koanf:"include"`
	Exclude []string `json:"exclude,omitempty" yaml:"exclude,omitempty" koanf:"exclude"`
}

// V1TelemetryAttributesConfig holds attribute redaction and hashing lists.
type V1TelemetryAttributesConfig struct {
	Redact []string `json:"redact,omitempty" yaml:"redact,omitempty" koanf:"redact"`
	Hash   []string `json:"hash,omitempty" yaml:"hash,omitempty" koanf:"hash"`
}

// V1TelemetrySamplingConfig holds sampling rate settings.
type V1TelemetrySamplingConfig struct {
	Default *float64           `json:"default,omitempty" yaml:"default,omitempty" koanf:"default"`
	Rates   map[string]float64 `json:"rates,omitempty" yaml:"rates,omitempty" koanf:"rates"`
}

// CloudRunConfig holds Cloud Run runtime settings.
type CloudRunConfig struct {
	ProjectID      string `json:"project_id,omitempty" koanf:"project_id"`
	Location       string `json:"location,omitempty" koanf:"location"`
	ServiceAccount string `json:"service_account,omitempty" koanf:"service_account"`
	Network        string `json:"network,omitempty" koanf:"network"`
	Subnetwork     string `json:"subnetwork,omitempty" koanf:"subnetwork"`
	NFSServer      string `json:"nfs_server,omitempty" koanf:"nfs_server"`
	NFSExport      string `json:"nfs_export,omitempty" koanf:"nfs_export"`
}

// V1CloudRunInstancesConfig holds Cloud Run Instances runtime settings.
// This is the per-agent-instance variant of Cloud Run, where each agent
// gets its own Cloud Run service instance.
type V1CloudRunInstancesConfig struct {
	// ProjectID is the GCP project ID for Cloud Run Instances API calls.
	ProjectID string `json:"project_id,omitempty" yaml:"project_id,omitempty" koanf:"project_id"`
	// Region is the GCP region for Cloud Run instances (e.g. "us-central1").
	Region string `json:"region,omitempty" yaml:"region,omitempty" koanf:"region"`
}

// V1CloudRunSandboxConfig holds Cloud Run Sandbox runtime settings.
// The sandbox runtime runs agents as sandboxes inside a Cloud Run Instance.
type V1CloudRunSandboxConfig struct {
	// SandboxBin is the path to the sandbox CLI binary.
	// Defaults to /usr/local/gcp/bin/sandbox.
	SandboxBin string `json:"sandbox_bin,omitempty" yaml:"sandbox_bin,omitempty" koanf:"sandbox_bin"`
}

// V1SubstrateConfig holds Substrate runtime settings (substrate-runtime.md
// §2). Substrate is a Kubernetes-hosted actor
// runtime; scion agents run as Substrate "actors". Selection is explicit
// only — there is no auto-detect branch in factory.go.
type V1SubstrateConfig struct {
	// APIEndpoint is the ateapi Control gRPC endpoint, e.g.
	// "api.ate-system.svc:443".
	APIEndpoint string `json:"api_endpoint,omitempty" yaml:"api_endpoint,omitempty" koanf:"api_endpoint"`
	// RouterEndpoint is the atenet-router inbound endpoint the broker uses
	// to reach an actor's control server, e.g.
	// "http://atenet-router.ate-system.svc:80". Used as a base URL (scheme
	// required), not a bare host:port. This hop is plain HTTP: CAFile and
	// ClusterTrustBundle below apply only to the ateapi Control gRPC dial
	// (APIEndpoint), not to this one, and there is no separate TLS setting
	// for it (see substrate-runtime.md §10).
	RouterEndpoint string `json:"router_endpoint,omitempty" yaml:"router_endpoint,omitempty" koanf:"router_endpoint"`
	// TokenAudience is the audience requested for the in-cluster
	// ServiceAccount TokenRequest used to authenticate to the ateapi
	// Control API. Defaults to "api.ate-system.svc" when empty.
	TokenAudience string `json:"token_audience,omitempty" yaml:"token_audience,omitempty" koanf:"token_audience"`
	// CAFile is a path to a PEM CA bundle used to verify the ateapi Control
	// gRPC server certificate.
	CAFile string `json:"ca_file,omitempty" yaml:"ca_file,omitempty" koanf:"ca_file"`
	// ClusterTrustBundle names a Kubernetes ClusterTrustBundle object
	// holding the CA used to verify the ateapi Control gRPC server
	// certificate. When both this and CAFile are set, the dialer prefers
	// ClusterTrustBundle.
	ClusterTrustBundle string `json:"cluster_trust_bundle,omitempty" yaml:"cluster_trust_bundle,omitempty" koanf:"cluster_trust_bundle"`
	// SandboxClass selects the actor sandbox isolation technology
	// ("gvisor" or "microvm"). Defaults to "gvisor" when empty.
	SandboxClass string `json:"sandbox_class,omitempty" yaml:"sandbox_class,omitempty" koanf:"sandbox_class"`
	// SandboxConfigName names the Substrate SandboxConfig CRD instance used
	// by actor templates.
	SandboxConfigName string `json:"sandbox_config_name,omitempty" yaml:"sandbox_config_name,omitempty" koanf:"sandbox_config_name"`
	// WorkerSelector is copied into the ActorTemplate's workerSelector, to
	// pin actors to a labeled WorkerPool.
	WorkerSelector map[string]string `json:"worker_selector,omitempty" yaml:"worker_selector,omitempty" koanf:"worker_selector"`
	// SnapshotStorage is the configured bucket/prefix used for the
	// ActorTemplate's snapshotsConfig storage, e.g. "gs://bucket/prefix/".
	SnapshotStorage string `json:"snapshot_storage,omitempty" yaml:"snapshot_storage,omitempty" koanf:"snapshot_storage"`
	// EgressAllow lists additional hostnames allowed through the per-actor
	// EgressPolicy, beyond the hub/git/model/telemetry hosts the runtime
	// always adds. Despite the field's own shape (a bare string list), no
	// IP addresses or CIDRs are accepted here — see below.
	//
	// Only public FQDNs are accepted here — no IP addresses or CIDRs at
	// all (Substrate's own HostnameRule, which is where every entry ends
	// up, rejects IP addresses outright), and only a hostname whose
	// top-level domain is a real, ICANN-delegated one, with at least one
	// label beneath its actual matched suffix (which may be a private
	// multi-tenant-platform suffix like "googleapis.com"/"github.io", not
	// only an ICANN one). See pkg/runtime/substrate.ValidateEgressAllow's
	// doc comment for the exact rule set, which changes more often than
	// this comment would otherwise be kept in sync with.
	//
	// Residual risk this does not close: a validly-public hostname can
	// still be made to resolve to a private or in-cluster address (DNS
	// rebinding, or services like nip.io/sslip.io that do this by design).
	// Only a check by the egress proxy itself, after DNS resolution,
	// against the address actually connected to, can close that gap.
	//
	// Validated by pkg/runtime/substrate.Validate, which NewSubstrateRuntime
	// calls when the runtime is constructed, and Run calls again once at
	// its start — not at settings-load time or by `scion config validate`
	// (there is no generic settings-validation hook for this yet), and not
	// a second time inside Run's egress-policy step (r.cfg is immutable for
	// a single Run call, so one check per call is enough). Substrate's
	// egress default-deny plus the actor's EgressPolicy is what keeps an
	// actor off the atenet-router and other in-cluster services
	// (substrate-runtime.md §5.2); an entry that reaches either of those
	// would defeat it.
	EgressAllow []string `json:"egress_allow,omitempty" yaml:"egress_allow,omitempty" koanf:"egress_allow"`
	// EgressTrustBundle names a Substrate trust bundle to project into every
	// actor as a system-info volume, so the actor can validate the egress
	// gateway's own TLS certificate (docs/egress-trust-bundle.md, vendored
	// Substrate d277088b).
	//
	// Required iff the cluster runs the sdsmint egress gateway
	// (`hack/install-ate.sh --deploy-atenet --experimental-use-sdsmint`) and
	// the actor makes any HTTPS/TLS request: under sdsmint, the gateway
	// terminates every TLS connection and re-originates it with a per-SNI
	// leaf certificate chained to its own CA, which the actor otherwise has
	// no way to validate. Setting this on a plain (non-sdsmint) install
	// breaks every actor instead: nothing backs the named
	// ClusterTrustBundle, so the actor fails to start (see
	// pkg/runtime/substrate.Validate and buildActorTemplate's doc comment).
	//
	// Empty (the default) is off, and off is byte-identical to today: no
	// system-info volume, no mount, no env. Validated by
	// pkg/runtime/substrate.Validate: when non-empty it must be exactly
	// "egress-mitm.ate.dev", the only trust bundle name Substrate d277088b
	// supports. Kept as a string validated against a one-name allowlist,
	// not a bool, deliberately: it mirrors Substrate's own
	// trustBundle.name, and a future additional name needs only an
	// allowlist entry here, not a schema change.
	EgressTrustBundle string `json:"egress_trust_bundle,omitempty" yaml:"egress_trust_bundle,omitempty" koanf:"egress_trust_bundle"`
	// TemplateReadyTimeout bounds how long Run waits for a newly created
	// ActorTemplate to become ready (a Go duration string, e.g. "10m").
	// Defaults to 10 minutes when empty.
	TemplateReadyTimeout string `json:"template_ready_timeout,omitempty" yaml:"template_ready_timeout,omitempty" koanf:"template_ready_timeout"`
}

// V1RuntimeConfig extends RuntimeConfig with a Type field.
//
// Env is parsed and round-tripped but not applied to agent containers; no
// code reads it.
type V1RuntimeConfig struct {
	Type              string            `json:"type,omitempty" yaml:"type,omitempty" koanf:"type"`
	Host              string            `json:"host,omitempty" yaml:"host,omitempty" koanf:"host"`
	Context           string            `json:"context,omitempty" yaml:"context,omitempty" koanf:"context"`
	Namespace         string            `json:"namespace,omitempty" yaml:"namespace,omitempty" koanf:"namespace"`
	Env               map[string]string `json:"env,omitempty" yaml:"env,omitempty" koanf:"env"`
	Sync              string            `json:"sync,omitempty" yaml:"sync,omitempty" koanf:"sync"`
	GKE               bool              `json:"gke,omitempty" yaml:"gke,omitempty" koanf:"gke"`
	ListAllNamespaces bool              `json:"list_all_namespaces,omitempty" yaml:"list_all_namespaces,omitempty" koanf:"list_all_namespaces"`
	// PriorityClassName is the Kubernetes-runtime-only default
	// spec.priorityClassName for agent pods using this runtime entry. Must
	// name a PriorityClass that already exists on the cluster — Scion does
	// not create one. Validated as a DNS-1123 subdomain. An explicit
	// template/agent-config kubernetes.priorityClassName outranks this.
	PriorityClassName string `json:"priority_class_name,omitempty" yaml:"priority_class_name,omitempty" koanf:"priority_class_name"`
	// SharedDirStorageClass and SharedDirSize are Kubernetes-only defaults
	// for the ReadWriteMany PVCs backing project shared dirs. They are the
	// lowest settings tier: a profile's values win over them, and a
	// template's or agent's kubernetes block wins over both. See
	// ResolveSharedDirDefaults.
	SharedDirStorageClass string `json:"shared_dir_storage_class,omitempty" yaml:"shared_dir_storage_class,omitempty" koanf:"shared_dir_storage_class"`
	SharedDirSize         string `json:"shared_dir_size,omitempty" yaml:"shared_dir_size,omitempty" koanf:"shared_dir_size"`
	// SafeToEvict is the Kubernetes-only default for the
	// cluster-autoscaler.kubernetes.io/safe-to-evict pod annotation. Only an
	// explicit false has an effect (the pod is annotated "false"); true is
	// accepted and adds nothing. It is the lowest tier: a profile's
	// safe_to_evict wins over it, and a template's or agent's
	// kubernetes.safeToEvict wins over both. See ResolveSafeToEvict.
	SafeToEvict *bool `json:"safe_to_evict,omitempty" yaml:"safe_to_evict,omitempty" koanf:"safe_to_evict"`
	// SharedDirStorageBackend overrides server.shared_dir_storage.backend
	// ("local" or "nfs") for agents whose profile uses this runtime entry.
	// A profile's own value wins over it. The nfs details always come from
	// server.shared_dir_storage.nfs. Read from global settings only; see
	// ResolveSharedDirStorage.
	SharedDirStorageBackend string `json:"shared_dir_storage_backend,omitempty" yaml:"shared_dir_storage_backend,omitempty" koanf:"shared_dir_storage_backend"`
	// CloudRun holds Cloud Run-specific settings when Type is "cloudrun".
	CloudRun *CloudRunConfig `json:"cloudrun,omitempty" yaml:"cloudrun,omitempty" koanf:"cloudrun"`
	// CloudRunInstances holds Cloud Run Instances-specific settings when Type is "cloudrun-instances".
	CloudRunInstances *V1CloudRunInstancesConfig `json:"cloudrun_instances,omitempty" yaml:"cloudrun_instances,omitempty" koanf:"cloudrun_instances"`
	// CloudRunSandbox holds Cloud Run Sandbox-specific settings when Type is "cloudrun-sandbox".
	CloudRunSandbox *V1CloudRunSandboxConfig `json:"cloudrun_sandbox,omitempty" yaml:"cloudrun_sandbox,omitempty" koanf:"cloudrun_sandbox"`
	// Substrate holds Substrate-specific settings when Type is "substrate".
	Substrate *V1SubstrateConfig `json:"substrate,omitempty" yaml:"substrate,omitempty" koanf:"substrate"`
	// KubernetesServiceAccountMappings maps a GCP service account (GSA) email
	// to the Kubernetes ServiceAccount (KSA) name it is bound to via Workload
	// Identity, when Type is "kubernetes". Used by GCP identity mode "assign"
	// dispatches on Kubernetes (ptone/scion#2328) to set the pod's
	// spec.serviceAccountName. Scion never creates, annotates, or binds these
	// KSAs — the operator must pre-provision each one (KSA exists, is
	// annotated with iam.gke.io/gcp-service-account, and holds
	// roles/iam.workloadIdentityUser for the named GSA) out of band. A
	// same-keyed entry in the active profile's own
	// KubernetesServiceAccountMappings overrides this one; see
	// VersionedSettings.ResolveKubernetesServiceAccountMapping.
	KubernetesServiceAccountMappings map[string]string `json:"kubernetes_service_account_mappings,omitempty" yaml:"kubernetes_service_account_mappings,omitempty" koanf:"kubernetes_service_account_mappings"`
}

// V1RuntimeDefaultsConfig holds runtime-wide behaviour that is not specific to
// any single named entry in the `runtimes` map. It lives under the singular
// `runtime` key; `runtimes` (plural) remains the map of named runtime targets.
type V1RuntimeDefaultsConfig struct {
	// EnforceResourceDefaults controls whether the built-in resource defaults
	// (config.BuiltinDefaultResources) are applied to agents that have no
	// resource limits from any other tier.
	//
	// Nil means enabled: the fail-safe direction is to apply a CPU limit.
	// Set to false to restore the pre-2026-07 behaviour of running Docker and
	// Podman containers with no cgroup limits at all.
	EnforceResourceDefaults *bool `json:"enforce_resource_defaults,omitempty" yaml:"enforce_resource_defaults,omitempty" koanf:"enforce_resource_defaults"`
}

// HarnessConfigEntry defines a harness configuration entry in versioned settings.
// The Harness field is required and specifies the harness type this config applies to.
//
// ImagePullPolicy is a Kubernetes-runtime-only default (Always, IfNotPresent,
// or Never). It is ignored by non-Kubernetes runtimes. See ResolveHarnessConfig
// and provision.go for its precedence relative to the on-disk harness-config
// file default and an explicit template/agent kubernetes.imagePullPolicy
// override.
type HarnessConfigEntry struct {
	Name             string               `json:"name,omitempty" yaml:"name,omitempty" koanf:"name"`
	Harness          string               `json:"harness" yaml:"harness" koanf:"harness"`
	Image            string               `json:"image,omitempty" yaml:"image,omitempty" koanf:"image"`
	ImagePullPolicy  string               `json:"image_pull_policy,omitempty" yaml:"image_pull_policy,omitempty" koanf:"image_pull_policy"`
	User             string               `json:"user,omitempty" yaml:"user,omitempty" koanf:"user"`
	Model            string               `json:"model,omitempty" yaml:"model,omitempty" koanf:"model"`
	TaskFlag         string               `json:"task_flag,omitempty" yaml:"task_flag,omitempty" koanf:"task_flag"`
	Args             []string             `json:"args,omitempty" yaml:"args,omitempty" koanf:"args"`
	Env              map[string]string    `json:"env,omitempty" yaml:"env,omitempty" koanf:"env"`
	Volumes          []api.VolumeMount    `json:"volumes,omitempty" yaml:"volumes,omitempty" koanf:"volumes"`
	AuthSelectedType string               `json:"auth_selected_type,omitempty" yaml:"auth_selected_type,omitempty" koanf:"auth_selected_type"`
	Secrets          []api.RequiredSecret `json:"secrets,omitempty" yaml:"secrets,omitempty" koanf:"secrets"`

	// ModelAliases maps abstract size aliases (e.g. "small", "medium", "large")
	// to concrete, harness-specific model names. Templates use the alias in their
	// model field; the alias is resolved to the concrete name at provision time.
	ModelAliases map[string]string `json:"model_aliases,omitempty" yaml:"model_aliases,omitempty" koanf:"model_aliases"`

	// Thinking maps the canonical 0-100 thinking level to harness-native values.
	// Applied in-container by scion_harness.resolve_thinking.
	Thinking *HarnessThinkingConfig `json:"thinking,omitempty" yaml:"thinking,omitempty" koanf:"thinking"`

	Provisioner       *HarnessProvisionerConfig        `json:"provisioner,omitempty" yaml:"provisioner,omitempty" koanf:"provisioner"`
	ConfigDir         string                           `json:"config_dir,omitempty" yaml:"config_dir,omitempty" koanf:"config_dir"`
	SkillsDir         string                           `json:"skills_dir,omitempty" yaml:"skills_dir,omitempty" koanf:"skills_dir"`
	InterruptKey      string                           `json:"interrupt_key,omitempty" yaml:"interrupt_key,omitempty" koanf:"interrupt_key"`
	InterruptSequence []string                         `json:"interrupt_sequence,omitempty" yaml:"interrupt_sequence,omitempty" koanf:"interrupt_sequence"`
	InterruptSignal   string                           `json:"interrupt_signal,omitempty" yaml:"interrupt_signal,omitempty" koanf:"interrupt_signal"`
	InstructionsFile  string                           `json:"instructions_file,omitempty" yaml:"instructions_file,omitempty" koanf:"instructions_file"`
	SystemPromptFile  string                           `json:"system_prompt_file,omitempty" yaml:"system_prompt_file,omitempty" koanf:"system_prompt_file"`
	SystemPromptMode  string                           `json:"system_prompt_mode,omitempty" yaml:"system_prompt_mode,omitempty" koanf:"system_prompt_mode"`
	Command           *HarnessCommandConfig            `json:"command,omitempty" yaml:"command,omitempty" koanf:"command"`
	EnvTemplate       map[string]string                `json:"env_template,omitempty" yaml:"env_template,omitempty" koanf:"env_template"`
	Capabilities      *api.HarnessAdvancedCapabilities `json:"capabilities,omitempty" yaml:"capabilities,omitempty" koanf:"capabilities"`
	Auth              *HarnessAuthMetadata             `json:"auth,omitempty" yaml:"auth,omitempty" koanf:"auth"`
	NoAuthConfig      *HarnessNoAuthConfig             `json:"no_auth,omitempty" yaml:"no_auth,omitempty" koanf:"no_auth"`
	MCP               *HarnessMCPConfig                `json:"mcp,omitempty" yaml:"mcp,omitempty" koanf:"mcp"`
	Dialect           map[string]interface{}           `json:"dialect,omitempty" yaml:"dialect,omitempty" koanf:"dialect"`
}

// HarnessThinkingConfig maps the canonical 0-100 thinking level
// (SCION_THINKING_LEVEL) to harness-native values. A level L maps to the Value
// of the first entry in Levels whose Max >= L. Default is emitted when the
// level is unset or invalid; when empty, nothing is emitted and the harness
// CLI's own default applies. The mapping is resolved in-container by
// scion_harness.resolve_thinking; Go only carries and validates it.
type HarnessThinkingConfig struct {
	Levels  []HarnessThinkingLevel `json:"levels" yaml:"levels" koanf:"levels"`
	Default string                 `json:"default,omitempty" yaml:"default,omitempty" koanf:"default"`
}

// HarnessThinkingLevel is one entry of a HarnessThinkingConfig: Max is the
// inclusive upper bound (0-100) of the level range that maps to Value.
type HarnessThinkingLevel struct {
	Max   int    `json:"max" yaml:"max" koanf:"max"` // no omitempty: 0 is meaningful
	Value string `json:"value" yaml:"value" koanf:"value"`
}

// Validate checks the ordering rules the JSON schema cannot express: Levels
// must be non-empty, each Max must be strictly greater than the previous one,
// and the last Max must be 100 so every clamped level maps to a value.
func (t *HarnessThinkingConfig) Validate() error {
	if t == nil {
		return nil
	}
	if len(t.Levels) == 0 {
		return fmt.Errorf("thinking.levels must not be empty")
	}
	for i, lvl := range t.Levels {
		if lvl.Max < 0 || lvl.Max > 100 {
			return fmt.Errorf("thinking.levels[%d].max must be between 0 and 100, got %d", i, lvl.Max)
		}
		if lvl.Value == "" {
			return fmt.Errorf("thinking.levels[%d].value must not be empty", i)
		}
		if i > 0 && lvl.Max <= t.Levels[i-1].Max {
			return fmt.Errorf("thinking.levels[%d].max (%d) must be greater than thinking.levels[%d].max (%d)", i, lvl.Max, i-1, t.Levels[i-1].Max)
		}
	}
	if last := t.Levels[len(t.Levels)-1].Max; last != 100 {
		return fmt.Errorf("thinking.levels last max must be 100, got %d", last)
	}
	return nil
}

// HarnessProvisionerConfig declares how a harness-config is provisioned.
type HarnessProvisionerConfig struct {
	Type               string   `json:"type,omitempty" yaml:"type,omitempty" koanf:"type"`
	InterfaceVersion   int      `json:"interface_version,omitempty" yaml:"interface_version,omitempty" koanf:"interface_version"`
	Lib                string   `json:"lib,omitempty" yaml:"lib,omitempty" koanf:"lib"`
	Command            []string `json:"command,omitempty" yaml:"command,omitempty" koanf:"command"`
	Timeout            string   `json:"timeout,omitempty" yaml:"timeout,omitempty" koanf:"timeout"`
	LifecycleEvents    []string `json:"lifecycle_events,omitempty" yaml:"lifecycle_events,omitempty" koanf:"lifecycle_events"`
	RequiredImageTools []string `json:"required_image_tools,omitempty" yaml:"required_image_tools,omitempty" koanf:"required_image_tools"`
}

// HarnessCommandConfig describes harness CLI command construction.
type HarnessCommandConfig struct {
	Base             []string `json:"base,omitempty" yaml:"base,omitempty" koanf:"base"`
	ResumeFlag       string   `json:"resume_flag,omitempty" yaml:"resume_flag,omitempty" koanf:"resume_flag"`
	TaskFlag         string   `json:"task_flag,omitempty" yaml:"task_flag,omitempty" koanf:"task_flag"`
	TaskPosition     string   `json:"task_position,omitempty" yaml:"task_position,omitempty" koanf:"task_position"`
	SystemPromptFlag string   `json:"system_prompt_flag,omitempty" yaml:"system_prompt_flag,omitempty" koanf:"system_prompt_flag"`
}

// HarnessAuthMetadata is defined in pkg/api to avoid import cycles.
type HarnessAuthMetadata = api.HarnessAuthMetadata

type HarnessAuthTypeMetadata = api.HarnessAuthTypeMetadata

type HarnessAuthEnvRequirement = api.HarnessAuthEnvRequirement

type HarnessAuthFileRequirement = api.HarnessAuthFileRequirement

type HarnessAuthAutodetect = api.HarnessAuthAutodetect

// HarnessNoAuthConfig defines harness behavior when an agent starts without credentials.
type HarnessNoAuthConfig struct {
	Behavior string `json:"behavior,omitempty" yaml:"behavior,omitempty" koanf:"behavior"`
	Message  string `json:"message,omitempty" yaml:"message,omitempty" koanf:"message"`
	Command  string `json:"command,omitempty" yaml:"command,omitempty" koanf:"command"`
}

// HarnessMCPConfig is the declarative mapping that lets a harness's
// container-side provisioner translate the universal mcp_servers map into the
// harness's native MCP config without bespoke per-harness Python. Used by
// the scion_harness.apply_mcp_servers_simple helper. Harnesses whose native
// format does not fit the simple-merge pattern (e.g. OpenCode) leave this
// empty and translate themselves in provision.py.
type HarnessMCPConfig struct {
	// GlobalConfigFile is the agent-home-relative path to the file that holds
	// global-scope MCP entries (e.g. ".claude.json").
	GlobalConfigFile string `json:"global_config_file,omitempty" yaml:"global_config_file,omitempty" koanf:"global_config_file"`
	// GlobalConfigPath is the dotted JSON path inside GlobalConfigFile where
	// the per-name MCP map lives (e.g. "mcpServers").
	GlobalConfigPath string `json:"global_config_path,omitempty" yaml:"global_config_path,omitempty" koanf:"global_config_path"`
	// ProjectConfigFile and ProjectConfigPath are the same for project-scope
	// servers; the path may include "{workspace}" which is substituted with
	// the agent_workspace path.
	ProjectConfigFile string `json:"project_config_file,omitempty" yaml:"project_config_file,omitempty" koanf:"project_config_file"`
	ProjectConfigPath string `json:"project_config_path,omitempty" yaml:"project_config_path,omitempty" koanf:"project_config_path"`
	// TransportField is the field name in the native server object that holds
	// the transport identifier (e.g. "type" for Claude/Gemini).
	TransportField string `json:"transport_field,omitempty" yaml:"transport_field,omitempty" koanf:"transport_field"`
	// TransportMap maps universal transport values to the native value, e.g.
	//   stdio: stdio
	//   sse: sse
	//   streamable-http: streamable-http
	TransportMap map[string]string `json:"transport_map,omitempty" yaml:"transport_map,omitempty" koanf:"transport_map"`
}

// V1HarnessOverride defines a harness override entry in versioned settings.
// Uses snake_case tags, unlike the legacy HarnessOverride (which uses camelCase auth_selectedType).
type V1HarnessOverride struct {
	Image            string            `json:"image,omitempty" yaml:"image,omitempty" koanf:"image"`
	ImagePullPolicy  string            `json:"image_pull_policy,omitempty" yaml:"image_pull_policy,omitempty" koanf:"image_pull_policy"`
	User             string            `json:"user,omitempty" yaml:"user,omitempty" koanf:"user"`
	Env              map[string]string `json:"env,omitempty" yaml:"env,omitempty" koanf:"env"`
	Volumes          []api.VolumeMount `json:"volumes,omitempty" yaml:"volumes,omitempty" koanf:"volumes"`
	AuthSelectedType string            `json:"auth_selected_type,omitempty" yaml:"auth_selected_type,omitempty" koanf:"auth_selected_type"`
	Resources        *api.ResourceSpec `json:"resources,omitempty" yaml:"resources,omitempty" koanf:"resources"`
}

// V1ProfileConfig extends ProfileConfig with new fields for versioned settings.
type V1ProfileConfig struct {
	Runtime              string                       `json:"runtime" yaml:"runtime" koanf:"runtime"`
	DefaultTemplate      string                       `json:"default_template,omitempty" yaml:"default_template,omitempty" koanf:"default_template"`
	DefaultHarnessConfig string                       `json:"default_harness_config,omitempty" yaml:"default_harness_config,omitempty" koanf:"default_harness_config"`
	ImageRegistry        string                       `json:"image_registry,omitempty" yaml:"image_registry,omitempty" koanf:"image_registry"`
	Volumes              []api.VolumeMount            `json:"volumes,omitempty" yaml:"volumes,omitempty" koanf:"volumes"`
	Resources            *api.ResourceSpec            `json:"resources,omitempty" yaml:"resources,omitempty" koanf:"resources"`
	HarnessOverrides     map[string]V1HarnessOverride `json:"harness_overrides,omitempty" yaml:"harness_overrides,omitempty" koanf:"harness_overrides"`
	Secrets              []api.RequiredSecret         `json:"secrets,omitempty" yaml:"secrets,omitempty" koanf:"secrets"`
	// SharedDirStorageClass and SharedDirSize are Kubernetes-only defaults
	// for shared-dir PVCs created by agents using this profile. They win
	// over the same keys on the profile's runtime entry and lose to a
	// template's or agent's kubernetes block. See ResolveSharedDirDefaults.
	SharedDirStorageClass string `json:"shared_dir_storage_class,omitempty" yaml:"shared_dir_storage_class,omitempty" koanf:"shared_dir_storage_class"`
	SharedDirSize         string `json:"shared_dir_size,omitempty" yaml:"shared_dir_size,omitempty" koanf:"shared_dir_size"`
	// SafeToEvict is the Kubernetes-only safe-to-evict default for agents
	// using this profile. It wins over the profile's runtime entry and
	// loses to a template's or agent's kubernetes.safeToEvict. Only false
	// has an effect. See ResolveSafeToEvict.
	SafeToEvict *bool `json:"safe_to_evict,omitempty" yaml:"safe_to_evict,omitempty" koanf:"safe_to_evict"`
	// SharedDirStorageBackend overrides server.shared_dir_storage.backend
	// ("local" or "nfs") for agents using this profile. It wins over the
	// same key on the profile's runtime entry. The nfs details always come
	// from server.shared_dir_storage.nfs. Read from global settings only;
	// see ResolveSharedDirStorage.
	SharedDirStorageBackend string `json:"shared_dir_storage_backend,omitempty" yaml:"shared_dir_storage_backend,omitempty" koanf:"shared_dir_storage_backend"`
	// KubernetesServiceAccountMappings overrides, per GSA email, the
	// runtime-level mapping of the same name for agents created under this
	// profile. See V1RuntimeConfig.KubernetesServiceAccountMappings and
	// VersionedSettings.ResolveKubernetesServiceAccountMapping for the
	// precedence and full contract.
	KubernetesServiceAccountMappings map[string]string `json:"kubernetes_service_account_mappings,omitempty" yaml:"kubernetes_service_account_mappings,omitempty" koanf:"kubernetes_service_account_mappings"`
}

// resolveEffectiveProjectPath resolves the effective project path for settings loading.
// Shared by both LoadSettingsKoanf and LoadVersionedSettings.
// For git projects with split storage, this redirects to the external config dir
// so that settings are loaded from ~/.scion/project-configs/<slug>__<uuid>/.scion/.
func resolveEffectiveProjectPath(projectPath string) string {
	effectiveProjectPath := projectPath
	switch effectiveProjectPath {
	case "":
		if projectPath, ok := FindProjectRoot(); ok {
			effectiveProjectPath = projectPath
		}
	case "global", "home":
		effectiveProjectPath = ""
	}
	if effectiveProjectPath != "" {
		effectiveProjectPath = GetProjectConfigDir(effectiveProjectPath)
	}
	return effectiveProjectPath
}

// LoadVersionedSettings loads settings using Koanf into VersionedSettings.
// Provider priority:
// 1. Embedded defaults (YAML) with OS-specific runtime adjustment
// 2. Global settings file (~/.scion/settings.yaml or .json)
// 3. In-repo project settings file (.scion/settings.yaml or .json)
// 4. External project config settings (for git projects with split storage)
// 5. Environment variables (SCION_ prefix)
func LoadVersionedSettings(projectPath string) (*VersionedSettings, error) {
	k := koanf.New(".")

	// 1. Load embedded defaults (YAML)
	if defaultData, err := GetDefaultSettingsDataYAML(); err == nil {
		_ = k.Load(rawbytes.Provider(defaultData), yaml.Parser())
	}

	// 2. Load global settings (~/.scion/settings.yaml or .json)
	globalDir, _ := GetGlobalDir()
	var globalMigratedHub bool
	if globalDir != "" {
		var err error
		globalMigratedHub, err = loadSettingsFile(k, globalDir)
		if err != nil {
			return nil, err
		}
	}
	// Captured once, right after the global layer loads — see
	// LoadSettingsKoanf's identical comment for why both step 3 and step 4
	// compare against this same value, and why a migrated global value is
	// not compared against at all.
	globalHubProjectID := k.String(projectkeys.ConfigHubProjectIDKey)
	if globalMigratedHub {
		globalHubProjectID = ""
	}

	// 3. Load in-repo project settings (.scion/settings.yaml)
	effectiveProjectPath := resolveEffectiveProjectPath(projectPath)
	if projectPath != "" && projectPath != globalDir {
		migratedHub, err := loadSettingsFile(k, projectPath)
		if err != nil {
			return nil, err
		}
		if migratedHub {
			logHubProjectIDPrecedenceChange(k, projectPath, globalHubProjectID)
		}
		warnIfInRepoHasGlobalKeys(projectPath, effectiveProjectPath)
	}

	// 4. Load external project config settings (overrides in-repo for split
	// storage), and where a plain project's settings.yaml is actually loaded
	// when projectPath is "" (cwd-resolved) — see LoadSettingsKoanf's step 4.
	if effectiveProjectPath != "" && effectiveProjectPath != globalDir && effectiveProjectPath != projectPath {
		migratedHub, err := loadSettingsFile(k, effectiveProjectPath)
		if err != nil {
			return nil, err
		}
		if migratedHub {
			logHubProjectIDPrecedenceChange(k, effectiveProjectPath, globalHubProjectID)
		}
	}

	// 4. Load environment variables (SCION_ prefix)
	_ = k.Load(env.ProviderWithValue("SCION_", ".", func(key, value string) (string, interface{}) {
		// An empty plaintext switch is unset, not a request to disable TLS.
		// Skip it before koanf maps the empty value onto tls.enabled.
		if key == "SCION_OTEL_INSECURE" && value == "" {
			return "", nil
		}
		return versionedEnvKeyMapper(key), value
	}), nil)
	// SCION_OTEL_INSECURE is a plaintext switch. Its value is the inverse of
	// telemetry.cloud.tls.enabled, so a key-only mapper cannot apply it.
	if raw, present := os.LookupEnv("SCION_OTEL_INSECURE"); present && raw != "" {
		plaintext, err := strconv.ParseBool(raw)
		if err != nil {
			return nil, fmt.Errorf("SCION_OTEL_INSECURE: %w", err)
		}
		_ = k.Load(confmap.Provider(map[string]interface{}{"telemetry.cloud.tls.enabled": !plaintext}, "."), nil)
	}

	// For git projects, the project_id is stored in a project-id file inside the
	// .scion directory rather than in the settings file. Read it here so that
	// it overrides any hub.project_id inherited from global settings.
	if projectPath != "" {
		globalDir, _ := GetGlobalDir()
		if projectPath != globalDir {
			if projectID, err := ReadProjectID(projectPath); err == nil && projectID != "" {
				_ = k.Load(confmap.Provider(map[string]interface{}{
					projectkeys.ConfigHubProjectIDKey: projectID,
				}, "."), nil)
			}
		}
	}

	// Unmarshal into VersionedSettings struct
	settings := &VersionedSettings{
		Runtimes:       make(map[string]V1RuntimeConfig),
		HarnessConfigs: make(map[string]HarnessConfigEntry),
		Profiles:       make(map[string]V1ProfileConfig),
	}

	if err := k.Unmarshal("", settings); err != nil {
		return nil, err
	}
	if settings.Telemetry != nil && settings.Telemetry.Cloud != nil && settings.Telemetry.Cloud.TLS != nil {
		tls := settings.Telemetry.Cloud.TLS
		if tls.Enabled != nil && !*tls.Enabled && ((tls.InsecureSkipVerify != nil && *tls.InsecureSkipVerify) || tls.CAFile != "") {
			return nil, fmt.Errorf("telemetry.cloud.tls: plaintext conflicts with certificate verification settings")
		}
	}

	return settings, nil
}

// settingsExcludedEnvVars lists SCION_* variables that are never settings
// overrides: they are consumed directly by another subsystem, and their
// generic mapped key happens to collide with a struct-typed settings field.
// Both the versioned and legacy env key mappers drop them via this list so
// that koanf's Unmarshal never fails just because one of them is present in
// the process environment.
var settingsExcludedEnvVars = []string{
	"SCION_AUTO_EXPOSE_PORTS",
	"SCION_AUTO_EXPOSE_PORTS_LIST",
}

// isSettingsExcludedEnv reports whether name is in settingsExcludedEnvVars.
func isSettingsExcludedEnv(name string) bool {
	for _, e := range settingsExcludedEnvVars {
		if e == name {
			return true
		}
	}
	return false
}

// versionedEnvKeyMapper maps SCION_* environment variables to versioned settings keys.
// All keys are snake_case so no camelCase conversion is needed.
func versionedEnvKeyMapper(s string) string {
	if mapped, ok := projectkeys.EnvProjectIDConfigKey(s, false); ok {
		return mapped
	}
	if isSettingsExcludedEnv(s) {
		// SCION_AUTO_EXPOSE_PORTS and SCION_AUTO_EXPOSE_PORTS_LIST are
		// consumed directly by sciontool's auto-expose scanner
		// (pkg/sciontool/autoexpose), not read as settings overrides. Left
		// mapped, the bare key "auto_expose_ports" collides with the
		// struct-typed AutoExposePorts field and makes koanf's Unmarshal
		// fail outright whenever the process happens to have that variable
		// set (e.g. a broker started inside an agent container, which the
		// hub sets it in). Returning "" makes the env provider drop the
		// variable entirely, the same idiom used below for a removed
		// legacy env var and for SCION_OTEL_INSECURE's empty-value case.
		return ""
	}
	if isRemovedLegacyEnv(s) {
		// SCION_HUB_GROVE_ID is no longer read, not even via the generic
		// "hub_" mapping below, which would otherwise land on the
		// unrecognised key hub.grove_id. Returning "" makes the env
		// provider drop the variable entirely, the same idiom used for
		// SCION_OTEL_INSECURE above. WarnRemovedLegacyEnv reports it
		// separately.
		return ""
	}
	key := strings.ToLower(strings.TrimPrefix(s, "SCION_"))

	// Handle nested hub keys (single level: hub.endpoint, hub.project_id, etc.)
	if strings.HasPrefix(key, "hub_") {
		return "hub." + strings.TrimPrefix(key, "hub_")
	}
	// Handle nested cli keys (single level: cli.autohelp, cli.interactive_disabled)
	if strings.HasPrefix(key, "cli_") {
		return "cli." + strings.TrimPrefix(key, "cli_")
	}
	// Handle nested server keys — deep nesting requires mapping compound field names
	if strings.HasPrefix(key, "server_") {
		rest := strings.TrimPrefix(key, "server_")
		return "server." + mapServerEnvKey(rest)
	}
	// Handle nested telemetry keys — deep nesting with compound field names
	if strings.HasPrefix(key, "telemetry_") {
		rest := strings.TrimPrefix(key, "telemetry_")
		return "telemetry." + mapTelemetryEnvKey(rest)
	}
	// Handle SCION_OTEL_* → telemetry.cloud.* mappings
	if strings.HasPrefix(key, "otel_") {
		return mapOtelEnvKey(strings.TrimPrefix(key, "otel_"))
	}

	return key
}

// knownCompoundFields lists multi-word snake_case field names used in server config.
// These must be recognized as single fields rather than split into nested keys.
// IMPORTANT: Sorted longest-first so that "dev_token_file" matches before "dev_token".
var knownCompoundFields = []string{
	"start_create_unconfirmed_hold",
	"require_trusted_proxy_ip",
	"soft_delete_retain_files",
	"start_unconfirmed_hold",
	"start_claim_lease_ttl",
	"soft_delete_retention",
	"missing_agent_grace",
	"start_max_duration",
	"stalled_threshold",
	"authorized_domains",
	"platform_auth_sa",
	"interval_seconds",
	"public_key_file",
	"max_concurrency",
	"oidc_audience",
	"jwks_url",
	"jwks_file",
	"broker_nickname",
	"allowed_origins",
	"allowed_methods",
	"allowed_headers",
	"dev_token_file",
	"agent_endpoint",
	"gcp_project_id",
	"gcp_credentials",
	"client_secret",
	"write_timeout",
	"read_timeout",
	"display_name",
	"broker_token",
	"admin_emails",
	"hub_endpoint",
	"container_hub_endpoint",
	"broker_name",
	"public_url",
	"local_path",
	"log_format",
	"broker_id",
	"client_id",
	"dev_token",
	"log_level",
	"dev_mode",
	"max_age",
}

// mapServerEnvKey maps the portion after "server_" to a dotted path, recognizing
// known multi-word snake_case fields so they are not incorrectly split.
// For example: "hub_read_timeout" -> "hub.read_timeout"
//
//	"broker_broker_id" -> "broker.broker_id"
//	"oauth_web_google_client_id" -> "oauth.web.google.client_id"
func mapServerEnvKey(key string) string {
	return mapEnvKeyRecursive(key)
}

// mapEnvKeyRecursive splits an env key fragment into dotted segments, recognizing
// compound field names at each position.
func mapEnvKeyRecursive(key string) string {
	if key == "" {
		return ""
	}

	// Check if the entire remaining key is a known compound field
	for _, compound := range knownCompoundFields {
		if key == compound {
			return key
		}
	}

	// Try to match the longest known compound field at the end,
	// treating the prefix as nesting segments.
	// We scan for underscores and try to split at each one.
	for i := 0; i < len(key); i++ {
		if key[i] == '_' {
			prefix := key[:i]
			rest := key[i+1:]

			// Check if the rest starts with a known compound field
			matched := false
			for _, compound := range knownCompoundFields {
				if strings.HasPrefix(rest, compound) {
					if len(rest) == len(compound) {
						// Exact match for the rest
						return prefix + "." + compound
					}
					if rest[len(compound)] == '_' {
						// Compound field followed by more segments
						return prefix + "." + compound + "." + mapEnvKeyRecursive(rest[len(compound)+1:])
					}
				}
			}

			if !matched {
				// The prefix could be a section name (hub, broker, database, auth, oauth, storage, secrets, cors)
				// Try recursively
				subResult := mapEnvKeyRecursive(rest)
				if subResult != rest || isSectionName(prefix) {
					return prefix + "." + subResult
				}
			}
		}
	}

	// No compound match found — the key is a simple single-word field
	return key
}

// isSectionName checks if a name is a known section in the server config hierarchy.
func isSectionName(name string) bool {
	switch name {
	case "hub", "broker", "database", "auth", "oauth", "storage", "secrets", "cors",
		"web", "cli", "device", "google", "github", "proxy", "iap", "jwt", "claims",
		"transport", "scheduler":
		return true
	}
	return false
}

// knownTelemetryCompoundFields lists multi-word snake_case field names used in telemetry config.
// Sorted longest-first so longer matches take priority.
var knownTelemetryCompoundFields = []string{
	"insecure_skip_verify",
	"respect_debug_mode",
	"report_interval",
	"cloud_logging",
	"max_size",
}

// telemetrySectionNames are sub-sections within the telemetry config tree.
var telemetrySectionNames = map[string]bool{
	"cloud": true, "hub": true, "local": true, "filter": true,
	"tls": true, "batch": true, "events": true, "attributes": true, "sampling": true,
}

// mapTelemetryEnvKey maps the portion after "telemetry_" to a dotted path.
// Examples:
//
//	"enabled" -> "enabled"
//	"cloud_enabled" -> "cloud.enabled"
//	"cloud_tls_insecure_skip_verify" -> "cloud.tls.insecure_skip_verify"
//	"filter_respect_debug_mode" -> "filter.respect_debug_mode"
//	"hub_report_interval" -> "hub.report_interval"
func mapTelemetryEnvKey(key string) string {
	if key == "" {
		return ""
	}

	// Check if the entire key is a known compound field
	for _, compound := range knownTelemetryCompoundFields {
		if key == compound {
			return key
		}
	}

	// Try splitting at underscores
	for i := 0; i < len(key); i++ {
		if key[i] == '_' {
			prefix := key[:i]
			rest := key[i+1:]

			// Check if the rest starts with a known compound field
			for _, compound := range knownTelemetryCompoundFields {
				if strings.HasPrefix(rest, compound) {
					if len(rest) == len(compound) {
						return prefix + "." + compound
					}
					if rest[len(compound)] == '_' {
						return prefix + "." + compound + "." + mapTelemetryEnvKey(rest[len(compound)+1:])
					}
				}
			}

			// If prefix is a known section, recurse on the rest
			if telemetrySectionNames[prefix] {
				return prefix + "." + mapTelemetryEnvKey(rest)
			}
		}
	}

	return key
}

// mapOtelEnvKey maps OTEL_* env keys to telemetry.cloud.* settings paths.
// Examples:
//
//	"endpoint" -> "telemetry.cloud.endpoint"
//	"protocol" -> "telemetry.cloud.protocol"
//	"headers" -> "telemetry.cloud.headers"
//	"insecure" -> "telemetry.cloud.tls.enabled" (value inverted after env load)
//	"skip_tls_verify" -> "telemetry.cloud.tls.insecure_skip_verify"
//	"ca_file" -> "telemetry.cloud.tls.ca_file"
func mapOtelEnvKey(key string) string {
	switch key {
	case "endpoint":
		return "telemetry.cloud.endpoint"
	case "protocol":
		return "telemetry.cloud.protocol"
	case "headers":
		return "telemetry.cloud.headers"
	case "insecure":
		return "telemetry.cloud.tls.enabled"
	case "skip_tls_verify":
		return "telemetry.cloud.tls.insecure_skip_verify"
	case "ca_file":
		return "telemetry.cloud.tls.ca_file"
	default:
		return "telemetry.cloud." + key
	}
}

// ConvertV1ServerToGlobalConfig maps a V1ServerConfig (snake_case) to a GlobalConfig (camelCase).
// This allows server/broker commands to continue operating on GlobalConfig internally
// while loading from the versioned settings.yaml format.
func ConvertV1ServerToGlobalConfig(v1 *V1ServerConfig) *GlobalConfig {
	if v1 == nil {
		gc := DefaultGlobalConfig()
		return &gc
	}

	gc := DefaultGlobalConfig()

	// Mode
	if v1.Mode != "" {
		gc.Mode = v1.Mode
	}

	// Top-level fields
	if v1.LogLevel != "" {
		gc.LogLevel = v1.LogLevel
	}
	if v1.LogFormat != "" {
		gc.LogFormat = v1.LogFormat
	}

	// Hub server config
	if v1.Hub != nil {
		if v1.Hub.Port != 0 {
			gc.Hub.Port = v1.Hub.Port
		}
		if v1.Hub.Host != "" {
			gc.Hub.Host = v1.Hub.Host
		}
		if v1.Hub.HubID != "" {
			gc.Hub.HubID = v1.Hub.HubID
		}
		if v1.Hub.HubName != "" {
			gc.Hub.HubName = v1.Hub.HubName
		}
		if v1.Hub.PublicURL != "" {
			gc.Hub.Endpoint = v1.Hub.PublicURL
		}
		if v1.Hub.AgentEndpoint != "" {
			gc.Hub.AgentEndpoint = v1.Hub.AgentEndpoint
		}
		if v1.Hub.ReadTimeout != "" {
			if d, err := time.ParseDuration(v1.Hub.ReadTimeout); err == nil {
				gc.Hub.ReadTimeout = d
			}
		}
		if v1.Hub.WriteTimeout != "" {
			if d, err := time.ParseDuration(v1.Hub.WriteTimeout); err == nil {
				gc.Hub.WriteTimeout = d
			}
		}
		if v1.Hub.CORS != nil {
			gc.Hub.CORSEnabled = v1.Hub.CORS.Enabled
			if v1.Hub.CORS.AllowedOrigins != nil {
				gc.Hub.CORSAllowedOrigins = v1.Hub.CORS.AllowedOrigins
			}
			if v1.Hub.CORS.AllowedMethods != nil {
				gc.Hub.CORSAllowedMethods = v1.Hub.CORS.AllowedMethods
			}
			if v1.Hub.CORS.AllowedHeaders != nil {
				gc.Hub.CORSAllowedHeaders = v1.Hub.CORS.AllowedHeaders
			}
			if v1.Hub.CORS.MaxAge != 0 {
				gc.Hub.CORSMaxAge = v1.Hub.CORS.MaxAge
			}
		}
		if v1.Hub.AdminEmails != nil {
			gc.Hub.AdminEmails = v1.Hub.AdminEmails
		}
		if v1.Hub.SoftDeleteRetention != "" {
			if d, err := time.ParseDuration(v1.Hub.SoftDeleteRetention); err == nil {
				gc.Hub.SoftDeleteRetention = d
			}
		}
		if v1.Hub.SoftDeleteRetainFiles != nil {
			gc.Hub.SoftDeleteRetainFiles = *v1.Hub.SoftDeleteRetainFiles
		}
		if v1.Hub.GCPIAMCheckMode != "" {
			gc.Hub.GCPIAMCheckMode = v1.Hub.GCPIAMCheckMode
		}
		if v1.Hub.GCPIAMDenyUnknownPolicy != "" {
			gc.Hub.GCPIAMDenyUnknownPolicy = v1.Hub.GCPIAMDenyUnknownPolicy
		}
		if v1.Hub.AutoSuspendStalled != nil {
			gc.Hub.AutoSuspendStalled = *v1.Hub.AutoSuspendStalled
		}
		if v1.Hub.StalledThreshold != "" {
			if d, err := time.ParseDuration(v1.Hub.StalledThreshold); err == nil {
				gc.Hub.StalledThreshold = d
			}
		}
		if v1.Hub.AsyncAgentLaunch != nil {
			gc.Hub.AsyncAgentLaunch = *v1.Hub.AsyncAgentLaunch
		}
		if v1.Hub.LaunchTimeout != "" {
			if d, err := time.ParseDuration(v1.Hub.LaunchTimeout); err == nil {
				gc.Hub.LaunchTimeout = d
			}
		}
		if v1.Hub.LaunchKeepaliveSeconds != nil {
			gc.Hub.LaunchKeepaliveSeconds = *v1.Hub.LaunchKeepaliveSeconds
		}
		if v1.Hub.MissingAgentGrace != "" {
			if d, err := time.ParseDuration(v1.Hub.MissingAgentGrace); err == nil {
				gc.Hub.MissingAgentGrace = d
			}
		}
		for _, f := range []struct {
			v   string
			dst *time.Duration
		}{
			{v1.Hub.StartClaimLeaseTTL, &gc.Hub.StartClaimLeaseTTL},
			{v1.Hub.StartMaxDuration, &gc.Hub.StartMaxDuration},
			{v1.Hub.StartUnconfirmedHold, &gc.Hub.StartUnconfirmedHold},
			{v1.Hub.StartCreateUnconfirmedHold, &gc.Hub.StartCreateUnconfirmedHold},
		} {
			if f.v == "" {
				continue
			}
			if d, err := time.ParseDuration(f.v); err == nil {
				*f.dst = d
			}
		}
		if v1.Hub.DisableLegacyStorageFallback != nil {
			gc.Hub.DisableLegacyStorageFallback = *v1.Hub.DisableLegacyStorageFallback
		}
	}

	// Broker config
	if v1.Broker != nil {
		gc.RuntimeBroker.Enabled = v1.Broker.Enabled
		if v1.Broker.Port != 0 {
			gc.RuntimeBroker.Port = v1.Broker.Port
		}
		if v1.Broker.Host != "" {
			gc.RuntimeBroker.Host = v1.Broker.Host
		}
		if v1.Broker.ReadTimeout != "" {
			if d, err := time.ParseDuration(v1.Broker.ReadTimeout); err == nil {
				gc.RuntimeBroker.ReadTimeout = d
			}
		}
		if v1.Broker.WriteTimeout != "" {
			if d, err := time.ParseDuration(v1.Broker.WriteTimeout); err == nil {
				gc.RuntimeBroker.WriteTimeout = d
			}
		}
		if v1.Broker.HubEndpoint != "" {
			gc.RuntimeBroker.HubEndpoint = v1.Broker.HubEndpoint
		}
		if v1.Broker.ContainerHubEndpoint != "" {
			gc.RuntimeBroker.ContainerHubEndpoint = v1.Broker.ContainerHubEndpoint
		}
		if v1.Broker.BrokerID != "" {
			gc.RuntimeBroker.BrokerID = v1.Broker.BrokerID
		}
		// Map BrokerName and BrokerNickname to BrokerName in GlobalConfig
		if v1.Broker.BrokerName != "" {
			gc.RuntimeBroker.BrokerName = v1.Broker.BrokerName
		} else if v1.Broker.BrokerNickname != "" {
			gc.RuntimeBroker.BrokerName = v1.Broker.BrokerNickname
		}
		if v1.Broker.CORS != nil {
			gc.RuntimeBroker.CORSEnabled = v1.Broker.CORS.Enabled
			if v1.Broker.CORS.AllowedOrigins != nil {
				gc.RuntimeBroker.CORSAllowedOrigins = v1.Broker.CORS.AllowedOrigins
			}
			if v1.Broker.CORS.AllowedMethods != nil {
				gc.RuntimeBroker.CORSAllowedMethods = v1.Broker.CORS.AllowedMethods
			}
			if v1.Broker.CORS.AllowedHeaders != nil {
				gc.RuntimeBroker.CORSAllowedHeaders = v1.Broker.CORS.AllowedHeaders
			}
			if v1.Broker.CORS.MaxAge != 0 {
				gc.RuntimeBroker.CORSMaxAge = v1.Broker.CORS.MaxAge
			}
		}
		if v1.Broker.AllowContainerScriptHarnesses != nil {
			gc.RuntimeBroker.AllowContainerScriptHarnesses = *v1.Broker.AllowContainerScriptHarnesses
		} else {
			gc.RuntimeBroker.AllowContainerScriptHarnesses = true
		}
	}

	// Database config
	if v1.Database != nil {
		if v1.Database.Driver != "" {
			gc.Database.Driver = v1.Database.Driver
		}
		if v1.Database.URL != "" {
			gc.Database.URL = v1.Database.URL
		}
		if v1.Database.MaxOpenConns != 0 {
			gc.Database.MaxOpenConns = v1.Database.MaxOpenConns
		}
		if v1.Database.MaxIdleConns != 0 {
			gc.Database.MaxIdleConns = v1.Database.MaxIdleConns
		}
		if v1.Database.ConnMaxLifetime != "" {
			gc.Database.ConnMaxLifetime = v1.Database.ConnMaxLifetime
		}
		if v1.Database.ConnMaxIdleTime != "" {
			gc.Database.ConnMaxIdleTime = v1.Database.ConnMaxIdleTime
		}
	}

	// Auth config
	if v1.Auth != nil {
		if v1.Auth.Mode != "" {
			gc.Auth.Mode = v1.Auth.Mode
		}
		gc.Auth.Enabled = v1.Auth.DevMode
		gc.Auth.Token = v1.Auth.DevToken
		gc.Auth.TokenFile = v1.Auth.DevTokenFile
		if v1.Auth.AuthorizedDomains != nil {
			gc.Auth.AuthorizedDomains = v1.Auth.AuthorizedDomains
		}
		if v1.Auth.UserAccessMode != "" {
			gc.Auth.UserAccessMode = v1.Auth.UserAccessMode
		}
		if v1.Auth.DefaultUserRole != "" {
			gc.Auth.DefaultUserRole = v1.Auth.DefaultUserRole
		}
		if v1.Auth.Proxy != nil {
			gc.Auth.Proxy = &ProxyAuthConfig{
				Provider:              v1.Auth.Proxy.Provider,
				RequireTrustedProxyIP: v1.Auth.Proxy.RequireTrustedProxyIP,
			}
			if v1.Auth.Proxy.IAP != nil {
				gc.Auth.Proxy.IAP = &IAPAuthConfig{
					Audience: v1.Auth.Proxy.IAP.Audience,
					Issuer:   v1.Auth.Proxy.IAP.Issuer,
					JWKSURL:  v1.Auth.Proxy.IAP.JWKSURL,
				}
			}
			if v1.Auth.Proxy.JWT != nil {
				gc.Auth.Proxy.JWT = &JWTAuthConfig{
					Header:        v1.Auth.Proxy.JWT.Header,
					Algorithm:     v1.Auth.Proxy.JWT.Algorithm,
					Issuer:        v1.Auth.Proxy.JWT.Issuer,
					Audience:      v1.Auth.Proxy.JWT.Audience,
					JWKSURL:       v1.Auth.Proxy.JWT.JWKSURL,
					JWKSFile:      v1.Auth.Proxy.JWT.JWKSFile,
					PublicKeyFile: v1.Auth.Proxy.JWT.PublicKeyFile,
				}
				if v1.Auth.Proxy.JWT.Claims != nil {
					gc.Auth.Proxy.JWT.Claims = &JWTClaimsConfig{
						Email:       v1.Auth.Proxy.JWT.Claims.Email,
						Subject:     v1.Auth.Proxy.JWT.Claims.Subject,
						DisplayName: v1.Auth.Proxy.JWT.Claims.DisplayName,
						Domain:      v1.Auth.Proxy.JWT.Claims.Domain,
					}
				}
			}
		}
		if v1.Auth.Transport != nil {
			gc.Auth.Transport = &TransportAuthConfig{
				Mode:           v1.Auth.Transport.Mode,
				OIDCAudience:   v1.Auth.Transport.OIDCAudience,
				PlatformAuthSA: v1.Auth.Transport.PlatformAuthSA,
			}
		}
		if v1.Auth.Username != "" {
			gc.Auth.Username = v1.Auth.Username
		}
		if v1.Auth.DisplayName != "" {
			gc.Auth.DisplayName = v1.Auth.DisplayName
		}
		if v1.Auth.Email != "" {
			gc.Auth.Email = v1.Auth.Email
		}
	}

	// OAuth config
	if v1.OAuth != nil {
		if v1.OAuth.Web != nil {
			if v1.OAuth.Web.Google != nil {
				gc.OAuth.Web.Google.ClientID = v1.OAuth.Web.Google.ClientID
				gc.OAuth.Web.Google.ClientSecret = v1.OAuth.Web.Google.ClientSecret
			}
			if v1.OAuth.Web.GitHub != nil {
				gc.OAuth.Web.GitHub.ClientID = v1.OAuth.Web.GitHub.ClientID
				gc.OAuth.Web.GitHub.ClientSecret = v1.OAuth.Web.GitHub.ClientSecret
			}
		}
		if v1.OAuth.CLI != nil {
			if v1.OAuth.CLI.Google != nil {
				gc.OAuth.CLI.Google.ClientID = v1.OAuth.CLI.Google.ClientID
				gc.OAuth.CLI.Google.ClientSecret = v1.OAuth.CLI.Google.ClientSecret
			}
			if v1.OAuth.CLI.GitHub != nil {
				gc.OAuth.CLI.GitHub.ClientID = v1.OAuth.CLI.GitHub.ClientID
				gc.OAuth.CLI.GitHub.ClientSecret = v1.OAuth.CLI.GitHub.ClientSecret
			}
		}
		if v1.OAuth.Device != nil {
			if v1.OAuth.Device.Google != nil {
				gc.OAuth.Device.Google.ClientID = v1.OAuth.Device.Google.ClientID
				gc.OAuth.Device.Google.ClientSecret = v1.OAuth.Device.Google.ClientSecret
			}
			if v1.OAuth.Device.GitHub != nil {
				gc.OAuth.Device.GitHub.ClientID = v1.OAuth.Device.GitHub.ClientID
				gc.OAuth.Device.GitHub.ClientSecret = v1.OAuth.Device.GitHub.ClientSecret
			}
		}
	}

	// Storage config
	if v1.Storage != nil {
		if v1.Storage.Provider != "" {
			gc.Storage.Provider = v1.Storage.Provider
		}
		if v1.Storage.Bucket != "" {
			gc.Storage.Bucket = v1.Storage.Bucket
		}
		if v1.Storage.LocalPath != "" {
			gc.Storage.LocalPath = v1.Storage.LocalPath
		}
	}

	// Secrets config
	if v1.Secrets != nil {
		if v1.Secrets.Backend != "" {
			gc.Secrets.Backend = v1.Secrets.Backend
		}
		if v1.Secrets.GCPProjectID != "" {
			gc.Secrets.GCPProjectID = v1.Secrets.GCPProjectID
		}
		if v1.Secrets.GCPCredentials != "" {
			gc.Secrets.GCPCredentials = v1.Secrets.GCPCredentials
		}
		if len(v1.Secrets.GCPReplicationLocations) > 0 {
			gc.Secrets.GCPReplicationLocations = v1.Secrets.GCPReplicationLocations
		}
	}

	// Workspace storage — thread into GlobalConfig so the hub can read it.
	if v1.WorkspaceStorage != nil {
		v1.WorkspaceStorage.ApplyNFSDefaults()
		gc.WorkspaceStorage = v1.WorkspaceStorage
	}

	// Native chat — thread into GlobalConfig so the hub can gate chat routes.
	if v1.NativeChat != nil {
		gc.NativeChat = v1.NativeChat
	}

	// GitHub App
	if v1.GitHubApp != nil {
		gc.GitHubApp.AppID = v1.GitHubApp.AppID
		gc.GitHubApp.PrivateKeyPath = v1.GitHubApp.PrivateKeyPath
		gc.GitHubApp.PrivateKey = v1.GitHubApp.PrivateKey
		gc.GitHubApp.WebhookSecret = v1.GitHubApp.WebhookSecret
		gc.GitHubApp.APIBaseURL = v1.GitHubApp.APIBaseURL
		gc.GitHubApp.WebhooksEnabled = v1.GitHubApp.WebhooksEnabled
		gc.GitHubApp.InstallationURL = v1.GitHubApp.InstallationURL
	}

	// Scheduler
	if v1.Scheduler != nil {
		gc.Scheduler.IntervalSeconds = v1.Scheduler.IntervalSeconds
		gc.Scheduler.MaxConcurrency = v1.Scheduler.MaxConcurrency // both are *int; nil propagates
	}

	// OIDC Identity Provider
	if v1.OIDC != nil {
		gc.OIDC = *v1.OIDC
	}

	// OIDC Login (external OIDC provider for web login)
	if v1.OIDCLogin != nil {
		if v1.OIDCLogin.Enabled != nil {
			gc.OIDCLogin.Enabled = *v1.OIDCLogin.Enabled
		}
		gc.OIDCLogin.DisplayName = v1.OIDCLogin.DisplayName
		gc.OIDCLogin.IssuerURL = v1.OIDCLogin.IssuerURL
		gc.OIDCLogin.ClientID = v1.OIDCLogin.ClientID
		gc.OIDCLogin.ClientSecret = v1.OIDCLogin.ClientSecret
		if len(v1.OIDCLogin.Scopes) > 0 {
			gc.OIDCLogin.Scopes = v1.OIDCLogin.Scopes
		}
	}

	// Federation
	if v1.Federation != nil {
		if v1.Federation.Enabled != nil {
			gc.Federation.Enabled = *v1.Federation.Enabled
		}
		for _, vi := range v1.Federation.TrustedIssuers {
			gc.Federation.TrustedIssuers = append(gc.Federation.TrustedIssuers, TrustedIssuerConfig(vi))
		}
		gc.Federation.Algorithms = v1.Federation.Algorithms
		if v1.Federation.RefreshInterval != "" {
			if d, err := time.ParseDuration(v1.Federation.RefreshInterval); err == nil {
				gc.Federation.Cache.RefreshInterval = d
			}
		}
		if v1.Federation.DebounceInterval != "" {
			if d, err := time.ParseDuration(v1.Federation.DebounceInterval); err == nil {
				gc.Federation.Cache.DebounceInterval = d
			}
		}
	}

	return &gc
}

// ConvertGlobalToV1ServerConfig maps a GlobalConfig (camelCase) to a V1ServerConfig (snake_case).
// Used by AdaptLegacySettings and migration tooling.
func ConvertGlobalToV1ServerConfig(gc *GlobalConfig) *V1ServerConfig {
	if gc == nil {
		return &V1ServerConfig{}
	}

	v1 := &V1ServerConfig{
		Mode:      gc.Mode,
		LogLevel:  gc.LogLevel,
		LogFormat: gc.LogFormat,
	}

	// Hub server config
	v1Hub := &V1ServerHubConfig{
		Port:          gc.Hub.Port,
		Host:          gc.Hub.Host,
		HubID:         gc.Hub.HubID,
		HubName:       gc.Hub.HubName,
		PublicURL:     gc.Hub.Endpoint,
		AgentEndpoint: gc.Hub.AgentEndpoint,
		ReadTimeout:   gc.Hub.ReadTimeout.String(),
		WriteTimeout:  gc.Hub.WriteTimeout.String(),
		AdminEmails:   gc.Hub.AdminEmails,
		CORS: &V1CORSConfig{
			Enabled:        gc.Hub.CORSEnabled,
			AllowedOrigins: gc.Hub.CORSAllowedOrigins,
			AllowedMethods: gc.Hub.CORSAllowedMethods,
			AllowedHeaders: gc.Hub.CORSAllowedHeaders,
			MaxAge:         gc.Hub.CORSMaxAge,
		},
	}
	if gc.Hub.SoftDeleteRetention > 0 {
		v1Hub.SoftDeleteRetention = gc.Hub.SoftDeleteRetention.String()
	}
	if gc.Hub.StalledThreshold > 0 {
		v1Hub.StalledThreshold = gc.Hub.StalledThreshold.String()
	}
	if gc.Hub.MissingAgentGrace > 0 {
		v1Hub.MissingAgentGrace = gc.Hub.MissingAgentGrace.String()
	}
	if gc.Hub.StartClaimLeaseTTL > 0 {
		v1Hub.StartClaimLeaseTTL = gc.Hub.StartClaimLeaseTTL.String()
	}
	if gc.Hub.StartMaxDuration > 0 {
		v1Hub.StartMaxDuration = gc.Hub.StartMaxDuration.String()
	}
	if gc.Hub.StartUnconfirmedHold > 0 {
		v1Hub.StartUnconfirmedHold = gc.Hub.StartUnconfirmedHold.String()
	}
	if gc.Hub.StartCreateUnconfirmedHold > 0 {
		v1Hub.StartCreateUnconfirmedHold = gc.Hub.StartCreateUnconfirmedHold.String()
	}
	if gc.Hub.SoftDeleteRetainFiles {
		retainFiles := true
		v1Hub.SoftDeleteRetainFiles = &retainFiles
	}
	if gc.Hub.DisableLegacyStorageFallback {
		disableLegacy := true
		v1Hub.DisableLegacyStorageFallback = &disableLegacy
	}
	if gc.Hub.AsyncAgentLaunch {
		asyncLaunch := true
		v1Hub.AsyncAgentLaunch = &asyncLaunch
	}
	if gc.Hub.LaunchTimeout > 0 {
		v1Hub.LaunchTimeout = gc.Hub.LaunchTimeout.String()
	}
	if gc.Hub.LaunchKeepaliveSeconds > 0 {
		keepalive := gc.Hub.LaunchKeepaliveSeconds
		v1Hub.LaunchKeepaliveSeconds = &keepalive
	}
	v1.Hub = v1Hub

	// Broker config
	v1.Broker = &V1BrokerConfig{
		Enabled:                       gc.RuntimeBroker.Enabled,
		Port:                          gc.RuntimeBroker.Port,
		Host:                          gc.RuntimeBroker.Host,
		ReadTimeout:                   gc.RuntimeBroker.ReadTimeout.String(),
		WriteTimeout:                  gc.RuntimeBroker.WriteTimeout.String(),
		HubEndpoint:                   gc.RuntimeBroker.HubEndpoint,
		ContainerHubEndpoint:          gc.RuntimeBroker.ContainerHubEndpoint,
		BrokerID:                      gc.RuntimeBroker.BrokerID,
		BrokerName:                    gc.RuntimeBroker.BrokerName,
		AllowContainerScriptHarnesses: &gc.RuntimeBroker.AllowContainerScriptHarnesses,
		CORS: &V1CORSConfig{
			Enabled:        gc.RuntimeBroker.CORSEnabled,
			AllowedOrigins: gc.RuntimeBroker.CORSAllowedOrigins,
			AllowedMethods: gc.RuntimeBroker.CORSAllowedMethods,
			AllowedHeaders: gc.RuntimeBroker.CORSAllowedHeaders,
			MaxAge:         gc.RuntimeBroker.CORSMaxAge,
		},
	}

	// Database config
	v1.Database = &V1DatabaseConfig{
		Driver:          gc.Database.Driver,
		URL:             gc.Database.URL,
		MaxOpenConns:    gc.Database.MaxOpenConns,
		MaxIdleConns:    gc.Database.MaxIdleConns,
		ConnMaxLifetime: gc.Database.ConnMaxLifetime,
		ConnMaxIdleTime: gc.Database.ConnMaxIdleTime,
	}

	// Auth config
	v1.Auth = &V1AuthConfig{
		Mode:              gc.Auth.Mode,
		DevMode:           gc.Auth.Enabled,
		DevToken:          gc.Auth.Token,
		DevTokenFile:      gc.Auth.TokenFile,
		AuthorizedDomains: gc.Auth.AuthorizedDomains,
		UserAccessMode:    gc.Auth.UserAccessMode,
		DefaultUserRole:   gc.Auth.DefaultUserRole,
		Username:          gc.Auth.Username,
		DisplayName:       gc.Auth.DisplayName,
		Email:             gc.Auth.Email,
	}
	if gc.Auth.Proxy != nil {
		v1.Auth.Proxy = &V1ProxyConfig{
			Provider:              gc.Auth.Proxy.Provider,
			RequireTrustedProxyIP: gc.Auth.Proxy.RequireTrustedProxyIP,
		}
		if gc.Auth.Proxy.IAP != nil {
			v1.Auth.Proxy.IAP = &V1IAPConfig{
				Audience: gc.Auth.Proxy.IAP.Audience,
				Issuer:   gc.Auth.Proxy.IAP.Issuer,
				JWKSURL:  gc.Auth.Proxy.IAP.JWKSURL,
			}
		}
		if gc.Auth.Proxy.JWT != nil {
			v1.Auth.Proxy.JWT = &V1JWTAuthConfig{
				Header:        gc.Auth.Proxy.JWT.Header,
				Algorithm:     gc.Auth.Proxy.JWT.Algorithm,
				Issuer:        gc.Auth.Proxy.JWT.Issuer,
				Audience:      gc.Auth.Proxy.JWT.Audience,
				JWKSURL:       gc.Auth.Proxy.JWT.JWKSURL,
				JWKSFile:      gc.Auth.Proxy.JWT.JWKSFile,
				PublicKeyFile: gc.Auth.Proxy.JWT.PublicKeyFile,
			}
			if gc.Auth.Proxy.JWT.Claims != nil {
				v1.Auth.Proxy.JWT.Claims = &V1JWTClaimsConfig{
					Email:       gc.Auth.Proxy.JWT.Claims.Email,
					Subject:     gc.Auth.Proxy.JWT.Claims.Subject,
					DisplayName: gc.Auth.Proxy.JWT.Claims.DisplayName,
					Domain:      gc.Auth.Proxy.JWT.Claims.Domain,
				}
			}
		}
	}
	if gc.Auth.Transport != nil {
		v1.Auth.Transport = &V1TransportConfig{
			Mode:           gc.Auth.Transport.Mode,
			OIDCAudience:   gc.Auth.Transport.OIDCAudience,
			PlatformAuthSA: gc.Auth.Transport.PlatformAuthSA,
		}
	}

	// OAuth config
	v1.OAuth = &V1OAuthConfig{
		Web: &V1OAuthClientConfig{
			Google: &V1OAuthProviderConfig{ClientID: gc.OAuth.Web.Google.ClientID, ClientSecret: gc.OAuth.Web.Google.ClientSecret},
			GitHub: &V1OAuthProviderConfig{ClientID: gc.OAuth.Web.GitHub.ClientID, ClientSecret: gc.OAuth.Web.GitHub.ClientSecret},
		},
		CLI: &V1OAuthClientConfig{
			Google: &V1OAuthProviderConfig{ClientID: gc.OAuth.CLI.Google.ClientID, ClientSecret: gc.OAuth.CLI.Google.ClientSecret},
			GitHub: &V1OAuthProviderConfig{ClientID: gc.OAuth.CLI.GitHub.ClientID, ClientSecret: gc.OAuth.CLI.GitHub.ClientSecret},
		},
		Device: &V1OAuthClientConfig{
			Google: &V1OAuthProviderConfig{ClientID: gc.OAuth.Device.Google.ClientID, ClientSecret: gc.OAuth.Device.Google.ClientSecret},
			GitHub: &V1OAuthProviderConfig{ClientID: gc.OAuth.Device.GitHub.ClientID, ClientSecret: gc.OAuth.Device.GitHub.ClientSecret},
		},
	}

	// Storage config
	v1.Storage = &V1StorageConfig{
		Provider:  gc.Storage.Provider,
		Bucket:    gc.Storage.Bucket,
		LocalPath: gc.Storage.LocalPath,
	}

	// Secrets config
	v1.Secrets = &V1SecretsConfig{
		Backend:                 gc.Secrets.Backend,
		GCPProjectID:            gc.Secrets.GCPProjectID,
		GCPCredentials:          gc.Secrets.GCPCredentials,
		GCPReplicationLocations: gc.Secrets.GCPReplicationLocations,
	}

	// GitHub App config
	if gc.GitHubApp.AppID != 0 {
		v1.GitHubApp = &V1GitHubAppConfig{
			AppID:           gc.GitHubApp.AppID,
			PrivateKeyPath:  gc.GitHubApp.PrivateKeyPath,
			PrivateKey:      gc.GitHubApp.PrivateKey,
			WebhookSecret:   gc.GitHubApp.WebhookSecret,
			APIBaseURL:      gc.GitHubApp.APIBaseURL,
			WebhooksEnabled: gc.GitHubApp.WebhooksEnabled,
			InstallationURL: gc.GitHubApp.InstallationURL,
		}
	}

	// Scheduler config
	if gc.Scheduler.IntervalSeconds != 0 || gc.Scheduler.MaxConcurrency != nil {
		v1.Scheduler = &V1SchedulerConfig{
			IntervalSeconds: gc.Scheduler.IntervalSeconds,
			MaxConcurrency:  gc.Scheduler.MaxConcurrency,
		}
	}

	// OIDC Login config (external OIDC provider for web login)
	if gc.OIDCLogin.Enabled || gc.OIDCLogin.IssuerURL != "" {
		v1.OIDCLogin = &V1OIDCLoginConfig{
			Enabled:      &gc.OIDCLogin.Enabled,
			DisplayName:  gc.OIDCLogin.DisplayName,
			IssuerURL:    gc.OIDCLogin.IssuerURL,
			ClientID:     gc.OIDCLogin.ClientID,
			ClientSecret: gc.OIDCLogin.ClientSecret,
			Scopes:       gc.OIDCLogin.Scopes,
		}
	}

	// OIDC Identity Provider config
	if gc.OIDC.Enabled || gc.OIDC.IssuerURL != "" {
		oidc := gc.OIDC
		v1.OIDC = &oidc
	}

	// Federation
	if gc.Federation.Enabled || len(gc.Federation.TrustedIssuers) > 0 {
		v1.Federation = &V1FederationConfig{
			Enabled:    &gc.Federation.Enabled,
			Algorithms: gc.Federation.Algorithms,
		}
		if gc.Federation.Cache.RefreshInterval > 0 {
			v1.Federation.RefreshInterval = gc.Federation.Cache.RefreshInterval.String()
		}
		if gc.Federation.Cache.DebounceInterval > 0 {
			v1.Federation.DebounceInterval = gc.Federation.Cache.DebounceInterval.String()
		}
		for _, ti := range gc.Federation.TrustedIssuers {
			v1.Federation.TrustedIssuers = append(v1.Federation.TrustedIssuers, V1TrustedIssuerConfig(ti))
		}
	}

	return v1
}

// AdaptLegacySettings converts a legacy Settings struct to VersionedSettings.
// Returns the adapted settings and a slice of deprecation warnings.
// This is a pure function with no I/O.
func AdaptLegacySettings(legacy *Settings) (*VersionedSettings, []string) {
	if legacy == nil {
		return &VersionedSettings{SchemaVersion: "1"}, nil
	}

	var warnings []string

	vs := &VersionedSettings{
		SchemaVersion:   "1",
		ActiveProfile:   legacy.ActiveProfile,
		DefaultTemplate: legacy.DefaultTemplate,
	}

	// Adapt Hub config
	if legacy.Hub != nil {
		vs.Hub = &V1HubClientConfig{
			Enabled:   legacy.Hub.Enabled,
			Linked:    legacy.Hub.Linked,
			Endpoint:  legacy.Hub.Endpoint,
			ProjectID: legacy.Hub.ProjectID,
			LocalOnly: legacy.Hub.LocalOnly,
		}
		if legacy.Hub.Token != "" {
			warnings = append(warnings, "hub.token is deprecated; use server.auth.dev_token for dev mode authentication")
		}
		if legacy.Hub.APIKey != "" {
			warnings = append(warnings, "hub.apiKey is deprecated; API key authentication is no longer supported")
		}
		if legacy.Hub.BrokerID != "" || legacy.Hub.BrokerNickname != "" || legacy.Hub.BrokerToken != "" {
			if vs.Server == nil {
				vs.Server = &V1ServerConfig{}
			}
			if vs.Server.Broker == nil {
				vs.Server.Broker = &V1BrokerConfig{}
			}
			if legacy.Hub.BrokerID != "" {
				vs.Server.Broker.BrokerID = legacy.Hub.BrokerID
				warnings = append(warnings, "hub.brokerId is deprecated; moved to server.broker.broker_id")
			}
			if legacy.Hub.BrokerNickname != "" {
				vs.Server.Broker.BrokerNickname = legacy.Hub.BrokerNickname
				warnings = append(warnings, "hub.brokerNickname is deprecated; moved to server.broker.broker_nickname")
			}
			if legacy.Hub.BrokerToken != "" {
				vs.Server.Broker.BrokerToken = legacy.Hub.BrokerToken
				warnings = append(warnings, "hub.brokerToken is deprecated; moved to server.broker.broker_token")
			}
		}
		if legacy.Hub.LastSyncedAt != "" {
			warnings = append(warnings, "hub.lastSyncedAt is deprecated; moved to state.yaml")
		}
	}

	// Adapt CLI config
	if legacy.CLI != nil {
		vs.CLI = &V1CLIConfig{
			AutoHelp: legacy.CLI.AutoHelp,
		}
	}

	// Adapt Runtimes — set Type from map key
	if legacy.Runtimes != nil {
		vs.Runtimes = make(map[string]V1RuntimeConfig, len(legacy.Runtimes))
		for name, rc := range legacy.Runtimes {
			vs.Runtimes[name] = V1RuntimeConfig{
				Type:      name,
				Host:      rc.Host,
				Context:   rc.Context,
				Namespace: rc.Namespace,
				Env:       rc.Env,
				Sync:      rc.Sync,
			}
		}
	}

	// Adapt Harnesses → HarnessConfigs — set Harness from map key
	if legacy.Harnesses != nil {
		vs.HarnessConfigs = make(map[string]HarnessConfigEntry, len(legacy.Harnesses))
		for name, hc := range legacy.Harnesses {
			vs.HarnessConfigs[name] = HarnessConfigEntry{
				Harness:          name,
				Image:            hc.Image,
				User:             hc.User,
				Env:              hc.Env,
				Volumes:          hc.Volumes,
				AuthSelectedType: hc.AuthSelectedType,
			}
		}
		warnings = append(warnings, "harnesses is deprecated; renamed to harness_configs with a required 'harness' field")
	}

	// Adapt Profiles
	if legacy.Profiles != nil {
		vs.Profiles = make(map[string]V1ProfileConfig, len(legacy.Profiles))
		for name, pc := range legacy.Profiles {
			profile := V1ProfileConfig{
				Runtime:   pc.Runtime,
				Volumes:   pc.Volumes,
				Resources: pc.Resources,
			}
			// Convert HarnessOverride → V1HarnessOverride (camelCase → snake_case
			// tags). Field-by-field, not a type conversion: the legacy struct has
			// no ImagePullPolicy (that field is versioned-settings-only), so the
			// two types no longer share an identical field set.
			if pc.HarnessOverrides != nil {
				profile.HarnessOverrides = make(map[string]V1HarnessOverride, len(pc.HarnessOverrides))
				for hk, ho := range pc.HarnessOverrides {
					profile.HarnessOverrides[hk] = V1HarnessOverride{
						Image:            ho.Image,
						User:             ho.User,
						Env:              ho.Env,
						Volumes:          ho.Volumes,
						AuthSelectedType: ho.AuthSelectedType,
						Resources:        ho.Resources,
					}
				}
			}
			vs.Profiles[name] = profile
		}
	}

	// Warn about Bucket config
	if legacy.Bucket != nil && (legacy.Bucket.Provider != "" || legacy.Bucket.Name != "" || legacy.Bucket.Prefix != "") {
		warnings = append(warnings, "bucket config is deprecated; will consolidate into server.storage")
	}

	return vs, warnings
}

// convertVersionedToLegacy maps VersionedSettings back to legacy Settings.
// Used by GetDefaultSettingsData() so the legacy Koanf loader receives valid data
// after the default file changes format.
func convertVersionedToLegacy(vs *VersionedSettings) *Settings {
	if vs == nil {
		return &Settings{}
	}

	s := &Settings{
		ActiveProfile:   vs.ActiveProfile,
		DefaultTemplate: vs.DefaultTemplate,
		WorkspacePath:   vs.WorkspacePath,
	}

	// Convert Hub
	if vs.Hub != nil {
		s.Hub = &HubClientConfig{
			Enabled:   vs.Hub.Enabled,
			Linked:    vs.Hub.Linked,
			Endpoint:  vs.Hub.Endpoint,
			ProjectID: vs.Hub.ProjectID,
			LocalOnly: vs.Hub.LocalOnly,
		}
	}

	// Map broker identity fields from v1 Server.Broker back to legacy Hub
	if vs.Server != nil && vs.Server.Broker != nil {
		if s.Hub == nil {
			s.Hub = &HubClientConfig{}
		}
		if vs.Server.Broker.BrokerID != "" {
			s.Hub.BrokerID = vs.Server.Broker.BrokerID
		}
		if vs.Server.Broker.BrokerToken != "" {
			s.Hub.BrokerToken = vs.Server.Broker.BrokerToken
		}
		if vs.Server.Broker.BrokerNickname != "" {
			s.Hub.BrokerNickname = vs.Server.Broker.BrokerNickname
		}
	}

	// Convert CLI
	if vs.CLI != nil {
		s.CLI = &CLIConfig{
			AutoHelp: vs.CLI.AutoHelp,
		}
	}

	// Convert Runtimes — drop Type field
	if vs.Runtimes != nil {
		s.Runtimes = make(map[string]RuntimeConfig, len(vs.Runtimes))
		for name, rc := range vs.Runtimes {
			s.Runtimes[name] = RuntimeConfig{
				Host:      rc.Host,
				Context:   rc.Context,
				Namespace: rc.Namespace,
				Env:       rc.Env,
				Sync:      rc.Sync,
			}
		}
	}

	// Convert HarnessConfigs → Harnesses — drop new fields (Model, Args, Harness)
	if vs.HarnessConfigs != nil {
		s.Harnesses = make(map[string]HarnessConfig, len(vs.HarnessConfigs))
		for name, hc := range vs.HarnessConfigs {
			s.Harnesses[name] = HarnessConfig{
				Image:            hc.Image,
				User:             hc.User,
				Env:              hc.Env,
				Volumes:          hc.Volumes,
				AuthSelectedType: hc.AuthSelectedType,
			}
		}
	}

	// Convert Profiles — drop new fields (DefaultTemplate, DefaultHarnessConfig)
	// Convert V1HarnessOverride → HarnessOverride (snake_case → camelCase tags)
	if vs.Profiles != nil {
		s.Profiles = make(map[string]ProfileConfig, len(vs.Profiles))
		for name, pc := range vs.Profiles {
			profile := ProfileConfig{
				Runtime:   pc.Runtime,
				Volumes:   pc.Volumes,
				Resources: pc.Resources,
			}
			// Field-by-field, not a type conversion: V1HarnessOverride carries
			// ImagePullPolicy, which the legacy HarnessOverride has no room for
			// (and doesn't need — it's not part of the live resolution path).
			if pc.HarnessOverrides != nil {
				profile.HarnessOverrides = make(map[string]HarnessOverride, len(pc.HarnessOverrides))
				for hk, ho := range pc.HarnessOverrides {
					profile.HarnessOverrides[hk] = HarnessOverride{
						Image:            ho.Image,
						User:             ho.User,
						Env:              ho.Env,
						Volumes:          ho.Volumes,
						AuthSelectedType: ho.AuthSelectedType,
						Resources:        ho.Resources,
					}
				}
			}
			s.Profiles[name] = profile
		}
	}

	return s
}

// detectDirSettingsFormat checks a single directory's settings file (NOT the
// hierarchy — no project resolution, no walking up from anywhere) to
// determine whether it uses the versioned format. Returns (false, false) if
// dir is empty, has no settings file, or the file cannot be read.
//
// Extracted from detectHierarchyFormat (which uses this for both its global
// and project checks) so that a caller needing a strictly single-file,
// global-only answer — detectGlobalSettingsFormatOnly, round 6 addendum —
// can get one without going anywhere near resolveEffectiveProjectPath.
func detectDirSettingsFormat(dir string) (hasVersioned bool, missingSchemaVersion bool) {
	if dir == "" {
		return false, false
	}
	path := GetSettingsPath(dir)
	if path == "" {
		return false, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false, false
	}
	version, _ := DetectSettingsFormat(data)
	if version == "" {
		return false, false
	}
	var raw map[string]interface{}
	hasExplicitVersion := false
	if err := yamlv3.Unmarshal(data, &raw); err == nil && raw != nil {
		_, hasExplicitVersion = raw["schema_version"]
	}
	return true, !hasExplicitVersion
}

// detectHierarchyFormat checks settings files in the global and project directories
// to determine if any user file uses the versioned format.
// Returns:
//   - hasVersioned: true if any user file is versioned (has schema_version or v1 structural indicators)
//   - missingSchemaVersion: true if versioned was detected via v1 structural indicators only (schema_version absent)
func detectHierarchyFormat(projectPath string) (hasVersioned bool, missingSchemaVersion bool) {
	// Check global settings
	globalDir, _ := GetGlobalDir()
	if hv, missing := detectDirSettingsFormat(globalDir); hv {
		return true, missing
	}

	// Check project settings
	effectiveProjectPath := resolveEffectiveProjectPath(projectPath)
	if effectiveProjectPath != "" && effectiveProjectPath != globalDir {
		if hv, missing := detectDirSettingsFormat(effectiveProjectPath); hv {
			return true, missing
		}
	}

	return false, false
}

// settingsCandidateDirs returns the directories that may contain user settings
// files (global and project), used to scan for files missing schema_version.
func settingsCandidateDirs(projectPath string) []string {
	var dirs []string
	globalDir, _ := GetGlobalDir()
	if globalDir != "" {
		dirs = append(dirs, globalDir)
	}
	if effectiveProjectPath := resolveEffectiveProjectPath(projectPath); effectiveProjectPath != "" {
		// Avoid duplicates if project path == global dir.
		if effectiveProjectPath != globalDir {
			dirs = append(dirs, effectiveProjectPath)
		}
	}
	return dirs
}

// LoadEffectiveSettings is a unified entry point that detects the settings format
// and loads using the appropriate path.
// - If any user file is versioned → uses LoadVersionedSettings
// - If all user files are legacy or absent → uses LoadSettingsKoanf + AdaptLegacySettings
// Returns (settings, deprecation_warnings, error).
func LoadEffectiveSettings(projectPath string) (*VersionedSettings, []string, error) {
	hasVersioned, missingSchemaVersion := detectHierarchyFormat(projectPath)
	if hasVersioned {
		vs, err := LoadVersionedSettings(projectPath)
		if err != nil {
			return nil, nil, fmt.Errorf("loading versioned settings: %w", err)
		}
		var warnings []string
		if missingSchemaVersion {
			warnings = append(warnings, missingSchemaVersionWarning)
		}
		// Apply DB-backed settings overlay (co-located hub+broker mode).
		// DB values win over file values for runtimes, profiles, harness_configs.
		if o := globalOverlay; o != nil {
			o.Apply(vs)
		}
		return vs, warnings, nil
	}

	// Before falling through to the legacy path, check if any settings file
	// has real content but lacks schema_version, harnesses key, and v1 runtime
	// indicators — such files may not load correctly in future versions.
	var warnings []string
	for _, dir := range settingsCandidateDirs(projectPath) {
		path := GetSettingsPath(dir)
		if path == "" {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil || len(data) == 0 {
			continue
		}
		version, isLegacy := DetectSettingsFormat(data)
		if version != "" || isLegacy {
			// File was recognized — not silently ignored.
			continue
		}
		// Parse to check for real keys.
		var raw map[string]interface{}
		if err := yamlv3.Unmarshal(data, &raw); err != nil || len(raw) == 0 {
			continue
		}
		warnings = append(warnings, fmt.Sprintf(
			"settings file %s has no schema_version field and no recognized format indicators. Add 'schema_version: \"1\"' as the first line for reliable versioned settings loading; without it, settings may not load correctly in future versions.",
			path,
		))
	}

	// Legacy path: load via existing loader, then adapt
	legacy, err := LoadSettingsKoanf(projectPath)
	if err != nil {
		return nil, nil, fmt.Errorf("loading legacy settings: %w", err)
	}
	vs, legacyWarnings := AdaptLegacySettings(legacy)
	warnings = append(warnings, legacyWarnings...)
	// Apply DB-backed settings overlay (co-located hub+broker mode).
	if o := globalOverlay; o != nil {
		o.Apply(vs)
	}
	return vs, warnings, nil
}

// LoadGlobalSettings loads settings from the broker's global directory
// (~/.scion) ONLY — no project-level file is merged on top, regardless of
// the calling process's current working directory.
//
// This is deliberately NOT the same as LoadEffectiveSettings(""): an empty
// projectPath does not mean "global only". resolveEffectiveProjectPath("")
// calls FindProjectRoot(), which walks up from os.Getwd() and, if the
// process happens to be running inside a project checkout (the common case
// for `scion start` run from a repo, or a broker started from a project
// directory), merges that project's settings.yaml on top of global (design
// deploy-config-explore round 2 review findings C1/S-F2). Passing the
// global directory explicitly short-circuits that: with projectPath equal
// to the global directory, LoadVersionedSettings's project-layer steps are
// skipped entirely (they each check projectPath != globalDir).
//
// Use this for any Layer-0, global-only setting (see
// opsettings/koanf.go's layer0Prefixes) that must not be overridable by a
// project's settings.yaml — for example server.shared_dir_storage, which
// must not let a project (including in-repo settings.yaml content from a
// cloned repository) redirect where its shared directories are mounted.
//
// Round 6 addendum (nfs-gke UAT blocker, hy-em/hy-rev-6/hy-aud-6): this read
// is now a fully dedicated, standalone loader (loadGlobalSettingsOnly,
// below) rather than a call into LoadEffectiveSettings/LoadVersionedSettings/
// LoadSettingsKoanf, for two independent reasons found in the same round:
//
//  1. Env-free. Those three functions all merge koanf's SCION_ environment
//     provider unconditionally. There is no env mapping for
//     server.shared_dir_storage itself, so an env-free read of the file is
//     semantically identical for this setting — but koanf's env provider
//     merges EVERY SCION_* variable present in the broker's process
//     environment, not just ones this setting cares about, and unmarshals
//     the merged result as a single struct. A broker env can contain
//     SCION_* variables with no business being here at all (the ii2 hub's
//     own 21-variable set, findings/ii2-hub-env-names.txt, or an
//     in-container helper var like SCION_AUTO_EXPOSE_PORTS) whose mapped
//     key happens to collide with a struct-typed field elsewhere in
//     VersionedSettings — e.g. SCION_AUTO_EXPOSE_PORTS maps to the bare key
//     "auto_expose_ports", and koanf's Unmarshal fails outright if that
//     collides with a nested map/struct. Before this fix, such a collision
//     made the ENTIRE global settings load fail to decode, and because
//     GlobalSettingsMentions found "shared_dir_storage" in the raw file
//     bytes (an unrelated part of the same file), every agent Start failed
//     closed — even though the operator's shared_dir_storage config was
//     never at risk and nothing about it depends on any environment
//     variable. This is not hypothetical: it reproduces in any shell/agent
//     container that happens to export SCION_AUTO_EXPOSE_PORTS or similar.
//  2. Global-only, for real this time. resolveEffectiveProjectPath, called
//     internally by LoadVersionedSettings/LoadSettingsKoanf/
//     detectHierarchyFormat, treats WHATEVER path it is given as if it
//     might be a project directory: if ~/.scion itself contains a
//     project-id or grove-id file (plausible — a broker's ~/.scion can
//     double as its own hub-side project identity), it resolves to
//     GetProjectConfigDir(globalDir), i.e.
//     ~/.scion/project-configs/<slug>__<id>/.scion — a completely
//     unrelated project's split-storage settings.yaml — and merges it on
//     top. That defeats the entire point of "global-only, a project's
//     settings.yaml cannot override this", which is this setting's core
//     security property (see the CWD-independence paragraph above).
//
// loadGlobalSettingsOnly therefore reads ONLY GetSettingsPath(GetGlobalDir())
// (plus embedded defaults): no resolveEffectiveProjectPath, no
// GetProjectConfigDir, no SCION_ environment provider, no DB-backed
// settings overlay. Every other property is unchanged: global-only (still
// via GetGlobalDir), CWD-independent, legacy-format detection (via
// GlobalSettingsIsLegacyFormat, itself rewritten the same way — see its doc
// comment), the 6' malformed-file substring heuristic, and
// v1-loader-authoritative behavior for a successfully-parsed file.
func LoadGlobalSettings() (*VersionedSettings, []string, error) {
	globalDir, err := GetGlobalDir()
	if err != nil {
		return nil, nil, fmt.Errorf("resolving global settings directory: %w", err)
	}
	return loadGlobalSettingsOnly(globalDir)
}

// LoadGlobalSettingsWithOverlay is LoadGlobalSettings plus the
// process-wide DB settings overlay (co-located hub and broker), which
// replaces the runtimes, profiles, harness_configs and image_registry
// sections with the hub's stored values. No project-level settings file is
// ever merged, so a project still cannot set a value read from here. The
// overlay is read on every call, so a change made through the hub settings
// API applies to the next call without a restart. The returned settings
// are a fresh copy; the overlay itself is never modified.
func LoadGlobalSettingsWithOverlay() (*VersionedSettings, []string, error) {
	vs, warnings, err := LoadGlobalSettings()
	if err != nil {
		return nil, warnings, err
	}
	if o := globalOverlay; o != nil && vs != nil {
		o.Apply(vs)
	}
	return vs, warnings, nil
}

// GlobalSettingsMentions reports whether the RAW bytes of the global
// settings file (read directly, without parsing) contain substr. Intended
// for a caller that has already failed to parse the global settings file
// via LoadGlobalSettings and needs to decide whether the operator likely
// intended to configure a specific Layer-0 setting — round 3 review
// disposition 6': failing closed on every malformed global settings file
// would regress every deployment with a default shared-dir scratchpad, even
// ones that never configured server.shared_dir_storage. Checking for the
// raw key name lets a caller fail closed only when the setting in question
// was plausibly in play, and otherwise degrade the way `main` already does.
//
// Returns false — "assume not configured" — if the global directory or its
// settings file cannot be resolved or read; a caller in that situation is
// already handling a read/parse error itself and should not additionally
// fail closed because of an unrelated problem finding the raw bytes.
func GlobalSettingsMentions(substr string) bool {
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
	return strings.Contains(string(data), substr)
}

// GlobalSettingsIsLegacyFormat reports whether the global settings file was
// loaded via the legacy (pre-schema_version) format, as opposed to a
// well-formed versioned (schema_version: "1") file.
//
// Round 5 review finding C1=T1=S-L2: GlobalSettingsMentions' raw substring
// check is a deliberately crude heuristic, accepted ONLY for a global
// settings file that failed to parse at all (round 3 disposition 6' — see
// GlobalSettingsMentions' doc comment). It must not also be applied to a
// file that DID load successfully as a v1 file: a well-formed v1 file with,
// say, a commented-out "# shared_dir_storage:" block mentions the substring
// but was not configured, and the loader — not a substring match — is
// authoritative for what a successfully-parsed file means. Callers must
// gate any GlobalSettingsMentions-based fail-closed decision on this
// function returning true, so that a successfully-loaded v1 file's absence
// of a parsed block is trusted at face value, exactly like main.
//
// This calls detectDirSettingsFormat directly on GetGlobalDir(), NOT
// detectHierarchyFormat(globalDir) as an earlier version of this function
// did. Round 6 addendum (hy-aud-6): detectHierarchyFormat(globalDir) is NOT
// actually global-only — it calls resolveEffectiveProjectPath(globalDir)
// internally, which treats the global directory as if it might itself be a
// project directory and, if ~/.scion contains a project-id or grove-id
// file, resolves to a completely unrelated project's split-storage
// settings.yaml under ~/.scion/project-configs/<slug>__<id>/.scion and
// merges its format in too. detectDirSettingsFormat never calls
// resolveEffectiveProjectPath or GetProjectConfigDir, so this function now
// keys the legacy-format decision off the global settings file alone, with
// no project layer involved at all — matching LoadGlobalSettings' own
// global-only, project-free read exactly (see loadGlobalSettingsOnly).
//
// Returns true — "treat as legacy" — if the global directory cannot be
// resolved at all, since there is then no versioned file to trust as
// authoritative and the caller's substring-based fallback is the only
// remaining signal.
func GlobalSettingsIsLegacyFormat() bool {
	globalDir, err := GetGlobalDir()
	if err != nil {
		return true
	}
	hasVersioned, _ := detectDirSettingsFormat(globalDir)
	return !hasVersioned
}

// loadGlobalSettingsOnly loads the broker's global settings file — and
// ONLY that file, GetSettingsPath(globalDir), plus embedded defaults — into
// a VersionedSettings struct. It is entirely independent of
// LoadVersionedSettings/LoadSettingsKoanf/LoadEffectiveSettings: no
// resolveEffectiveProjectPath, no GetProjectConfigDir, no SCION_
// environment provider, no DB-backed settings overlay. See
// LoadGlobalSettings' doc comment for why (round 6 addendum). The
// versioned/legacy format decision is made via detectDirSettingsFormat on
// this same single file, matching GlobalSettingsIsLegacyFormat exactly.
func loadGlobalSettingsOnly(globalDir string) (*VersionedSettings, []string, error) {
	hasVersioned, missingSchemaVersion := detectDirSettingsFormat(globalDir)

	if hasVersioned {
		vs, err := loadVersionedSettingsFileOnly(globalDir)
		if err != nil {
			return nil, nil, fmt.Errorf("loading versioned settings: %w", err)
		}
		var warnings []string
		if missingSchemaVersion {
			warnings = append(warnings, missingSchemaVersionWarning)
		}
		return vs, warnings, nil
	}

	legacy, err := loadLegacySettingsFileOnly(globalDir)
	if err != nil {
		return nil, nil, fmt.Errorf("loading legacy settings: %w", err)
	}
	vs, warnings := AdaptLegacySettings(legacy)
	return vs, warnings, nil
}

// loadVersionedSettingsFileOnly and loadLegacySettingsFileOnly (the two
// helpers loadGlobalSettingsOnly calls) live in koanf.go, which already
// imports the koanf JSON parser under the bare name "json" — settings_v1.go
// imports the standard library "encoding/json" under that name instead, so
// they cannot both live here without an import alias.

// MigrationResult reports what happened during a migration.
type MigrationResult struct {
	Path             string   `json:"path"`               // settings file that was migrated
	BackupPath       string   `json:"backup_path"`        // path of backup file created
	Format           string   `json:"format"`             // "legacy" or "versioned" (already up-to-date)
	Warnings         []string `json:"warnings"`           // deprecation warnings from AdaptLegacySettings
	StateMigrated    bool     `json:"state_migrated"`     // true if hub.lastSyncedAt was moved to state.yaml
	ServerMigrated   bool     `json:"server_migrated"`    // true if server.yaml was merged into settings
	ServerBackupPath string   `json:"server_backup_path"` // path of server.yaml backup created
	WasJSON          bool     `json:"was_json"`           // true if source was .json format
	Skipped          bool     `json:"skipped"`            // true if file was already versioned or missing
	SkipReason       string   `json:"skip_reason"`        // reason for skipping
}

// loadSingleFileVersioned loads a single settings file from dir into a VersionedSettings struct.
// Unlike LoadVersionedSettings, this does NOT merge defaults, global settings, or env vars.
// It reads only the file at the given directory, which is needed for UpdateVersionedSetting
// to avoid clobbering other layers.
func LoadSingleFileVersioned(dir string) (*VersionedSettings, error) {
	settingsPath := GetSettingsPath(dir)
	if settingsPath == "" {
		// No file exists yet — return empty versioned settings
		return &VersionedSettings{SchemaVersion: "1"}, nil
	}

	// Migrate any legacy hub.grove_id key to hub.project_id in place before
	// reading, so the parse below sees the canonical key directly. override
	// covers the case where the rewrite could not happen (read-only
	// filesystem, not owner): the value to use in memory for this call.
	_, override := migrateProjectSettingsFile(settingsPath)

	data, err := os.ReadFile(settingsPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read %s: %w", settingsPath, err)
	}

	var vs VersionedSettings
	ext := filepath.Ext(settingsPath)
	if ext == ".json" {
		if err := json.Unmarshal(data, &vs); err != nil {
			return nil, fmt.Errorf("failed to parse JSON settings at %s: %w", settingsPath, err)
		}
	} else {
		if err := yamlv3.Unmarshal(data, &vs); err != nil {
			return nil, fmt.Errorf("failed to parse YAML settings at %s: %w", settingsPath, err)
		}
	}

	// Ensure schema_version is set
	if vs.SchemaVersion == "" {
		vs.SchemaVersion = "1"
	}

	if override != "" && (vs.Hub == nil || vs.Hub.ProjectID == "") {
		if vs.Hub == nil {
			vs.Hub = &V1HubClientConfig{}
		}
		vs.Hub.ProjectID = override
	}

	return &vs, nil
}

// UpdateVersionedSetting updates a specific setting key in a v1 versioned settings file.
// It loads only the single file at dir (not merged settings), maps legacy key names to
// their v1 equivalents, updates the appropriate field, and saves via SaveVersionedSettings.
func UpdateVersionedSetting(dir string, key string, value string) error {
	vs, err := LoadSingleFileVersioned(dir)
	if err != nil {
		return err
	}

	if projectkeys.IsProjectIDConfigKey(key) || projectkeys.IsHubProjectIDConfigKey(key) {
		if vs.Hub == nil {
			vs.Hub = &V1HubClientConfig{}
		}
		vs.Hub.ProjectID = value
		return SaveVersionedSettings(dir, vs)
	}

	switch key {
	// --- Direct mappings (same in both formats) ---
	case "active_profile":
		vs.ActiveProfile = value
	case "default_template":
		vs.DefaultTemplate = value
	case "default_harness_config":
		vs.DefaultHarnessConfig = value
	case "workspace_path":
		vs.WorkspacePath = value
	case "image_registry":
		vs.ImageRegistry = value
	case "cli.autohelp":
		if vs.CLI == nil {
			vs.CLI = &V1CLIConfig{}
		}
		autohelp := value == "true"
		vs.CLI.AutoHelp = &autohelp

	// --- Hub client settings ---
	case "hub.enabled":
		if vs.Hub == nil {
			vs.Hub = &V1HubClientConfig{}
		}
		enabled := value == "true"
		vs.Hub.Enabled = &enabled
	case "hub.linked":
		if vs.Hub == nil {
			vs.Hub = &V1HubClientConfig{}
		}
		linked := value == "true"
		vs.Hub.Linked = &linked
	case "hub.endpoint":
		if vs.Hub == nil {
			vs.Hub = &V1HubClientConfig{}
		}
		vs.Hub.Endpoint = value
	case "hub.local_only":
		if vs.Hub == nil {
			vs.Hub = &V1HubClientConfig{}
		}
		localOnly := value == "true"
		vs.Hub.LocalOnly = &localOnly

	// --- Broker identity: legacy hub.broker* → v1 server.broker.* ---
	case "hub.brokerId":
		if vs.Server == nil {
			vs.Server = &V1ServerConfig{}
		}
		if vs.Server.Broker == nil {
			vs.Server.Broker = &V1BrokerConfig{}
		}
		vs.Server.Broker.BrokerID = value
	case "hub.brokerToken":
		if vs.Server == nil {
			vs.Server = &V1ServerConfig{}
		}
		if vs.Server.Broker == nil {
			vs.Server.Broker = &V1BrokerConfig{}
		}
		vs.Server.Broker.BrokerToken = value
	case "hub.brokerNickname":
		if vs.Server == nil {
			vs.Server = &V1ServerConfig{}
		}
		if vs.Server.Broker == nil {
			vs.Server.Broker = &V1BrokerConfig{}
		}
		vs.Server.Broker.BrokerNickname = value

	// --- Server auth identity ---
	case "server.auth.display_name":
		if vs.Server == nil {
			vs.Server = &V1ServerConfig{}
		}
		if vs.Server.Auth == nil {
			vs.Server.Auth = &V1AuthConfig{}
		}
		vs.Server.Auth.DisplayName = value
	case "server.auth.email":
		if vs.Server == nil {
			vs.Server = &V1ServerConfig{}
		}
		if vs.Server.Auth == nil {
			vs.Server.Auth = &V1AuthConfig{}
		}
		vs.Server.Auth.Email = value
	case "server.auth.username":
		if vs.Server == nil {
			vs.Server = &V1ServerConfig{}
		}
		if vs.Server.Auth == nil {
			vs.Server.Auth = &V1AuthConfig{}
		}
		vs.Server.Auth.Username = value

	// --- Deprecated keys: skip silently in v1 ---
	case "hub.token", "hub.apiKey", "hub.lastSyncedAt":
		// These fields don't exist in v1 — skip without error
		return nil

	// --- Bucket: not present in v1 ---
	case "bucket.provider", "bucket.name", "bucket.prefix":
		// Bucket config is deprecated in v1 — skip without error
		return nil

	default:
		// Handle hub_connections.* keys: skip in v1 format (not supported)
		if strings.HasPrefix(key, "hub_connections.") {
			return nil
		}
		return fmt.Errorf("unknown or complex setting key: %s (manual edit recommended for registries)", key)
	}

	return SaveVersionedSettings(dir, vs)
}

// GetVersionedSettingValue retrieves a specific setting value from a VersionedSettings struct.
// It mirrors the keys supported by UpdateVersionedSetting for read access.
func GetVersionedSettingValue(vs *VersionedSettings, key string) (string, error) {
	if projectkeys.IsProjectIDConfigKey(key) || projectkeys.IsHubProjectIDConfigKey(key) {
		if vs.Hub != nil {
			return vs.Hub.ProjectID, nil
		}
		return "", nil
	}

	switch key {
	case "active_profile":
		return vs.ActiveProfile, nil
	case "default_template":
		return vs.DefaultTemplate, nil
	case "default_harness_config":
		return vs.DefaultHarnessConfig, nil
	case "workspace_path":
		return vs.WorkspacePath, nil
	case "image_registry":
		return vs.ImageRegistry, nil
	case "cli.autohelp":
		if vs.CLI != nil && vs.CLI.AutoHelp != nil {
			if *vs.CLI.AutoHelp {
				return "true", nil
			}
			return "false", nil
		}
		return "", nil
	case "hub.enabled":
		if vs.Hub != nil && vs.Hub.Enabled != nil {
			if *vs.Hub.Enabled {
				return "true", nil
			}
			return "false", nil
		}
		return "", nil
	case "hub.linked":
		if vs.Hub != nil && vs.Hub.Linked != nil {
			if *vs.Hub.Linked {
				return "true", nil
			}
			return "false", nil
		}
		return "", nil
	case "hub.endpoint":
		if vs.Hub != nil {
			return vs.Hub.Endpoint, nil
		}
		return "", nil
	case "hub.local_only":
		if vs.Hub != nil && vs.Hub.LocalOnly != nil {
			if *vs.Hub.LocalOnly {
				return "true", nil
			}
			return "false", nil
		}
		return "", nil
	case "hub.brokerId":
		if vs.Server != nil && vs.Server.Broker != nil {
			return vs.Server.Broker.BrokerID, nil
		}
		return "", nil
	case "hub.brokerToken":
		if vs.Server != nil && vs.Server.Broker != nil {
			return vs.Server.Broker.BrokerToken, nil
		}
		return "", nil
	case "hub.brokerNickname":
		if vs.Server != nil && vs.Server.Broker != nil {
			return vs.Server.Broker.BrokerNickname, nil
		}
		return "", nil
	}

	if val, err, handled := getNestedMapSettingValue(vs, key); handled {
		return val, err
	}

	return "", fmt.Errorf("unknown or complex setting key: %s", key)
}

// getNestedMapSettingValue resolves dotted config-get keys of the form
// "<category>.<name>.<field>" that address a scalar field of a named entry
// inside the profiles or runtimes maps — e.g. "profiles.local.runtime" or
// "runtimes.kubernetes.namespace". handled is false when key does not match
// one of these known-category forms, in which case the caller falls back to
// its own "unknown key" error.
//
// harness_configs is deliberately not supported here even though
// VersionedSettings has a HarnessConfigs map of the same shape: harness
// configs are normally resolved from on-disk harness-config directories
// (LoadHarnessConfigDir / FindHarnessConfigDir, with template/project/global
// precedence), not merged into this map, so a settings.yaml-only lookup
// would silently miss the harness configs "scion harness-config" reports.
// Wiring config get through the same directory-aware resolution is a
// separate change; use "scion harness-config" to inspect harness configs.
//
// A nil vs returns handled=false rather than panicking on vs.Profiles or
// vs.Runtimes, so the caller's own "unknown key" error fires instead.
func getNestedMapSettingValue(vs *VersionedSettings, key string) (value string, err error, handled bool) {
	if vs == nil {
		return "", nil, false
	}
	parts := strings.SplitN(key, ".", 3)
	if len(parts) != 3 {
		return "", nil, false
	}
	name, field := parts[1], parts[2]

	switch parts[0] {
	case "profiles":
		return lookupMapEntryField(vs.Profiles, name, field, key, "profile")
	case "runtimes":
		return lookupMapEntryField(vs.Runtimes, name, field, key, "runtime")
	default:
		return "", nil, false
	}
}

// lookupMapEntryField looks up name in m and, if found, resolves field on
// the entry via lookupScalarField. label is the human-readable singular name
// of the map (e.g. "profile") used in error messages; key is the original
// dotted key, quoted back to the caller so error messages are unambiguous
// about which lookup failed.
func lookupMapEntryField[T any](m map[string]T, name, field, key, label string) (value string, err error, handled bool) {
	entry, ok := m[name]
	if !ok {
		return "", fmt.Errorf("unknown or complex setting key: %s (no %s named %q)", key, label, name), true
	}
	val, lookupErr := lookupScalarField(entry, field, key, label, name)
	return val, lookupErr, true
}

// credentialLikeFieldPattern matches koanf/yaml tag names that look like
// they hold credential material. lookupScalarField refuses any field whose
// tag matches, regardless of its Go kind, so that a future scalar field
// named e.g. "api_key" or "token" on V1ProfileConfig or V1RuntimeConfig is
// refused by name rather than rendered — see
// TestLookupScalarFieldRefusesCredentialLikeNames, which proves the refusal
// fires, and TestScalarFieldsExcludeCredentialLikeNames, a tripwire that
// fails if a matching scalar field is ever added, forcing a conscious
// decision about it rather than a silent pass-through.
//
// This is name-based coverage only: it protects fields whose tag matches
// the pattern, not the general case. A scalar field holding credential
// material under an unmatched name — e.g. "passphrase", "client_cert", or
// "signing_blob" — would still render in plaintext; nothing here can detect
// that.
var credentialLikeFieldPattern = regexp.MustCompile(`(?i)(token|secret|password|passwd|api_?key|credential|_pat$|^pat$|private_?key|ssh_?key|bearer|auth_?header)`)

// lookupScalarField finds the field on struct value v whose koanf (or yaml,
// as fallback) tag equals the first dot-separated segment of field, and
// renders it as a config-get string value.
//
// Only scalar fields (string, bool, integer, and pointers to those) are
// supported; structured fields (maps, slices, nested structs) return an
// error naming the key rather than being silently rendered or partially
// serialized, since config get has no established rendering for them. A
// field name matching credentialLikeFieldPattern is refused outright, even
// if it happens to be a plain scalar. A field parameter containing further
// dots (e.g. "cloudrun.region") means the caller asked for a path nested
// below what this lookup supports; that is reported as a "nested paths are
// not supported" error naming the first segment: as a missing field if it
// does not resolve, otherwise as a field that has no sub-fields or is not a
// scalar value.
func lookupScalarField(v interface{}, field, key, label, name string) (string, error) {
	head, nested := field, false
	if idx := strings.IndexByte(field, '.'); idx >= 0 {
		head, nested = field[:idx], true
	}

	fv, ok := findFieldByConfigTag(reflect.ValueOf(v), head)
	if !ok {
		if nested {
			return "", fmt.Errorf("unknown or complex setting key: %s (%s %q has no field %q; nested paths are not supported)", key, label, name, head)
		}
		return "", fmt.Errorf("unknown or complex setting key: %s (%s %q has no field %q)", key, label, name, head)
	}
	if credentialLikeFieldPattern.MatchString(head) {
		return "", fmt.Errorf("unknown or complex setting key: %s (field %q of %s %q looks like a credential and is not readable via config get)", key, head, label, name)
	}
	str, ok := scalarValueString(fv)
	if nested {
		if ok {
			return "", fmt.Errorf("unknown or complex setting key: %s (field %q of %s %q is a scalar value and has no sub-fields; nested paths are not supported)", key, head, label, name)
		}
		return "", fmt.Errorf("unknown or complex setting key: %s (field %q of %s %q is not a scalar value; nested paths are not supported)", key, head, label, name)
	}
	if !ok {
		return "", fmt.Errorf("unknown or complex setting key: %s (field %q of %s %q is not a scalar value)", key, field, label, name)
	}
	return str, nil
}

// configTagName returns the config-get field name for struct field f: its
// koanf tag, falling back to its yaml tag, with any trailing tag options
// (e.g. ",omitempty") stripped. It returns "" for a field with no koanf or
// yaml tag, and for one explicitly marked "-" (skip) in either tag — both of
// which are never valid config-get field names, since findFieldByConfigTag
// only matches a non-empty name.
//
// This is the single source of truth for how a struct field's tag maps to
// the field name config get resolves: findFieldByConfigTag uses it to find
// a field by name, and TestScalarFieldsExcludeCredentialLikeNames' tripwire
// uses it to enumerate the same names, so the two cannot silently diverge —
// e.g. if this function is later changed to also honor a "json" tag or an
// ",inline" option, both callers pick that up together.
// TestConfigTagName exercises the yaml fallback and the "-" skip directly,
// against a local struct, since no production field takes either path
// today.
func configTagName(f reflect.StructField) string {
	tag := f.Tag.Get("koanf")
	if tag == "" {
		tag = f.Tag.Get("yaml")
	}
	tag = strings.SplitN(tag, ",", 2)[0]
	if tag == "-" {
		return ""
	}
	return tag
}

// findFieldByConfigTag returns the value of the field in struct v whose
// configTagName (koanf tag, falling back to yaml) matches name exactly. v
// may be a struct or a (possibly nested) pointer to one; a nil pointer or a
// non-struct kind returns false rather than panicking, since
// reflect.Type.NumField panics for non-struct kinds.
//
// It does not recurse into anonymous (embedded) struct fields. Neither
// V1ProfileConfig nor V1RuntimeConfig — the only structs this is used
// against — embeds another struct today; if one of them gains an
// inline/squashed embedded field, that field's own fields become
// unreachable here and lookupScalarField reports "has no field" for them
// rather than resolving through the embed.
func findFieldByConfigTag(v reflect.Value, name string) (reflect.Value, bool) {
	for v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return reflect.Value{}, false
		}
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return reflect.Value{}, false
	}
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		if tag := configTagName(t.Field(i)); tag != "" && tag == name {
			return v.Field(i), true
		}
	}
	return reflect.Value{}, false
}

// isScalarKind is the single source of truth for which Go kinds
// scalarValueString can render: string, bool, or an integer kind. Both
// branches of scalarValueString — the nil-pointer path and the concrete
// (non-nil) value path — gate on this function rather than each keeping its
// own kind list, and the test-only isScalarFieldType (classifying a static
// struct field type without a value to inspect) also calls it, so all three
// are provably in agreement: changing what counts as scalar means changing
// this one function. TestScalarKindAgreesWithScalarValueString checks that
// agreement for every reflect.Kind from Bool through UnsafePointer (except
// Pointer itself, which is exercised separately via pointer indirection),
// not just the ones currently in use, and asserts the expected scalar-ness
// of each kind independently rather than deriving it from this function.
func isScalarKind(k reflect.Kind) bool {
	switch k {
	case reflect.String, reflect.Bool,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return true
	default:
		return false
	}
}

// underlyingKind unwraps t through any levels of pointer indirection and
// returns the final (pointee) kind. Used to classify a nil pointer by its
// static type — via reflect.Type, which does not require a value to
// dereference — rather than by a reflect.Value, which cannot be dereferenced
// once nil.
func underlyingKind(t reflect.Type) reflect.Kind {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t.Kind()
}

// scalarValueString renders v as a config-get string if it is a scalar kind
// (string, bool, or integer) or a pointer to one; a nil pointer renders as
// an empty string only if it points to a scalar kind (e.g. a nil *bool).
// Maps, slices, and structs — including a nil pointer to a struct, such as
// an unset V1RuntimeConfig.CloudRun — are reported as not scalar via
// ok=false; classifying by the pointee's static type (rather than treating
// every nil pointer as scalar) means a field's scalar-ness does not depend
// on whether it happens to be set.
func scalarValueString(v reflect.Value) (s string, ok bool) {
	for v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return "", isScalarKind(underlyingKind(v.Type()))
		}
		v = v.Elem()
	}
	if !isScalarKind(v.Kind()) {
		return "", false
	}
	switch v.Kind() {
	case reflect.String:
		return v.String(), true
	case reflect.Bool:
		return strconv.FormatBool(v.Bool()), true
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.FormatInt(v.Int(), 10), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return strconv.FormatUint(v.Uint(), 10), true
	default:
		// Unreachable: isScalarKind above already rejected every kind not
		// handled here.
		return "", false
	}
}

// SaveVersionedSettings writes a VersionedSettings struct as YAML to settings.yaml in dir.
func SaveVersionedSettings(dir string, vs *VersionedSettings) error {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create directory %s: %w", dir, err)
	}

	data, err := yamlv3.Marshal(vs)
	if err != nil {
		return fmt.Errorf("failed to marshal versioned settings: %w", err)
	}

	targetPath := filepath.Join(dir, "settings.yaml")
	return os.WriteFile(targetPath, data, 0644)
}

// MigrateSettingsFile migrates a single legacy settings file in dir to versioned format.
// If a server.yaml exists in the same directory, it is also merged into the settings
// under the "server" key and backed up.
// If dryRun is true, no files are written.
// Returns MigrationResult describing what was (or would be) done.
func MigrateSettingsFile(dir string, dryRun bool) (*MigrationResult, error) {
	result := &MigrationResult{}

	// 1. Find settings file
	settingsPath := GetSettingsPath(dir)
	if settingsPath == "" {
		result.Skipped = true
		result.SkipReason = "no settings file found"
		return result, nil
	}
	result.Path = settingsPath

	// 2. Read and detect format
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read %s: %w", settingsPath, err)
	}

	version, _ := DetectSettingsFormat(data)
	if version != "" {
		result.Skipped = true
		result.SkipReason = fmt.Sprintf("already versioned (schema_version: %s)", version)
		result.Format = "versioned"
		return result, nil
	}

	result.Format = "legacy"
	result.WasJSON = filepath.Ext(settingsPath) == ".json"

	// 3. Parse legacy settings
	var legacy Settings
	if result.WasJSON {
		if err := json.Unmarshal(data, &legacy); err != nil {
			return nil, fmt.Errorf("failed to parse JSON settings: %w", err)
		}
	} else {
		if err := yamlv3.Unmarshal(data, &legacy); err != nil {
			return nil, fmt.Errorf("failed to parse YAML settings: %w", err)
		}
	}

	// If file has no legacy indicators and is effectively empty, still migrate it
	// (add schema_version to make it versioned).
	// Both legacy and minimal/empty files fall through to conversion below.

	// 4. Convert via AdaptLegacySettings
	vs, warnings := AdaptLegacySettings(&legacy)
	result.Warnings = warnings

	// 4b. Check for server.yaml and merge if present
	serverPath := GetServerConfigPath(dir)
	if serverPath != "" {
		serverGC, err := loadServerConfigOnly(serverPath)
		if err != nil {
			result.Warnings = append(result.Warnings,
				fmt.Sprintf("failed to load %s: %v (skipping server migration)", serverPath, err))
		} else {
			v1Server := ConvertGlobalToV1ServerConfig(serverGC)
			// Merge: preserve broker identity from legacy hub settings if the
			// server.yaml doesn't already have them.
			legacyBroker := vs.Server
			vs.Server = v1Server
			if legacyBroker != nil && legacyBroker.Broker != nil {
				if vs.Server.Broker == nil {
					vs.Server.Broker = legacyBroker.Broker
				} else {
					if vs.Server.Broker.BrokerID == "" {
						vs.Server.Broker.BrokerID = legacyBroker.Broker.BrokerID
					}
					if vs.Server.Broker.BrokerNickname == "" {
						vs.Server.Broker.BrokerNickname = legacyBroker.Broker.BrokerNickname
					}
					if vs.Server.Broker.BrokerToken == "" {
						vs.Server.Broker.BrokerToken = legacyBroker.Broker.BrokerToken
					}
				}
			}
			result.ServerMigrated = true
			result.Warnings = append(result.Warnings,
				fmt.Sprintf("server.yaml merged into settings.yaml under 'server' key (source: %s)", serverPath))
		}
	}

	// 5. Handle hub.lastSyncedAt: migrate to state.yaml
	if legacy.Hub != nil && legacy.Hub.LastSyncedAt != "" {
		result.StateMigrated = true
		if !dryRun {
			state, err := LoadProjectState(dir)
			if err != nil {
				return nil, fmt.Errorf("failed to load project state: %w", err)
			}
			state.LastSyncedAt = legacy.Hub.LastSyncedAt
			if err := SaveProjectState(dir, state); err != nil {
				return nil, fmt.Errorf("failed to save project state: %w", err)
			}
		}
	}

	// 6. Validate the output
	outputData, err := yamlv3.Marshal(vs)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal converted settings: %w", err)
	}

	validationErrors, err := ValidateSettings(outputData, "1")
	if err != nil {
		return nil, fmt.Errorf("validation error: %w", err)
	}
	if len(validationErrors) > 0 {
		var errMsgs []string
		for _, ve := range validationErrors {
			errMsgs = append(errMsgs, ve.Error())
		}
		return nil, fmt.Errorf("migrated settings failed validation: %s", strings.Join(errMsgs, "; "))
	}

	// 7. If dryRun, return result without writing
	if dryRun {
		return result, nil
	}

	// 8. Back up the original settings file
	backupPath := getBackupPath(settingsPath)
	if err := os.Rename(settingsPath, backupPath); err != nil {
		return nil, fmt.Errorf("failed to create backup %s: %w", backupPath, err)
	}
	result.BackupPath = backupPath

	// 8b. Back up server.yaml if it was merged
	if result.ServerMigrated && serverPath != "" {
		serverBackup := getBackupPath(serverPath)
		if err := os.Rename(serverPath, serverBackup); err != nil {
			// Non-fatal: settings migration succeeded, server.yaml backup failed
			result.Warnings = append(result.Warnings,
				fmt.Sprintf("warning: failed to back up %s: %v", serverPath, err))
		} else {
			result.ServerBackupPath = serverBackup
		}
	}

	// 9. Write versioned settings
	if err := SaveVersionedSettings(dir, vs); err != nil {
		// Attempt to restore backups on failure
		_ = os.Rename(backupPath, settingsPath)
		if result.ServerBackupPath != "" {
			_ = os.Rename(result.ServerBackupPath, serverPath)
		}
		return nil, fmt.Errorf("failed to write versioned settings: %w", err)
	}

	return result, nil
}

// loadServerConfigOnly loads a GlobalConfig from a single server.yaml file
// without applying defaults or environment variable overrides.
func loadServerConfigOnly(path string) (*GlobalConfig, error) {
	k := koanf.New(".")
	if err := k.Load(file.Provider(path), yaml.Parser()); err != nil {
		return nil, fmt.Errorf("failed to load %s: %w", path, err)
	}
	var gc GlobalConfig
	if err := k.Unmarshal("", &gc); err != nil {
		return nil, fmt.Errorf("failed to unmarshal server config: %w", err)
	}
	return &gc, nil
}

// V1ManagedAgentsConfig holds configuration for cloud-managed agent backends.
type V1ManagedAgentsConfig struct {
	Google *V1GoogleManagedAgentConfig `json:"google,omitempty" yaml:"google,omitempty" koanf:"google"`
}

// V1GoogleManagedAgentConfig holds Google-specific managed agent settings.
type V1GoogleManagedAgentConfig struct {
	APIKey    string `json:"api_key,omitempty" yaml:"api_key,omitempty" koanf:"api_key"`
	BaseAgent string `json:"base_agent,omitempty" yaml:"base_agent,omitempty" koanf:"base_agent"`
	Model     string `json:"model,omitempty" yaml:"model,omitempty" koanf:"model"`
}

// getBackupPath returns a backup file path that does not already exist.
// Uses <path>.bak, <path>.bak.1, <path>.bak.2, etc.
func getBackupPath(path string) string {
	backup := path + ".bak"
	if _, err := os.Stat(backup); os.IsNotExist(err) {
		return backup
	}
	for i := 1; ; i++ {
		numbered := fmt.Sprintf("%s.bak.%d", path, i)
		if _, err := os.Stat(numbered); os.IsNotExist(err) {
			return numbered
		}
	}
}
