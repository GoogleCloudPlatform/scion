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
	"context"
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// Environment values in the persisted agent config (ptone/scion#4561).
//
// A template describes the agent's job; image, kubernetes, resources and
// volumes are environment keys (ptone/scion#4524). Their values come from
// settings (harness-config file, settings harness_configs, profile, profile
// harness_overrides, hub agent_defaults, default_resources, built-in
// defaults), and those are applied by Start on every start. ProvisionAgent
// persists only the requester's own values (the inline config) plus the
// workspace mount it computed, so a later settings or profile change reaches
// the agent on its next start instead of being frozen at provision.

// agentEnvironment holds the environment fields of an agent config.
type agentEnvironment struct {
	Image      string
	Kubernetes *api.KubernetesConfig
	Resources  *api.ResourceSpec
	Volumes    []api.VolumeMount
}

// environmentOf returns cfg's environment fields. The slices and pointers
// are copied, so later changes to cfg do not reach the result.
func environmentOf(cfg *api.ScionConfig) agentEnvironment {
	if cfg == nil {
		return agentEnvironment{}
	}
	env := agentEnvironment{Image: cfg.Image}
	if cfg.Kubernetes != nil {
		k := *cfg.Kubernetes
		env.Kubernetes = &k
	}
	if cfg.Resources != nil {
		r := *cfg.Resources
		env.Resources = &r
	}
	if len(cfg.Volumes) > 0 {
		env.Volumes = append([]api.VolumeMount(nil), cfg.Volumes...)
	}
	return env
}

// clone returns a copy of e that shares no pointers or slices with it.
func (e agentEnvironment) clone() agentEnvironment {
	return environmentOf(withEnvironment(&api.ScionConfig{}, e))
}

// withEnvironment returns a shallow copy of cfg whose environment fields are
// env's. Nil cfg yields nil.
func withEnvironment(cfg *api.ScionConfig, env agentEnvironment) *api.ScionConfig {
	if cfg == nil {
		return nil
	}
	out := *cfg
	out.Image = env.Image
	out.Kubernetes = env.Kubernetes
	out.Resources = env.Resources
	out.Volumes = env.Volumes
	return &out
}

// provisionedAgentEnvironment returns the environment ProvisionAgent
// persists: the requester's inline image, kubernetes, resources and volumes,
// then the workspace mount (if any) ProvisionAgent resolved.
func provisionedAgentEnvironment(inline *api.ScionConfig, workspaceVolume *api.VolumeMount) agentEnvironment {
	env := environmentOf(inline)
	if workspaceVolume != nil {
		env.Volumes = append(env.Volumes, *workspaceVolume)
	}
	return env
}

// readPersistedAgentEnvironment reads the environment fields of the agent's
// persisted scion-agent.json in agentDir. ok is false when the file cannot
// be read or parsed.
func readPersistedAgentEnvironment(agentDir string) (env agentEnvironment, ok bool) {
	if agentDir == "" {
		return agentEnvironment{}, false
	}
	data, err := os.ReadFile(filepath.Join(agentDir, "scion-agent.json"))
	if err != nil {
		return agentEnvironment{}, false
	}
	var cfg api.ScionConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return agentEnvironment{}, false
	}
	return environmentOf(&cfg), true
}

// mergeEnvironmentVolumes returns the volumes Start mounts: the settings
// harness_configs volumes, then the harness-config file's, then the agent's
// own (persisted) volumes. Per target, the settings and harness-config
// volumes replace an agent volume with the same target, so an agent
// provisioned before ptone/scion#4561, whose persisted config still holds
// the settings volumes of that time, mounts the current value once rather
// than the stale one or both. The agent's /workspace mount is never
// replaced: it is the workspace ProvisionAgent resolved.
func mergeEnvironmentVolumes(settingsVolumes, fileVolumes, own []api.VolumeMount) []api.VolumeMount {
	envTargets := map[string]struct{}{}
	var out []api.VolumeMount
	for _, v := range settingsVolumes {
		envTargets[v.Target] = struct{}{}
		out = append(out, v)
	}
	for _, v := range fileVolumes {
		envTargets[v.Target] = struct{}{}
		out = append(out, v)
	}
	for _, v := range own {
		if _, ok := envTargets[v.Target]; ok && v.Target != "/workspace" {
			continue
		}
		out = append(out, v)
	}
	return out
}

// resolveEnvironmentResources returns the resources for an agent whose own
// resources are own, lowest tier first: built-in default (applied separately
// by applyBuiltinResourceDefaults) < settings default_resources < hub
// agent_defaults.resources < profile resources < the agent's own <
// profile harness_overrides.<harness-config>.resources.
//
// The hub tier sits below broker profile resources and harness overrides
// but above default_resources only (see the hub agent_defaults comment in
// ProvisionAgent). own and the tiers are never aliased into the result.
func resolveEnvironmentResources(ctx context.Context, own *api.ResourceSpec, settings *config.VersionedSettings, profileName, harnessConfigName string) *api.ResourceSpec {
	var res *api.ResourceSpec
	if own != nil {
		cpy := *own
		res = &cpy
	}
	if settings != nil {
		effectiveProfile := profileName
		if effectiveProfile == "" {
			effectiveProfile = settings.ActiveProfile
		}
		if p, ok := settings.Profiles[effectiveProfile]; ok {
			if p.Resources != nil {
				base := *p.Resources
				res = config.MergeResourceSpec(&base, res)
			}
			if ho, ok := p.HarnessOverrides[harnessConfigName]; ok && ho.Resources != nil {
				res = config.MergeResourceSpec(res, ho.Resources)
			}
		}
	}
	if hd := api.HubAgentDefaultsFromContext(ctx); hd != nil && hd.Resources != nil {
		base := *hd.Resources
		res = config.MergeResourceSpec(&base, res)
	}
	if settings != nil && settings.DefaultResources != nil {
		base := *settings.DefaultResources
		res = config.MergeResourceSpec(&base, res)
	}
	return res
}

// applyEnvironmentResources sets cfg.Resources from the resource tiers (see
// resolveEnvironmentResources) over cfg's own resources, then applies the
// built-in CPU default when runtime.enforce_resource_defaults allows it.
func applyEnvironmentResources(ctx context.Context, cfg *api.ScionConfig, settings *config.VersionedSettings, profileName, harnessConfigName string) {
	if cfg == nil {
		return
	}
	cfg.Resources = resolveEnvironmentResources(ctx, cfg.Resources, settings, profileName, harnessConfigName)
	if config.ShouldEnforceResourceDefaults(settings) {
		applyBuiltinResourceDefaults(cfg)
	}
}
