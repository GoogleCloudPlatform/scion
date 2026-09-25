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

// AppliedConfigEnvCleanupExecutor strips agent.AppliedConfig.Env and
// agent.AppliedConfig.InlineConfig.Env entries that should never have been
// written there by a merge-back that predates the allowlist enforced in
// shouldPersistResolvedEnvKey (httpdispatcher.go). It is a maintenance
// migration (see resolveMaintenanceExecutor, key
// "applied-config-env-cleanup") run once per hub; the write-path fix stops
// new rows from acquiring these entries, this cleans up rows written before
// the fix shipped. InlineConfig.Env is affected on any row where the create
// path's `ac.Env = req.Config.Env; ac.InlineConfig = req.Config` aliasing
// (handlers_agent_create_helpers.go) meant a later Env-only merge-back wrote
// into the same underlying map InlineConfig.Env pointed at; the DB round
// trip does not preserve that aliasing, so both fields must be swept
// independently once a row is loaded back.
//
// It never reads or logs the *value* of any Env entry -- only key names,
// scopes and counts. Every mutation goes through the same optimistic-lock
// retry pattern as updateAgentAfterDispatch, so it is safe to run
// concurrently with normal traffic and safe to re-run (a clean row is a
// no-op, so the operation is idempotent).
//
// The decision for each AppliedConfig.Env key, other than GITHUB_TOKEN
// (always stripped):
//
//   - If the key matches the name (or, for an environment-type secret, the
//     injection target) of an entry in the agent's reachable secret scopes
//     (user, project, runtime broker, hub) or a Secret==true EnvVar, it is
//     stripped unconditionally. A live secret sharing a name with the key is
//     reason enough on its own, independent of what value is currently
//     sitting in the row -- a plain var that happens to share that same name
//     does not override this.
//   - Otherwise, the key is kept only if its persisted value equals what a
//     currently resolvable plain source would produce for that same key
//     today: the agent's applied template's default env, its InlineConfig
//     (explicit config-time env), or a Secret==false EnvVar in a reachable
//     scope. This is an allowlist, not a denylist -- a key with no matching
//     live source (for example, one whose originating secret has since been
//     deleted) is stripped along with the rest, since this job cannot tell
//     that case apart from a value that predates the write-path fix.
//
// InlineConfig.Env keys are decided by a narrower rule: only the GITHUB_TOKEN
// and live-secret-name checks above apply. InlineConfig is itself one of the
// currently-resolvable plain sources the AppliedConfig.Env allowlist checks
// against, so an explicit, user-typed --config value has no other source to
// match by design -- applying the same allowlist to InlineConfig.Env would
// delete legitimate explicit env instead of only the residue this cleanup
// exists to remove.
//
// The response-side redaction (redactAppliedConfigEnvForResponse,
// AgentAppliedConfig.ResponseView) remains the backstop regardless of what
// this sweep identifies in either field: GITHUB_TOKEN is always withheld,
// and the rest is withheld from any caller who does not already have
// attach-equivalent access to the agent.
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

			if agent.AppliedConfig == nil {
				continue
			}
			hasAppliedEnv := len(agent.AppliedConfig.Env) > 0
			hasInlineEnv := agent.AppliedConfig.InlineConfig != nil && len(agent.AppliedConfig.InlineConfig.Env) > 0
			if !hasAppliedEnv && !hasInlineEnv {
				continue
			}

			appliedStrip, inlineStrip := e.keysToStrip(ctx, agent)
			if len(appliedStrip) == 0 && len(inlineStrip) == 0 {
				continue
			}

			for _, k := range appliedStrip {
				_, _ = fmt.Fprintf(logger, "  %s agent=%s field=appliedConfig.env key=%s\n", stripVerb(dryRun), agent.ID, k)
			}
			for _, k := range inlineStrip {
				_, _ = fmt.Fprintf(logger, "  %s agent=%s field=inlineConfig.env key=%s\n", stripVerb(dryRun), agent.ID, k)
			}
			result.KeysStripped += len(appliedStrip) + len(inlineStrip)
			result.AgentsUpdated++

			if dryRun {
				continue
			}
			if err := e.stripKeysWithRetry(ctx, agent.ID, appliedStrip, inlineStrip); err != nil {
				_, _ = fmt.Fprintf(logger, "  WARN agent=%s - failed to update: %v\n", agent.ID, err)
			}
		}

		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}

	_, _ = fmt.Fprintf(logger, "Scanned %d agent(s); normalized %d agent config row(s) (%d field(s)).\n",
		result.AgentsScanned, result.AgentsUpdated, result.KeysStripped)
	return nil
}

func stripVerb(dryRun bool) string {
	if dryRun {
		return "WOULD STRIP"
	}
	return "STRIP"
}

// keysToStrip returns the keys to remove from AppliedConfig.Env (an
// allowlist -- see the type doc) and, separately, the keys to remove from
// AppliedConfig.InlineConfig.Env (a narrower denylist: GITHUB_TOKEN and any
// live secret-name match only).
//
// InlineConfig.Env is not put through the same allowlist as AppliedConfig.Env
// because InlineConfig is itself one of the currently-resolvable plain
// sources that allowlist checks a key's value against (see
// resolvablePlainValues) -- an explicit, user-typed --config value has no
// other source to match against by design, so demanding one here would
// delete legitimate explicit env instead of only the residue this cleanup
// exists to remove.
func (e *AppliedConfigEnvCleanupExecutor) keysToStrip(ctx context.Context, agent *store.Agent) (applied, inline []string) {
	envVarsByKey := e.reachableEnvVarsByKey(ctx, agent)
	secretNames := e.reachableSecretNames(ctx, agent)
	plainValues := e.resolvablePlainValues(ctx, agent, envVarsByKey)

	isKnownSecret := func(k string) bool {
		if k == "GITHUB_TOKEN" {
			return true
		}
		if envVar, ok := envVarsByKey[k]; ok && envVar.Secret {
			return true
		}
		return secretNames[k]
	}

	for k, v := range agent.AppliedConfig.Env {
		if isKnownSecret(k) {
			applied = append(applied, k)
			continue
		}
		if values, ok := plainValues[k]; ok && values[v] {
			continue // matches a currently resolvable plain source: keep
		}
		applied = append(applied, k)
	}

	if agent.AppliedConfig.InlineConfig != nil {
		for k := range agent.AppliedConfig.InlineConfig.Env {
			if isKnownSecret(k) {
				inline = append(inline, k)
			}
		}
	}

	return applied, inline
}

// resolvablePlainValues returns, for every key, the set of values a
// currently live, non-secret source would produce for that key today: the
// agent's applied template's default env, its InlineConfig (explicit
// config-time env), and any reachable Secret==false EnvVar. A key/value pair
// present here is provably still an intentional plain declaration as of this
// run, not a residual value from before the write-path fix. This mirrors the
// persistence allowlist in shouldPersistResolvedEnvKey (httpdispatcher.go),
// applied after the fact to rows that were already written.
func (e *AppliedConfigEnvCleanupExecutor) resolvablePlainValues(ctx context.Context, agent *store.Agent, envVarsByKey map[string]store.EnvVar) map[string]map[string]bool {
	values := make(map[string]map[string]bool)
	add := func(k, v string) {
		set, ok := values[k]
		if !ok {
			set = make(map[string]bool)
			values[k] = set
		}
		set[v] = true
	}

	if agent.AppliedConfig != nil {
		if agent.AppliedConfig.TemplateID != "" {
			if tmpl, err := e.Store.GetTemplate(ctx, agent.AppliedConfig.TemplateID); err == nil && tmpl != nil && tmpl.Config != nil {
				for k, v := range tmpl.Config.Env {
					add(k, v)
				}
			}
		}
		if agent.AppliedConfig.InlineConfig != nil {
			for k, v := range agent.AppliedConfig.InlineConfig.Env {
				add(k, v)
			}
		}
	}

	for _, ev := range envVarsByKey {
		if !ev.Secret {
			add(ev.Key, ev.Value)
		}
	}

	return values
}

// reachableEnvVarsByKey lists the agent's user/project/runtime-broker/hub
// scoped env vars, keyed by name. Precedence does not matter here (unlike
// dispatch resolution): any scope's declaration of a key is enough to
// identify it as a legitimate storage-declared entry.
func (e *AppliedConfigEnvCleanupExecutor) reachableEnvVarsByKey(ctx context.Context, agent *store.Agent) map[string]store.EnvVar {
	out := make(map[string]store.EnvVar)
	for _, filter := range e.envVarScopeFilters(agent) {
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
// from the agent's user, project, runtime-broker and hub scopes.
func (e *AppliedConfigEnvCleanupExecutor) reachableSecretNames(ctx context.Context, agent *store.Agent) map[string]bool {
	out := make(map[string]bool)
	if e.SecretBackend == nil {
		return out
	}
	for _, sc := range e.secretScopeFilters(agent) {
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

// hubScopeID returns the hub instance ID to use for hub-scoped lookups, or
// "" if none is configured (SecretBackend is the only source of it this
// executor has). An empty ScopeID still queries the hub scope -- the store
// only constrains by ScopeID when it is non-empty -- so this degrades to
// "every hub-scoped row" rather than silently skipping the scope.
func (e *AppliedConfigEnvCleanupExecutor) hubScopeID() string {
	if e.SecretBackend == nil {
		return ""
	}
	return e.SecretBackend.HubID()
}

func (e *AppliedConfigEnvCleanupExecutor) envVarScopeFilters(agent *store.Agent) []store.EnvVarFilter {
	filters := []store.EnvVarFilter{{Scope: store.ScopeHub, ScopeID: e.hubScopeID()}}
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

func (e *AppliedConfigEnvCleanupExecutor) secretScopeFilters(agent *store.Agent) []secret.Filter {
	filters := []secret.Filter{{Scope: secret.ScopeHub, ScopeID: e.hubScopeID()}}
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

// stripKeysWithRetry deletes appliedKeys from the agent's persisted
// AppliedConfig.Env and inlineKeys from AppliedConfig.InlineConfig.Env (both
// in the same read-modify-write), retrying on an optimistic-lock conflict by
// re-reading the latest row and recomputing which of the target keys are
// still present (a concurrent writer may have already removed or changed
// them). Bounded at a handful of attempts so a pathologically hot row cannot
// spin the migration forever; a row that keeps losing the race is simply
// picked up again on the next run of this (idempotent) migration.
func (e *AppliedConfigEnvCleanupExecutor) stripKeysWithRetry(ctx context.Context, agentID string, appliedKeys, inlineKeys []string) error {
	const maxAttempts = 5
	removeApplied := make(map[string]bool, len(appliedKeys))
	for _, k := range appliedKeys {
		removeApplied[k] = true
	}
	removeInline := make(map[string]bool, len(inlineKeys))
	for _, k := range inlineKeys {
		removeInline[k] = true
	}

	for attempt := 0; attempt < maxAttempts; attempt++ {
		agent, err := e.Store.GetAgent(ctx, agentID)
		if err != nil {
			return err
		}
		if agent.AppliedConfig == nil {
			return nil // already clean
		}

		changed := false
		for k := range removeApplied {
			if _, ok := agent.AppliedConfig.Env[k]; ok {
				delete(agent.AppliedConfig.Env, k)
				changed = true
			}
		}
		if agent.AppliedConfig.InlineConfig != nil {
			for k := range removeInline {
				if _, ok := agent.AppliedConfig.InlineConfig.Env[k]; ok {
					delete(agent.AppliedConfig.InlineConfig.Env, k)
					changed = true
				}
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
