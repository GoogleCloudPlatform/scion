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
// CreateInputs.InlineConfig.Env (the create request's explicit config, kept
// for `scion reincarnate`) gets the InlineConfig.Env rule for the same reason.
//
// The same sweep then visits every agent_reincarnations record, including
// records whose agent has since been deleted, and applies these rules to both
// config snapshots (previous_applied_config and new_applied_config). A
// snapshot's Env gets the AppliedConfig.Env rule, evaluated against the
// owning agent's scopes and the snapshot's own template and InlineConfig;
// its InlineConfig.Env and CreateInputs.InlineConfig.Env get the narrow
// rule. When the owning agent row no longer exists there are no agent scopes
// to evaluate against, so Env falls back to the narrow rule too (hub-scope
// secret names plus GITHUB_TOKEN). Non-terminal records are skipped and
// counted: they are live rollback sources owned by a running reincarnation,
// and a later run picks them up once they are terminal.
//
// The response-side redaction (redactAppliedConfigEnvForResponse,
// AgentAppliedConfig.ResponseView) remains the backstop regardless of what
// this sweep identifies in either field: GITHUB_TOKEN is always withheld,
// and the rest is withheld from any caller who does not already have
// attach-equivalent access to the agent.
type AppliedConfigEnvCleanupExecutor struct {
	Store         store.Store
	SecretBackend secret.SecretBackend // optional; nil is handled

	// envVarCache and secretCache memoize per-scope lookups (keyed by
	// scope+"/"+scopeID) across the agents visited by a single Run call.
	// The hub scope and, within one project, the project scope are shared
	// by every agent that reaches them, so without this cache a sweep of N
	// agents re-fetches the same scope's data up to N times. Run
	// (re)initializes both maps at the start of every call, so caching is
	// scoped to a single sweep and carries nothing between Run invocations
	// even if a caller reused one executor instance across calls -- not how
	// resolveMaintenanceExecutor wires it today, but cheap to keep true. A
	// failed lookup is cached too (as a nil/empty result): this matches the
	// pre-existing best-effort handling of an unreachable scope, just
	// applied once per sweep instead of once per agent.
	envVarCache map[string][]store.EnvVar
	secretCache map[string][]secret.SecretMeta

	// agentCache memoizes the owning-agent lookup for reincarnation records
	// within one Run; a nil entry records that the agent row is gone.
	agentCache map[string]*store.Agent
}

// appliedConfigEnvCleanupResult is a machine-readable summary, mirroring the
// shape of SecretMigrationResult for consistency with other migrations.
type appliedConfigEnvCleanupResult struct {
	AgentsScanned int `json:"agentsScanned"`
	AgentsUpdated int `json:"agentsUpdated"`
	KeysStripped  int `json:"keysStripped"`

	ReincarnationsScanned            int `json:"reincarnationsScanned"`
	ReincarnationsUpdated            int `json:"reincarnationsUpdated"`
	ReincarnationsSkippedNonTerminal int `json:"reincarnationsSkippedNonTerminal"`
	ReincarnationKeysStripped        int `json:"reincarnationKeysStripped"`
}

func (e *AppliedConfigEnvCleanupExecutor) Run(ctx context.Context, logger io.Writer, params map[string]string) error {
	dryRun := params["dryRun"] == "true"
	if dryRun {
		_, _ = fmt.Fprintln(logger, "DRY RUN: no changes will be made.")
	}

	e.envVarCache = make(map[string][]store.EnvVar)
	e.secretCache = make(map[string][]secret.SecretMeta)
	e.agentCache = make(map[string]*store.Agent)

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
			if !appliedConfigHasEnv(agent.AppliedConfig) {
				continue
			}

			appliedStrip, inlineStrip, createInputsStrip := e.keysToStrip(ctx, agent, agent.AppliedConfig, false)
			if len(appliedStrip) == 0 && len(inlineStrip) == 0 && len(createInputsStrip) == 0 {
				continue
			}

			for _, k := range appliedStrip {
				_, _ = fmt.Fprintf(logger, "  %s agent=%s field=appliedConfig.env key=%s\n", stripVerb(dryRun), agent.ID, k)
			}
			for _, k := range inlineStrip {
				_, _ = fmt.Fprintf(logger, "  %s agent=%s field=inlineConfig.env key=%s\n", stripVerb(dryRun), agent.ID, k)
			}
			for _, k := range createInputsStrip {
				_, _ = fmt.Fprintf(logger, "  %s agent=%s field=createInputs.inlineConfig.env key=%s\n", stripVerb(dryRun), agent.ID, k)
			}
			result.KeysStripped += len(appliedStrip) + len(inlineStrip) + len(createInputsStrip)
			result.AgentsUpdated++

			if dryRun {
				continue
			}
			if err := e.stripKeysWithRetry(ctx, agent.ID, appliedStrip, inlineStrip, createInputsStrip); err != nil {
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

	if err := e.cleanReincarnationSnapshots(ctx, logger, dryRun, &result); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(logger, "Scanned %d reincarnation record(s); normalized %d (%d field(s)); skipped %d non-terminal record(s).\n",
		result.ReincarnationsScanned, result.ReincarnationsUpdated, result.ReincarnationKeysStripped, result.ReincarnationsSkippedNonTerminal)
	return nil
}

// appliedConfigHasEnv reports whether cfg has any env map this cleanup
// inspects.
func appliedConfigHasEnv(cfg *store.AgentAppliedConfig) bool {
	if cfg == nil {
		return false
	}
	if len(cfg.Env) > 0 {
		return true
	}
	if cfg.InlineConfig != nil && len(cfg.InlineConfig.Env) > 0 {
		return true
	}
	return createInputsInlineEnv(cfg) != nil
}

// createInputsInlineEnv returns cfg.CreateInputs.InlineConfig.Env, or nil.
func createInputsInlineEnv(cfg *store.AgentAppliedConfig) map[string]string {
	if cfg == nil || cfg.CreateInputs == nil || cfg.CreateInputs.InlineConfig == nil || len(cfg.CreateInputs.InlineConfig.Env) == 0 {
		return nil
	}
	return cfg.CreateInputs.InlineConfig.Env
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
//
// agent supplies the scopes (owner, project, runtime broker) the secret and
// env-var lookups run against; cfg is the config being decided, which is the
// agent's own AppliedConfig for an agent row or a snapshot for a
// reincarnation record. createInputs lists keys to remove from
// cfg.CreateInputs.InlineConfig.Env, decided by the same narrow rule as
// inline.
//
// narrowEnv applies the narrow rule to cfg.Env as well. It is set for a
// reincarnation snapshot whose agent row is gone: without the agent's scopes
// the "currently resolvable plain source" test cannot be evaluated, so only
// keys positively known to be secret are removed.
func (e *AppliedConfigEnvCleanupExecutor) keysToStrip(ctx context.Context, agent *store.Agent, cfg *store.AgentAppliedConfig, narrowEnv bool) (applied, inline, createInputs []string) {
	envVarsByKey := e.reachableEnvVarsByKey(ctx, agent)
	secretNames := e.reachableSecretNames(ctx, agent)
	plainValues := e.resolvablePlainValues(ctx, cfg, envVarsByKey)

	isKnownSecret := func(k string) bool {
		if k == "GITHUB_TOKEN" {
			return true
		}
		if envVar, ok := envVarsByKey[k]; ok && envVar.Secret {
			return true
		}
		return secretNames[k]
	}

	for k, v := range cfg.Env {
		if isKnownSecret(k) {
			applied = append(applied, k)
			continue
		}
		if narrowEnv {
			continue
		}
		if values, ok := plainValues[k]; ok && values[v] {
			continue // matches a currently resolvable plain source: keep
		}
		applied = append(applied, k)
	}

	if cfg.InlineConfig != nil {
		for k := range cfg.InlineConfig.Env {
			if isKnownSecret(k) {
				inline = append(inline, k)
			}
		}
	}

	for k := range createInputsInlineEnv(cfg) {
		if isKnownSecret(k) {
			createInputs = append(createInputs, k)
		}
	}

	return applied, inline, createInputs
}

// resolvablePlainValues returns, for every key, the set of values a
// currently live, non-secret source would produce for that key today: the
// agent's applied template's default env, its InlineConfig (explicit
// config-time env), and any reachable Secret==false EnvVar. A key/value pair
// present here is provably still an intentional plain declaration as of this
// run, not a residual value from before the write-path fix. This mirrors the
// persistence allowlist in shouldPersistResolvedEnvKey (httpdispatcher.go),
// applied after the fact to rows that were already written.
func (e *AppliedConfigEnvCleanupExecutor) resolvablePlainValues(ctx context.Context, cfg *store.AgentAppliedConfig, envVarsByKey map[string]store.EnvVar) map[string]map[string]bool {
	values := make(map[string]map[string]bool)
	add := func(k, v string) {
		set, ok := values[k]
		if !ok {
			set = make(map[string]bool)
			values[k] = set
		}
		set[v] = true
	}

	if cfg != nil {
		if cfg.TemplateID != "" {
			if tmpl, err := e.Store.GetTemplate(ctx, cfg.TemplateID); err == nil && tmpl != nil && tmpl.Config != nil {
				for k, v := range tmpl.Config.Env {
					add(k, v)
				}
			}
		}
		if cfg.InlineConfig != nil {
			for k, v := range cfg.InlineConfig.Env {
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

// cachedEnvVarsForScope returns filter's env vars, fetching them from the
// store only on the first call for that scope+scopeID within the current
// Run sweep and serving every subsequent call for the same key from
// envVarCache.
func (e *AppliedConfigEnvCleanupExecutor) cachedEnvVarsForScope(ctx context.Context, filter store.EnvVarFilter) []store.EnvVar {
	key := filter.Scope + "/" + filter.ScopeID
	if vars, ok := e.envVarCache[key]; ok {
		return vars
	}
	vars, err := e.Store.ListEnvVars(ctx, filter)
	if err != nil {
		vars = nil // best-effort: an unreachable scope just yields no match
	}
	e.envVarCache[key] = vars
	return vars
}

// cachedSecretsForScope is cachedEnvVarsForScope's counterpart for the
// secret backend.
func (e *AppliedConfigEnvCleanupExecutor) cachedSecretsForScope(ctx context.Context, filter secret.Filter) []secret.SecretMeta {
	key := filter.Scope + "/" + filter.ScopeID
	if metas, ok := e.secretCache[key]; ok {
		return metas
	}
	metas, err := e.SecretBackend.List(ctx, filter)
	if err != nil {
		metas = nil
	}
	e.secretCache[key] = metas
	return metas
}

// reachableEnvVarsByKey lists the agent's user/project/runtime-broker/hub
// scoped env vars, keyed by name. Precedence does not matter here (unlike
// dispatch resolution): any scope's declaration of a key is enough to
// identify it as a legitimate storage-declared entry.
func (e *AppliedConfigEnvCleanupExecutor) reachableEnvVarsByKey(ctx context.Context, agent *store.Agent) map[string]store.EnvVar {
	out := make(map[string]store.EnvVar)
	for _, filter := range e.envVarScopeFilters(agent) {
		for _, v := range e.cachedEnvVarsForScope(ctx, filter) {
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
		for _, m := range e.cachedSecretsForScope(ctx, sc) {
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
// AppliedConfig.Env, inlineKeys from AppliedConfig.InlineConfig.Env and
// createInputsKeys from AppliedConfig.CreateInputs.InlineConfig.Env (all in
// the same read-modify-write), retrying on an optimistic-lock conflict by
// re-reading the latest row and recomputing which of the target keys are
// still present (a concurrent writer may have already removed or changed
// them). Bounded at a handful of attempts so a pathologically hot row cannot
// spin the migration forever; a row that keeps losing the race is simply
// picked up again on the next run of this (idempotent) migration.
func (e *AppliedConfigEnvCleanupExecutor) stripKeysWithRetry(ctx context.Context, agentID string, appliedKeys, inlineKeys, createInputsKeys []string) error {
	const maxAttempts = 5
	removeApplied := make(map[string]bool, len(appliedKeys))
	for _, k := range appliedKeys {
		removeApplied[k] = true
	}
	removeInline := make(map[string]bool, len(inlineKeys))
	for _, k := range inlineKeys {
		removeInline[k] = true
	}
	removeCreateInputs := make(map[string]bool, len(createInputsKeys))
	for _, k := range createInputsKeys {
		removeCreateInputs[k] = true
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
		if env := createInputsInlineEnv(agent.AppliedConfig); env != nil {
			for k := range removeCreateInputs {
				if _, ok := env[k]; ok {
					delete(env, k)
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

// reincarnationSnapshotPageSize bounds each ListAgentReincarnationsPage call.
const reincarnationSnapshotPageSize = 200

// cleanReincarnationSnapshots applies the agent-row rules to both config
// snapshots of every terminal agent_reincarnations record. See the type doc
// for the per-field rules and the deleted-agent fallback.
func (e *AppliedConfigEnvCleanupExecutor) cleanReincarnationSnapshots(ctx context.Context, logger io.Writer, dryRun bool, result *appliedConfigEnvCleanupResult) error {
	afterID := ""
	for {
		page, err := e.Store.ListAgentReincarnationsPage(ctx, afterID, reincarnationSnapshotPageSize)
		if err != nil {
			return fmt.Errorf("list agent reincarnations: %w", err)
		}
		for _, rec := range page {
			result.ReincarnationsScanned++
			if store.IsAgentReincarnationStateNonTerminal(rec.State) {
				result.ReincarnationsSkippedNonTerminal++
				continue
			}
			if !appliedConfigHasEnv(rec.PreviousAppliedConfig) && !appliedConfigHasEnv(rec.NewAppliedConfig) {
				continue
			}

			scopeAgent, gone, err := e.reincarnationOwner(ctx, rec.AgentID)
			if err != nil {
				return err
			}

			stripped := 0
			for _, snap := range []struct {
				name string
				cfg  *store.AgentAppliedConfig
			}{
				{"previous", rec.PreviousAppliedConfig},
				{"new", rec.NewAppliedConfig},
			} {
				if !appliedConfigHasEnv(snap.cfg) {
					continue
				}
				applied, inline, createInputs := e.keysToStrip(ctx, scopeAgent, snap.cfg, gone)
				for _, field := range []struct {
					suffix string
					keys   []string
				}{
					{"env", applied},
					{"inlineConfig.env", inline},
					{"createInputs.inlineConfig.env", createInputs},
				} {
					for _, k := range field.keys {
						_, _ = fmt.Fprintf(logger, "  %s reincarnation=%s agent=%s field=reincarnation.%s.%s key=%s\n",
							stripVerb(dryRun), rec.ID, rec.AgentID, snap.name, field.suffix, k)
					}
				}
				stripped += len(applied) + len(inline) + len(createInputs)
				removeSnapshotEnvKeys(snap.cfg, applied, inline, createInputs)
			}
			if stripped == 0 {
				continue
			}
			result.ReincarnationKeysStripped += stripped
			result.ReincarnationsUpdated++

			if dryRun {
				continue
			}
			ok, err := e.Store.UpdateAgentReincarnationSnapshots(ctx, rec, rec.State)
			if err != nil {
				return fmt.Errorf("update reincarnation %s snapshots: %w", rec.ID, err)
			}
			if !ok {
				// The record changed state or was removed since it was read;
				// the next (idempotent) run re-evaluates it.
				_, _ = fmt.Fprintf(logger, "  WARN reincarnation=%s changed concurrently; left for the next run\n", rec.ID)
			}
		}
		if len(page) < reincarnationSnapshotPageSize {
			break
		}
		afterID = page[len(page)-1].ID
	}
	if result.ReincarnationsSkippedNonTerminal > 0 {
		_, _ = fmt.Fprintf(logger, "  skipped %d non-terminal reincarnation record(s); re-run once they finish\n",
			result.ReincarnationsSkippedNonTerminal)
	}
	return nil
}

// reincarnationOwner returns the agent whose scopes a reincarnation record's
// snapshots are evaluated against. When the agent row no longer exists it
// returns a stub with no owner, project or broker, so the scope lookups
// resolve hub-scope entries only, and gone == true.
func (e *AppliedConfigEnvCleanupExecutor) reincarnationOwner(ctx context.Context, agentID string) (agent *store.Agent, gone bool, err error) {
	if cached, ok := e.agentCache[agentID]; ok {
		if cached == nil {
			return &store.Agent{ID: agentID}, true, nil
		}
		return cached, false, nil
	}
	a, err := e.Store.GetAgent(ctx, agentID)
	if errors.Is(err, store.ErrNotFound) {
		e.agentCache[agentID] = nil
		return &store.Agent{ID: agentID}, true, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("get agent %s: %w", agentID, err)
	}
	e.agentCache[agentID] = a
	return a, false, nil
}

// removeSnapshotEnvKeys deletes the given keys from cfg's env maps in place.
func removeSnapshotEnvKeys(cfg *store.AgentAppliedConfig, applied, inline, createInputs []string) {
	for _, k := range applied {
		delete(cfg.Env, k)
	}
	if cfg.InlineConfig != nil {
		for _, k := range inline {
			delete(cfg.InlineConfig.Env, k)
		}
	}
	if env := createInputsInlineEnv(cfg); env != nil {
		for _, k := range createInputs {
			delete(env, k)
		}
	}
}
