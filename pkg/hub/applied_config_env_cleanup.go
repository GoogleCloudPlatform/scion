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

	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// AppliedConfigEnvCleanupExecutor strips agent.AppliedConfig.Env entries that
// should never have been written there by a merge-back that predates the
// allowlist enforced in shouldPersistResolvedEnvKey (httpdispatcher.go). It
// is a maintenance migration (see resolveMaintenanceExecutor, key
// "applied-config-env-cleanup") run once per hub; the write-path fix stops
// new rows from acquiring these entries, this cleans up rows written before
// the fix shipped.
//
// It never reads or logs the *value* of any Env entry -- only key names,
// scopes and counts. Every mutation goes through the same optimistic-lock
// retry pattern as updateAgentAfterDispatch, so it is safe to run
// concurrently with normal traffic and safe to re-run (a clean row is a
// no-op, so the operation is idempotent).
//
// What it can and cannot detect, precisely:
//
//   - "GITHUB_TOKEN" is stripped from every row unconditionally. It is never
//     a legitimate entry in this map.
//   - Any other key is stripped if it matches the name of an entry in the
//     agent's reachable env-var or secret scopes (user, project, runtime
//     broker) AND that entry is itself secret-flagged (EnvVar.Secret==true)
//     or lives in the secret store. A matching EnvVar with Secret==false is
//     left alone -- that is a legitimately plain, user-declared value, and
//     the whole reason the merge-back this cleanup is undoing existed in the
//     first place was to surface exactly those values in the config-edit UI.
//   - A key that matches nothing live is left in place by this sweep: if the
//     env var or secret it came from has since been deleted, there is no
//     remaining record to identify it by, and this job cannot tell that case
//     apart from a value the user genuinely typed into their agent's config
//     directly (which this job must not delete). That residual case is
//     covered independently by the response-side redaction
//     (redactAppliedConfigEnvForResponse): GITHUB_TOKEN is always withheld,
//     and the rest of the map is withheld from any caller who does not
//     already have attach-equivalent access to the agent, regardless of
//     whether this sweep identified the specific key.
type AppliedConfigEnvCleanupExecutor struct {
	Store         store.Store
	SecretBackend secret.SecretBackend // optional; nil is handled
}

// appliedConfigEnvCleanupResult is a machine-readable summary, mirroring the
// shape of SecretMigrationResult for consistency with other migrations.
type appliedConfigEnvCleanupResult struct {
	AgentsScanned int `json:"agentsScanned"`
	AgentsUpdated int `json:"agentsUpdated"`
	KeysStripped  int `json:"keysStripped"`
}

func (e *AppliedConfigEnvCleanupExecutor) Run(ctx context.Context, logger io.Writer, params map[string]string) error {
	dryRun := params["dryRun"] == "true"
	if dryRun {
		_, _ = fmt.Fprintln(logger, "DRY RUN: no changes will be made.")
	}

	result := appliedConfigEnvCleanupResult{}
	cursor := ""
	const pageSize = 200

	for {
		page, err := e.Store.ListAgents(ctx, store.AgentFilter{IncludeDeleted: true}, store.ListOptions{
			Limit:          pageSize,
			Cursor:         cursor,
			SkipTotalCount: true,
		})
		if err != nil {
			return fmt.Errorf("list agents: %w", err)
		}

		for i := range page.Items {
			agent := &page.Items[i]
			result.AgentsScanned++

			if agent.AppliedConfig == nil || len(agent.AppliedConfig.Env) == 0 {
				continue
			}

			toStrip := e.keysToStrip(ctx, agent)
			if len(toStrip) == 0 {
				continue
			}

			for _, k := range toStrip {
				_, _ = fmt.Fprintf(logger, "  %s agent=%s key=%s\n", stripVerb(dryRun), agent.ID, k)
			}
			result.KeysStripped += len(toStrip)
			result.AgentsUpdated++

			if dryRun {
				continue
			}
			if err := e.stripKeysWithRetry(ctx, agent.ID, toStrip); err != nil {
				_, _ = fmt.Fprintf(logger, "  WARN agent=%s - failed to update: %v\n", agent.ID, err)
			}
		}

		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}

	_, _ = fmt.Fprintf(logger, "Scanned %d agent(s); stripped %d key(s) across %d agent(s).\n",
		result.AgentsScanned, result.KeysStripped, result.AgentsUpdated)
	return nil
}

func stripVerb(dryRun bool) string {
	if dryRun {
		return "WOULD STRIP"
	}
	return "STRIP"
}

// keysToStrip returns the AppliedConfig.Env keys of agent that this cleanup
// can positively identify as unsafe to keep, per the rules documented on
// AppliedConfigEnvCleanupExecutor. It never returns a value, only key names.
func (e *AppliedConfigEnvCleanupExecutor) keysToStrip(ctx context.Context, agent *store.Agent) []string {
	envVarsByKey := e.reachableEnvVarsByKey(ctx, agent)
	secretNames := e.reachableSecretNames(ctx, agent)

	var toStrip []string
	for k := range agent.AppliedConfig.Env {
		if k == "GITHUB_TOKEN" {
			toStrip = append(toStrip, k)
			continue
		}
		if v, ok := envVarsByKey[k]; ok {
			if v.Secret {
				toStrip = append(toStrip, k)
			}
			continue // a matching non-secret EnvVar is known-plain: preserve
		}
		if secretNames[k] {
			toStrip = append(toStrip, k)
		}
	}
	return toStrip
}

// reachableEnvVarsByKey lists the agent's user/project/runtime-broker scoped
// env vars, keyed by name. Precedence does not matter here (unlike dispatch
// resolution): any scope's declaration of a key is enough to identify it as
// a legitimate storage-declared entry.
func (e *AppliedConfigEnvCleanupExecutor) reachableEnvVarsByKey(ctx context.Context, agent *store.Agent) map[string]store.EnvVar {
	out := make(map[string]store.EnvVar)
	for _, filter := range envVarScopeFilters(agent) {
		vars, err := e.Store.ListEnvVars(ctx, filter)
		if err != nil {
			continue // best-effort: an unreachable scope just yields no match
		}
		for _, v := range vars {
			if _, exists := out[v.Key]; !exists {
				out[v.Key] = v
			}
		}
	}
	return out
}

// reachableSecretNames lists the names (never values) of secrets reachable
// from the agent's user, project, and runtime-broker scopes.
func (e *AppliedConfigEnvCleanupExecutor) reachableSecretNames(ctx context.Context, agent *store.Agent) map[string]bool {
	out := make(map[string]bool)
	if e.SecretBackend == nil {
		return out
	}
	for _, sc := range secretScopeFilters(agent) {
		metas, err := e.SecretBackend.List(ctx, sc)
		if err != nil {
			continue
		}
		for _, m := range metas {
			out[m.Name] = true
			// Environment-type secrets are injected under their Target key,
			// not their store Name (see buildCreateRequest's
			// s.Type == "environment" handling), so both must be checked.
			if m.Target != "" {
				out[m.Target] = true
			}
		}
	}
	return out
}

func envVarScopeFilters(agent *store.Agent) []store.EnvVarFilter {
	var filters []store.EnvVarFilter
	if agent.OwnerID != "" {
		filters = append(filters, store.EnvVarFilter{Scope: store.ScopeUser, ScopeID: agent.OwnerID})
	}
	if agent.ProjectID != "" {
		filters = append(filters, store.EnvVarFilter{Scope: store.ScopeProject, ScopeID: agent.ProjectID})
	}
	if agent.RuntimeBrokerID != "" {
		filters = append(filters, store.EnvVarFilter{Scope: store.ScopeRuntimeBroker, ScopeID: agent.RuntimeBrokerID})
	}
	return filters
}

func secretScopeFilters(agent *store.Agent) []secret.Filter {
	var filters []secret.Filter
	if agent.OwnerID != "" {
		filters = append(filters, secret.Filter{Scope: secret.ScopeUser, ScopeID: agent.OwnerID})
	}
	if agent.ProjectID != "" {
		filters = append(filters, secret.Filter{Scope: secret.ScopeProject, ScopeID: agent.ProjectID})
	}
	if agent.RuntimeBrokerID != "" {
		filters = append(filters, secret.Filter{Scope: secret.ScopeRuntimeBroker, ScopeID: agent.RuntimeBrokerID})
	}
	return filters
}

// stripKeysWithRetry deletes the given keys from the agent's persisted
// AppliedConfig.Env, retrying on an optimistic-lock conflict by re-reading
// the latest row and recomputing which of the target keys are still
// present (a concurrent writer may have already removed or changed them).
// Bounded at a handful of attempts so a pathologically hot row cannot spin
// the migration forever; a row that keeps losing the race is simply picked
// up again on the next run of this (idempotent) migration.
func (e *AppliedConfigEnvCleanupExecutor) stripKeysWithRetry(ctx context.Context, agentID string, keys []string) error {
	const maxAttempts = 5
	remove := make(map[string]bool, len(keys))
	for _, k := range keys {
		remove[k] = true
	}

	for attempt := 0; attempt < maxAttempts; attempt++ {
		agent, err := e.Store.GetAgent(ctx, agentID)
		if err != nil {
			return err
		}
		if agent.AppliedConfig == nil || len(agent.AppliedConfig.Env) == 0 {
			return nil // already clean
		}

		changed := false
		for k := range remove {
			if _, ok := agent.AppliedConfig.Env[k]; ok {
				delete(agent.AppliedConfig.Env, k)
				changed = true
			}
		}
		if !changed {
			return nil
		}

		err = e.Store.UpdateAgent(ctx, agent)
		if err == nil {
			return nil
		}
		if !errors.Is(err, store.ErrVersionConflict) {
			return err
		}
		// Lost the race: loop and retry against the latest row.
	}
	return fmt.Errorf("agent %s: gave up after %d version-conflict retries", agentID, maxAttempts)
}
