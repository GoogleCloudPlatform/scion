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
	"sort"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

// computeHookWithheldEnv returns the sorted list of environment variable names
// whose final value in agentEnv comes from the harness-config bundle env layer
// and must be withheld from lifecycle hooks.
//
// Keys supplied with a non-empty value by a higher-precedence layer in
// buildAgentEnv (caller/hub env captured before settings-hc injection, or
// inline env) are excluded so those sources remain visible to hooks. Keys
// absent from agentEnv (for example, an empty bundle declaration satisfied only
// by resolved auth or an environment secret at runtime) are also omitted;
// conversely, when a bundle key is emitted into agentEnv, runtime config.Env
// outranks resolved auth and colliding environment secrets, so the key stays
// withheld.
//
// Bundle keys are expanded with the same ExpandEnvRefusing rule buildAgentEnv
// uses so keys containing ${...} are matched under their emitted name.
func computeHookWithheldEnv(
	bundleEnv map[string]string,
	agentEnv []string,
	callerEnv map[string]string,
	inlineEnv map[string]string,
) []string {
	if len(bundleEnv) == 0 {
		return nil
	}

	emitted := make(map[string]struct{}, len(agentEnv))
	for _, entry := range agentEnv {
		if k, _, ok := strings.Cut(entry, "="); ok && k != "" {
			emitted[k] = struct{}{}
		}
	}

	higher := make(map[string]struct{})
	for k, v := range callerEnv {
		if v != "" {
			higher[k] = struct{}{}
		}
	}
	for k, v := range inlineEnv {
		if v != "" {
			higher[k] = struct{}{}
		}
	}

	seen := make(map[string]struct{}, len(bundleEnv))
	var withheld []string
	for rawK := range bundleEnv {
		k, _, _ := util.ExpandEnvRefusing(rawK, refuseHostEnvForAgent)
		if k == "" {
			continue
		}
		if _, dup := seen[k]; dup {
			continue
		}
		if _, ok := higher[k]; ok {
			continue
		}
		if _, ok := emitted[k]; !ok {
			continue
		}
		seen[k] = struct{}{}
		withheld = append(withheld, k)
	}
	sort.Strings(withheld)
	return withheld
}
