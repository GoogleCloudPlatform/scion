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
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/envpolicy"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
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

// envLayerSource identifies which layer supplied the final value of an
// environment variable emitted by buildAgentEnvWithBundle.
type envLayerSource uint8

const (
	envSourceNone envLayerSource = iota
	envSourceBundle
	envSourceConfig
	envSourceExtra
	envSourceDefault
)

// hookWithheldInput collects the sanitized harness-config env layers and the
// higher-precedence env sources needed to compute RunConfig.HookWithheldEnv.
type hookWithheldInput struct {
	runtimeName     string
	bundleEnv       map[string]string
	envTemplateEnv  map[string]string
	settingsHCEnv   map[string]string
	agentEnv        []string
	provenance      map[string]envLayerSource
	callerEnv       map[string]string
	inlineEnv       map[string]string
	resolvedAuth    *api.ResolvedAuth
	resolvedSecrets []api.ResolvedSecret
}

// computeHookWithheldEnv returns the sorted list of environment variable names
// whose final container value comes from a harness-config layer (bundle env,
// bundle env_template, or settings/profile harness_configs.<name>.env) and must
// be withheld from lifecycle hooks.
//
// Final container-level precedence across RunConfig.Env, Harness.GetEnv(),
// ResolvedAuth, and ResolvedSecrets is resolved through
// runtime.ResolveRuntimeEnvWinner using in.runtimeName, so withholding matches
// each runtime's actual emission order. Within RunConfig.Env, provenance from
// buildAgentEnvWithBundle identifies bundle, config, extra (caller/settings-hc),
// and default sources directly. Legacy persisted keys that differ from the live
// bundle value remain in envSourceConfig and are still withheld when declared
// by bundleEnv unless overridden by callerEnv or inlineEnv.
func computeHookWithheldEnv(in hookWithheldInput) []string {
	if len(in.bundleEnv) == 0 && len(in.settingsHCEnv) == 0 && len(in.envTemplateEnv) == 0 {
		return nil
	}

	emitted := make(map[string]struct{}, len(in.agentEnv))
	for _, entry := range in.agentEnv {
		if k, _, ok := strings.Cut(entry, "="); ok && k != "" {
			emitted[k] = struct{}{}
		}
	}

	tplKeys := make(map[string]struct{}, len(in.envTemplateEnv))
	for k := range in.envTemplateEnv {
		if k != "" {
			tplKeys[k] = struct{}{}
		}
	}

	higher := make(map[string]struct{})
	for k, v := range in.callerEnv {
		if v != "" {
			higher[k] = struct{}{}
		}
	}
	for k, v := range in.inlineEnv {
		if v != "" {
			higher[k] = struct{}{}
		}
	}

	winners := runtime.ResolveRuntimeEnvWinner(
		in.runtimeName,
		emitted,
		tplKeys,
		in.resolvedAuth,
		in.resolvedSecrets,
	)

	seen := make(map[string]struct{})
	var withheld []string
	add := func(k string) {
		if k == "" {
			return
		}
		if _, dup := seen[k]; dup {
			return
		}
		seen[k] = struct{}{}
		withheld = append(withheld, k)
	}

	for k, winner := range winners {
		switch winner {
		case runtime.EnvSourceHarnessTemplate:
			// Harness.GetEnv() (env_template) won the final container value on
			// this runtime, so it must be withheld from hooks.
			add(k)
		case runtime.EnvSourceConfigEnv:
			if len(in.provenance) > 0 {
				switch in.provenance[k] {
				case envSourceBundle:
					add(k)
				case envSourceConfig:
					// Persisted own env or a legacy non-demoted bundle key:
					// withhold when the live bundle declares that name and no
					// higher-precedence caller or inline override set it.
					if _, declared := in.bundleEnv[k]; declared {
						if _, ok := higher[k]; !ok {
							add(k)
						}
					}
				case envSourceExtra:
					// opts.Env carries callerEnv plus settingsHCEnv for absent keys.
					if _, fromSettings := in.settingsHCEnv[k]; fromSettings {
						if v, ok := in.callerEnv[k]; !ok || v == "" {
							add(k)
						}
					}
				}
			} else {
				if _, ok := higher[k]; ok {
					continue
				}
				if _, fromBundle := in.bundleEnv[k]; fromBundle {
					add(k)
				} else if _, fromSettings := in.settingsHCEnv[k]; fromSettings {
					add(k)
				}
			}
		}
	}

	sort.Strings(withheld)
	return withheld
}
