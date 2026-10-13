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
	"fmt"
	"sort"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// Template key classes (ptone/scion#4524).
//
// A template describes the agent's job, not its operating environment.
// Template configs may set job keys (model, skills, instructions, harness
// config, ...). Environment keys come from settings (hub, broker, profile,
// harness-config). An environment key found in a template is ignored with a
// warning; uploads that carry one are not rejected.
//
// A template's env is for the job's own tools. It is not an environment key,
// but it never takes part in system settings: agents apply it as the
// lowest-precedence fill (a key is set only when no other source sets it),
// with literal values (see pkg/agent's template env layer). The one reserved
// env prefix is SCION_, the platform's own namespace, whose keys can carry
// meaning by their absence.
//
// This file is the single classification. Agent provisioning strips every
// template-chain config with StripTemplateEnvironmentKeys before using it,
// and the hub reports templates that set environment keys.

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

// reservedTemplateEnvPrefix is the env-key prefix a template may not set:
// the platform's own SCION_* namespace.
const reservedTemplateEnvPrefix = "SCION_"

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

// IsReservedTemplateEnvKey reports whether a template may not set env key
// key: a key in the platform's SCION_* namespace.
func IsReservedTemplateEnvKey(key string) bool {
	return strings.HasPrefix(key, reservedTemplateEnvPrefix)
}

// TemplateEnvironmentKeysWarning is the warning for a template that sets
// environment keys (names as returned by TemplateEnvironmentKeys). Agent
// start warnings and the hub's validate report use the same text.
func TemplateEnvironmentKeysWarning(templateName string, keys []string) string {
	return fmt.Sprintf("template %s sets environment key(s) %s; ignored (environment comes from settings)",
		templateName, strings.Join(keys, ", "))
}

// TemplateEnvironmentKeys returns the environment keys cfg sets, sorted:
// the top-level names (e.g. "telemetry", "image") and "env.<KEY>" for each
// reserved env key. It does not modify cfg.
func TemplateEnvironmentKeys(cfg *api.ScionConfig) []string {
	_, ignored := StripTemplateEnvironmentKeys(cfg)
	return ignored
}

// StripTemplateEnvironmentKeys returns a copy of cfg without its environment
// keys and reserved env keys, plus the sorted names it dropped (see
// TemplateEnvironmentKeys for the naming). The copy is shallow: dropped
// fields are cleared and Env is rebuilt, every other field is shared with
// cfg, which is not modified. A nil cfg yields (nil, nil); a cfg with no
// environment keys is returned as is.
func StripTemplateEnvironmentKeys(cfg *api.ScionConfig) (*api.ScionConfig, []string) {
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
	var reservedEnv []string
	for k := range cfg.Env {
		if IsReservedTemplateEnvKey(k) {
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
	// Env is always rebuilt in the copy, never shared with cfg, so a caller
	// that mutates the stripped config's Env cannot reach cfg's map.
	if cfg.Env != nil {
		env := make(map[string]string, len(cfg.Env)-len(reservedEnv))
		for k, v := range cfg.Env {
			if !IsReservedTemplateEnvKey(k) {
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
