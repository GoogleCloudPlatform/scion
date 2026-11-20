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

// hookWithheldInput collects the sanitized harness-config env layers and the
// higher-precedence env sources needed to compute RunConfig.HookWithheldEnv.
type hookWithheldInput struct {
	bundleEnv       map[string]string
	envTemplateEnv  map[string]string
	settingsHCEnv   map[string]string
	agentEnv        []string
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
// Keys supplied with a non-empty value by a higher-precedence non-harness-config
// source (caller/hub env captured before settings-hc injection, or inline env)
// are excluded so those sources remain visible to hooks. For bundle env and
// settings-hc layers (which travel in RunConfig.Env and outrank resolved auth
// and colliding environment secrets at runtime), keys absent from agentEnv are
// omitted. For env_template (which travels via Harness.GetEnv below
// RunConfig.Env, resolved auth, and environment secrets), keys overridden by
// agentEnv, resolved auth, or environment secrets are excluded.
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

	authOrSecret := make(map[string]struct{})
	if in.resolvedAuth != nil {
		for k, v := range in.resolvedAuth.EnvVars {
			if v != "" {
				authOrSecret[k] = struct{}{}
			}
		}
	}
	for _, s := range in.resolvedSecrets {
		if (s.Type == "environment" || s.Type == "") && s.Target != "" && s.Value != "" {
			authOrSecret[s.Target] = struct{}{}
		}
	}

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

	for rawK := range in.bundleEnv {
		k, _, _ := util.ExpandEnvRefusing(rawK, refuseHostEnvForAgent)
		if k == "" {
			continue
		}
		if _, ok := higher[k]; ok {
			continue
		}
		if _, ok := emitted[k]; !ok {
			continue
		}
		add(k)
	}

	for k := range in.settingsHCEnv {
		if k == "" {
			continue
		}
		if _, ok := higher[k]; ok {
			continue
		}
		if _, ok := emitted[k]; !ok {
			continue
		}
		add(k)
	}

	for k := range in.envTemplateEnv {
		if k == "" {
			continue
		}
		if _, ok := higher[k]; ok {
			continue
		}
		if _, ok := authOrSecret[k]; ok {
			continue
		}
		if _, ok := emitted[k]; ok {
			// If emitted came from bundleEnv or settingsHCEnv, it was already
			// added above; otherwise a non-harness-config layer in agentEnv
			// overrides Harness.GetEnv().
			continue
		}
		add(k)
	}

	sort.Strings(withheld)
	return withheld
}
