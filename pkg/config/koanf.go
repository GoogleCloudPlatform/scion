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
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync"

	"github.com/GoogleCloudPlatform/scion/pkg/projectcompat"
	mapstructure "github.com/go-viper/mapstructure/v2"
	"github.com/knadh/koanf/parsers/json"
	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/confmap"
	"github.com/knadh/koanf/providers/env"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/providers/rawbytes"
	"github.com/knadh/koanf/v2"
)

// detectLocalRuntimeOnce caches the result of DetectLocalRuntime so that
// expensive external-command probes (container, podman, docker --version)
// run at most once per process. GetDefaultSettingsDataYAML, which is called
// on every LoadVersionedSettings invocation, uses this wrapper.
var detectLocalRuntimeOnce = sync.OnceValues(func() (string, error) {
	return DetectLocalRuntime()
})

// resetDetectLocalRuntimeCache resets the cached runtime detection result.
// This must be called in tests that override lookPathFunc/runCheckFunc to
// ensure the new mock is picked up.
func resetDetectLocalRuntimeCache() {
	detectLocalRuntimeOnce = sync.OnceValues(func() (string, error) {
		return DetectLocalRuntime()
	})
}

// LoadSettingsKoanf loads settings using Koanf with provider priority:
// 1. Embedded defaults (YAML) with OS-specific runtime adjustment
// 2. Global settings file (~/.scion/settings.yaml or .json)
// 3. In-repo project settings file (.scion/settings.yaml or .json)
// 4. External project config settings (for git projects with split storage)
// 5. Environment variables (SCION_ prefix, top-level only)
func LoadSettingsKoanf(projectPath string) (*Settings, error) {
	k := koanf.New(".")

	// 1. Load embedded defaults (YAML with fallback to JSON)
	// GetDefaultSettingsData applies OS-specific runtime adjustments
	if defaultData, err := GetDefaultSettingsData(); err == nil {
		_ = k.Load(rawbytes.Provider(defaultData), json.Parser())
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
	// Captured once, right after the global layer loads: the precedence
	// check below always compares a project layer's newly migrated value
	// against the global value specifically, regardless of which layer
	// (in-repo or external) turns out to hold the project's own file — see
	// logHubProjectIDPrecedenceChange. A global value that was itself
	// migrated from hub.grove_id in this load is not a pre-existing
	// canonical value, so no precedence change is reported against it: two
	// legacy hub.grove_id values resolve to the same project-over-global
	// precedence whether or not either side has been migrated yet.
	globalHubProjectID := k.String(projectcompat.ConfigHubProjectIDKey)
	if globalMigratedHub {
		globalHubProjectID = ""
	}

	// 3. Load in-repo project settings (.scion/settings.yaml)
	// For git projects with split storage, the in-repo settings provide
	// project-level defaults checked into the repo.
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
	// storage). This is also where a plain (non-split-storage) project's own
	// settings.yaml is actually loaded when projectPath is "" and the
	// project is found via the current directory (resolveEffectiveProjectPath
	// -> FindProjectRoot): step 3 above never runs in that case, so the
	// precedence check must run here too, not only in step 3.
	if effectiveProjectPath != "" && effectiveProjectPath != globalDir && effectiveProjectPath != projectPath {
		migratedHub, err := loadSettingsFile(k, effectiveProjectPath)
		if err != nil {
			return nil, err
		}
		if migratedHub {
			logHubProjectIDPrecedenceChange(k, effectiveProjectPath, globalHubProjectID)
		}
	}

	// Check for unrecognized keys BEFORE environment variables are loaded.
	// Environment variables like SCION_PROJECT, SCION_GROVE, SCION_CREATOR
	// do not map to Settings struct fields and would produce false-positive
	// warnings if the check ran on the merged koanf instance.
	{
		var probe Settings
		_ = unmarshalWithUnusedKeyCheck(k, &probe, "settings")
	}

	// 5. Load environment variables (SCION_ prefix, top-level only)
	// Maps: SCION_ACTIVE_PROFILE -> active_profile
	//       SCION_DEFAULT_TEMPLATE -> default_template
	//       SCION_BUCKET_PROVIDER -> bucket.provider
	//       SCION_BUCKET_NAME -> bucket.name
	//       SCION_BUCKET_PREFIX -> bucket.prefix
	//       SCION_HUB_ENDPOINT -> hub.endpoint
	//       SCION_HUB_TOKEN -> hub.token
	//       SCION_HUB_API_KEY -> hub.apiKey
	//       SCION_HUB_BROKER_ID -> hub.brokerId
	//       SCION_HUB_BROKER_TOKEN -> hub.brokerToken
	_ = k.Load(env.Provider("SCION_", ".", func(s string) string {
		if mapped, ok := projectcompat.EnvProjectIDConfigKey(s, true); ok {
			return mapped
		}
		if isRemovedLegacyEnv(s) {
			// SCION_HUB_GROVE_ID is no longer read. Without this check
			// it would otherwise fall through to the generic "hub_" mapping
			// below and land on the unrecognised key hub.grove_id.
			// Returning "" makes the env provider drop the variable
			// entirely (env.go's Provider skips a "" key), the same idiom
			// settings_v1.go already uses for SCION_OTEL_INSECURE.
			// WarnRemovedLegacyEnv reports it separately.
			return ""
		}
		key := strings.ToLower(strings.TrimPrefix(s, "SCION_"))
		// Handle nested bucket keys
		if strings.HasPrefix(key, "bucket_") {
			return "bucket." + strings.TrimPrefix(key, "bucket_")
		}
		// Handle nested hub keys
		if strings.HasPrefix(key, "hub_") {
			subkey := strings.TrimPrefix(key, "hub_")
			// Convert snake_case to camelCase for specific keys
			switch subkey {
			case "api_key":
				return "hub.apiKey"
			case "broker_id":
				return "hub.brokerId"
			case "broker_token":
				return "hub.brokerToken"
			default:
				return "hub." + subkey
			}
		}
		return key
	}), nil)

	// Normalize v1 settings keys to legacy keyspace.
	// In v1 format, project_id is stored at hub.project_id (snake_case), but
	// the legacy Settings struct expects it at the top level (project_id).
	// The HubClientConfig struct uses koanf tag "projectId" (camelCase), so
	// the v1 key hub.project_id doesn't match either location without
	// remapping. Always remap (unconditionally) because after the koanf
	// merge chain, hub.project_id reflects the most specific
	// (project-level) value and must take precedence over any top-level
	// project_id inherited from global.
	hubProjectID := ""
	if k.Exists(projectcompat.ConfigHubProjectIDKey) {
		hubProjectID = k.String(projectcompat.ConfigHubProjectIDKey)
	}

	if hubProjectID != "" {
		_ = k.Load(confmap.Provider(map[string]interface{}{
			projectcompat.ConfigProjectIDKey: hubProjectID,
		}, "."), nil)
		// Also remap to hub.projectId (camelCase) so the legacy
		// HubClientConfig.ProjectID field (koanf tag "projectId") is populated.
		// Without this, GetHubProjectID() returns "" for V1 settings, causing
		// EnsureHubReady to fall back to the local project_id and loop on
		// project registration when the hub project ID differs from the local ID.
		if !k.Exists(projectcompat.ConfigHubProjectIDJSON) {
			_ = k.Load(confmap.Provider(map[string]interface{}{
				projectcompat.ConfigHubProjectIDJSON: hubProjectID,
			}, "."), nil)
		}
	}

	// For git projects, the project_id is stored in a project-id file inside the
	// .scion directory rather than in the settings file. Read it here so that
	// it overrides any project_id inherited from global settings. The original
	// projectPath points to the .scion directory (before resolveEffectiveProjectPath
	// redirects to the external config dir).
	if projectPath != "" && projectPath != globalDir {
		if projectID, err := ReadProjectID(projectPath); err == nil && projectID != "" {
			_ = k.Load(confmap.Provider(map[string]interface{}{
				projectcompat.ConfigProjectIDKey: projectID,
			}, "."), nil)
		}
	}

	// In v1 format, broker identity fields are stored under server.broker.*
	// (snake_case), but the legacy Settings struct expects them at hub.brokerId
	// (camelCase). Remap so LoadSettingsKoanf produces correct HubClientConfig.
	if k.Exists("server.broker.broker_id") && !k.Exists("hub.brokerId") {
		_ = k.Load(confmap.Provider(map[string]interface{}{
			"hub.brokerId": k.String("server.broker.broker_id"),
		}, "."), nil)
	}
	if k.Exists("server.broker.broker_token") && !k.Exists("hub.brokerToken") {
		_ = k.Load(confmap.Provider(map[string]interface{}{
			"hub.brokerToken": k.String("server.broker.broker_token"),
		}, "."), nil)
	}
	if k.Exists("server.broker.broker_nickname") && !k.Exists("hub.brokerNickname") {
		_ = k.Load(confmap.Provider(map[string]interface{}{
			"hub.brokerNickname": k.String("server.broker.broker_nickname"),
		}, "."), nil)
	}

	// Unmarshal into Settings struct
	settings := &Settings{
		Runtimes:  make(map[string]RuntimeConfig),
		Harnesses: make(map[string]HarnessConfig),
		Profiles:  make(map[string]ProfileConfig),
	}

	if err := k.Unmarshal("", settings); err != nil {
		return nil, err
	}

	return settings, nil
}

// LoadSettingsFromDir loads settings from a single directory's settings file
// without applying embedded defaults, global settings, or environment variables.
// This is useful when you need to read just one project's settings file in isolation,
// for example to get the project's hub.endpoint without the broker's own env vars
// overriding it.
func LoadSettingsFromDir(dir string) (*Settings, error) {
	k := koanf.New(".")
	if _, err := loadSettingsFile(k, dir); err != nil {
		return nil, err
	}
	settings := &Settings{
		Runtimes:  make(map[string]RuntimeConfig),
		Harnesses: make(map[string]HarnessConfig),
		Profiles:  make(map[string]ProfileConfig),
	}
	if err := unmarshalWithUnusedKeyCheck(k, settings, "settings"); err != nil {
		return nil, err
	}
	return settings, nil
}

// loadVersionedSettingsFileOnly is LoadVersionedSettings restricted to a
// single directory's settings file plus embedded defaults: no project
// layers (no resolveEffectiveProjectPath, no GetProjectConfigDir), no
// SCION_ environment provider, no DB-backed settings overlay. Used only by
// loadGlobalSettingsOnly (round 6 addendum — see LoadGlobalSettings' doc
// comment in settings_v1.go for why the global-only, Layer-0
// server.shared_dir_storage read needs a loader this narrow).
func loadVersionedSettingsFileOnly(dir string) (*VersionedSettings, error) {
	k := koanf.New(".")

	if defaultData, err := GetDefaultSettingsDataYAML(); err == nil {
		_ = k.Load(rawbytes.Provider(defaultData), yaml.Parser())
	}
	if _, err := loadSettingsFile(k, dir); err != nil {
		return nil, err
	}

	settings := &VersionedSettings{
		Runtimes:       make(map[string]V1RuntimeConfig),
		HarnessConfigs: make(map[string]HarnessConfigEntry),
		Profiles:       make(map[string]V1ProfileConfig),
	}
	if err := k.Unmarshal("", settings); err != nil {
		return nil, err
	}
	return settings, nil
}

// loadLegacySettingsFileOnly is LoadSettingsKoanf restricted to a single
// directory's settings file plus embedded defaults: no project layers, no
// SCION_ environment provider. Used only by loadGlobalSettingsOnly (see
// loadVersionedSettingsFileOnly above).
func loadLegacySettingsFileOnly(dir string) (*Settings, error) {
	k := koanf.New(".")

	if defaultData, err := GetDefaultSettingsData(); err == nil {
		_ = k.Load(rawbytes.Provider(defaultData), json.Parser())
	}
	if _, err := loadSettingsFile(k, dir); err != nil {
		return nil, err
	}

	settings := &Settings{
		Runtimes:  make(map[string]RuntimeConfig),
		Harnesses: make(map[string]HarnessConfig),
		Profiles:  make(map[string]ProfileConfig),
	}
	if err := k.Unmarshal("", settings); err != nil {
		return nil, err
	}
	return settings, nil
}

// loadSettingsFile loads settings from a directory, preferring YAML over
// JSON. Before loading a YAML file it migrates any legacy hub.grove_id key
// to hub.project_id in place (see migrateProjectSettingsFile). Whenever that
// migration found a legacy key with no existing hub.project_id, its value is
// also loaded into k directly, whether or not the on-disk rewrite itself
// succeeded: when it did, the value read from the file moments later is
// already identical, so the extra load is a harmless no-op; when it did
// not, it is the only place that value comes from. migratedHubProjectID
// reports whether this call found and processed such a key — callers use
// this to log the one-time precedence-change note right after the file
// that just changed is loaded.
func loadSettingsFile(k *koanf.Koanf, dir string) (migratedHubProjectID bool, err error) {
	yamlPath := filepath.Join(dir, "settings.yaml")
	ymlPath := filepath.Join(dir, "settings.yml")
	jsonPath := filepath.Join(dir, "settings.json")

	load := func(path string, parser koanf.Parser, isYAML bool) (bool, error) {
		var migrated bool
		var override string
		if isYAML {
			migrated, override = migrateProjectSettingsFile(path)
		} else if settingsJSONHasLegacyHubGroveID(path) {
			warnUnmigratedHubGroveID(path, currentProjectMigrationReporter(), "JSON settings are not migrated automatically")
		}
		if isYAML && override == "" {
			if v, ok := unreachableHubGroveIDValue(path, parser); ok {
				// The migrator's own key search walks the raw YAML node
				// tree, which does not resolve a "<<: *anchor" merge key the
				// way koanf's own parse (used for the real k.Load below)
				// does. The value is real and in effect, just invisible to
				// that search, so it is carried the same way an unwritable
				// file's override is: reported once, and used in memory for
				// this invocation so behaviour keeps matching what koanf
				// itself resolves.
				warnUnmigratedHubGroveID(path, currentProjectMigrationReporter(),
					"reached only through a YAML merge key; rename grove_id to project_id in the merged mapping")
				override = v
			}
		}
		if err := k.Load(file.Provider(path), parser); err != nil {
			return false, err
		}
		if override != "" {
			// The override is applied at this file's own layer,
			// unconditionally: it stands in for a real hub.project_id key
			// in this file, so it overrides whatever a less specific (e.g.
			// global) layer set, exactly the way any other project-level
			// setting does.
			_ = k.Load(confmap.Provider(map[string]interface{}{
				projectcompat.ConfigHubProjectIDKey: override,
			}, "."), nil)
		}
		return migrated, nil
	}

	// Try YAML first (.yaml then .yml)
	if _, err := os.Stat(yamlPath); err == nil {
		return load(yamlPath, yaml.Parser(), true)
	}
	if _, err := os.Stat(ymlPath); err == nil {
		return load(ymlPath, yaml.Parser(), true)
	}
	// Fall back to JSON: out of scope for the hub.grove_id rewrite itself
	// (see migrateProjectSettingsFile's doc comment), but still warned
	// about above if the legacy key is present.
	if _, err := os.Stat(jsonPath); err == nil {
		return load(jsonPath, json.Parser(), false)
	}
	return false, nil
}

// unreachableHubGroveIDValue parses path's own content on its own (so YAML
// merge keys are resolved the way koanf's parser resolves them, unlike the
// migrator's raw Node-tree walk) and, if it has hub.grove_id with no
// hub.project_id alongside it, returns that value. ok is false when the
// file has no such value — including a parse error, no legacy key at all,
// or a canonical key already present, in which case there is nothing
// unreachable to report. parser must be yaml.Parser(); the merge-key shape
// this looks for does not exist in JSON.
func unreachableHubGroveIDValue(path string, parser koanf.Parser) (value string, ok bool) {
	scratch := koanf.New(".")
	if err := scratch.Load(file.Provider(path), parser); err != nil {
		return "", false
	}
	legacy := joinKey(hubGroveIDRename[0].parent, hubGroveIDRename[0].legacy)
	canonical := joinKey(hubGroveIDRename[0].parent, hubGroveIDRename[0].canonical)
	if !scratch.Exists(legacy) || scratch.Exists(canonical) {
		return "", false
	}
	return scratch.String(legacy), true
}

// logHubProjectIDPrecedenceChange reports a precedence change: migrating a
// project's own hub.grove_id can newly populate the merged hub.project_id with a
// value that differs from what an already-loaded global settings file
// provided. Before per-file migration, the old merged-config remap would
// have kept the global value in that situation; this reports the change so
// it is never a silent behaviour change. globalValue is the value read from
// k right before the project's own file was loaded — "" means the global
// layer never set hub.project_id, so there is nothing to report. dir is the
// directory whose settings file was just (re)loaded; the file itself
// already exists by the time this runs (loadSettingsFile only calls this
// after a successful load), so GetSettingsPath(dir) resolves it for the
// message.
//
// Reported at most once per process for the same (path, value, other)
// triple — reusing the same per-path dedup state as the migrator's other
// events. Without this, an unwritable project file (its in-memory fallback
// value) would re-report the same precedence change on every single
// settings load for the rest of the process, since the underlying
// hub.grove_id key is never actually removed from disk.
func logHubProjectIDPrecedenceChange(k *koanf.Koanf, dir, globalValue string) {
	if globalValue == "" {
		return
	}
	value := k.String(projectcompat.ConfigHubProjectIDKey)
	if value == "" || value == globalValue {
		return
	}
	path := GetSettingsPath(dir)
	if path == "" {
		path = dir
	}
	st := projectMigrationStateFor(path)
	st.mu.Lock()
	defer st.mu.Unlock()
	if !st.yamlEventOnce("precedence", value+">"+globalValue) {
		return
	}
	currentProjectMigrationReporter().PrecedenceChanged(path, value, globalValue)
}

// getDefaultSettingsYAMLForRuntime generates the default settings YAML with the
// specified runtime for the local profile. The embedded template defaults to
// "container"; if a different runtime is specified, the template is adjusted.
func getDefaultSettingsYAMLForRuntime(targetRuntime string) ([]byte, error) {
	data, err := EmbedsFS.ReadFile("embeds/default_settings.yaml")
	if err != nil {
		return nil, err
	}

	if targetRuntime != "container" {
		data = bytes.Replace(data,
			[]byte("runtime: container  # Auto-adjusted by OS"),
			[]byte(fmt.Sprintf("runtime: %s  # Auto-detected", targetRuntime)),
			1)
	}

	return data, nil
}

// GetDefaultSettingsDataYAML returns the embedded default settings in YAML format.
// This function adjusts the local profile runtime based on the OS. It is used as
// a fallback default for settings loaders; during init, DetectLocalRuntime is used
// instead for actual runtime probing.
func GetDefaultSettingsDataYAML() ([]byte, error) {
	// Cloud Run Instance with sandbox launcher: use the tier-specific
	// defaults so that LoadVersionedSettings never starts from workstation
	// profiles that cannot work on this tier.
	if isCloudRunSandboxEnvironment() {
		return EmbedsFS.ReadFile("embeds/default_settings_cloudrun_sandbox.yaml")
	}

	if goruntime.GOOS != "darwin" {
		return getDefaultSettingsYAMLForRuntime("docker")
	}
	// On macOS, detect the available runtime instead of hardcoding "container".
	// This prevents failures when only podman is installed (no Apple Container CLI).
	detected, err := detectLocalRuntimeOnce()
	if err != nil {
		return getDefaultSettingsYAMLForRuntime("container") // fallback
	}
	return getDefaultSettingsYAMLForRuntime(detected)
}

// GetProjectDefaultSettingsYAML returns the embedded project-level default settings YAML.
// Unlike the full default settings, project settings do not include profiles or runtimes;
// those are managed at the global/broker level (~/.scion/settings.yaml).
func GetProjectDefaultSettingsYAML() ([]byte, error) {

	return EmbedsFS.ReadFile("embeds/default_project_settings.yaml")
}

// GetSettingsPath returns the path to the settings file in a directory,
// preferring YAML over JSON. Returns empty string if no settings file exists.
func GetSettingsPath(dir string) string {
	yamlPath := filepath.Join(dir, "settings.yaml")
	ymlPath := filepath.Join(dir, "settings.yml")
	jsonPath := filepath.Join(dir, "settings.json")

	if _, err := os.Stat(yamlPath); err == nil {
		return yamlPath
	}
	if _, err := os.Stat(ymlPath); err == nil {
		return ymlPath
	}
	if _, err := os.Stat(jsonPath); err == nil {
		return jsonPath
	}
	return ""
}

// GetScionAgentConfigPath returns the path to the scion-agent config file,
// preferring YAML over JSON. Returns empty string if no config file exists.
func GetScionAgentConfigPath(dir string) string {
	yamlPath := filepath.Join(dir, "scion-agent.yaml")
	ymlPath := filepath.Join(dir, "scion-agent.yml")
	jsonPath := filepath.Join(dir, "scion-agent.json")

	if _, err := os.Stat(yamlPath); err == nil {
		return yamlPath
	}
	if _, err := os.Stat(ymlPath); err == nil {
		return ymlPath
	}
	if _, err := os.Stat(jsonPath); err == nil {
		return jsonPath
	}
	return ""
}

// SettingsFileExists checks if a settings file exists in a directory (YAML or JSON)
func SettingsFileExists(dir string) bool {
	return GetSettingsPath(dir) != ""
}

// ScionAgentConfigExists checks if a scion-agent config file exists (YAML or JSON)
func ScionAgentConfigExists(dir string) bool {
	return GetScionAgentConfigPath(dir) != ""
}

// unmarshalWithUnusedKeyCheck unmarshals the koanf instance into the target struct
// and logs a warning for any config keys that do not map to struct fields.
// It uses mapstructure's Metadata to collect unused keys without causing a hard error.
// The label parameter identifies the config source in warning messages (e.g. "settings", "server config").
func unmarshalWithUnusedKeyCheck(k *koanf.Koanf, target interface{}, label string) error {
	var md mapstructure.Metadata
	conf := koanf.UnmarshalConf{
		DecoderConfig: &mapstructure.DecoderConfig{
			DecodeHook: mapstructure.ComposeDecodeHookFunc(
				mapstructure.StringToTimeDurationHookFunc(),
				mapstructure.TextUnmarshallerHookFunc(),
			),
			Metadata:         &md,
			WeaklyTypedInput: true,
			Squash:           true,
		},
	}
	if err := k.UnmarshalWithConf("", target, conf); err != nil {
		return err
	}
	if len(md.Unused) > 0 {
		slog.Warn(label+" file contains unrecognized keys (these will be ignored)",
			"keys", md.Unused)
	}
	return nil
}

// warnIfInRepoHasGlobalKeys emits a warning if an in-repo settings file contains
// profiles or runtimes keys, which are typically managed at the global level.
// Only warns when split storage is active (effectivePath differs from inRepoPath).
func warnIfInRepoHasGlobalKeys(inRepoPath, effectivePath string) {
	if effectivePath == "" || effectivePath == inRepoPath {
		return
	}
	if GetSettingsPath(inRepoPath) == "" {
		return
	}

	probe := koanf.New(".")
	if _, err := loadSettingsFile(probe, inRepoPath); err != nil {
		return
	}
	var keys []string
	if probe.Exists("profiles") {
		keys = append(keys, "profiles")
	}
	if probe.Exists("runtimes") {
		keys = append(keys, "runtimes")
	}
	if len(keys) > 0 {
		fmt.Fprintf(os.Stderr, "Warning: in-repo %s/settings.yaml contains %s; these are typically managed at the global level (~/.scion/settings.yaml).\n",
			inRepoPath, strings.Join(keys, " and "))
	}
}
