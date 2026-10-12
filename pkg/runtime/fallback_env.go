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
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// FallbackEnvEntries returns the "KEY=VALUE" entries of fallback (a
// RunConfig's FallbackEnv, the template env layer of ptone/scion#4524) that a
// runtime adds as the LAST step of its container env assembly: every
// non-empty fallback key that no other source emitted (emitted holds the keys
// the runtime has already set, with any value), that is not suppressed
// (RunConfig.SuppressedFallbackKeys) and that is not a reserved SCION_* key
// (config.IsReservedTemplateEnvKey). Values are used literally. Entries are
// sorted by key, so the result is deterministic.
//
// The SCION_* skip keeps the fallback final even against env a runtime adds
// after this step: the only such additions are broker-owned SCION_* keys,
// SCION_SUPPLEMENTAL_GIDS (appendSharedDirGroupArgs, called by the docker,
// podman and apple runtimes after buildCommonRunArgs) and Podman's
// SCION_KEEPID_UID, which envKeysOfArgs cannot see. The agent manager
// already strips SCION_* from template env; this is the runtime-side
// guarantee.
//
// Being applied last by every runtime makes the fallback final by
// construction: auth env, harness env, telemetry env, env-type secrets,
// RunConfig.Env and anything a runtime adds all win over it.
func FallbackEnvEntries(fallback map[string]string, suppressed []string, emitted map[string]bool) []string {
	if len(fallback) == 0 {
		return nil
	}
	blocked := make(map[string]bool, len(suppressed))
	for _, k := range suppressed {
		blocked[k] = true
	}
	keys := make([]string, 0, len(fallback))
	for k, v := range fallback {
		if k == "" || v == "" || strings.Contains(k, "=") || emitted[k] || blocked[k] || config.IsReservedTemplateEnvKey(k) {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+fallback[k])
	}
	return out
}

// envKeysOf returns the keys of "KEY=VALUE" (or bare "KEY") entries.
func envKeysOf(entries []string) map[string]bool {
	keys := make(map[string]bool, len(entries))
	for _, e := range entries {
		k, _, _ := strings.Cut(e, "=")
		if k != "" {
			keys[k] = true
		}
	}
	return keys
}

// envKeysOfArgs returns the keys set by "-e"/"--env" flags in a container
// CLI argument list (each flag followed by its "KEY=VALUE" value).
func envKeysOfArgs(args []string) map[string]bool {
	keys := map[string]bool{}
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-e" || args[i] == "--env" {
			if k, _, _ := strings.Cut(args[i+1], "="); k != "" {
				keys[k] = true
			}
			i++
		}
	}
	return keys
}

// appendK8sFallbackEnv appends the fallback env to a pod's env list as its
// last step (see FallbackEnvEntries). Every existing entry, plain or
// secret-backed (ValueFrom), counts as emitted.
func appendK8sFallbackEnv(envVars []corev1.EnvVar, fallback map[string]string, suppressed []string) []corev1.EnvVar {
	emitted := make(map[string]bool, len(envVars))
	for _, ev := range envVars {
		emitted[ev.Name] = true
	}
	for _, e := range FallbackEnvEntries(fallback, suppressed, emitted) {
		k, v, _ := strings.Cut(e, "=")
		envVars = append(envVars, corev1.EnvVar{Name: k, Value: v})
	}
	return envVars
}
