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

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// computeHookWithheldEnv returns the sorted list of environment variable names
// whose final value comes from the harness-config bundle env layer and must be
// withheld from lifecycle hooks. Keys supplied with a non-empty value by a
// higher-precedence source (caller/hub env, inline env, resolved auth, or
// environment-type secrets) are excluded so those sources remain visible to
// hooks. Keys not present in the final agentEnv are also omitted.
func computeHookWithheldEnv(
	bundleEnv map[string]string,
	agentEnv []string,
	callerEnv map[string]string,
	inlineEnv map[string]string,
	resolvedAuth *api.ResolvedAuth,
	resolvedSecrets []api.ResolvedSecret,
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
	if resolvedAuth != nil {
		for k, v := range resolvedAuth.EnvVars {
			if v != "" {
				higher[k] = struct{}{}
			}
		}
	}
	for _, s := range resolvedSecrets {
		if (s.Type == "" || s.Type == "environment") && s.Target != "" && s.Value != "" {
			higher[s.Target] = struct{}{}
		}
	}

	var withheld []string
	for k := range bundleEnv {
		if k == "" {
			continue
		}
		if _, ok := higher[k]; ok {
			continue
		}
		if _, ok := emitted[k]; !ok {
			continue
		}
		withheld = append(withheld, k)
	}
	sort.Strings(withheld)
	return withheld
}
