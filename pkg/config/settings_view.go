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
	return settingsViewOf(vs, res, ignoreEnvProjectID), nil
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
func settingsViewOf(vs *VersionedSettings, res *versionedLoadResult, ignoreEnvProjectID bool) *Settings {
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
	case res == nil:
		if vs.Hub != nil && vs.Hub.ProjectID != "" {
			s.ProjectID = vs.Hub.ProjectID
		}
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
		if res != nil && res.fileHubProjectID != "" {
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
	camelOnly := !fk.Exists(projectkeys.ConfigHubProjectIDKey) && fk.String(projectkeys.ConfigHubProjectIDJSON) != ""
	if version, _ := DetectSettingsFormat(data); version != "" {
		if fk.Exists(projectkeys.ConfigHubProjectIDKey) {
			st.hubIDFromCamel = false
		}
		return k.Merge(fk)
	}

	converted, w, err := convertLegacySettingsLayer(fk)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	st.warnings = append(st.warnings, w...)
	if hub, isMap := converted["hub"].(map[string]interface{}); isMap {
		if _, set := hub["project_id"]; set {
			st.hubIDFromCamel = camelOnly
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
}

// convertLegacySettingsLayer returns the v1 form of one legacy-format file
// already loaded into fk: the file's own keys, with every key the legacy
// Settings shape decodes replaced by its AdaptLegacySettings conversion.
// It is an error when the legacy shape cannot decode the file.
func convertLegacySettingsLayer(fk *koanf.Koanf) (converted map[string]interface{}, warnings []string, err error) {
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

	raw := fk.Raw()
	delete(raw, "harnesses")
	delete(raw, "bucket")
	if hub, isMap := raw["hub"].(map[string]interface{}); isMap {
		for _, key := range legacyOnlyHubKeys {
			delete(hub, key)
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
					delete(override, "auth_selectedType")
				}
			}
		}
	}
	maps.Merge(adapted, raw)
	return raw, warnings, nil
}

// versionedLoadOptions selects the layers loadVersionedLayers merges.
type versionedLoadOptions struct {
	// withDefaults loads the embedded defaults as the first layer.
	withDefaults bool
	// ignoreEnvProjectID leaves out the SCION_PROJECT_ID and
	// SCION_HUB_PROJECT_ID environment overlay (see
	// LoadSettingsIgnoringEnvProjectID).
	ignoreEnvProjectID bool
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
