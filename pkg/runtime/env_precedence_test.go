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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
)

type precedenceTestHarness struct {
	harness.Generic
	env map[string]string
}

func (h *precedenceTestHarness) GetEnv(agentName, agentHome, unixUsername string) map[string]string {
	out := h.Generic.GetEnv(agentName, agentHome, unixUsername)
	for k, v := range h.env {
		out[k] = v
	}
	return out
}

// TestResolveRuntimeEnvWinner_MatchesActualRuntimeEmission verifies that
// ResolveRuntimeEnvWinner agrees with the actual container env emitted by every
// runtime across all 6 pairs of runtime-level layers (ConfigEnv,
// HarnessTemplate, ResolvedAuth, ResolvedSecret).
func TestResolveRuntimeEnvWinner_MatchesActualRuntimeEmission(t *testing.T) {
	// Six keys, one per unordered pair of the 4 runtime-level sources:
	//   K_CFG_TPL:  ConfigEnv vs HarnessTemplate
	//   K_CFG_AUTH: ConfigEnv vs ResolvedAuth
	//   K_CFG_SEC:  ConfigEnv vs ResolvedSecret
	//   K_TPL_AUTH: HarnessTemplate vs ResolvedAuth
	//   K_TPL_SEC:  HarnessTemplate vs ResolvedSecret
	//   K_AUTH_SEC: ResolvedAuth vs ResolvedSecret
	// Note: for Cloud Run, non-discovery ResolvedAuth keys go to the staged
	// blob; use GOOGLE_CLOUD_PROJECT (a discovery key emitted as plain env)
	// alongside custom keys so plain-env emission is directly observable too.
	const (
		valCfg  = "from-config-env"
		valTpl  = "from-harness-template"
		valAuth = "from-resolved-auth"
		valSec  = "from-resolved-secret"
	)

	sourceVal := map[EnvSource]string{
		EnvSourceConfigEnv:       valCfg,
		EnvSourceHarnessTemplate: valTpl,
		EnvSourceResolvedAuth:    valAuth,
		EnvSourceResolvedSecret:  valSec,
	}

	makeCfg := func(rtName string) RunConfig {
		return RunConfig{
			Harness: &precedenceTestHarness{
				env: map[string]string{
					"K_CFG_TPL":            valTpl,
					"K_TPL_AUTH":           valTpl,
					"K_TPL_SEC":            valTpl,
					"GOOGLE_CLOUD_PROJECT": valTpl,
				},
			},
			Name:         "prec-agent",
			UnixUsername: "scion",
			Image:        "scion-agent:latest",
			RuntimeName:  rtName,
			Labels:       map[string]string{"agent_id": "agent-prec", "scion.name": "prec-agent"},
			Env: []string{
				"K_CFG_TPL=" + valCfg,
				"K_CFG_AUTH=" + valCfg,
				"K_CFG_SEC=" + valCfg,
			},
			ResolvedAuth: &api.ResolvedAuth{
				Method: "api-key",
				EnvVars: map[string]string{
					"K_CFG_AUTH":           valAuth,
					"K_TPL_AUTH":           valAuth,
					"K_AUTH_SEC":           valAuth,
					"GOOGLE_CLOUD_PROJECT": valAuth,
				},
			},
			ResolvedSecrets: []api.ResolvedSecret{
				{Type: "environment", Name: "sec-cfg", Target: "K_CFG_SEC", Value: valSec},
				{Type: "environment", Name: "sec-tpl", Target: "K_TPL_SEC", Value: valSec},
				{Type: "environment", Name: "sec-auth", Target: "K_AUTH_SEC", Value: valSec},
			},
		}
	}

	// emitActual invokes each runtime's real env assembly and returns the final
	// effective value (last entry wins for -e / K8s EnvVar slices) for each key.
	emitActual := func(t *testing.T, rtName string, cfg RunConfig) map[string]string {
		t.Helper()
		out := make(map[string]string)
		switch rtName {
		case "docker", "podman", "container":
			args, err := buildCommonRunArgs(cfg)
			if err != nil {
				t.Fatalf("buildCommonRunArgs(%s): %v", rtName, err)
			}
			if rtName == "container" {
				args = stripUnsupportedAppleFlags(args[3:])
			}
			for i := 0; i+1 < len(args); i++ {
				if args[i] == "-e" {
					if k, v, ok := strings.Cut(args[i+1], "="); ok {
						out[k] = v
					}
					i++
				}
			}
		case "kubernetes":
			pod, err := newNFSTestK8sRuntime().buildPod("default", cfg)
			if err != nil {
				t.Fatalf("buildPod: %v", err)
			}
			for _, c := range pod.Spec.Containers {
				if c.Name != agentContainerName {
					continue
				}
				for _, ev := range c.Env {
					if ev.Value != "" {
						out[ev.Name] = ev.Value
					} else if ev.ValueFrom != nil && ev.ValueFrom.SecretKeyRef != nil {
						out[ev.Name] = valSec
					}
				}
			}
		case "cloudrun":
			rt, err := NewCloudRunRuntime(&config.CloudRunConfig{ProjectID: "test-project", Location: "us-central1"})
			if err != nil {
				t.Fatal(err)
			}
			// Mirror CloudRunRuntime.Run: fold non-colliding env secrets into cfg.Env
			// before buildCloudRunInstance.
			_, resolvedAuthStaged := splitResolvedAuthEnv(cfg.ResolvedAuth, cloudRunResolvedAuthTaken(cfg))
			if _, _, err := applyStagedSecretsToEnvExtras(&cfg, cloudRunEnvLimit, stagedExtras{ResolvedAuthEnv: resolvedAuthStaged}, cloudRunRuntimeEnvKeys...); err != nil {
				t.Fatalf("applyStagedSecretsToEnvExtras: %v", err)
			}
			inst, err := rt.buildCloudRunInstance(cfg, 1000, 1000, nil)
			if err != nil {
				t.Fatalf("buildCloudRunInstance: %v", err)
			}
			for _, ev := range inst.Containers[0].Env {
				out[ev.GetName()] = ev.GetValue()
			}
			// Non-discovery staged auth entries only reach the container when
			// not already taken by cfg.Env, secrets, or harness env.
			for k, v := range resolvedAuthStaged {
				if _, exists := out[k]; !exists {
					out[k] = v
				}
			}
		case "cloudrun-sandbox":
			// Mirror CloudRunSandboxRuntime.Run env assembly without spawning a
			// subprocess (keeps this test platform-neutral for Windows).
			secretKeys, err := applyResolvedSecretsToEnv(&cfg, sandboxEnvLimit, sandboxRuntimeEnvKeys...)
			if err != nil {
				t.Fatalf("applyResolvedSecretsToEnv: %v", err)
			}
			env := envFor(cfg, scionPaths{agentHome: "/tmp/home", workspace: "/tmp/ws"})
			if cfg.ResolvedAuth != nil {
				for k, v := range cfg.ResolvedAuth.EnvVars {
					env[k] = v
				}
			}
			applySecretEnvOverrides(env, cfg.Env, secretKeys)
			out = env
		case "substrate":
			out = buildBootstrapEnv(cfg)
		default:
			t.Fatalf("unhandled runtime %q", rtName)
		}
		return out
	}

	runtimes := []string{"docker", "podman", "container", "kubernetes", "cloudrun", "cloudrun-sandbox", "substrate"}
	keys := []string{"K_CFG_TPL", "K_CFG_AUTH", "K_CFG_SEC", "K_TPL_AUTH", "K_TPL_SEC", "K_AUTH_SEC", "GOOGLE_CLOUD_PROJECT"}

	for _, rtName := range runtimes {
		t.Run(rtName, func(t *testing.T) {
			cfg := makeCfg(rtName)
			cfgSet := map[string]struct{}{"K_CFG_TPL": {}, "K_CFG_AUTH": {}, "K_CFG_SEC": {}}
			tplSet := map[string]struct{}{"K_CFG_TPL": {}, "K_TPL_AUTH": {}, "K_TPL_SEC": {}, "GOOGLE_CLOUD_PROJECT": {}}

			winners := ResolveRuntimeEnvWinner(rtName, cfgSet, tplSet, cfg.ResolvedAuth, cfg.ResolvedSecrets)
			actual := emitActual(t, rtName, cfg)

			for _, k := range keys {
				gotWinner := winners[k]
				wantVal := sourceVal[gotWinner]
				gotVal := actual[k]
				if gotVal != wantVal {
					t.Errorf("runtime %s key %s: actual emitted=%q, ResolveRuntimeEnvWinner=%v (expected value %q)",
						rtName, k, gotVal, gotWinner, wantVal)
				}
			}
		})
	}
}
