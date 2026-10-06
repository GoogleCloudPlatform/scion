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
	"io"

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

// AutoExposeEnvNormalizeExecutor re-derives SCION_AUTO_EXPOSE_PORTS for
// agents whose InlineConfig.Env still holds a value an older hub stamped
// there (see normalizeAutoExposeEnv). It is a rerunnable maintenance
// migration (see resolveMaintenanceExecutor, key
// "auto-expose-env-normalize") and touches no other env key.
//
// An agent whose project or template lookup fails is skipped with a WARN and
// left as it was, so a later run retries it; a skipped agent never loses a
// tier. Values are never logged, only agent IDs and counts. Every write goes
// through an optimistic-lock retry that re-applies the normalization to the
// latest row, so the migration is safe alongside normal traffic, and it is
// idempotent: a normalized agent no longer matches
// needsAutoExposeNormalization.
type AutoExposeEnvNormalizeExecutor struct {
	Store store.Store

	// projectCache memoizes project lookups within one Run; a nil entry
	// records a missing project.
	projectCache map[string]*store.Project
}

// autoExposeEnvNormalizeResult summarizes one run.
type autoExposeEnvNormalizeResult struct {
	AgentsScanned    int `json:"agentsScanned"`
	AgentsNormalized int `json:"agentsNormalized"`
	AgentsSkipped    int `json:"agentsSkipped"`
}

func (e *AutoExposeEnvNormalizeExecutor) Run(ctx context.Context, logger io.Writer, params map[string]string) error {
	_, err := e.run(ctx, logger, params)
	return err
}

// run does the work of Run and returns the summary it logs.
func (e *AutoExposeEnvNormalizeExecutor) run(ctx context.Context, logger io.Writer, params map[string]string) (autoExposeEnvNormalizeResult, error) {
	dryRun := params["dryRun"] == "true"
	if dryRun {
		_, _ = fmt.Fprintln(logger, "DRY RUN: no changes will be made.")
	}
	e.projectCache = make(map[string]*store.Project)

	result := autoExposeEnvNormalizeResult{}
	cursor := ""
	const pageSize = 200
	for {
		page, err := e.Store.ListAgents(ctx, store.AgentFilter{IncludeDeleted: true}, store.ListOptions{
			Limit:          pageSize,
			Cursor:         cursor,
			SkipTotalCount: true,
		})
		if err != nil {
			return result, fmt.Errorf("list agents: %w", err)
		}
		for i := range page.Items {
			agent := &page.Items[i]
			result.AgentsScanned++
			if !needsAutoExposeNormalization(agent.AppliedConfig) {
				continue
			}
			src, err := e.autoExposeSourcesFor(ctx, agent)
			if err != nil {
				result.AgentsSkipped++
				_, _ = fmt.Fprintf(logger, "  WARN agent=%s - skipped, retried by the next run: %v\n", agent.ID, err)
				continue
			}
			if !dryRun {
				written, err := e.normalizeWithRetry(ctx, agent.ID, src)
				if err != nil {
					result.AgentsSkipped++
					_, _ = fmt.Fprintf(logger, "  WARN agent=%s - failed to update, retried by the next run: %v\n", agent.ID, err)
					continue
				}
				if !written {
					continue
				}
			}
			result.AgentsNormalized++
			_, _ = fmt.Fprintf(logger, "  %s agent=%s key=%s\n", normalizeVerb(dryRun), agent.ID, api.EnvAutoExposePorts)
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}

	_, _ = fmt.Fprintf(logger, "Scanned %d agent(s); re-derived auto-expose on %d agent(s); skipped %d agent(s).\n",
		result.AgentsScanned, result.AgentsNormalized, result.AgentsSkipped)
	return result, nil
}

// normalizeWithRetry re-reads the agent, applies normalizeAutoExposeEnv and
// writes the row, retrying on an optimistic-lock conflict against the latest
// row. It reports whether a row was written; a row a concurrent writer has
// already normalized is not.
func (e *AutoExposeEnvNormalizeExecutor) normalizeWithRetry(ctx context.Context, agentID string, src *autoExposeSources) (bool, error) {
	const maxAttempts = 5
	for attempt := 0; attempt < maxAttempts; attempt++ {
		agent, err := e.Store.GetAgent(ctx, agentID)
		if err != nil {
			return false, err
		}
		if !normalizeAutoExposeEnv(agent.AppliedConfig, src) {
			return false, nil
		}
		err = e.Store.UpdateAgent(ctx, agent)
		if err == nil {
			return true, nil
		}
		if !errors.Is(err, store.ErrVersionConflict) {
			return false, err
		}
	}
	return false, fmt.Errorf("agent %s: gave up after %d version-conflict retries", agentID, maxAttempts)
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

// autoExposeSourcesFor loads the project and template tiers for agent. The
// template is resolved by the agent's template reference, as reincarnate
// does (resolveTemplateRef), so a template re-pushed under a new ID is still
// found. A project or template that no longer exists contributes nothing, as
// at reincarnate; any other lookup error is returned so the caller skips the
// agent rather than dropping a tier.
func (e *AutoExposeEnvNormalizeExecutor) autoExposeSourcesFor(ctx context.Context, agent *store.Agent) (*autoExposeSources, error) {
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
	if agent.Template != "" {
		tmpl, err := resolveTemplateRef(ctx, e.Store, agent.Template, agent.ProjectID)
		if err != nil {
			return nil, fmt.Errorf("resolve template %s: %w", agent.Template, err)
		}
		if tmpl != nil && tmpl.Config != nil {
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
