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
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"

	"github.com/knadh/koanf/maps"
	"github.com/knadh/koanf/providers/confmap"
	"github.com/knadh/koanf/providers/rawbytes"
	"github.com/knadh/koanf/v2"
	yamlv3 "gopkg.in/yaml.v3"
)

// This file holds the one settings loader's legacy edges: the conversion of
// a legacy-format settings file to the v1 shape at its own layer, and the
// read-only Settings view that CLI callers still use. There is no separate
// legacy loader: LoadSettings and LoadEffectiveSettings both read through
// LoadVersionedSettings' layers (ptone/scion#3919).

// LoadSettings loads the effective settings for projectPath and returns them
// as the legacy Settings shape: a read-only view built from the versioned
// loader (LoadVersionedSettings' layers: embedded defaults, global, in-repo
// project, external project, SCION_* environment, the project-id file).
// Supports YAML (.yaml/.yml) and JSON (.json) files, preferring YAML. A
// legacy-format file is converted to the v1 shape at its own layer.
//
// Writes go through UpdateSetting and the locked writers, never through this
// view.
func LoadSettings(projectPath string) (*Settings, error) {
	return loadSettingsView(projectPath, false)
}

// LoadSettingsIgnoringEnvProjectID is LoadSettings without the
// SCION_PROJECT_ID / SCION_HUB_PROJECT_ID environment overlay on the project
// IDs. All other environment variables still apply.
//
// It is for callers that resolve an explicitly named project (the --project
// or --global flag): the project ID must come from that project's own
// settings, not from the environment of the agent container the CLI runs
// in (ptone/scion#3123).
func LoadSettingsIgnoringEnvProjectID(projectPath string) (*Settings, error) {
	return loadSettingsView(projectPath, true)
}

func loadSettingsView(projectPath string, ignoreEnvProjectID bool) (*Settings, error) {
	vs, res, err := loadVersionedSettings(projectPath, versionedLoadOptions{
		withDefaults:       true,
		ignoreEnvProjectID: ignoreEnvProjectID,
	})
	if err != nil {
		return nil, err
	}
	return settingsViewOf(vs, res, viewOptions{ignoreEnvProjectID: ignoreEnvProjectID}), nil
}

// loadSingleDirSettingsView is the Settings view of dir's own settings
// file alone: no embedded defaults, no global or other project layer, no
// project-id file and no environment variable. The file is read through
// the same layer conversion as the settings loaders (a legacy-format file
// becomes the v1 shape), so the view's fields mean what they mean in
// LoadSettings. A missing file is an empty view.
func loadSingleDirSettingsView(dir string) (*Settings, error) {
	k := koanf.New(".")
	legacy := &legacyLayerState{}
	if _, err := loadSettingsFileConverting(k, dir, legacy); err != nil {
		return nil, err
	}
	res := &versionedLoadResult{
		k:                         k,
		fileHubProjectID:          k.String(projectkeys.ConfigHubProjectIDKey),
		fileHubProjectIDFromCamel: legacy.hubIDFromCamel,
		legacyWarnings:            dedupeStrings(legacy.warnings),
	}
	vs := &VersionedSettings{
		Runtimes:       make(map[string]V1RuntimeConfig),
		HarnessConfigs: make(map[string]HarnessConfigEntry),
		Profiles:       make(map[string]V1ProfileConfig),
	}
	if err := unmarshalVersionedSettings(k, vs); err != nil {
		return nil, err
	}
	return settingsViewOf(vs, res, viewOptions{fileOnly: true}), nil
}

// viewOptions are settingsViewOf's environment rules.
type viewOptions struct {
	// ignoreEnvProjectID leaves out SCION_HUB_PROJECT_ID and the project-ID
	// override warning (see LoadSettingsIgnoringEnvProjectID).
	ignoreEnvProjectID bool
	// fileOnly reads no environment variable at all (implies
	// ignoreEnvProjectID): the view holds what the settings file says
	// (see loadSingleDirSettingsView).
	fileOnly bool
}

// settingsViewOf builds the legacy Settings view of vs. Every field a caller
// outside this package reads is carried; the legacy-only fields that the v1
// settings never read (bucket, hub.lastSyncedAt, hub.token and hub.apiKey in
// a file) are left empty.
//
// The two project IDs keep their legacy meaning:
//   - ProjectID is the project's own ID: SCION_HUB_PROJECT_ID, then the
//     project-id file, then hub.project_id in the settings files (not when
//     it comes from a legacy file's hub.projectId, which named the hub
//     project only), then SCION_PROJECT_ID, then a top-level project_id in
//     the files.
//   - Hub.ProjectID (GetHubProjectID) is the hub project ID recorded in the
//     settings files, not the project-id file. SCION_HUB_PROJECT_ID replaces
//     it only when the files record one.
func settingsViewOf(vs *VersionedSettings, res *versionedLoadResult, opts viewOptions) *Settings {
	ignoreEnvProjectID := opts.ignoreEnvProjectID || opts.fileOnly
	s := convertVersionedToLegacy(vs)
	if s.Runtimes == nil {
		s.Runtimes = make(map[string]RuntimeConfig)
	}
	if s.Harnesses == nil {
		s.Harnesses = make(map[string]HarnessConfig)
	}
	if s.Profiles == nil {
		s.Profiles = make(map[string]ProfileConfig)
	}

	s.ProjectID = vs.ProjectID
	switch {
	case res.hubEnvProjectID != "":
		s.ProjectID = res.hubEnvProjectID
	case res.idFileProjectID != "":
		s.ProjectID = res.idFileProjectID
	case res.fileHubProjectID != "" && !res.fileHubProjectIDFromCamel:
		s.ProjectID = res.fileHubProjectID
	}
	if !ignoreEnvProjectID {
		// In this view both variables compete for ProjectID, and
		// SCION_HUB_PROJECT_ID wins. The versioned loader keeps them apart
		// (SCION_PROJECT_ID sets only the top-level project_id), so the
		// disagreement is reported here.
		warnProjectIDOverride(projectkeys.EnvHubProjectID, envHubProjectID(), projectkeys.EnvProjectID, os.Getenv(projectkeys.EnvProjectID))
	}

	if s.Hub != nil {
		s.Hub.ProjectID = ""
		if res.fileHubProjectID != "" {
			s.Hub.ProjectID = res.fileHubProjectID
			if hubEnv := envHubProjectID(); hubEnv != "" && !ignoreEnvProjectID {
				s.Hub.ProjectID = hubEnv
			}
		}
	}
	if vs.Hub != nil && vs.Hub.Transport != nil {
		s.Hub.Transport = &HubTransportConfig{Mode: vs.Hub.Transport.Mode, Audience: vs.Hub.Transport.Audience}
	}

	// Parity guard: the legacy loader read SCION_HUB_TOKEN and
	// SCION_HUB_API_KEY into Hub.Token and Hub.APIKey, and IsHubEnabled
	// still counts them (pkg/agent's non-broker start path). The v1
	// settings have no such keys, so the view takes them from the
	// environment only. A hub.token or hub.apiKey key in a settings file is
	// not read (the write policy refuses it).
	token, apiKey := os.Getenv("SCION_HUB_TOKEN"), os.Getenv("SCION_HUB_API_KEY")
	if opts.fileOnly {
		token, apiKey = "", ""
	}
	if token != "" || apiKey != "" {
		if s.Hub == nil {
			s.Hub = &HubClientConfig{}
		}
		s.Hub.Token = token
		s.Hub.APIKey = apiKey
	}

	if vs.CLI != nil {
		s.CLI.Mode = vs.CLI.Mode
	}

	if len(vs.HubConnections) > 0 {
		s.HubConnections = make(map[string]HubConnectionConfig, len(vs.HubConnections))
		for name, c := range vs.HubConnections {
			s.HubConnections[name] = HubConnectionConfig(c)
		}
	}
	return s
}

// legacyOnlyHubKeys are the hub.* keys of a legacy-format file that have no
// v1 key of the same name. AdaptLegacySettings maps each of them (the broker
// fields to server.broker, projectId to hub.project_id) or drops it with a
// deprecation warning, so the raw key is removed before the converted value
// is merged.
var legacyOnlyHubKeys = []string{"brokerId", "brokerToken", "brokerNickname", "projectId", "token", "apiKey", "lastSyncedAt"}

// loadLegacySettingsLayer merges the settings file at path into k. A file
// that is not versioned (no schema_version and no v1 structural indicator,
// see DetectSettingsFormat) is converted to the v1 shape first, with the
// same conversion "scion config migrate" applies (AdaptLegacySettings): the
// legacy keys become their v1 keys, and every other key of the file is kept
// as written. The conversion's deprecation warnings are appended to
// *warnings. A versioned file is merged as written.
//
// A legacy-format file the legacy shape cannot decode (a value of the wrong
// type) is an error, as it was for the legacy loader.
func loadLegacySettingsLayer(k *koanf.Koanf, path string, parser koanf.Parser, st *legacyLayerState) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	fk := koanf.New(".")
	if err := fk.Load(rawbytes.Provider(data), parser); err != nil {
		return err
	}
	// Whether this layer's hub.project_id comes only from a legacy
	// hub.projectId key (see legacyLayerState.hubIDFromCamel).
	paths := foldedKeyPaths(fk)
	camelKey, hasCamel := paths[strings.ToLower(projectkeys.ConfigHubProjectIDJSON)]
	_, hasSnake := paths[strings.ToLower(projectkeys.ConfigHubProjectIDKey)]
	camelOnly := !hasSnake && hasCamel && fk.String(camelKey) != ""
	if version, _ := DetectSettingsFormat(data); version != "" {
		if camelOnly {
			// Read-only fallback: a versioned file whose hub project ID
			// is under the legacy camelCase key (a hand edit, or an older
			// writer) keeps it, as the legacy loader read it. It is the
			// hub project ID only, like a legacy file's.
			warnUnparseableSettingOnce(projectkeys.ConfigHubProjectIDJSON+"@"+path,
				"settings: "+path+" uses the legacy key hub.projectId; reading it as hub.project_id (rename the key)")
			_ = fk.Load(confmap.Provider(map[string]interface{}{
				projectkeys.ConfigHubProjectIDKey: fk.String(camelKey),
			}, "."), nil)
			fk.Delete(camelKey)
			st.hubIDFromCamel = true
		} else if fk.Exists(projectkeys.ConfigHubProjectIDKey) {
			st.hubIDFromCamel = false
		}
		st.touch(fk.Keys()...)
		return k.Merge(fk)
	}

	converted, w, err := convertLegacySettingsLayer(fk, k)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	st.warnings = append(st.warnings, w...)
	if hub, isMap := converted["hub"].(map[string]interface{}); isMap {
		if _, set := hub["project_id"]; set {
			st.hubIDFromCamel = camelOnly
		}
	}
	if st.touched != nil {
		ck := koanf.New(".")
		if err := ck.Load(confmap.Provider(converted, ""), nil); err == nil {
			st.touch(ck.Keys()...)
		}
	}
	return k.Load(confmap.Provider(converted, ""), nil)
}

// legacyLayerState is what loading the settings file layers found about the
// legacy-format files among them.
type legacyLayerState struct {
	// warnings are the deprecation warnings of the converted files.
	warnings []string
	// hubIDFromCamel reports that the merged hub.project_id so far comes
	// from a legacy file's camelCase hub.projectId alone. That key was the
	// hub project ID only, never the project's own ID, so the Settings view
	// does not take ProjectID from it (settingsViewOf).
	hubIDFromCamel bool
	// touched, when not nil, collects the keys each loaded file sets (in
	// their v1 form), for the setting sources (see versionedLoadResult).
	touched *[]string
}

// touch records keys as set by the file being loaded (see touched).
func (st *legacyLayerState) touch(keys ...string) {
	if st != nil && st.touched != nil {
		*st.touched = append(*st.touched, keys...)
	}
}

// convertLegacySettingsLayer returns the v1 form of one legacy-format file
// already loaded into fk: the file's own keys, with every key the legacy
// Settings shape decodes replaced by its AdaptLegacySettings conversion.
// It is an error when the legacy shape cannot decode the file.
func convertLegacySettingsLayer(fk, lower *koanf.Koanf) (converted map[string]interface{}, warnings []string, err error) {
	legacy := &Settings{}
	if err := fk.Unmarshal("", legacy); err != nil {
		return nil, nil, fmt.Errorf("decoding legacy settings: %w", err)
	}
	vs, warnings := AdaptLegacySettings(legacy)

	adaptedYAML, err := yamlv3.Marshal(vs)
	if err != nil {
		return nil, nil, fmt.Errorf("converting legacy settings: %w", err)
	}
	var adapted map[string]interface{}
	if err := yamlv3.Unmarshal(adaptedYAML, &adapted); err != nil {
		return nil, nil, fmt.Errorf("converting legacy settings: %w", err)
	}
	delete(adapted, "schema_version")
	adapted = keepLegacySourcedLeaves(adapted, fk, lower)

	raw := fk.Raw()
	delete(raw, "harnesses")
	delete(raw, "bucket")
	if hub, isMap := raw["hub"].(map[string]interface{}); isMap {
		for key := range hub {
			for _, legacyKey := range legacyOnlyHubKeys {
				if strings.EqualFold(key, legacyKey) {
					delete(hub, key)
				}
			}
		}
	}
	// A legacy harness override spells auth_selected_type in camelCase;
	// the converted entry carries the v1 spelling.
	if profiles, isMap := raw["profiles"].(map[string]interface{}); isMap {
		for _, p := range profiles {
			profile, isMap := p.(map[string]interface{})
			if !isMap {
				continue
			}
			overrides, isMap := profile["harness_overrides"].(map[string]interface{})
			if !isMap {
				continue
			}
			for _, o := range overrides {
				if override, isMap := o.(map[string]interface{}); isMap {
					for key := range override {
						if strings.EqualFold(key, "auth_selectedType") {
							delete(override, key)
						}
					}
				}
			}
		}
	}
	maps.Merge(adapted, raw)
	return raw, warnings, nil
}

// keepLegacySourcedLeaves returns the converted settings with only the
// leaves whose legacy source key is present in the file (fk): a converted
// entry the file only partly set must not blank, or replace, what a lower
// layer sets for its other keys. For example a legacy profile without
// runtime converts to profiles.NAME.runtime: "" (the field is not
// omitempty), and every converted runtime entry gets type: NAME; neither
// was set by the file, so both are dropped here.
//
// Source keys match case-insensitively, as the legacy decode does (a
// hand-edited hub.projectID converts like hub.projectId).
//
// A harness config's harness is derived from the entry's name, like a
// runtime's type. It is kept, as the legacy conversion always set it, so a
// legacy entry's name keeps resolving as its harness; it never overrides a
// lower layer (lower) that already sets that entry's harness.
func keepLegacySourcedLeaves(adapted map[string]interface{}, fk, lower *koanf.Koanf) map[string]interface{} {
	ak := koanf.New(".")
	if err := ak.Load(confmap.Provider(adapted, ""), nil); err != nil {
		return adapted
	}
	paths := foldedKeyPaths(fk)
	for _, key := range ak.Keys() {
		src := legacySourceKey(key)
		if _, ok := paths[strings.ToLower(src)]; src == "" || !ok {
			ak.Delete(key)
			continue
		}
		if isDerivedHarnessKey(key) && lower != nil && lower.Exists(key) {
			ak.Delete(key)
		}
	}
	return ak.Raw()
}

// isDerivedHarnessKey reports whether key is harness_configs.NAME.harness,
// which AdaptLegacySettings derives from the entry's name.
func isDerivedHarnessKey(key string) bool {
	parts := strings.Split(key, ".")
	return len(parts) == 3 && parts[0] == "harness_configs" && parts[2] == "harness"
}

// foldedKeyPaths returns every key path of k (each leaf key and each of its
// parent paths), lower-cased, mapped to the path as k spells it.
func foldedKeyPaths(k *koanf.Koanf) map[string]string {
	paths := map[string]string{}
	for _, key := range k.Keys() {
		parts := strings.Split(key, ".")
		for i := 1; i <= len(parts); i++ {
			p := strings.Join(parts[:i], ".")
			if _, seen := paths[strings.ToLower(p)]; !seen {
				paths[strings.ToLower(p)] = p
			}
		}
	}
	return paths
}

// legacySourceKey returns the key of a legacy-format file that
// AdaptLegacySettings converts to the v1 key, or "" when no file key is the
// source (a runtime's type, which the conversion derives from the entry's
// name). Keys the conversion does not rename are their own source.
func legacySourceKey(key string) string {
	parts := strings.Split(key, ".")
	switch {
	case key == projectkeys.ConfigHubProjectIDKey:
		return projectkeys.ConfigHubProjectIDJSON
	case key == "server.broker.broker_id":
		return "hub.brokerId"
	case key == "server.broker.broker_token":
		return "hub.brokerToken"
	case key == "server.broker.broker_nickname":
		return "hub.brokerNickname"
	case len(parts) == 3 && parts[0] == "runtimes" && parts[2] == "type":
		return ""
	case len(parts) >= 3 && parts[0] == "harness_configs":
		// harness is the entry's name; the other fields keep their names,
		// except auth_selected_type.
		if len(parts) == 3 && parts[2] == "harness" {
			return "harnesses." + parts[1]
		}
		if len(parts) == 3 && parts[2] == "auth_selected_type" {
			return "harnesses." + parts[1] + ".auth_selectedType"
		}
		return "harnesses." + strings.Join(parts[1:], ".")
	case len(parts) == 5 && parts[0] == "profiles" && parts[2] == "harness_overrides" && parts[4] == "auth_selected_type":
		return strings.Join(parts[:4], ".") + ".auth_selectedType"
	}
	return key
}

// versionedLoadOptions selects the layers loadVersionedLayers merges.
type versionedLoadOptions struct {
	// withDefaults loads the embedded defaults as the first layer.
	withDefaults bool
	// ignoreEnvProjectID leaves out the SCION_PROJECT_ID and
	// SCION_HUB_PROJECT_ID environment overlay (see
	// LoadSettingsIgnoringEnvProjectID).
	ignoreEnvProjectID bool
	// trackSources records which layer set each key
	// (versionedLoadResult.sources).
	trackSources bool
	// skipEnv names SCION_* environment variables the load leaves out
	// (see ResolveSettingValueIgnoringEnv).
	skipEnv map[string]bool
}

// versionedLoadResult is what loadVersionedLayers found besides the merged
// settings.
type versionedLoadResult struct {
	k *koanf.Koanf
	// fileHubProjectID is hub.project_id as the settings files set it,
	// before the environment and the project-id file overlay it.
	fileHubProjectID string
	// fileHubProjectIDFromCamel reports that fileHubProjectID comes from a
	// legacy file's hub.projectId alone (see legacyLayerState).
	fileHubProjectIDFromCamel bool
	// idFileProjectID is the project-id file's value when it was applied.
	idFileProjectID string
	// hubEnvProjectID is SCION_HUB_PROJECT_ID when it was applied.
	hubEnvProjectID string
	// legacyWarnings are the deprecation warnings of the legacy-format files
	// converted on the way (see loadLegacySettingsLayer).
	legacyWarnings []string
	// sources maps each key of k (koanf's flattened form) to the layer that
	// last set it, when versionedLoadOptions.trackSources is set: "default",
	// "global <file>", "project <file>", "env <VARIABLE>" or
	// "project-id file <file>".
	sources map[string]string
}

// loadVersionedSettings is LoadVersionedSettings with options, also
// returning what the load found besides the settings.
func loadVersionedSettings(projectPath string, opts versionedLoadOptions) (*VersionedSettings, *versionedLoadResult, error) {
	res, err := loadVersionedLayers(projectPath, opts)
	if err != nil {
		return nil, nil, err
	}

	settings := &VersionedSettings{
		Runtimes:       make(map[string]V1RuntimeConfig),
		HarnessConfigs: make(map[string]HarnessConfigEntry),
		Profiles:       make(map[string]V1ProfileConfig),
	}
	if err := unmarshalVersionedSettings(res.k, settings); err != nil {
		return nil, nil, err
	}
	if settings.Telemetry != nil && settings.Telemetry.Cloud != nil && settings.Telemetry.Cloud.TLS != nil {
		if err := ValidateV1TelemetryTLS(settings.Telemetry.Cloud.TLS); err != nil {
			return nil, nil, err
		}
	}
	return settings, res, nil
}

// dedupeStrings returns ss without repeats, in first-seen order.
func dedupeStrings(ss []string) []string {
	if len(ss) == 0 {
		return ss
	}
	seen := make(map[string]bool, len(ss))
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}
