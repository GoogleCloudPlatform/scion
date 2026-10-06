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
	"sort"
	"strings"
	"sync"

	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
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
	return loadSettingsKoanf(projectPath, false)
}

// LoadSettingsIgnoringEnvProjectID is LoadSettingsKoanf without the
// SCION_PROJECT_ID / SCION_HUB_PROJECT_ID environment overlay on project_id.
// All other environment variables still apply.
//
// It is for callers that resolve an explicitly named project (the --project
// or --global flag): the project ID must come from that project's own
// settings, not from the environment of the agent container the CLI runs
// in (ptone/scion#3123).
func LoadSettingsIgnoringEnvProjectID(projectPath string) (*Settings, error) {
	return loadSettingsKoanf(projectPath, true)
}

func loadSettingsKoanf(projectPath string, ignoreEnvProjectID bool) (*Settings, error) {
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
	globalHubProjectID := k.String(projectkeys.ConfigHubProjectIDKey)
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
	// Environment variables like SCION_PROJECT, SCION_CREATOR do not map to
	// Settings struct fields and would produce false-positive warnings if the
	// check ran on the merged koanf instance.
	if settingsUnused, err := decodeCollectingUnused(k, settingsProbeStruct(false)); err == nil {
		warnSettingsUnusedKeys(k, settingsUnused, func() bool {
			hasVersioned, _ := detectHierarchyFormat(projectPath)
			return hasVersioned
		}, "settings", settingsHierarchySources(globalDir, projectPath, effectiveProjectPath))
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
		if mapped, ok := projectkeys.EnvProjectIDConfigKey(s, true); ok {
			if ignoreEnvProjectID {
				return ""
			}
			return mapped
		}
		if isSettingsExcludedEnv(s) {
			// SCION_AUTO_EXPOSE_PORTS and SCION_AUTO_EXPOSE_PORTS_LIST are
			// sciontool-only (see settings_v1.go's versionedEnvKeyMapper,
			// which drops them for the same reason). The legacy Settings
			// struct has no colliding field today, but dropping them here
			// too keeps both mappers' exclusions in sync.
			return ""
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
	if k.Exists(projectkeys.ConfigHubProjectIDKey) {
		hubProjectID = k.String(projectkeys.ConfigHubProjectIDKey)
	}

	if hubProjectID != "" {
		_ = k.Load(confmap.Provider(map[string]interface{}{
			projectkeys.ConfigProjectIDKey: hubProjectID,
		}, "."), nil)
		// Also remap to hub.projectId (camelCase) so the legacy
		// HubClientConfig.ProjectID field (koanf tag "projectId") is populated.
		// Without this, GetHubProjectID() returns "" for V1 settings, causing
		// EnsureHubReady to fall back to the local project_id and loop on
		// project registration when the hub project ID differs from the local ID.
		if !k.Exists(projectkeys.ConfigHubProjectIDJSON) {
			_ = k.Load(confmap.Provider(map[string]interface{}{
				projectkeys.ConfigHubProjectIDJSON: hubProjectID,
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
				projectkeys.ConfigProjectIDKey: projectID,
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
	settingsUnused, err := decodeCollectingUnused(k, settings)
	if err != nil {
		return nil, err
	}
	warnSettingsUnusedKeys(k, settingsUnused, func() bool {
		hasVersioned, _ := detectDirSettingsFormat(dir)
		return hasVersioned
	}, "settings", settingsHierarchySources(dir))
	return settings, nil
}

// settingsHierarchySources resolves each directory to its settings file path
// (if any) for the unused-keys warning's dedup key and log message. Empty
// directories and directories with no settings file are omitted; each
// resolved path is made absolute (like serverConfigSources, and without
// symlink resolution, for the same reason) before the dedup check so that a
// relative and absolute spelling of the same directory (e.g. projectPath and
// effectiveProjectPath) collapse to one entry instead of being warned about
// twice.
func settingsHierarchySources(dirs ...string) []string {
	seen := make(map[string]struct{}, len(dirs))
	var out []string
	for _, dir := range dirs {
		if dir == "" {
			continue
		}
		path := GetSettingsPath(dir)
		if path == "" {
			continue
		}
		if abs, err := filepath.Abs(path); err == nil {
			path = abs
		}
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		out = append(out, path)
	}
	return out
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
				projectkeys.ConfigHubProjectIDKey: override,
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
	value := k.String(projectkeys.ConfigHubProjectIDKey)
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
// The label parameter identifies the config source in warning messages (e.g.
// "settings", "server config"); sources are the resolved file path(s) the
// warning applies to, used to scope the once-per-process dedup and named in
// the log message.
//
// This is for config shapes that have exactly one valid schema (e.g.
// GlobalConfig/server config). Settings (settings.yaml) has two — legacy
// Settings and v1 VersionedSettings — and needs warnSettingsUnusedKeys
// instead; see its doc comment for why.
func unmarshalWithUnusedKeyCheck(k *koanf.Koanf, target interface{}, label string, sources []string) error {
	unused, err := decodeCollectingUnused(k, target)
	if err != nil {
		return err
	}
	warnUnusedKeysOnce(label, sources, unused)
	return nil
}

// decodeCollectingUnused decodes the koanf instance into target and returns
// the set of keys mapstructure could not match to a struct field (via
// mapstructure.Metadata), without treating that as a decode error.
func decodeCollectingUnused(k *koanf.Koanf, target interface{}) ([]string, error) {
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
		return nil, err
	}
	return md.Unused, nil
}

// settingsProbeStruct returns a freshly initialized, empty settings struct of
// the shape matching hasVersioned. It exists only to be decoded into so that
// mapstructure.Metadata.Unused reflects which keys that schema recognizes —
// never to hold a real result.
func settingsProbeStruct(hasVersioned bool) interface{} {
	if hasVersioned {
		return &VersionedSettings{
			Runtimes:       make(map[string]V1RuntimeConfig),
			HarnessConfigs: make(map[string]HarnessConfigEntry),
			Profiles:       make(map[string]V1ProfileConfig),
		}
	}
	return &Settings{
		Runtimes:  make(map[string]RuntimeConfig),
		Harnesses: make(map[string]HarnessConfig),
		Profiles:  make(map[string]ProfileConfig),
	}
}

// warnSettingsUnusedKeys warns about unrecognized keys in merged settings.yaml
// data. settingsUnused is the unused-key list from a decode into the legacy
// Settings struct that the CALLER already performed for its own purposes
// (LoadSettingsKoanf's probe decode, or LoadSettingsFromDir's real result
// decode) — this function never decodes into Settings itself, so callers
// never pay for that decode twice.
//
// Settings-unused is always the base: LoadSettingsKoanf seeds the koanf
// instance with embedded defaults in the legacy JSON shape (see
// GetDefaultSettingsData) regardless of the user's own file format, so even a
// purely v1-format user file is decoded from data that is a mix of both
// shapes — Settings alone still correctly recognizes every default-seeded
// legacy key (e.g. "harnesses", converted from the default's v1
// harness_configs).
//
// Settings does not, however, carry the user's own v1-only fields
// (schema_version, top-level server/image_registry, runtimes[*].type,
// profiles[*].image_registry, ...), so when hasVersioned() is true (from
// detectHierarchyFormat or detectDirSettingsFormat — the source is genuinely
// v1-format) a second decode into VersionedSettings cross-checks Settings'
// unused list via combineSettingsUnused: a key survives only if the other
// decode corroborates it, so a real v1 field settings alone can't see is
// dropped, a genuinely unknown key both decodes agree on is kept, and a
// default-seeded legacy-only key VersionedSettings alone can't see (e.g.
// "harnesses") is also dropped. When hasVersioned() is false, the source is
// legacy-format and is never read by anything but the legacy Settings shape
// (LoadEffectiveSettings sends it through LoadSettingsKoanf +
// AdaptLegacySettings, never LoadVersionedSettings), so the cross-check is
// skipped and Settings-unused is reported as-is: a v1-only key mistakenly
// placed in a legacy file is genuinely unused by every loader that will ever
// read it, and must still warn (ptone/scion#2258 round-1 review finding 2).
//
// hasVersioned is a func, not a bool, and is called only when settingsUnused
// is non-empty: detecting the format re-reads the settings file(s) from disk
// (detectHierarchyFormat) or re-reads and re-parses one (detectDirSettingsFormat),
// and settings are loaded from many call sites per CLI invocation, so that
// work — like the VersionedSettings cross-check decode itself — should not
// run on the (overwhelmingly common) path where there is nothing to warn
// about.
func warnSettingsUnusedKeys(k *koanf.Koanf, settingsUnused []string, hasVersioned func() bool, label string, sources []string) {
	if len(settingsUnused) == 0 {
		return
	}

	unused := settingsUnused
	if hasVersioned() {
		if versionedUnused, err := decodeCollectingUnused(k, settingsProbeStruct(true)); err == nil {
			unused = combineSettingsUnused(settingsUnused, versionedUnused)
		}
		// A decode error here is not expected (the same koanf data already
		// decoded cleanly into Settings above), but if it happens, fall back
		// to reporting settingsUnused as-is rather than losing the warning
		// entirely — this never touches or re-decodes the caller's real
		// result.
	}

	warnUnusedKeysOnce(label, sources, unused)
}

// combineSettingsUnused reconciles the unused-key lists from decoding the
// same merged settings data into Settings and into VersionedSettings. A key
// from either list is kept only if the OTHER list corroborates it: contains
// that exact key, or a shorter path prefix of it (see isSettingsKeyPrefix).
//
// This is prefix-aware, not an exact-string intersection, because
// mapstructure reports a whole section a struct entirely lacks as a single
// coarse key: legacy Settings has no top-level `server` field at all, so a
// v1 file's `server.brokr` typo shows up against Settings as bare "server"
// covering the whole subtree, never as "server.brokr". An exact intersection
// of that bare "server" against VersionedSettings' fine-grained
// "server.brokr" is never equal, so the typo would silently vanish. Prefix
// matching finds the corroboration instead: "server" is a path-prefix of
// "server.brokr", so "server.brokr" — the more precise of the two — is kept,
// and the coarse "server" is not (nothing in the other list is "server" or a
// prefix of the bare word "server", so it fails its own corroboration check).
// The result never reports both the coarse parent and the precise child for
// the same miss.
func combineSettingsUnused(settingsUnused, versionedUnused []string) []string {
	corroborated := func(candidates, other []string) []string {
		var kept []string
		for _, key := range candidates {
			for _, o := range other {
				if key == o || isSettingsKeyPrefix(o, key) {
					kept = append(kept, key)
					break
				}
			}
		}
		return kept
	}

	combined := corroborated(settingsUnused, versionedUnused)
	combined = append(combined, corroborated(versionedUnused, settingsUnused)...)

	seen := make(map[string]struct{}, len(combined))
	var deduped []string
	for _, key := range combined {
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		deduped = append(deduped, key)
	}
	return deduped
}

// isSettingsKeyPrefix reports whether prefix is a path-prefix of key in
// mapstructure's unused-key naming: equal, or immediately followed by "."
// (nested struct field) or "[" (map-of-struct entry) — e.g. "server" is a
// path-prefix of "server.brokr", and "harness_configs" is a path-prefix of
// "harness_configs[x].imagee".
func isSettingsKeyPrefix(prefix, key string) bool {
	if prefix == key {
		return true
	}
	if !strings.HasPrefix(key, prefix) {
		return false
	}
	switch key[len(prefix)] {
	case '.', '[':
		return true
	default:
		return false
	}
}

// warnedUnusedKeys records which (label, sources, unused keys) combinations
// have already been warned about in this process. Settings are loaded
// repeatedly within a single CLI invocation (once per call site that needs
// them), and a broker or hub may load settings for many projects in one
// process, so warning on every load would repeat the same message for one
// underlying file, or suppress a genuinely distinct file that happens to
// share another file's unknown-key set.
var warnedUnusedKeys sync.Map

// resetWarnedUnusedKeysCache clears the unused-keys warning dedup cache. Tests
// that assert whether this warning fires must call this first, so that state
// left behind by an earlier test in the same process cannot suppress it.
// Clear (not reassignment) keeps this safe against a concurrent LoadOrStore
// from another test or a leftover background goroutine.
func resetWarnedUnusedKeysCache() {
	warnedUnusedKeys.Clear()
}

// warnUnusedKeysOnce logs the unrecognized-keys warning the first time a
// given label and source path set reports a given set of unused keys. A
// later load that turns up a different set of unknown keys (e.g. after an
// edit), or the same keys from a different source, warns again.
func warnUnusedKeysOnce(label string, sources []string, keys []string) {
	if len(keys) == 0 {
		return
	}
	sortedKeys := append([]string(nil), keys...)
	sort.Strings(sortedKeys)
	sortedSources := append([]string(nil), sources...)
	sort.Strings(sortedSources)
	dedupKey := label + "\x00" + strings.Join(sortedSources, ",") + "\x00" + strings.Join(sortedKeys, ",")
	if _, seen := warnedUnusedKeys.LoadOrStore(dedupKey, struct{}{}); seen {
		return
	}
	slog.Warn(label+" file contains unrecognized keys (these will be ignored)",
		"keys", keys, "path", strings.Join(sources, ", "))
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
