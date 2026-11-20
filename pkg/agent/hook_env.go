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
	"fmt"
	"sort"

	"github.com/GoogleCloudPlatform/scion/pkg/envpolicy"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

// sanitizeHarnessConfigEnv filters an environment map from a harness-config
// layer (bundle env, bundle env_template, or settings/profile
// harness_configs.<name>.env) using envpolicy.ClassifyHarnessConfigEnvKey:
//
//   - Reserved SCION_* keys are stripped and produce a start warning.
//   - Grandfathered LegacyHarnessHints keys are rewritten to their
//     HARNESS_HINT_* equivalents with a deprecation warning, unless the explicit
//     hint key is already declared in raw.
//   - All other keys are kept verbatim.
//
// When expandKey is true, keys are also checked after ExpandEnvRefusing so a
// key containing ${...} cannot expand into a reserved SCION_* name.
func sanitizeHarnessConfigEnv(raw map[string]string, layerName string, expandKey bool) (sanitized map[string]string, warnings []string) {
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
		checkKey := k
		if expandKey {
			if exp, _, _ := util.ExpandEnvRefusing(k, refuseHostEnvForAgent); exp != "" {
				checkKey = exp
			}
		}
		if !envpolicy.ValidEnvName(checkKey) {
			warnings = append(warnings, fmt.Sprintf(
				"Warning: harness-config %s env key %q is invalid and was ignored.",
				layerName, checkKey,
			))
			continue
		}
		effectiveKey, disp := envpolicy.ClassifyHarnessConfigEnvKey(checkKey)
		switch disp {
		case envpolicy.HarnessConfigKeyRefuse:
			warnings = append(warnings, fmt.Sprintf(
				"Warning: harness-config %s env key %q is reserved (%s* keys are not allowed in harness configs) and was ignored.",
				layerName, checkKey, envpolicy.ReservedHarnessConfigPrefix,
			))
		case envpolicy.HarnessConfigKeyRewrite:
			warnings = append(warnings, fmt.Sprintf(
				"Warning: harness-config %s env key %q is deprecated and was rewritten to %q.",
				layerName, checkKey, effectiveKey,
			))
			if _, explicit := raw[effectiveKey]; !explicit {
				out[effectiveKey] = raw[k]
			}
		default:
			out[k] = raw[k]
		}
	}

	if len(out) == 0 {
		return nil, warnings
	}
	return out, warnings
}

// envOrigin identifies which layer supplied the winning value of a variable in
// trackedAgentEnv. Every write to trackedAgentEnv must name its origin so no
// key can enter RunConfig.Env without its harness-config provenance recorded.
type envOrigin uint8

const (
	envOriginNone envOrigin = iota
	envOriginBundle
	envOriginSettingsHC
	envOriginNoAuthHC
	envOriginLegacyHC
	envOriginConfigOwn
	envOriginCaller
	envOriginDefault
	envOriginBrokerSystem
)

// isHarnessConfig reports whether an envOrigin represents a harness-config
// layer whose keys must be withheld from lifecycle hooks.
func (o envOrigin) isHarnessConfig() bool {
	switch o {
	case envOriginBundle, envOriginSettingsHC, envOriginNoAuthHC, envOriginLegacyHC:
		return true
	default:
		return false
	}
}

type trackedEnvEntry struct {
	value  string
	origin envOrigin
}

// trackedAgentEnv is the single broker-side container environment builder.
// Every key added to RunConfig.Env passes through Set/Delete with an explicit
// envOrigin, so RunConfig.Env and RunConfig.HookWithheldEnv are derived from
// the exact same map at RunConfig construction time.
type trackedAgentEnv struct {
	entries map[string]trackedEnvEntry
}

func newTrackedAgentEnv() *trackedAgentEnv {
	return &trackedAgentEnv{entries: make(map[string]trackedEnvEntry)}
}

// Set writes key=value and records origin as its winning provenance.
func (e *trackedAgentEnv) Set(key, value string, origin envOrigin) {
	if key == "" {
		return
	}
	if e.entries == nil {
		e.entries = make(map[string]trackedEnvEntry)
	}
	e.entries[key] = trackedEnvEntry{value: value, origin: origin}
}

// Delete removes key (both value and provenance).
func (e *trackedAgentEnv) Delete(key string) {
	delete(e.entries, key)
}

// Has reports whether key is currently present.
func (e *trackedAgentEnv) Has(key string) bool {
	_, ok := e.entries[key]
	return ok
}

// Get returns the current value of key.
func (e *trackedAgentEnv) Get(key string) (string, bool) {
	ent, ok := e.entries[key]
	if !ok {
		return "", false
	}
	return ent.value, true
}

// Origin returns the recorded envOrigin of key (envOriginNone if absent).
func (e *trackedAgentEnv) Origin(key string) envOrigin {
	return e.entries[key].origin
}

// Slice returns a deterministic sorted snapshot of KEY=VALUE entries for
// read-only inspection and RunConfig.Env.
func (e *trackedAgentEnv) Slice() []string {
	if len(e.entries) == 0 {
		return []string{}
	}
	keys := make([]string, 0, len(e.entries))
	for k := range e.entries {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+e.entries[k].value)
	}
	return out
}

// HarnessConfigKeys returns the sorted list of keys currently in the env whose
// winning value came from a harness-config layer.
func (e *trackedAgentEnv) HarnessConfigKeys() []string {
	if len(e.entries) == 0 {
		return nil
	}
	var out []string
	for k, ent := range e.entries {
		if ent.origin.isHarnessConfig() {
			out = append(out, k)
		}
	}
	if len(out) == 0 {
		return nil
	}
	sort.Strings(out)
	return out
}
