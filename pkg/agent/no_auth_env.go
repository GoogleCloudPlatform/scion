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
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

// noAuthModeActive reports whether an agent launches in no-auth mode: no-auth
// was requested (or selected) and the harness config declares a no_auth
// behavior that runs without credentials. It is the condition for
// RunConfig.NoAuth and for applying no_auth.env.
func noAuthModeActive(noAuth bool, cfg *config.HarnessNoAuthConfig) bool {
	return noAuth && cfg != nil && (cfg.Behavior == "drop-to-shell" || cfg.Behavior == "allow")
}

// applyNoAuthEnv adds the harness config's no_auth.env entries to the
// container env when the agent launches in no-auth mode (active). Outside
// no-auth mode env is returned unchanged, so api-key and auth-file starts
// never see these entries.
//
// An entry never replaces a key env already sets: settings, agent and hub env
// win. A key reserved for scion itself (the SCION_* and GCE_METADATA_*
// namespaces, secret.IsReservedEnvTarget, which also covers the staged-token
// marker) or one only the hub may supply (config.IsHubOnlyEnvKey) is dropped
// with a warning, as is an entry with an empty value. Values are literal.
func applyNoAuthEnv(env []string, cfg *config.HarnessNoAuthConfig, active bool, agentName string) ([]string, []string) {
	if !active || cfg == nil || len(cfg.Env) == 0 {
		return env, nil
	}
	present := make(map[string]bool, len(env))
	for _, kv := range env {
		if k, _, ok := strings.Cut(kv, "="); ok {
			present[k] = true
		}
	}
	var warnings []string
	for _, k := range sortedKeys(cfg.Env) {
		v := cfg.Env[k]
		switch {
		case k == "" || strings.Contains(k, "="):
			continue
		case secret.IsReservedEnvTarget(k) || config.IsHubOnlyEnvKey(k):
			warnings = append(warnings, fmt.Sprintf("Warning: harness no_auth.env %s is reserved for scion; ignored", k))
		case v == "":
			warnings = append(warnings, fmt.Sprintf("Warning: harness no_auth.env %s has an empty value; ignored", k))
		case present[k]:
			util.Debugf("no_auth.env: %s already set for agent %s; kept", k, agentName)
		default:
			env = append(env, k+"="+v)
			present[k] = true
		}
	}
	return env, warnings
}
