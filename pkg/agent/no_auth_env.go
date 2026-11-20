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
	"github.com/GoogleCloudPlatform/scion/pkg/envpolicy"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

// noAuthModeActive reports whether an agent launches in no-auth mode: no-auth
// was requested (or selected) and the harness config declares a no_auth
// behavior that runs without credentials. It is the condition for
// RunConfig.NoAuth and for applying the no-auth env.
func noAuthModeActive(noAuth bool, cfg *config.HarnessNoAuthConfig) bool {
	return noAuth && cfg != nil && (cfg.Behavior == "drop-to-shell" || cfg.Behavior == "allow")
}

// applyNoAuthEnvTracked adds the harness-config's no-auth env (cfg.Env, read
// from the bundle's no-auth-env.yaml) to env with origin envOriginNoAuthHC when
// the agent launches in no-auth mode (active). Outside no-auth mode env is left
// unchanged, so api-key and auth-file starts never see these entries.
//
// An entry never replaces a key env already sets: settings, agent and hub env
// win (preserving their existing provenance). Invalid key names
// (envpolicy.ValidEnvName), keys reserved for scion itself (the SCION_* and
// GCE_METADATA_* namespaces, secret.IsReservedEnvTarget, which also covers the
// staged-token marker), hub-only keys (config.IsHubOnlyEnvKey), and entries
// with empty values are dropped with a warning. Values are literal.
func applyNoAuthEnvTracked(env *trackedAgentEnv, cfg *config.HarnessNoAuthConfig, active bool, agentName string) []string {
	if !active || cfg == nil || len(cfg.Env) == 0 {
		return nil
	}
	var warnings []string
	for _, k := range sortedKeys(cfg.Env) {
		v := cfg.Env[k]
		switch {
		case k == "":
			continue
		case !envpolicy.ValidEnvName(k):
			warnings = append(warnings, fmt.Sprintf("Warning: harness-config no-auth-env.yaml env key %q is invalid and was ignored.", k))
		case secret.IsReservedEnvTarget(k) || config.IsHubOnlyEnvKey(k):
			warnings = append(warnings, fmt.Sprintf("Warning: harness-config no-auth-env.yaml sets %s, which is reserved for scion; ignored", k))
		case v == "":
			warnings = append(warnings, fmt.Sprintf("Warning: harness-config no-auth-env.yaml sets %s to an empty value; ignored", k))
		case env.Has(k):
			util.Debugf("no-auth env: %s already set for agent %s; kept", k, agentName)
		default:
			env.Set(k, v, envOriginNoAuthHC)
		}
	}
	return warnings
}

// applyNoAuthEnv applies cfg.Env to a plain KEY=VALUE slice by delegating to
// applyNoAuthEnvTracked. Retained for direct slice-level unit tests.
func applyNoAuthEnv(env []string, cfg *config.HarnessNoAuthConfig, active bool, agentName string) ([]string, []string) {
	if !active || cfg == nil || len(cfg.Env) == 0 {
		return env, nil
	}
	tracked := newTrackedAgentEnv()
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok {
			tracked.Set(k, v, envOriginCaller)
		}
	}
	warnings := applyNoAuthEnvTracked(tracked, cfg, active, agentName)
	return tracked.Slice(), warnings
}
