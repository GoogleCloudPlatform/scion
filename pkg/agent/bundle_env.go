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

package agent

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"sort"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/envpolicy"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

// BundleEnvLayer evaluates a harness-config bundle's env map as a literal,
// per-start layer shared by both launch (buildAgentEnvWithBundle) and the
// runtime broker preflight (extractRequiredEnvKeys).
//
// Rules:
//   - Keys and values are used verbatim: no ${VAR} expansion and no
//     empty-value broker-host passthrough.
//   - Invalid keys (empty, containing "=" or ",", or having leading/trailing
//     whitespace) are ignored with a warning.
//   - Reserved SCION_* keys are refused with a warning; grandfathered
//     LegacyHarnessHints keys are rewritten to HARNESS_HINT_* with a
//     deprecation warning unless the explicit hint key is already declared.
//   - A key or value containing "${" or "$" followed by an ASCII letter is
//     kept verbatim and produces a start warning ("harness-config env values
//     are not expanded").
//   - Empty values are preserved in entries so higher layers (hub env/secrets,
//     inline, settings) win on conflict and otherwise launch/preflight report
//     the key as missing without reading the broker host environment.
func BundleEnvLayer(raw map[string]string) (entries map[string]string, warnings []string) {
	if len(raw) == 0 {
		return nil, nil
	}

	keys := make([]string, 0, len(raw))
	for k := range raw {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	out := make(map[string]string, len(raw))
	for _, k := range keys {
		v := raw[k]
		if !envpolicy.ValidEnvName(k) {
			warnings = append(warnings, fmt.Sprintf(
				"Warning: harness-config bundle env key %q is invalid and was ignored.",
				k,
			))
			continue
		}

		effectiveKey, disp := envpolicy.ClassifyHarnessConfigEnvKey(k)
		switch disp {
		case envpolicy.HarnessConfigKeyRefuse:
			warnings = append(warnings, fmt.Sprintf(
				"Warning: harness-config bundle env key %q is reserved (%s* keys are not allowed in harness configs) and was ignored.",
				k, envpolicy.ReservedHarnessConfigPrefix,
			))
			continue
		case envpolicy.HarnessConfigKeyRewrite:
			warnings = append(warnings, fmt.Sprintf(
				"Warning: harness-config bundle env key %q is deprecated and was rewritten to %q.",
				k, effectiveKey,
			))
			if _, explicit := raw[effectiveKey]; explicit {
				continue
			}
			k = effectiveKey
		}

		if containsEnvVarSyntax(k) || containsEnvVarSyntax(v) {
			warnings = append(warnings, fmt.Sprintf(
				"Warning: harness-config env key %q contains variable syntax; harness-config env values are not expanded and are used literally.",
				k,
			))
		}
		out[k] = v
	}

	if len(out) == 0 {
		return nil, warnings
	}
	return out, warnings
}

// containsEnvVarSyntax reports whether s contains "${" or "$" followed by an
// ASCII letter, which indicates shell/env variable expansion syntax.
func containsEnvVarSyntax(s string) bool {
	for i := 0; i < len(s)-1; i++ {
		if s[i] != '$' {
			continue
		}
		next := s[i+1]
		if next == '{' || (next >= 'A' && next <= 'Z') || (next >= 'a' && next <= 'z') {
			return true
		}
	}
	return false
}

// withOwnEnv returns a shallow copy of cfg whose Env is a clone of ownEnv
// (or nil when ownEnv is empty). Nil cfg yields nil.
func withOwnEnv(cfg *api.ScionConfig, ownEnv map[string]string) *api.ScionConfig {
	if cfg == nil {
		return nil
	}
	out := *cfg
	if len(ownEnv) == 0 {
		out.Env = nil
	} else {
		out.Env = maps.Clone(ownEnv)
	}
	return &out
}

// readPersistedAgentEnv reads the Env map from the agent's persisted
// scion-agent.json in agentDir. ok is false when the file cannot be read or
// parsed.
func readPersistedAgentEnv(agentDir string) (env map[string]string, ok bool) {
	if agentDir == "" {
		return nil, false
	}
	data, err := os.ReadFile(filepath.Join(agentDir, "scion-agent.json"))
	if err != nil {
		return nil, false
	}
	var cfg api.ScionConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, false
	}
	return maps.Clone(cfg.Env), true
}

// demoteLegacyHarnessConfigEnv returns cfg for this start's env assembly with
// every persisted Env key whose value equals the live raw or sanitized bundle
// env value, or the live raw or sanitized settings harness-config env value,
// removed so legacy scion-agent.json files that still contain baked-in
// harness-config env (including legacy SCION_* keys that are now refused or
// rewritten to HARNESS_HINT_*) defer to the live per-start layers. Nothing on
// disk changes; cfg itself is returned when no key is demoted.
func demoteLegacyHarnessConfigEnv(cfg *api.ScionConfig, rawBundleEnv, bundleEnv, rawSettingsHCEnv, settingsHCEnv map[string]string) (*api.ScionConfig, []string) {
	if cfg == nil || len(cfg.Env) == 0 || (len(rawBundleEnv) == 0 && len(bundleEnv) == 0 && len(rawSettingsHCEnv) == 0 && len(settingsHCEnv) == 0) {
		return cfg, nil
	}
	matchesLayer := func(k, v string) bool {
		if rv, ok := rawBundleEnv[k]; ok && rv == v {
			return true
		}
		if bv, ok := bundleEnv[k]; ok && bv == v {
			return true
		}
		if rv, ok := rawSettingsHCEnv[k]; ok && rv == v {
			// Only demote via rawSettingsHCEnv when the key was refused or
			// rewritten by ClassifyHarnessConfigEnvKey (legacy SCION_* keys);
			// ordinary keys (and broker-mode hub-only keys like TZ) defer to
			// settingsHCEnv so broker-mode config-layer drop accounting stays intact.
			if _, disp := envpolicy.ClassifyHarnessConfigEnvKey(k); disp != envpolicy.HarnessConfigKeyAllow {
				return true
			}
		}
		if sv, ok := settingsHCEnv[k]; ok && sv == v {
			return true
		}
		// Shipped bundles that already migrated from a grandfathered SCION_*
		// name to its HARNESS_HINT_* name: demote the legacy SCION_* key when
		// the live bundle or settings layer carries the matching hint value.
		if hint, ok := envpolicy.LegacyHarnessHint(k); ok {
			if bv, ok := bundleEnv[hint]; ok && bv == v {
				return true
			}
			if sv, ok := settingsHCEnv[hint]; ok && sv == v {
				return true
			}
		}
		return false
	}
	var demote []string
	for k, v := range cfg.Env {
		if matchesLayer(k, v) {
			demote = append(demote, k)
		}
	}
	if len(demote) == 0 {
		return cfg, nil
	}
	out := *cfg
	out.Env = make(map[string]string, len(cfg.Env)-len(demote))
	for k, v := range cfg.Env {
		out.Env[k] = v
	}
	for _, k := range demote {
		delete(out.Env, k)
	}
	sort.Strings(demote)
	util.Debugf("harness-config env: demoted legacy stored env key(s) %v to live per-start layers", demote)
	return &out, demote
}
