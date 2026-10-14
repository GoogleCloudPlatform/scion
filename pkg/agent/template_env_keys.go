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
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

// Template environment keys and the template env layer (ptone/scion#4524).
//
// A template describes the agent's job, not its operating environment.
//
//   - Environment keys (telemetry, hub, image, resources, kubernetes,
//     volumes) and reserved SCION_* env keys in a template are ignored:
//     every template-chain config goes through templateJobConfig before it
//     is merged, on the provisioning path (resolveTemplateAndHarnessConfig)
//     and the restart path (GetAgent). Each template that sets such keys
//     gives one WARN log and one start warning.
//   - A template's other env is for the job's own tools. It is its own
//     layer, never merged into the agent config and never persisted:
//     templateJobConfig drops it, Start recomputes it from the live template
//     chain on every start (templateChainEnv) and hands it to the runtime as
//     RunConfig.FallbackEnv (templateEnvFallback), which every runtime
//     applies as the last step of its own env assembly
//     (runtime.FallbackEnvEntries): a key is set only when no other source
//     emitted it, and never for the harness-config's auth-candidate keys
//     (RunConfig.SuppressedFallbackKeys). Values are literal: no expansion
//     against the broker host environment and no host passthrough for empty
//     values, so a template can never copy broker-host secrets into a
//     container. Template env is never in the request env or agent config,
//     so it never counts toward auth detection or env-gather.

// templateEnvWarnings collects the start warnings for ignored template
// environment keys. Start installs one in ctx around GetAgent and appends
// what it collected to the start warnings.
type templateEnvWarnings struct {
	mu       sync.Mutex
	quiet    bool
	warnings []string
	seen     map[string]struct{}
}

type templateEnvWarningsKey struct{}

// withTemplateEnvWarnings returns ctx carrying a new collector.
func withTemplateEnvWarnings(ctx context.Context) (context.Context, *templateEnvWarnings) {
	c := &templateEnvWarnings{seen: map[string]struct{}{}}
	return context.WithValue(ctx, templateEnvWarningsKey{}, c), c
}

// withQuietTemplateEnvWarnings returns ctx carrying a collector that neither
// logs nor keeps warnings, for a resolution that is repeated later by the
// real provisioning (PreflightResolve), so the warning is reported once.
func withQuietTemplateEnvWarnings(ctx context.Context) context.Context {
	return context.WithValue(ctx, templateEnvWarningsKey{}, &templateEnvWarnings{quiet: true})
}

func templateEnvWarningsFrom(ctx context.Context) *templateEnvWarnings {
	c, _ := ctx.Value(templateEnvWarningsKey{}).(*templateEnvWarnings)
	return c
}

// Warnings returns the collected warnings in the order they were reported.
func (c *templateEnvWarnings) Warnings() []string {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.warnings...)
}

// templateJobConfig returns tplCfg as merged into an agent's config: without
// its environment keys (see config.StripTemplateEnvironmentKeys) and without
// its env, which is the separate template env layer (templateChainEnv).
// When it drops environment keys, it logs a WARN and records a start
// warning in ctx's collector, once per distinct warning. tplCfg is not
// modified.
func templateJobConfig(ctx context.Context, tpl *config.Template, tplCfg *api.ScionConfig) *api.ScionConfig {
	stripped, ignored := config.StripTemplateEnvironmentKeys(tplCfg)
	if stripped == nil {
		return nil
	}
	job := *stripped
	job.Env = nil
	if len(ignored) == 0 {
		return &job
	}
	c := templateEnvWarningsFrom(ctx)
	if c != nil && c.quiet {
		return &job
	}
	name := templateRef(tpl)
	msg := config.TemplateEnvironmentKeysWarning(name, ignored)
	if c != nil {
		c.mu.Lock()
		if c.seen == nil {
			c.seen = map[string]struct{}{}
		}
		_, dup := c.seen[msg]
		if !dup {
			c.seen[msg] = struct{}{}
			c.warnings = append(c.warnings, msg)
		}
		c.mu.Unlock()
		if dup {
			return &job
		}
	}
	slog.Warn("template sets environment keys; ignored (environment comes from settings)",
		"template", name, "path", tpl.Path, "keys", ignored)
	return &job
}

// templateChainEnv returns the template env layer of chain: the merged env
// of its configs (a later template wins), without reserved SCION_* keys.
// Templates whose config cannot be loaded are skipped. It reports nothing;
// ignored keys are reported where the chain is merged (templateJobConfig).
func templateChainEnv(chain []*config.Template) map[string]string {
	var env map[string]string
	for _, tpl := range chain {
		loaded, err := tpl.LoadConfig()
		if err != nil || loaded == nil {
			continue
		}
		stripped, _ := config.StripTemplateEnvironmentKeys(loaded)
		for k, v := range stripped.Env {
			if env == nil {
				env = map[string]string{}
			}
			env[k] = v
		}
	}
	return env
}

// templateEnvFallback returns the template env layer tplEnv as the
// RunConfig.FallbackEnv every runtime applies as the last step of its env
// assembly (runtime.FallbackEnvEntries: a key is set only when no other
// source emitted it), plus start warnings. Values are literal: an empty
// value is skipped with a warning (never a host passthrough), and a value
// containing "${" is kept as is with a warning that it is not expanded. A
// hub-only key is skipped for a broker-mode start (skipBrokerLocalEnvKey:
// the template env is a broker-local layer).
func templateEnvFallback(tplEnv map[string]string, brokerMode bool, agentName string) (map[string]string, []string) {
	if len(tplEnv) == 0 {
		return nil, nil
	}
	var out map[string]string
	var warnings []string
	for _, k := range sortedKeys(tplEnv) {
		v := tplEnv[k]
		switch {
		case k == "" || strings.Contains(k, "="):
			continue
		case skipBrokerLocalEnvKey(brokerMode, k):
			util.Debugf("template env: %s skipped for agent %s (hub-only key)", k, agentName)
		case v == "":
			warnings = append(warnings, fmt.Sprintf("Warning: template env %s has an empty value; skipped (template env values are literal, never taken from the host)", k))
		default:
			if strings.Contains(v, "${") {
				warnings = append(warnings, fmt.Sprintf("Warning: template env %s value contains ${...}; used literally, not expanded", k))
			}
			if out == nil {
				out = map[string]string{}
			}
			out[k] = v
		}
	}
	return out, warnings
}

// sortedKeys returns the keys of m, sorted.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// demoteLegacyTemplateEnv returns cfg for this start's env assembly with
// every Env key whose value equals the live template env value removed, so
// a template value merged into scion-agent.json by an older version applies
// only through the template fallback, plus the demoted keys (sorted). cfg is
// not modified (nothing on disk changes); cfg itself is returned when no key
// is demoted.
func demoteLegacyTemplateEnv(cfg *api.ScionConfig, tplEnv map[string]string) (*api.ScionConfig, []string) {
	if cfg == nil || len(cfg.Env) == 0 || len(tplEnv) == 0 {
		return cfg, nil
	}
	var demote []string
	for k, v := range cfg.Env {
		if tv, ok := tplEnv[k]; ok && tv == v {
			demote = append(demote, k)
		}
	}
	if len(demote) == 0 {
		return cfg, nil
	}
	out := *cfg
	out.Env = make(map[string]string, len(cfg.Env)-len(demote))
	for k, v := range cfg.Env {
		out.Env[k] = v
	}
	for _, k := range demote {
		delete(out.Env, k)
	}
	sort.Strings(demote)
	util.Debugf("template env: demoted legacy stored env key(s) %v to the template fallback", demote)
	return &out, demote
}

// suppressedDemotedKeyWarnings returns a start warning for each demoted
// legacy key that is also suppressed for the fallback (an auth-candidate
// key): an agent whose scion-agent.json carries a credential merged in from
// its template by an older version no longer gets it, and must not lose it
// silently.
func suppressedDemotedKeyWarnings(demoted, suppressed []string) []string {
	if len(demoted) == 0 || len(suppressed) == 0 {
		return nil
	}
	blocked := make(map[string]bool, len(suppressed))
	for _, k := range suppressed {
		blocked[k] = true
	}
	var warnings []string
	for _, k := range demoted {
		if blocked[k] {
			warnings = append(warnings, fmt.Sprintf("Warning: stored env %s came from the template and is an auth credential key of the harness-config; it is no longer applied (template env never supplies credentials). Set it as a secret or harness-config env instead.", k))
		}
	}
	return warnings
}

// templateDisplayName returns the name to show for an agent's template in a
// start warning: the recorded display name (broker-side provenance, else
// agent info) when there is one, so a content-addressed cache path is never
// shown, else templateName. It returns "" when the agent has no template.
func templateDisplayName(templateName string, provenance *imageProvenance, cfg *api.ScionConfig) string {
	if provenance != nil && provenance.Template != "" {
		return provenance.Template
	}
	if cfg != nil && cfg.Info != nil && cfg.Info.Template != "" {
		return cfg.Info.Template
	}
	return templateName
}
