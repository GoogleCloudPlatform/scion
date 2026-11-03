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
	"log/slog"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// agentTZEnvKey is the container environment variable the agent TZ chain
// resolves.
const agentTZEnvKey = "TZ"

// gatherAnswerTZ is the value the resolver returns, when asked to answer a
// broker's env-gather need for TZ, in place of "" (no TZ). An older broker
// that reports TZ as needed would otherwise block on, or fall back to its
// host value for, an empty answer.
const gatherAnswerTZ = "UTC"

// Timezone sources reported by resolveAgentTZ. They name the rung of the
// agent TZ chain that supplied the value.
const (
	// TZSourceExplicit: AgentAppliedConfig.ExplicitTimezone, set by an
	// explicit act (create-time TZ or a PATCH pin).
	TZSourceExplicit = "explicit"
	// TZSourceLegacy: ExplicitTimezone adopted from a TZ that an older hub
	// persisted in AgentAppliedConfig.Env.
	TZSourceLegacy = "legacy"
	// TZSourceUser, TZSourceProject, TZSourceHub and TZSourceBroker: a hub
	// env-var storage entry (injectionMode always) at that scope.
	TZSourceUser    = "user"
	TZSourceProject = "project"
	TZSourceHub     = "hub"
	TZSourceBroker  = "broker"
	// TZSourceProgeny: an ancestor's user-scope env var shared with
	// allowProgeny.
	TZSourceProgeny = "progeny"
	// TZSourceHubDefault: the hub's agent_defaults.default_timezone.
	TZSourceHubDefault = "hub-default"
	// TZSourceNone: no rung supplied a value. No TZ is sent, so a
	// hub-dispatched container runs the image default (UTC).
	TZSourceNone = "none"
)

// agentTZ is the outcome of the agent TZ chain: the value to put in the
// container's TZ ("" means send no TZ) and the rung that supplied it.
type agentTZ struct {
	TZ     string
	Source string
}

// chooseAgentTZ applies the agent TZ chain to already-gathered inputs. The
// first non-empty rung wins:
//
//  1. ac.ExplicitTimezone (source "explicit", or "legacy" when it was
//     adopted from a persisted Env TZ);
//  2. storage, the winning hub env-var storage TZ with its scope source
//     (user > project > hub > broker > progeny);
//  3. hubDefault, the hub's agent_defaults.default_timezone ("hub-default");
//  4. nothing ("" with source "none").
//
// There is deliberately no runtime-profile rung, and the agent's own Env is
// not consulted: TZ is not an env record once ExplicitTimezone exists, and an
// unadopted Env TZ must be adopted into ExplicitTimezone before resolving.
//
// forGatherAnswer turns rung 4 into "UTC" (source still "none"). Use it only
// to answer a broker that reported TZ as an env-gather need.
func chooseAgentTZ(ac *store.AgentAppliedConfig, storage agentTZ, hubDefault string, forGatherAnswer bool) agentTZ {
	if ac != nil && ac.ExplicitTimezone != "" {
		source := TZSourceExplicit
		if ac.ExplicitTimezoneLegacy {
			source = TZSourceLegacy
		}
		return agentTZ{TZ: ac.ExplicitTimezone, Source: source}
	}
	if storage.TZ != "" {
		return storage
	}
	if hubDefault != "" {
		return agentTZ{TZ: hubDefault, Source: TZSourceHubDefault}
	}
	if forGatherAnswer {
		return agentTZ{TZ: gatherAnswerTZ, Source: TZSourceNone}
	}
	return agentTZ{TZ: "", Source: TZSourceNone}
}

// resolveAgentTZ resolves the agent's container TZ and reports which rung
// supplied it. It is the single source of the TZ value the hub sends to a
// broker on create, start and restart, and of the timezone source shown to
// users. See chooseAgentTZ for the chain. The storage rung is read with
// resolveStorageTZ and the hub default is read live from
// hubAgentDefaultsProvider, so an edit to either reaches every unpinned
// agent at its next start.
//
// A storage read error is returned, never skipped: a lower-precedence rung
// (a lower scope, the hub default or none) must not stand in for a TZ the
// hub could not read. The error carries the backend detail for the hub log
// only; callers answer with a fixed message (a *secretResolutionError on a
// dispatch, EnvSettingsUnavailableText on a handler).
func (d *HTTPAgentDispatcher) resolveAgentTZ(ctx context.Context, agent *store.Agent, forGatherAnswer bool) (agentTZ, error) {
	var ac *store.AgentAppliedConfig
	if agent != nil {
		ac = agent.AppliedConfig
	}
	if ac != nil && ac.ExplicitTimezone != "" {
		// Rung 1 wins outright; skip the storage and settings reads.
		return chooseAgentTZ(ac, agentTZ{}, "", forGatherAnswer), nil
	}
	if legacy := legacyEnvTZ(ac); legacy != "" {
		// Every caller must adopt a legacy env TZ first. The result is
		// unchanged (the agent's Env is not a rung); the log makes a missed
		// adoption visible instead of silently dropping the agent's zone.
		d.warnTZ("unadopted legacy TZ in agent env; adoptLegacyTZ must run before resolveAgentTZ",
			"agentID", agent.ID, "tz", legacy)
	}
	storage, err := d.resolveStorageTZ(ctx, agent)
	if err != nil {
		return agentTZ{}, err
	}
	hubDefault := ""
	if storage.TZ == "" && d.hubAgentDefaultsProvider != nil {
		hubDefault = d.hubAgentDefaultsProvider().DefaultTimezone
	}
	return chooseAgentTZ(ac, storage, hubDefault, forGatherAnswer), nil
}

// finalizeEnvTZError logs a TZ storage read error on the finalize-env pass
// and returns the *secretResolutionError that fails it, so the caller
// answers with the fixed env-settings message and the agent is not created.
func (d *HTTPAgentDispatcher) finalizeEnvTZError(ctx context.Context, agent *store.Agent, err error) error {
	d.log.WarnContext(ctx, "DispatchFinalizeEnv: failed to read stored TZ settings; agent not created",
		"agent_id", agent.ID, "scope", tzReadScope(err))
	return &secretResolutionError{Verb: "created", Source: resolutionSourceEnvSettings, Err: err}
}

// warnTZ logs a TZ resolution warning. The resolver also runs on
// dispatchers built without a logger, so a nil log is skipped rather than
// dereferenced.
func (d *HTTPAgentDispatcher) warnTZ(msg string, args ...any) {
	if d.log != nil {
		d.log.Warn(msg, args...)
	}
}

// legacyEnvTZ returns the TZ an older hub persisted in the agent's env
// records (AppliedConfig.Env first, then InlineConfig.Env), or "".
func legacyEnvTZ(ac *store.AgentAppliedConfig) string {
	if ac == nil {
		return ""
	}
	if v := ac.Env[agentTZEnvKey]; v != "" {
		return v
	}
	if ac.InlineConfig != nil {
		if v := ac.InlineConfig.Env[agentTZEnvKey]; v != "" {
			return v
		}
	}
	return ""
}

// resolveStorageTZ returns the winning hub env-var storage TZ for the agent
// and its scope source, or a zero agentTZ if no scope supplies a non-empty
// value.
//
// It walks the same scopes in the same order as resolveEnvFromStorage
// (envScopePrecedence, lowest first, so a later scope overwrites an earlier
// one), then falls back to progeny vars. It differs in one way: an
// empty-valued entry never wins, because the TZ chain takes the first
// non-empty rung. as_needed entries are skipped: they are not a rung of the
// TZ chain.
//
// A read error on any scope, or on the progeny list, is returned rather than
// skipped, so a lower-precedence value never wins because a higher scope
// could not be read. The error is a *tzStorageReadError: its text is a fixed
// reason and the scope kind, never the backend error, a key or a value.
func (d *HTTPAgentDispatcher) resolveStorageTZ(ctx context.Context, agent *store.Agent) (agentTZ, error) {
	var result agentTZ
	if agent == nil || d.store == nil {
		return result, nil
	}

	for _, filter := range d.envScopesInPrecedenceOrder(agent) {
		filter.Key = agentTZEnvKey
		vars, err := d.store.ListEnvVars(ctx, filter)
		if err != nil {
			return agentTZ{}, &tzStorageReadError{Scope: filter.Scope, Err: err}
		}
		for _, v := range vars {
			if v.Key != agentTZEnvKey || v.Value == "" || v.InjectionMode == store.InjectionModeAsNeeded {
				continue
			}
			result = agentTZ{TZ: v.Value, Source: envScopeSourceLabel(filter.Scope)}
		}
	}
	if result.TZ != "" {
		return result, nil
	}

	if len(agent.Ancestry) > 1 {
		progenyVars, err := d.store.ListProgenyEnvVars(ctx, agent.Ancestry)
		if err != nil {
			return agentTZ{}, &tzStorageReadError{Scope: TZSourceProgeny, Err: err}
		}
		for _, v := range progenyVars {
			if v.Key == agentTZEnvKey && v.Value != "" {
				return agentTZ{TZ: v.Value, Source: TZSourceProgeny}, nil
			}
		}
	}
	return result, nil
}

// tzStorageReadError is a failed TZ storage read in resolveStorageTZ. Error()
// is a fixed reason plus the scope kind ("user", "project", "hub",
// "runtime_broker" or "progeny"), so it is safe in a log line or a wrapped
// error message: the backend error, which could carry scope IDs or values,
// is reachable only through Unwrap (errors.Is/As), never printed.
type tzStorageReadError struct {
	Scope string
	Err   error
}

func (e *tzStorageReadError) Error() string {
	return "stored TZ settings could not be read (" + e.Scope + " scope)"
}

func (e *tzStorageReadError) Unwrap() error { return e.Err }

// tzReadScope returns the scope kind of the *tzStorageReadError in err's
// chain, for a log attribute that carries no backend detail.
func tzReadScope(err error) string {
	var e *tzStorageReadError
	if errors.As(err, &e) {
		return e.Scope
	}
	return "unknown"
}

// setResolvedAgentTZ makes env carry exactly the resolver's TZ: it removes
// any TZ already in env and, when the resolver supplied a value, sets it and
// classifies it as a plain config var. This is the only place the hub writes
// TZ into a dispatched env (create, finalize-env, start and restart).
func setResolvedAgentTZ(env map[string]string, classifications *map[string]api.EnvKind, tz agentTZ) {
	if env == nil {
		return
	}
	delete(env, agentTZEnvKey)
	if classifications != nil && *classifications != nil {
		delete(*classifications, agentTZEnvKey)
	}
	if tz.TZ == "" {
		return
	}
	env[agentTZEnvKey] = tz.TZ
	if classifications != nil {
		classifyEnv(classifications, agentTZEnvKey, api.EnvKindPlain)
	}
}

// withoutTZKey returns keys without TZ. It is used on env key lists the hub
// acts on (as_needed keys, broker env-gather needs): TZ is never gathered.
func withoutTZKey(keys []string) []string {
	if len(keys) == 0 {
		return keys
	}
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		if k != agentTZEnvKey {
			out = append(out, k)
		}
	}
	return out
}

// isTZTargetedSecret reports whether a resolved secret would be projected
// into the container's TZ environment variable.
func isTZTargetedSecret(s ResolvedSecret) bool {
	return (s.Type == "environment" || s.Type == "") && s.Target == agentTZEnvKey
}

// dropTZTargetedSecrets removes env-type secrets targeting TZ. Runtimes add
// env-type secrets after config env, so such a secret would override the
// resolver's TZ in the container. Each dropped secret is logged and reported
// as a dispatch warning; the secret's value is never included.
func (d *HTTPAgentDispatcher) dropTZTargetedSecrets(ctx context.Context, agent *store.Agent, secrets []ResolvedSecret) []ResolvedSecret {
	if len(secrets) == 0 {
		return secrets
	}
	out := make([]ResolvedSecret, 0, len(secrets))
	for _, s := range secrets {
		if !isTZTargetedSecret(s) {
			out = append(out, s)
			continue
		}
		agentName := ""
		agentID := ""
		if agent != nil {
			agentName, agentID = agent.Name, agent.ID
		}
		d.warnTZ("ignoring secret that targets TZ; the agent timezone comes only from the TZ resolver",
			"agent_id", agentID, "secret", s.Name, "source", s.Source)
		addDispatchWarnings(ctx, fmt.Sprintf(
			"Warning: ignoring secret %q (%s scope) targeting TZ for agent %s: set the agent timezone with explicitTimezone, a TZ environment variable or the hub default timezone",
			s.Name, s.Source, agentName))
	}
	return out
}

// takeTZGatherNeed removes TZ from the needs of a broker env-gather response
// (Needs, and the SecretInfo and Alternatives that describe needs), so the
// hub never acts on TZ as a gathered key or forwards it to the CLI, and
// reports whether the broker listed TZ as needed. TZ is never gathered: a
// current broker never reports it; an older one might, and the caller
// answers it with resolveAgentTZ(forGatherAnswer=true). The informational
// Required, HubHas and BrokerHas lists keep TZ; buildEnvGatherResponse
// labels a hub-supplied TZ with the resolver's source.
func takeTZGatherNeed(reqs *RemoteEnvRequirementsResponse) bool {
	if reqs == nil {
		return false
	}
	needed := false
	for _, k := range reqs.Needs {
		if k == agentTZEnvKey {
			needed = true
			break
		}
	}
	reqs.Needs = withoutTZKey(reqs.Needs)
	delete(reqs.SecretInfo, agentTZEnvKey)
	delete(reqs.Alternatives, agentTZEnvKey)
	for k, alts := range reqs.Alternatives {
		reqs.Alternatives[k] = withoutTZKey(alts)
	}
	return needed
}

// withoutCallerTZ returns a copy of a caller-supplied env map (CLI-gathered
// env, a reconcile replay, as_needed values) without TZ, which only the
// resolver writes into a dispatched env. A non-empty TZ is reported as a
// dispatch warning.
func (d *HTTPAgentDispatcher) withoutCallerTZ(ctx context.Context, agent *store.Agent, env map[string]string) map[string]string {
	v, ok := env[agentTZEnvKey]
	if !ok {
		return env
	}
	out := make(map[string]string, len(env))
	for k, val := range env {
		if k != agentTZEnvKey {
			out[k] = val
		}
	}
	if v != "" {
		agentID := ""
		if agent != nil {
			agentID = agent.ID
		}
		d.warnTZ("ignoring TZ in submitted env; the agent timezone comes only from the TZ resolver", "agent_id", agentID)
		addDispatchWarnings(ctx, "TZ in submitted env is ignored; use explicitTimezone")
	}
	return out
}

// agentTZ resolves an agent's container TZ for hub handlers (the agent
// PATCH response, the env-gather response). It uses the server's HTTP
// dispatcher when one is installed, so the result matches what a dispatch
// would send, and otherwise an equivalent resolver over the server's store,
// hub ID and live hub agent defaults. A storage read error is returned (see
// resolveAgentTZ); handlers answer it with writeEnvSettingsUnavailable.
func (s *Server) agentTZ(ctx context.Context, agent *store.Agent) (agentTZ, error) {
	if d, ok := s.GetDispatcher().(*HTTPAgentDispatcher); ok && d != nil {
		return d.resolveAgentTZ(ctx, agent, false)
	}
	r := &HTTPAgentDispatcher{
		store:                    s.store,
		hubID:                    s.HubID(),
		hubAgentDefaultsProvider: s.hubAgentDefaults,
		log:                      slog.Default(),
	}
	return r.resolveAgentTZ(ctx, agent, false)
}
