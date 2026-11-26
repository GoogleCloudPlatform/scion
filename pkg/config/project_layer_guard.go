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
	"log/slog"
	"reflect"
	"sort"
	"strings"
	"sync"

	"github.com/knadh/koanf/providers/confmap"
	"github.com/knadh/koanf/v2"
)

// rootClass classifies a top-level VersionedSettings root key for project-tier
// settings files (.scion/settings.* and external split-storage project dirs).
// The zero value is classDeny so any unknown or unlisted root is denied by
// default.
type rootClass int

const (
	classDeny rootClass = iota
	classAllow
)

// projectTierRoots classifies every exported top-level koanf root of
// VersionedSettings. Only classAllow roots survive project-tier settings
// layers; classDeny roots (and any unknown root) are ignored with a deduped
// warning. TestProjectTierRootsClassifyEveryField enforces that every
// VersionedSettings field is classified here.
var projectTierRoots = map[string]rootClass{
	// Project identity and format.
	"schema_version": classAllow,
	"project_type":   classAllow,
	"project_id":     classAllow,

	// Project-to-Hub link (hub.token, hub.apiKey, hub.lastSyncedAt are already
	// refused on write and never read from files).
	"hub": classAllow,

	// Agent defaults read in the project context.
	"active_profile":          classAllow,
	"default_template":        classAllow,
	"default_harness_config":  classAllow,
	"default_model":           classAllow,
	"default_thinking_level":  classAllow,
	"default_max_turns":       classAllow,
	"default_max_model_calls": classAllow,
	"default_max_duration":    classAllow,
	"default_resources":       classAllow,

	// Project runtime, profile, and harness configuration.
	"runtimes":        classAllow,
	"runtime":         classAllow,
	"profiles":        classAllow,
	"harness_configs": classAllow,

	// Per-project images, telemetry, CLI UX, workspace, and local experiments.
	"image_registry":    classAllow,
	"image_tag":         classAllow,
	"telemetry":         classAllow,
	"cli":               classAllow,
	"workspace_path":    classAllow,
	"shared_dirs":       classAllow,
	"local_experiments": classAllow,

	// Denied: operator/server scope, host credentials, and Hub-level policy/defaults.
	"server":                                  classDeny,
	"hub_connections":                         classDeny,
	"use_host_credentials":                    classDeny,
	"auto_inject_gcloud_adc":                  classDeny,
	"managed_agents":                          classDeny,
	"quotas":                                  classDeny,
	"agent_secrets":                           classDeny,
	"broker_policy":                           classDeny,
	"project_defaults":                        classDeny,
	"auto_expose_ports":                       classDeny,
	"default_runtime_broker":                  classDeny,
	"default_timezone":                        classDeny,
	"default_gcp_identity_mode":               classDeny,
	"default_gcp_identity_service_account_id": classDeny,
	"default_max_agent_role":                  classDeny,
	"default_agent_role":                      classDeny,
}

// projectTierAllowed reports whether a top-level settings root is allowed in
// project-tier settings files. Matching is case-insensitive (to align with
// koanf/mapstructure decoding) and unknown roots return false (deny-by-default).
func projectTierAllowed(root string) bool {
	return projectTierRoots[strings.ToLower(root)] == classAllow
}

// projectLayerGuard snapshots denied top-level roots (and their source
// attributions) before project-tier settings files are merged into a koanf
// instance, then removes any project-supplied denied or unknown roots and
// restores the pre-project values.
type projectLayerGuard struct {
	// snapRaw holds deep-copied values of denied roots present before project
	// layers loaded (maps, slices, or scalars).
	snapRaw map[string]any
	// snapLeaves holds deep-copied leaf values under denied roots before
	// project layers loaded, used to compute which leaf paths changed.
	snapLeaves map[string]any
	// snapSources holds source attributions of keys under denied roots before
	// project layers loaded.
	snapSources map[string]string
	// restoreRootFn defaults to defaultRestoreProjectRoot and may be
	// overridden in tests to exercise restore-failure handling.
	restoreRootFn func(k *koanf.Koanf, root string, val any) error
}

// guardProjectLayers captures every non-allowed top-level root currently in k
// (defaults and global layers) along with its source attribution.
func guardProjectLayers(k *koanf.Koanf, sources map[string]string) *projectLayerGuard {
	g := &projectLayerGuard{
		snapRaw:     make(map[string]any),
		snapLeaves:  make(map[string]any),
		snapSources: make(map[string]string),
	}
	raw := k.Raw()
	for root := range topLevelRoots(k, raw) {
		if projectTierAllowed(root) {
			continue
		}
		if val, ok := raw[root]; ok {
			g.snapRaw[root] = deepCopyKoanfValue(val)
		}
	}
	for _, key := range k.Keys() {
		root := rootOfKey(key)
		if projectTierAllowed(root) {
			continue
		}
		g.snapLeaves[key] = deepCopyKoanfValue(k.Get(key))
	}
	for key, src := range sources {
		if !projectTierAllowed(rootOfKey(key)) {
			g.snapSources[key] = src
		}
	}
	return g
}

// restore drops everything project layers added or changed under non-allowed
// roots, restores the pre-project snapshot, and emits one deduped warning
// listing the ignored key paths (never their values). Any restore error is
// returned so the caller fails closed.
func (g *projectLayerGuard) restore(k *koanf.Koanf, sources map[string]string, files []string) error {
	raw := k.Raw()
	roots := topLevelRoots(k, raw)
	// Also include any root present in snapRaw in case a project layer somehow
	// removed it.
	for root := range g.snapRaw {
		roots[root] = struct{}{}
	}

	keysByRoot := make(map[string][]string)
	for _, key := range k.Keys() {
		root := rootOfKey(key)
		if !projectTierAllowed(root) {
			keysByRoot[root] = append(keysByRoot[root], key)
		}
	}

	var droppedKeys []string
	// Process roots in deterministic order.
	sortedRoots := make([]string, 0, len(roots))
	for root := range roots {
		if !projectTierAllowed(root) {
			sortedRoots = append(sortedRoots, root)
		}
	}
	sort.Strings(sortedRoots)

	restoreFn := g.restoreRootFn
	if restoreFn == nil {
		restoreFn = defaultRestoreProjectRoot
	}

	for _, root := range sortedRoots {
		curVal, hasCur := raw[root]
		snapVal, hasSnap := g.snapRaw[root]
		if hasCur && (!hasSnap || !reflect.DeepEqual(curVal, snapVal)) {
			leaves := keysByRoot[root]
			if len(leaves) == 0 {
				droppedKeys = append(droppedKeys, root)
			} else {
				for _, leaf := range leaves {
					oldVal, existed := g.snapLeaves[leaf]
					if !existed || !reflect.DeepEqual(k.Get(leaf), oldVal) {
						droppedKeys = append(droppedKeys, leaf)
					}
				}
			}
		}

		k.Delete(root)
		for src := range sources {
			if src == root || strings.HasPrefix(src, root+".") {
				delete(sources, src)
			}
		}

		if hasSnap {
			if err := restoreFn(k, root, deepCopyKoanfValue(snapVal)); err != nil {
				return fmt.Errorf("restoring %s: %w", root, err)
			}
			if sources != nil {
				for key, src := range g.snapSources {
					if key == root || strings.HasPrefix(key, root+".") {
						sources[key] = src
					}
				}
			}
		}
	}

	warnProjectGuardKeysOnce(files, droppedKeys)
	return nil
}

// stripDeniedProjectRoots removes every non-allowed top-level root from a
// single-file project-tier koanf instance (used by LoadProjectSettingsFromDir)
// and emits the same deduped warning.
func stripDeniedProjectRoots(k *koanf.Koanf, files []string) {
	raw := k.Raw()
	roots := topLevelRoots(k, raw)
	keysByRoot := make(map[string][]string)
	for _, key := range k.Keys() {
		root := rootOfKey(key)
		if !projectTierAllowed(root) {
			keysByRoot[root] = append(keysByRoot[root], key)
		}
	}

	var droppedKeys []string
	sortedRoots := make([]string, 0, len(roots))
	for root := range roots {
		if !projectTierAllowed(root) {
			sortedRoots = append(sortedRoots, root)
		}
	}
	sort.Strings(sortedRoots)

	for _, root := range sortedRoots {
		leaves := keysByRoot[root]
		if len(leaves) == 0 {
			droppedKeys = append(droppedKeys, root)
		} else {
			droppedKeys = append(droppedKeys, leaves...)
		}
		k.Delete(root)
	}

	warnProjectGuardKeysOnce(files, droppedKeys)
}

func defaultRestoreProjectRoot(k *koanf.Koanf, root string, val any) error {
	return k.Load(confmap.Provider(map[string]any{root: val}, ""), nil)
}

func topLevelRoots(k *koanf.Koanf, raw map[string]any) map[string]struct{} {
	roots := make(map[string]struct{}, len(raw))
	for root := range raw {
		roots[root] = struct{}{}
	}
	for _, key := range k.Keys() {
		roots[rootOfKey(key)] = struct{}{}
	}
	return roots
}

func rootOfKey(key string) string {
	if i := strings.IndexByte(key, '.'); i >= 0 {
		return key[:i]
	}
	return key
}

func deepCopyKoanfValue(v any) any {
	switch tv := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(tv))
		for k, val := range tv {
			out[k] = deepCopyKoanfValue(val)
		}
		return out
	case []any:
		out := make([]any, len(tv))
		for i, val := range tv {
			out[i] = deepCopyKoanfValue(val)
		}
		return out
	default:
		return v
	}
}

const maxProjectGuardWarnKeys = 20

const projectGuardWarnMessage = "project settings set keys that are not read from project settings (project settings are moving to the Hub); ignored"

var warnedProjectGuardKeys sync.Map

func resetWarnedProjectGuardKeysCache() {
	warnedProjectGuardKeys.Clear()
}

func warnProjectGuardKeysOnce(files, keys []string) {
	if len(keys) == 0 {
		return
	}
	sortedKeys := append([]string(nil), keys...)
	sort.Strings(sortedKeys)
	sortedKeys = dedupeStrings(sortedKeys)

	sortedFiles := append([]string(nil), files...)
	sort.Strings(sortedFiles)
	sortedFiles = dedupeStrings(sortedFiles)

	dedupKey := strings.Join(sortedFiles, ",") + "\x00" + strings.Join(sortedKeys, ",")
	if _, seen := warnedProjectGuardKeys.LoadOrStore(dedupKey, struct{}{}); seen {
		return
	}

	displayKeys := sortedKeys
	if len(displayKeys) > maxProjectGuardWarnKeys {
		extra := len(displayKeys) - maxProjectGuardWarnKeys
		displayKeys = append(append([]string(nil), displayKeys[:maxProjectGuardWarnKeys]...), fmt.Sprintf("+%d more", extra))
	}
	slog.Warn(projectGuardWarnMessage, "keys", displayKeys, "files", sortedFiles)
}
