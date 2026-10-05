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

package hub

import (
	"context"
	"errors"
	"fmt"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// autoExposeAllowlistExemptKeys are the AppliedConfig.Env keys the env
// cleanup's plain-source allowlist never strips (see keysToStrip). The hub
// writes SCION_AUTO_EXPOSE_PORTS into AppliedConfig.Env from the project
// annotation (resolveAutoExposeEnv), and the configure PATCH keeps the
// previous value of each of these keys when the request omits it, so a value
// can legitimately sit in AppliedConfig.Env with no template, InlineConfig or
// env-var source to match. None of them is ever a secret; a live secret of
// the same name is still stripped.
var autoExposeAllowlistExemptKeys = map[string]bool{
	api.EnvAutoExposePorts:         true,
	"SCION_AUTO_EXPOSE_PORTS_LIST": true,
	"SCION_AUTO_EXPOSE_INTERVAL":   true,
}

// autoExposeSources are the tiers normalizeAutoExposeEnv re-derives
// SCION_AUTO_EXPOSE_PORTS from: the agent's project (nil when it has none or
// it no longer exists) and its applied template's env (nil without one).
type autoExposeSources struct {
	project     *store.Project
	templateEnv map[string]string
}

// needsAutoExposeNormalization reports whether ac carries a
// SCION_AUTO_EXPOSE_PORTS in InlineConfig.Env that its CreateInputs record
// lacks. Older hubs stamped the project or hub default there; such a value is
// not explicit. An agent without CreateInputs cannot tell a stamp from an
// explicit value, so it is never normalized (reincarnate strips it instead,
// see legacyCreateInputsFromAppliedConfig).
func needsAutoExposeNormalization(ac *store.AgentAppliedConfig) bool {
	if ac == nil || ac.CreateInputs == nil || ac.InlineConfig == nil {
		return false
	}
	if _, ok := ac.InlineConfig.Env[api.EnvAutoExposePorts]; !ok {
		return false
	}
	_, explicit := explicitEnvOf(ac)[api.EnvAutoExposePorts]
	return !explicit
}

// normalizeAutoExposeEnv rewrites a stamped SCION_AUTO_EXPOSE_PORTS into the
// shape the create pipeline produces today, and reports whether it changed
// ac. It runs only when needsAutoExposeNormalization holds. The stamp leaves
// InlineConfig.Env and AppliedConfig.Env (neither copy is explicit), the
// template value is filled as resolveDerivedConfig's template-env fill does,
// and resolveAutoExposeEnv applies the project tier. Without a project or
// template value the key stays absent and the hub default reaches the agent
// through HubAgentDefaults at dispatch, below harness-config env.
//
// The stamp is not moved into AppliedConfig.Env: that map is the broker's
// top env tier, while InlineConfig.Env sits below harness-config env, so a
// moved hub-default stamp would outrank harness-config env and freeze an old
// hub default. The result for SCION_AUTO_EXPOSE_PORTS equals what
// buildFreshAppliedConfig derives for the same agent. A second call is a
// no-op.
func normalizeAutoExposeEnv(ac *store.AgentAppliedConfig, src *autoExposeSources) bool {
	if !needsAutoExposeNormalization(ac) {
		return false
	}
	delete(ac.InlineConfig.Env, api.EnvAutoExposePorts)
	delete(ac.Env, api.EnvAutoExposePorts)
	var project *store.Project
	if src != nil {
		project = src.project
		if v, ok := src.templateEnv[api.EnvAutoExposePorts]; ok {
			if ac.Env == nil {
				ac.Env = make(map[string]string)
			}
			ac.Env[api.EnvAutoExposePorts] = v
		}
	}
	resolveAutoExposeEnv(ac, project, explicitEnvOf(ac))
	return true
}

// autoExposeSourcesFor loads the project and template tiers for agent. A
// project or template that no longer exists contributes nothing, as at
// reincarnate; any other lookup error is returned so the caller skips the
// agent rather than dropping a tier.
func (e *AppliedConfigEnvCleanupExecutor) autoExposeSourcesFor(ctx context.Context, agent *store.Agent) (*autoExposeSources, error) {
	src := &autoExposeSources{}
	if agent.ProjectID != "" {
		project, ok := e.projectCache[agent.ProjectID]
		if !ok {
			var err error
			project, err = e.Store.GetProject(ctx, agent.ProjectID)
			if errors.Is(err, store.ErrNotFound) {
				project, err = nil, nil
			}
			if err != nil {
				return nil, fmt.Errorf("get project %s: %w", agent.ProjectID, err)
			}
			e.projectCache[agent.ProjectID] = project
		}
		src.project = project
	}
	if id := agent.AppliedConfig.TemplateID; id != "" {
		tmpl, err := e.Store.GetTemplate(ctx, id)
		switch {
		case errors.Is(err, store.ErrNotFound):
		case err != nil:
			return nil, fmt.Errorf("get template %s: %w", id, err)
		case tmpl != nil && tmpl.Config != nil:
			src.templateEnv = tmpl.Config.Env
		}
	}
	return src, nil
}

func normalizeVerb(dryRun bool) string {
	if dryRun {
		return "WOULD RE-DERIVE"
	}
	return "RE-DERIVE"
}
