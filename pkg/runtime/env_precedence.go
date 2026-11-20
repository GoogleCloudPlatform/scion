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
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// EnvSource identifies which runtime-level environment layer supplies a
// variable's final container value.
type EnvSource uint8

const (
	EnvSourceNone EnvSource = iota
	// EnvSourceConfigEnv is RunConfig.Env (bundle < own < hub/settings < default).
	EnvSourceConfigEnv
	// EnvSourceHarnessTemplate is Harness.GetEnv() (harness-config env_template).
	EnvSourceHarnessTemplate
	// EnvSourceResolvedAuth is RunConfig.ResolvedAuth.EnvVars.
	EnvSourceResolvedAuth
	// EnvSourceResolvedSecret is env-type RunConfig.ResolvedSecrets.
	EnvSourceResolvedSecret
)

// ResolveRuntimeEnvWinner determines which runtime-level environment layer
// supplies the final container value for every key present in configEnv,
// harnessTemplateEnv, resolvedAuth, or resolvedSecrets, following the exact
// emission order of runtimeName:
//
//   - docker, podman, container (apple), kubernetes (k8s), cloudrun,
//     cloudrun-instances (default):
//     ResolvedAuth < HarnessTemplate < ResolvedSecret (skipped if in ConfigEnv) < ConfigEnv
//   - cloudrun-sandbox:
//     ConfigEnv < HarnessTemplate < ResolvedAuth < ResolvedSecret (skipped if in ConfigEnv)
//   - substrate:
//     HarnessTemplate < ConfigEnv < ResolvedAuth < ResolvedSecret
//
// Empty resolved-auth and env-secret values are ignored, matching runtime
// emission. Both hook-env provenance (pkg/agent) and per-runtime emission
// parity tests share this function so emission order and provenance stay in
// lockstep.
func ResolveRuntimeEnvWinner(
	runtimeName string,
	configEnv map[string]struct{},
	harnessTemplateEnv map[string]struct{},
	resolvedAuth *api.ResolvedAuth,
	resolvedSecrets []api.ResolvedSecret,
) map[string]EnvSource {
	authKeys := make(map[string]struct{})
	if resolvedAuth != nil {
		for k, v := range resolvedAuth.EnvVars {
			if k != "" && v != "" {
				authKeys[k] = struct{}{}
			}
		}
	}

	secretKeys := make(map[string]struct{})
	for _, s := range resolvedSecrets {
		if (s.Type == "environment" || s.Type == "") && s.Target != "" && s.Value != "" {
			secretKeys[s.Target] = struct{}{}
		}
	}

	winners := make(map[string]EnvSource, len(configEnv)+len(harnessTemplateEnv)+len(authKeys)+len(secretKeys))
	apply := func(keys map[string]struct{}, src EnvSource, skipIfInConfig bool) {
		for k := range keys {
			if k == "" {
				continue
			}
			if skipIfInConfig {
				if _, inCfg := configEnv[k]; inCfg {
					continue
				}
			}
			winners[k] = src
		}
	}

	switch strings.ToLower(strings.TrimSpace(runtimeName)) {
	case "substrate":
		// substrate_bootstrap.go buildBootstrapEnv:
		// Harness.GetEnv < cfg.Env < ResolvedAuth < ResolvedSecrets.
		apply(harnessTemplateEnv, EnvSourceHarnessTemplate, false)
		apply(configEnv, EnvSourceConfigEnv, false)
		apply(authKeys, EnvSourceResolvedAuth, false)
		apply(secretKeys, EnvSourceResolvedSecret, false)
	case "cloudrun-sandbox":
		// cloudrun_sandbox_runtime.go envFor + Run:
		// cfg.Env < Harness.GetEnv < ResolvedAuth < non-colliding ResolvedSecrets.
		apply(configEnv, EnvSourceConfigEnv, false)
		apply(harnessTemplateEnv, EnvSourceHarnessTemplate, false)
		apply(authKeys, EnvSourceResolvedAuth, false)
		apply(secretKeys, EnvSourceResolvedSecret, true)
	default:
		// docker, podman, container (apple), kubernetes (k8s), cloudrun,
		// cloudrun-instances:
		// ResolvedAuth < Harness.GetEnv < non-colliding ResolvedSecrets < cfg.Env.
		apply(authKeys, EnvSourceResolvedAuth, false)
		apply(harnessTemplateEnv, EnvSourceHarnessTemplate, false)
		apply(secretKeys, EnvSourceResolvedSecret, true)
		apply(configEnv, EnvSourceConfigEnv, false)
	}

	return winners
}
