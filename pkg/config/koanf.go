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

// dropInheritedHubProjectID removes the global layer's hub project ID from k
// when the load is for a project other than the global one, that is, when a
// project settings layer (the in-repo .scion directory, or the external
// directory of a git project with split storage) follows. Call it right after
// the global layer is loaded.
//
// A project's hub project ID comes only from its own settings files, falling
// back to its local project_id. Inheriting the global value would send a
// project whose own settings have no hub.project_id (for example after 'scion
// hub unlink' and 'scion hub link') to the hub project the global settings
// are linked to. The global project itself keeps the value.
func dropInheritedHubProjectID(k *koanf.Koanf, projectPath, effectiveProjectPath, globalDir string) {
	if !loadsProjectLayer(projectPath, effectiveProjectPath, globalDir) {
		return
	}
	k.Delete(projectkeys.ConfigHubProjectIDKey)
	k.Delete(projectkeys.ConfigHubProjectIDJSON)
}

// loadsProjectLayer reports whether a settings load for projectPath merges a
// project layer above the global one. It follows the in-repo and external
// project steps of LoadVersionedSettings, but compares
// against the global directory by resolved path: FindProjectRoot returns a
// symlink-resolved path, so a working directory under a symlinked $HOME still
// names the global project. The "global" and "home" aliases name the global
// project.
func loadsProjectLayer(projectPath, effectiveProjectPath, globalDir string) bool {
	if projectPath == "global" || projectPath == "home" {
		return false
	}
	isProject := func(p string) bool { return p != "" && !sameDirForComparison(p, globalDir) }
	return isProject(projectPath) || (effectiveProjectPath != projectPath && isProject(effectiveProjectPath))
}

// sameDirForComparison reports whether a and b name the same directory,
// literally or after resolving symlinks (see resolvePathForComparison). An
// empty b never matches a non-empty a.
func sameDirForComparison(a, b string) bool {
	if a == b {
		return true
	}
	if a == "" || b == "" {
		return false
	}
	return resolvePathForComparison(a) == resolvePathForComparison(b)
}

// LoadSettingsFromDir loads settings from a single directory's settings file
// without applying embedded defaults, global settings, or environment variables.
// This is useful when you need to read just one project's settings file in isolation,
// for example to get the project's hub.endpoint without the broker's own env vars
// overriding it.
//
// The result is the read-only Settings view of the versioned loader
// (settingsViewOf) for that one file: a legacy-format file is converted to
// the v1 shape first, and a v1 file's hub.project_id is carried to
// Hub.ProjectID (ptone/scion#5427). No environment variable is read.
func LoadSettingsFromDir(dir string) (*Settings, error) {
	s, err := loadSingleDirSettingsView(dir)
	if err != nil {
		return nil, err
	}
	// The unused-key warning reads the file as written. It is only a
	// warning: a file the legacy probe cannot decode gets none, and the
	// load does not fail for it.
	raw := koanf.New(".")
	if _, err := loadSettingsFile(raw, dir); err == nil {
		if settingsUnused, err := decodeCollectingUnused(raw, settingsProbeStruct(false)); err == nil {
			warnSettingsUnusedKeys(raw, settingsUnused, func() bool {
				hasVersioned, _ := detectDirSettingsFormat(dir)
				return hasVersioned
			}, "settings", settingsHierarchySources(dir))
		}
	}
	return s, nil
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
//
// A legacy-format file is converted to the v1 shape (see
// loadLegacySettingsLayer); the conversion's deprecation warnings are
// returned.
func loadVersionedSettingsFileOnly(dir string) (*VersionedSettings, []string, error) {
	k := koanf.New(".")

	if defaultData, err := GetDefaultSettingsDataYAML(); err == nil {
		_ = k.Load(rawbytes.Provider(defaultData), yaml.Parser())
	}
	legacy := &legacyLayerState{}
	if _, err := loadSettingsFileConverting(k, dir, legacy); err != nil {
		return nil, nil, err
	}
	warnings := legacy.warnings

	settings := &VersionedSettings{
		Runtimes:       make(map[string]V1RuntimeConfig),
		HarnessConfigs: make(map[string]HarnessConfigEntry),
		Profiles:       make(map[string]V1ProfileConfig),
	}
	if err := unmarshalVersionedSettings(k, settings); err != nil {
		return nil, nil, err
	}
	return settings, warnings, nil
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
//
// The file is loaded as written. The settings loaders use
// loadSettingsFileConverting instead, which also converts a legacy-format
// file to the v1 shape.
func loadSettingsFile(k *koanf.Koanf, dir string) (migratedHubProjectID bool, err error) {
	return loadSettingsFileConverting(k, dir, nil)
}

// loadSettingsFileConverting is loadSettingsFile for the settings loaders.
// When legacy is not nil, a file without schema_version is converted to the
// v1 shape at its own layer (loadLegacySettingsLayer) before it is merged,
// and legacy records what the conversion found. A versioned file is loaded
// as written.
func loadSettingsFileConverting(k *koanf.Koanf, dir string, legacy *legacyLayerState) (migratedHubProjectID bool, err error) {
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
		if legacy != nil {
			if err := loadLegacySettingsLayer(k, path, parser, legacy); err != nil {
				return false, err
			}
		} else if err := k.Load(file.Provider(path), parser); err != nil {
			return false, err
		}
		if override != "" && legacy != nil {
			legacy.hubIDFromCamel = false
			legacy.touch(projectkeys.ConfigHubProjectIDKey)
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

// warnSettingsUnusedKeys warns about unrecognized keys in the settings.yaml
// data LoadSettingsFromDir reads as written. settingsUnused is the
// unused-key list of the caller's decode of that data into the legacy
// Settings probe, so this function never decodes into Settings itself.
//
// Settings does not carry the v1-only fields (schema_version, top-level
// server/image_registry, runtimes[*].type, profiles[*].image_registry, ...),
// so when hasVersioned() is true (the source is v1-format) a second decode
// into VersionedSettings cross-checks Settings' unused list via
// combineSettingsUnused: a key survives only if both decodes leave it
// unused. When hasVersioned() is false the list is reported as-is
// (ptone/scion#2258 round-1 review finding 2).
//
// The settings loaders (LoadVersionedSettings' layers) warn on their own,
// from a VersionedSettings decode.
//
// hasVersioned is a func, not a bool, and is called only when settingsUnused
// is non-empty: detecting the format re-reads the settings file from disk,
// which should not happen on the (overwhelmingly common) path where there is
// nothing to warn about.
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
