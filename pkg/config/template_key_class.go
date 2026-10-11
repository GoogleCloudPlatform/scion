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

package config

import (
	"sort"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// Template key classes (ptone/scion#4524).
//
// A template describes the agent's job, not its operating environment.
// Template configs may set job keys (model, skills, instructions, harness
// config, non-reserved env, ...). Environment keys come from settings (hub,
// broker, profile, harness-config). An environment key found in a template
// is ignored with a warning; uploads that carry one are not rejected.
//
// This file is the single classification. Agent provisioning strips every
// template-chain config with StripTemplateEnvironmentKeys before merging
// it, and the hub reports templates that set environment keys.

// Environment keys of api.ScionConfig, by their scion-agent.yaml name.
const (
	TemplateEnvKeyTelemetry  = "telemetry"
	TemplateEnvKeyHub        = "hub"
	TemplateEnvKeyImage      = "image"
	TemplateEnvKeyResources  = "resources"
	TemplateEnvKeyKubernetes = "kubernetes"
	TemplateEnvKeyVolumes    = "volumes"
)

// templateEnvPrefix is the prefix of an ignored name for a reserved key
// inside env (e.g. "env.SCION_HUB_URL").
const templateEnvPrefix = "env."

// reservedTemplateEnvPrefixes are env-key prefixes a template may not set:
// the platform's own SCION_* contract and OpenTelemetry's OTEL_* settings.
var reservedTemplateEnvPrefixes = []string{"SCION_", "OTEL_"}

// hubOnlyEnvKeys lists the env keys that only the hub may supply for a
// hub-dispatched agent. pkg/agent's IsHubOnlyEnvKey delegates here.
var hubOnlyEnvKeys = map[string]struct{}{
	"TZ": {},
}

// IsHubOnlyEnvKey reports whether key may be supplied only by the hub for a
// hub-dispatched agent.
func IsHubOnlyEnvKey(key string) bool {
	_, ok := hubOnlyEnvKeys[key]
	return ok
}

// HarnessAuthCredentialEnvKeys returns the credential env keys declared by a
// harness-config's auth metadata, across all auth types: every
// required_env any_of key and every required_files alternative_env_keys
// entry. Nil metadata yields nil.
func HarnessAuthCredentialEnvKeys(authMeta *HarnessAuthMetadata) map[string]struct{} {
	if authMeta == nil {
		return nil
	}
	keys := map[string]struct{}{}
	for _, t := range authMeta.Types {
		for _, req := range t.RequiredEnv {
			for _, k := range req.AnyOf {
				if k != "" {
					keys[k] = struct{}{}
				}
			}
		}
		for _, f := range t.RequiredFiles {
			for _, k := range f.AlternativeEnvKeys {
				if k != "" {
					keys[k] = struct{}{}
				}
			}
		}
	}
	return keys
}

// IsReservedTemplateEnvKey reports whether a template may not set env key
// key: a SCION_ or OTEL_ key, a hub-only key, or one of credKeys (the
// resolved harness-config's credential keys, see
// HarnessAuthCredentialEnvKeys).
func IsReservedTemplateEnvKey(key string, credKeys map[string]struct{}) bool {
	for _, p := range reservedTemplateEnvPrefixes {
		if strings.HasPrefix(key, p) {
			return true
		}
	}
	if IsHubOnlyEnvKey(key) {
		return true
	}
	_, isCred := credKeys[key]
	return isCred
}

// TemplateEnvironmentKeys returns the environment keys cfg sets, sorted:
// the top-level names (e.g. "telemetry", "image") and "env.<KEY>" for each
// reserved env key. authMeta, when non-nil, adds the harness-config's
// credential keys to the reserved env keys. It does not modify cfg.
func TemplateEnvironmentKeys(cfg *api.ScionConfig, authMeta *HarnessAuthMetadata) []string {
	_, ignored := StripTemplateEnvironmentKeys(cfg, authMeta)
	return ignored
}

// StripTemplateEnvironmentKeys returns a copy of cfg without its environment
// keys, plus the sorted names it dropped (see TemplateEnvironmentKeys for
// the naming). The copy is shallow: dropped fields are cleared and Env is
// rebuilt, every other field is shared with cfg, which is not modified. A
// nil cfg yields (nil, nil); a cfg with no environment keys is returned as
// is.
func StripTemplateEnvironmentKeys(cfg *api.ScionConfig, authMeta *HarnessAuthMetadata) (*api.ScionConfig, []string) {
	if cfg == nil {
		return nil, nil
	}
	var ignored []string
	if cfg.Telemetry != nil {
		ignored = append(ignored, TemplateEnvKeyTelemetry)
	}
	if cfg.Hub != nil {
		ignored = append(ignored, TemplateEnvKeyHub)
	}
	if cfg.Image != "" {
		ignored = append(ignored, TemplateEnvKeyImage)
	}
	if cfg.Resources != nil {
		ignored = append(ignored, TemplateEnvKeyResources)
	}
	if cfg.Kubernetes != nil {
		ignored = append(ignored, TemplateEnvKeyKubernetes)
	}
	if len(cfg.Volumes) > 0 {
		ignored = append(ignored, TemplateEnvKeyVolumes)
	}
	credKeys := HarnessAuthCredentialEnvKeys(authMeta)
	var reservedEnv []string
	for k := range cfg.Env {
		if IsReservedTemplateEnvKey(k, credKeys) {
			reservedEnv = append(reservedEnv, k)
		}
	}
	if len(ignored) == 0 && len(reservedEnv) == 0 {
		return cfg, nil
	}

	out := *cfg
	out.Telemetry = nil
	out.Hub = nil
	out.Image = ""
	out.Resources = nil
	out.Kubernetes = nil
	out.Volumes = nil
	if len(reservedEnv) > 0 {
		env := make(map[string]string, len(cfg.Env)-len(reservedEnv))
		for k, v := range cfg.Env {
			if !IsReservedTemplateEnvKey(k, credKeys) {
				env[k] = v
			}
		}
		if len(env) == 0 {
			env = nil
		}
		out.Env = env
		for _, k := range reservedEnv {
			ignored = append(ignored, templateEnvPrefix+k)
		}
	}
	sort.Strings(ignored)
	return &out, ignored
}
