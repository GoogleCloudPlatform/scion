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
//     chain on every start (templateChainEnv), and applyTemplateEnvFill
//     applies it as the final fill at the single point where the container
//     env is finalized. A key is set only when no other source set it to a
//     non-empty value, and values are literal: no expansion against the
//     broker host environment and no host passthrough for empty values, so
//     a template can never copy broker-host secrets into a container. Being
//     applied last, after auth resolution and env-gather, template env never
//     counts toward either.

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

// applyTemplateEnvFill applies the template env layer tplEnv to the
// finalized container env as its last step, and returns the env plus start
// warnings. A template key is set only when env has no non-empty value for
// it; an overridden key is logged at debug level only (the intended
// fallback). Values are literal: an empty value is skipped with a warning
// (never a host passthrough), and a value containing "${" is used as is
// with a warning that it is not expanded. A hub-only key is skipped for a
// broker-mode start (skipBrokerLocalEnvKey: the template env is a
// broker-local layer). env is not modified.
func applyTemplateEnvFill(env []string, tplEnv map[string]string, brokerMode bool, agentName string) ([]string, []string) {
	if len(tplEnv) == 0 {
		return env, nil
	}
	set := make(map[string]bool, len(env))
	emptyEntry := map[string]bool{}
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok {
			if v != "" {
				set[k] = true
			} else {
				emptyEntry[k] = true
			}
		}
	}
	keys := make([]string, 0, len(tplEnv))
	for k := range tplEnv {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := env
	var warnings []string
	for _, k := range keys {
		v := tplEnv[k]
		switch {
		case k == "" || strings.Contains(k, "="):
			continue
		case skipBrokerLocalEnvKey(brokerMode, k):
			util.Debugf("template env: %s skipped for agent %s (hub-only key)", k, agentName)
		case set[k]:
			util.Debugf("template env: %s for agent %s is set by another source; template value not applied", k, agentName)
		case v == "":
			warnings = append(warnings, fmt.Sprintf("Warning: template env %s has an empty value; skipped (template env values are literal, never taken from the host)", k))
		default:
			if strings.Contains(v, "${") {
				warnings = append(warnings, fmt.Sprintf("Warning: template env %s value contains ${...}; used literally, not expanded", k))
			}
			out = withEnvEntry(out, env, k, v, emptyEntry[k])
		}
	}
	return out, warnings
}

// withEnvEntry returns out with k=v appended, dropping an existing empty
// "k=" entry when dropEmpty is set. It never writes through orig (the
// caller's slice): out is copied first while it still aliases orig.
func withEnvEntry(out, orig []string, k, v string, dropEmpty bool) []string {
	if len(out) > 0 && len(orig) > 0 && &out[0] == &orig[0] {
		out = append(make([]string, 0, len(orig)+1), orig...)
	}
	if dropEmpty {
		kept := out[:0]
		for _, kv := range out {
			if kv != k+"=" {
				kept = append(kept, kv)
			}
		}
		out = kept
	}
	return append(out, k+"="+v)
}

// demoteLegacyTemplateEnv returns cfg for this start's env assembly with
// every Env key whose value equals the live template env value removed, so
// a template value merged into scion-agent.json by an older version applies
// only through the template fill. cfg is not modified (nothing on disk
// changes); cfg itself is returned when no key is demoted.
func demoteLegacyTemplateEnv(cfg *api.ScionConfig, tplEnv map[string]string) *api.ScionConfig {
	if cfg == nil || len(cfg.Env) == 0 || len(tplEnv) == 0 {
		return cfg
	}
	var demote []string
	for k, v := range cfg.Env {
		if tv, ok := tplEnv[k]; ok && tv == v {
			demote = append(demote, k)
		}
	}
	if len(demote) == 0 {
		return cfg
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
	util.Debugf("template env: demoted legacy stored env key(s) %v to the template fill", demote)
	return &out
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
