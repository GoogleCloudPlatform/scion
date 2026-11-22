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

package runtime

import (
	"slices"
	"sort"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/envpolicy"
)

// isHarnessTemplateEnvKey reports whether a key returned by Harness.GetEnv
// originates from a harness-config env_template layer rather than the
// harness-authored SCION_AGENT_NAME identity variable.
func isHarnessTemplateEnvKey(name string) bool {
	return name != "" && name != "SCION_AGENT_NAME"
}

// harnessEnvEntry pairs a rendered harness environment entry with whether its
// winning value came from Harness.GetEnv (env_template) rather than
// Harness.GetTelemetryEnv or SCION_AGENT_NAME.
type harnessEnvEntry struct {
	Key          string
	Value        string
	FromTemplate bool
}

// harnessEnvEntriesWithSource returns the sorted, non-empty harness env entries
// (GetEnv, then GetTelemetryEnv when TelemetryEnabled) together with per-key
// template provenance so runtimes record withholding from the exact entries they
// emit.
func harnessEnvEntriesWithSource(cfg RunConfig, agentHome string) []harnessEnvEntry {
	if cfg.Harness == nil {
		return nil
	}
	env := make(map[string]harnessEnvEntry)
	for k, v := range cfg.Harness.GetEnv(cfg.Name, agentHome, cfg.UnixUsername) {
		if v != "" {
			env[k] = harnessEnvEntry{
				Key:          k,
				Value:        v,
				FromTemplate: isHarnessTemplateEnvKey(k),
			}
		}
	}
	if cfg.TelemetryEnabled {
		for k, v := range cfg.Harness.GetTelemetryEnv() {
			if v != "" {
				env[k] = harnessEnvEntry{
					Key:          k,
					Value:        v,
					FromTemplate: false,
				}
			}
		}
	}
	names := make([]string, 0, len(env))
	for k := range env {
		names = append(names, k)
	}
	slices.Sort(names)
	entries := make([]harnessEnvEntry, 0, len(names))
	for _, k := range names {
		entries = append(entries, env[k])
	}
	return entries
}

// hookWithheldTracker tracks, during a runtime's own container environment
// assembly, whether each emitted environment variable's final winning value
// came from a harness-config layer. Recording happens at the exact emission
// site in the exact emission order, so SCION_HOOK_WITHHELD_ENV is derived from
// the same source as container emission and cannot drift from it.
type hookWithheldTracker struct {
	configHC map[string]struct{} // keys in cfg.Env that came from a harness-config layer
	withheld map[string]bool     // current winning withheld state per emitted key
}

// newHookWithheldTracker initializes a tracker from cfg. Keys in
// cfg.HookWithheldEnv that also appear in cfg.Env mark those cfg.Env entries as
// harness-config-sourced (subject to being overridden if a higher-precedence
// non-harness-config layer emits the same key). Keys in cfg.HookWithheldEnv
// that are not in cfg.Env are seeded as withheld unless overridden by a
// non-harness-config layer emitted by the runtime.
func newHookWithheldTracker(cfg RunConfig) *hookWithheldTracker {
	envKeys := make(map[string]struct{}, len(cfg.Env))
	for _, e := range cfg.Env {
		k, _, _ := strings.Cut(e, "=")
		if k != "" {
			envKeys[k] = struct{}{}
		}
	}

	t := &hookWithheldTracker{
		configHC: make(map[string]struct{}, len(cfg.HookWithheldEnv)),
		withheld: make(map[string]bool, len(cfg.HookWithheldEnv)),
	}
	for _, raw := range cfg.HookWithheldEnv {
		k := strings.TrimSpace(raw)
		if k == "" || reservedAgentEnvName(k) {
			continue
		}
		if _, inEnv := envKeys[k]; inEnv {
			t.configHC[k] = struct{}{}
		} else {
			t.withheld[k] = true
		}
	}
	return t
}

// IsConfigHC reports whether key was marked by the broker as coming from a
// harness-config layer in RunConfig.Env.
func (t *hookWithheldTracker) IsConfigHC(key string) bool {
	if len(t.configHC) == 0 {
		return false
	}
	_, ok := t.configHC[key]
	return ok
}

// Record records that key was emitted by the runtime, with withheld indicating
// whether this emission came from a harness-config layer. Later calls overwrite
// earlier ones, matching last-wins container env semantics.
func (t *hookWithheldTracker) Record(key string, withheld bool) {
	if key == "" || reservedAgentEnvName(key) {
		return
	}
	t.withheld[key] = withheld
}

// Delete removes key from the tracker when the runtime removes an emitted key.
func (t *hookWithheldTracker) Delete(key string) {
	delete(t.withheld, key)
}

// DeleteReserved removes all reserved agent env names from the tracker.
func (t *hookWithheldTracker) DeleteReserved() {
	for k := range t.withheld {
		if reservedAgentEnvName(k) {
			delete(t.withheld, k)
		}
	}
}

// WithheldKeys returns the sorted list of environment variable names whose
// final winning container value came from a harness-config layer.
func (t *hookWithheldTracker) WithheldKeys() []string {
	if len(t.withheld) == 0 {
		return nil
	}
	var out []string
	for k, w := range t.withheld {
		if w && !reservedAgentEnvName(k) {
			out = append(out, k)
		}
	}
	if len(out) == 0 {
		return nil
	}
	sort.Strings(out)
	return out
}

// Marker formats the final SCION_HOOK_WITHHELD_ENV marker value.
func (t *hookWithheldTracker) Marker() string {
	return envpolicy.FormatWithheldMarker(t.WithheldKeys())
}
