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
	"net/http"
	"sync"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// GitHub token delivery policy (Stage 2 design §5.1a).
//
// When an agent has AppliedConfig.GitHubTokenPreference == "installation",
// non-installation GitHub token material — matched by the shared predicates in
// pkg/api/github_token_policy.go:
//   - env keys (api.IsBlockedInstallationGitHubEnvKey): GITHUB_TOKEN, GH_TOKEN,
//     GH_ENTERPRISE_TOKEN, GITHUB_ENTERPRISE_TOKEN, SCION_USER_GITHUB_TOKEN
//   - file-type secret credential targets
//     (api.IsBlockedInstallationGitHubCredentialFileTarget):
//     ~/.config/gh/hosts.{yml,yaml}, ~/.git-credentials,
//     ~/.config/git/credentials, ~/.netrc (under ~/, /root/, /home/<user>/,
//     /Users/<user>/), and /etc/netrc
//
// must never reach the agent or its descendants. Every door and its guard:
//
//   - Push (create / provision / reprovision):
//     HTTPAgentDispatcher.buildCreateRequestFor (httpdispatcher.go) ->
//     deliverGitHubToken (strips blocked env, classifications, resolved
//     secrets across broker/hub/project/user/agent scopes — including file
//     secrets targeting gh/git credential paths — and as-needed keys before
//     minting) + defense-in-depth noAuth token guard and noAuthProjectToken
//     reset before restrictCreateRequestTokenRemote.
//     Carries GitHubTokenInstallationOnly=true on RemoteCreateAgentRequest and
//     refuses dispatch fail-closed via gateGitHubTokenInstallationOnly
//     (ErrBrokerLacksGitHubTokenInstallationOnly) when the target broker does
//     not advertise Capabilities.GitHubTokenInstallationOnly.
//   - Push (start / restart / resume):
//     HTTPAgentDispatcher.buildStartEnv (httpdispatcher.go) ->
//     deliverGitHubToken (strips blocked material across all delivery scopes
//     including agent scope and gh/git credential file secrets) + defense-in-depth
//     noAuth token guard and projectToken reset before
//     restrictPersonalTokenRemote.
//     Carries GitHubTokenInstallationOnly=true in StartExtras and refuses
//     dispatch fail-closed via gateGitHubTokenInstallationOnly when the target
//     broker lacks Capabilities.GitHubTokenInstallationOnly.
//   - Push (broker-side host-env passthrough & non-Hub config layers):
//     runtimebroker (handlers.go, start_context.go) threads
//     githubTokenInstallationOnly into api.StartOptions, drops blocked GitHub
//     keys from env-gather requirements, and strips them from Config.Env,
//     InlineConfig.Env, and workstation altEnvKeySatisfiedWithPolicy host-env
//     lookups. pkg/agent/run.go (hostEnvRefuserForAgent,
//     resolveAuthEnvOverlayWithPolicy, expandConfigHubEndpoint,
//     buildTrackedAgentEnv, templateEnvFallback / RunConfig.FallbackEnv,
//     suppressedFallbackKeys) and pkg/runtime (RunConfig.GitHubTokenInstallationOnly,
//     volumeEnvRefuser, expandVolumeEnv, expandHostSource,
//     CheckVolumeSourcesWithPolicy) refuse broker host-env passthrough, ${VAR}
//     expansion (including SCION_HUB_ENDPOINT / SCION_HUB_URL and volume
//     source/target paths), harness-config settings/bundle entries, persisted
//     scionCfg.Env entries, template env fallback entries, and hub defaults for
//     blocked GitHub keys when GitHubTokenInstallationOnly is set, so only the
//     Hub-minted installation token in ResolvedEnv may supply GITHUB_TOKEN.
//   - Push (env-gather finalize):
//     HTTPAgentDispatcher.DispatchFinalizeEnv (httpdispatcher.go) ->
//     filters gathered env keys with isBlockedInstallationGitHubEnvKey.
//   - Push (env-gather response metadata):
//     Server.buildEnvGatherResponse / MissingEnvVars (handlers_agents_core.go) ->
//     omits blocked keys from Required, BrokerHas, Needs, HubHas, SecretInfo,
//     and HubWarnings (stored-scope warnings) when usesInstallationGitHubToken
//     is true, so neither blocked key names nor their storage scopes are
//     disclosed to an installation create caller.
//   - Push (as-needed secret second pass):
//     HTTPAgentDispatcher.resolveAsNeededForKeys (httpdispatcher.go) ->
//     filters requested keys and resolved secrets (including agent scope) with
//     isBlockedInstallationGitHubEnvKey.
//   - Pull (runtime secret fetch: GET /api/v1/agents/{id}/secrets/{key} and
//     POST /api/v1/agent/secrets):
//     Server.selectRuntimeMaterial (material_selection.go) -> refuses blocked
//     keys before backend read and again via isBlockedInstallationGitHubSecret
//     (env key or gh/git credential file target) after lookup
//     (ReasonGitHubTokenInstallationOnly, not_found-indistinguishable across
//     agent, project, and user scopes).
//   - Pull (runtime secret list: GET /api/v1/agents/{id}/secrets):
//     Server.agentListSecrets (handlers_env_secrets.go; agent and project
//     scopes) and Server.agentListUserSecrets (material_list_user.go; user
//     scope) -> omit blocked secrets via isBlockedInstallationGitHubSecret.
//   - Pull (env var by key: GET /api/v1/env/{key}?scope=...,
//     GET /api/v1/projects/{id}/env/{key},
//     GET /api/v1/runtime-brokers/{id}/env/{key} [not agent-reachable
//     (authorizeBrokerMaterial refuses non-user identities); defence in depth]):
//     Server.serveEnvVarByKey (handlers_env_secrets.go) -> checks
//     callerAgentUsesInstallationGitHubToken (fail-closed: 403 on deleted
//     caller, 503 on transient store error), returns 404 before lookup for
//     blocked key names and before returning an aliased environment-secret
//     fallback whose Target is blocked.
//   - Pull (env var lists: GET /api/v1/env?scope=...,
//     GET /api/v1/projects/{id}/env,
//     GET /api/v1/runtime-brokers/{id}/env [not agent-reachable
//     (authorizeBrokerMaterial refuses non-user identities); defence in depth]):
//     Server.listEnvVars, Server.handleProjectEnvVars, Server.handleBrokerEnvVars
//     (handlers_env_secrets.go) -> check callerAgentUsesInstallationGitHubToken
//     (fail-closed), omit blocked env keys and blocked merged environment
//     secrets (by Name or Target).
//   - Pull (secret metadata by key: GET /api/v1/secrets/{key}?scope=...
//     [scope=agent is not agent-reachable (resolveAgentSecretAccess refuses
//     non-user identities); defence in depth],
//     GET /api/v1/projects/{id}/secrets/{key},
//     GET /api/v1/runtime-brokers/{id}/secrets/{key} [not agent-reachable
//     (authorizeBrokerMaterial refuses non-user identities); defence in depth]):
//     Server.getSecret and Server.handleScopedSecretByKey
//     (handlers_env_secrets.go) -> check callerAgentUsesInstallationGitHubToken
//     (fail-closed), return 404 for blocked key names (before lookup) or
//     blocked secrets via isBlockedInstallationGitHubSecret (after lookup,
//     covering aliased env targets and gh/git credential file targets).
//   - Pull (secret metadata lists: GET /api/v1/secrets?scope=...
//     [scope=agent is not agent-reachable (resolveAgentSecretAccess refuses
//     non-user identities); defence in depth],
//     GET /api/v1/projects/{id}/secrets,
//     GET /api/v1/runtime-brokers/{id}/secrets [not agent-reachable
//     (authorizeBrokerMaterial refuses non-user identities); defence in depth]):
//     Server.listSecrets, Server.handleProjectSecrets, Server.handleBrokerSecrets
//     (handlers_env_secrets.go) -> check callerAgentUsesInstallationGitHubToken
//     (fail-closed), omit blocked secrets via withoutInstallationBlockedSecrets
//     (isBlockedInstallationGitHubSecret).
//   - Cross-agent observation and control (ActionAttach, ActionLifecycle, and
//     response env visibility):
//     Server.authorizeAgentTargetAction (authorize.go) -> calls
//     installationAgentCrossAgentDenied (fail-closed: 403 on deleted caller,
//     indeterminate 503 on transient store error) to refuse ActionAttach (exec
//     on both routes, PTY upgrade, .../env, .../keys, scheduled exec
//     authoring/reveal) and ActionLifecycle (start, stop, suspend, restart,
//     restore, reincarnate, existing-agent resume-with-task via
//     agentLifecycleAllowed, DM/scheduled wake) when an installation-only agent
//     targets a non-installation peer. Server.agentFullHistoryDecision
//     (authorize.go) applies the same check for full message/log history.
//     Server.crossAgentEnvViewAllowed (agent_env_redaction.go) is the single
//     shared helper used by both envViewAllowed and canIdentityViewAgentEnv
//     (with per-request caller-preference memoization) guarding AppliedConfig,
//     PendingConfig, and EffectiveNextRun env across every response site:
//     GET /api/v1/projects/{id}/agents (agent_sorted_project_list.go,
//     agent_sorted_project_list_jwt.go, handlers_projects_core.go),
//     GET /api/v1/agents (agent_sorted_global_list.go), single-agent GET
//     (handlers_agents_core.go), and lifecycle responses (redactedAgentCopy).
//     Own descendants inherit "installation" at create time and are unaffected.
//   - Descendants (interactive agent-created create):
//     Server.createAgentInProject (handlers_agents_core.go) ->
//     agentHasInstallationGitHubTokenPreference (tri-state; fails closed with
//     HTTP 503 on store lookup error, inherits "installation" otherwise).
//   - Descendants (headless / trigger-dispatched create):
//     Server.createAgentHeadless (agent_create_headless.go) and
//     fireTriggerDispatchAgent (action_dispatch_agent.go) ->
//     parentAgentUsesInstallationGitHubToken (tri-state; fails closed with
//     errAgentCreatorUnreadable on store lookup error, forces "installation"
//     otherwise).
//   - Reincarnation:
//     Server.reincarnateConfig (reincarnate_config.go) -> sticky: preserves
//     "installation" if either AppliedConfig or CreateInputs carries it.
//
// Scope limits (documented by design):
//   - Credential redirection via ~/.gitconfig or ~/.config/git/config
//     credential helpers, url.<...>.insteadOf rewrites, or XDG_CONFIG_HOME /
//     GH_CONFIG_DIR env redirection is outside the well-known-locations
//     contract (equivalent to storing a PAT under an arbitrary name).
//   - COPILOT_GITHUB_TOKEN is LLM harness auth (allowed under identity ruling
//     (b)), not a gh/git credential, and is intentionally not covered by
//     IsBlockedInstallationGitHubEnvKey.

// ErrBrokerLacksGitHubTokenInstallationOnly is returned when an
// installation-only GitHub token agent is dispatched to a Runtime Broker that
// does not advertise Capabilities.GitHubTokenInstallationOnly (Stage 2 §5.1a,
// ptone/scion#8058).
var ErrBrokerLacksGitHubTokenInstallationOnly = errors.New("broker does not support installation-only GitHub token delivery; upgrade the Runtime Broker to a version that advertises githubTokenInstallationOnly")

// ErrBrokerRequestedBlockedGitHubEnv is returned when a Runtime Broker answers
// an env-gather create with missing env vars containing only blocked GitHub
// token keys for an installation-only agent. Failing closed prevents the Hub
// from treating an unstarted 202 response as a successful dispatch.
var ErrBrokerRequestedBlockedGitHubEnv = errors.New("broker requested a blocked GitHub key for an installation-only agent")

type brokerLacksGitHubTokenInstallationOnlyError struct {
	broker string
}

func (e *brokerLacksGitHubTokenInstallationOnlyError) Error() string {
	if e.broker == "" {
		return ErrBrokerLacksGitHubTokenInstallationOnly.Error()
	}
	return fmt.Sprintf("broker %s does not support installation-only GitHub token delivery; upgrade the Runtime Broker to a version that advertises githubTokenInstallationOnly", e.broker)
}

func (e *brokerLacksGitHubTokenInstallationOnlyError) Unwrap() []error {
	return []error{ErrBrokerLacksGitHubTokenInstallationOnly, errStartRequestNotSent}
}

// checkBrokerGitHubTokenInstallationOnlyCapability fails closed when broker
// does not advertise GitHubTokenInstallationOnly. Mirrors
// checkBrokerHomeFilesCapability.
func checkBrokerGitHubTokenInstallationOnlyCapability(broker *store.RuntimeBroker) error {
	if broker != nil && broker.Capabilities != nil && broker.Capabilities.GitHubTokenInstallationOnly {
		return nil
	}
	name := ""
	if broker != nil {
		name = brokerDisplayName(broker)
	}
	return &brokerLacksGitHubTokenInstallationOnlyError{broker: name}
}

// gateGitHubTokenInstallationOnly enforces that installation-only agents are
// only dispatched to brokers advertising Capabilities.GitHubTokenInstallationOnly.
func (d *HTTPAgentDispatcher) gateGitHubTokenInstallationOnly(ctx context.Context, agent *store.Agent) error {
	if !usesInstallationGitHubToken(agent) {
		return nil
	}
	if d.store == nil || agent == nil || agent.RuntimeBrokerID == "" {
		return &brokerLacksGitHubTokenInstallationOnlyError{}
	}
	broker, err := d.store.GetRuntimeBroker(ctx, agent.RuntimeBrokerID)
	if err != nil {
		return fmt.Errorf("failed to load runtime broker for installation-only GitHub token capability check: %w (%w)", err, errStartRequestNotSent)
	}
	return checkBrokerGitHubTokenInstallationOnlyCapability(broker)
}

// isGitHubTokenEnvKey reports whether key is a GitHub token environment
// variable that git's credential helper, gh CLI, or sciontool gh-wrapper
// would pick up instead of a minted GitHub App installation token
// (Stage 2 design §5.1a).
func isGitHubTokenEnvKey(key string) bool {
	return api.IsGitHubTokenEnvKey(key)
}

// isBlockedInstallationGitHubEnvKey reports whether key must never be
// delivered from user/project/config storage when an agent's
// GitHubTokenPreference is "installation".
func isBlockedInstallationGitHubEnvKey(key string) bool {
	return api.IsBlockedInstallationGitHubEnvKey(key)
}

// isBlockedInstallationGitHubSecret reports whether a secret of the given type,
// name, and target must be withheld from installation-only GitHub token agents
// (either because its name/target is a blocked GitHub token env key, or because
// it is a file-type secret targeting a gh/git/netrc credential file).
func isBlockedInstallationGitHubSecret(secretType, name, target string) bool {
	if isBlockedInstallationGitHubEnvKey(name) || isBlockedInstallationGitHubEnvKey(target) {
		return true
	}
	return secretType == store.SecretTypeFile && api.IsBlockedInstallationGitHubCredentialFileTarget(target)
}

// agentGitHubTokenPreference returns the agent's stored GitHub token delivery
// preference, or "" when unset (default user-preferred precedence).
func agentGitHubTokenPreference(agent *store.Agent) string {
	if agent == nil || agent.AppliedConfig == nil {
		return ""
	}
	return agent.AppliedConfig.GitHubTokenPreference
}

// usesInstallationGitHubToken reports whether agent requires installation-only
// GitHub token delivery (Stage 2 design §5.1a).
func usesInstallationGitHubToken(agent *store.Agent) bool {
	return agentGitHubTokenPreference(agent) == DispatchAgentGitHubTokenInstallation
}

// agentHasInstallationGitHubTokenPreference reports whether the stored agent
// with ID agentID carries GitHubTokenPreference == "installation".
//
// Tri-state: returns (true, nil) when the parent requires installation-only
// tokens, (false, nil) when agentID is empty or the parent has no installation
// preference, and (false, err) on any store read failure (including a missing
// row for a non-empty agentID) so callers fail closed rather than silently
// dropping inheritance across a transient fault.
func (s *Server) agentHasInstallationGitHubTokenPreference(ctx context.Context, agentID string) (bool, error) {
	if agentID == "" {
		return false, nil
	}
	if s == nil || s.store == nil {
		return false, errors.New("store unavailable")
	}
	ag, err := s.store.GetAgent(ctx, agentID)
	if err != nil {
		return false, err
	}
	return usesInstallationGitHubToken(ag), nil
}

// parentAgentUsesInstallationGitHubToken reports whether a headless create's
// agent principal carries GitHubTokenPreference == "installation". Reuses
// auth.agent when already loaded and falls back to reading the store; any
// lookup error is returned so callers fail closed.
func (s *Server) parentAgentUsesInstallationGitHubToken(ctx context.Context, auth FireAuthority) (bool, error) {
	if auth.PrincipalKind != store.DelegationPrincipalAgent {
		return false, nil
	}
	if auth.agent != nil {
		return usesInstallationGitHubToken(auth.agent), nil
	}
	return s.agentHasInstallationGitHubTokenPreference(ctx, auth.PrincipalID)
}

// callerAgentUsesInstallationGitHubToken reports whether the HTTP request
// caller is an agent carrying GitHubTokenPreference == "installation".
// Non-agent callers return (false, true). If the caller is an agent and the
// store lookup fails, this fails closed: store.ErrNotFound (deleted caller)
// writes HTTP 403 without retry advice; other lookup errors write HTTP 503
// ErrCodeUnavailable, returning (false, false) so the handler aborts.
func (s *Server) callerAgentUsesInstallationGitHubToken(w http.ResponseWriter, ctx context.Context) (installationOnly bool, ok bool) {
	agentIdent := GetAgentIdentityFromContext(ctx)
	if agentIdent == nil {
		return false, true
	}
	usesInst, err := s.agentHasInstallationGitHubTokenPreference(ctx, agentIdent.ID())
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			slog.Warn("Refusing material read: calling agent record no longer exists",
				"agent_id", agentIdent.ID())
			Forbidden(w)
			return false, false
		}
		slog.Warn("Refusing material read: failed to verify calling agent GitHub token preference",
			"agent_id", agentIdent.ID(), "error", err)
		writeError(w, http.StatusServiceUnavailable, ErrCodeUnavailable,
			"Could not verify agent GitHub token preference; retry", nil)
		return false, false
	}
	return usesInst, true
}

type callerGitHubTokenPrefCacheKey struct{}

type callerGitHubTokenPrefEntry struct {
	usesInst bool
	err      error
}

type callerGitHubTokenPrefCache struct {
	mu      sync.Mutex
	entries map[string]callerGitHubTokenPrefEntry
}

// withCallerGitHubTokenPrefCache attaches a per-request memoization cache for
// agentHasInstallationGitHubTokenPreference so list and multi-target handlers
// perform at most one store lookup per calling agent per request.
func withCallerGitHubTokenPrefCache(ctx context.Context) context.Context {
	if _, ok := ctx.Value(callerGitHubTokenPrefCacheKey{}).(*callerGitHubTokenPrefCache); ok {
		return ctx
	}
	return context.WithValue(ctx, callerGitHubTokenPrefCacheKey{}, &callerGitHubTokenPrefCache{})
}

func (s *Server) agentHasInstallationGitHubTokenPreferenceCached(ctx context.Context, agentID string) (bool, error) {
	if agentID == "" {
		return false, nil
	}
	cache, _ := ctx.Value(callerGitHubTokenPrefCacheKey{}).(*callerGitHubTokenPrefCache)
	if cache != nil {
		cache.mu.Lock()
		if entry, ok := cache.entries[agentID]; ok {
			cache.mu.Unlock()
			return entry.usesInst, entry.err
		}
		cache.mu.Unlock()
	}
	usesInst, err := s.agentHasInstallationGitHubTokenPreference(ctx, agentID)
	if cache != nil {
		cache.mu.Lock()
		if cache.entries == nil {
			cache.entries = make(map[string]callerGitHubTokenPrefEntry, 1)
		}
		cache.entries[agentID] = callerGitHubTokenPrefEntry{usesInst: usesInst, err: err}
		cache.mu.Unlock()
	}
	return usesInst, err
}

// installationAgentCrossAgentDenied reports whether an agent caller using
// GitHubTokenPreference == "installation" must be refused ActionAttach,
// ActionLifecycle, or cross-agent env views on target because target does not
// also carry GitHubTokenPreference == "installation" (Stage 2 §5.1a).
//
// Own descendants inherit "installation" at create time and are unaffected.
// Lookup errors fail closed: store.ErrNotFound (deleted caller) returns
// (denied=true, indeterminate=false); any other store error returns
// (denied=true, indeterminate=true) so callers can answer 503/retryable.
func (s *Server) installationAgentCrossAgentDenied(ctx context.Context, identity Identity, target *store.Agent) (denied, indeterminate bool, reason string) {
	if identity == nil || identity.Type() != "agent" {
		return false, false, ""
	}
	agentIdent, ok := identity.(AgentIdentity)
	if !ok {
		return false, false, ""
	}
	if target != nil && target.ID == agentIdent.ID() {
		return false, false, ""
	}
	if usesInstallationGitHubToken(target) {
		return false, false, ""
	}
	callerInst, err := s.agentHasInstallationGitHubTokenPreferenceCached(ctx, agentIdent.ID())
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return true, false, "installation github token caller record not found"
		}
		return true, true, "installation github token caller lookup failed: " + err.Error()
	}
	if callerInst {
		return true, false, "installation-only agent may not attach to or control non-installation peer agent"
	}
	return false, false, ""
}

// withoutInstallationBlockedEnvVars omits env vars whose key is blocked for
// installation-only agents.
func withoutInstallationBlockedEnvVars(envVars []store.EnvVar) []store.EnvVar {
	if len(envVars) == 0 {
		return envVars
	}
	out := make([]store.EnvVar, 0, len(envVars))
	for _, ev := range envVars {
		if isBlockedInstallationGitHubEnvKey(ev.Key) {
			continue
		}
		out = append(out, ev)
	}
	return out
}

// withoutInstallationBlockedSecrets omits secret metadata whose Name or Target
// (including file-type gh/git credential paths) is blocked for
// installation-only agents.
func withoutInstallationBlockedSecrets(metas []secret.SecretMeta) []secret.SecretMeta {
	if len(metas) == 0 {
		return metas
	}
	out := make([]secret.SecretMeta, 0, len(metas))
	for _, m := range metas {
		if isBlockedInstallationGitHubSecret(m.SecretType, m.Name, m.Target) {
			continue
		}
		out = append(out, m)
	}
	return out
}

// hasGitHubMintProjectForHeadless reports whether the hub dispatcher can
// resolve a GitHub App mint project for a prospective headless agent using the
// exact same resolver as deliverGitHubToken (resolveGitHubMintProject).
// Treated as "no GitHub access" (returns false) when no HTTP dispatcher is
// wired (never happens in production), no GitHub App minter is configured, or
// resolveGitHubMintProject returns nil.
func (s *Server) hasGitHubMintProjectForHeadless(
	ctx context.Context,
	project *store.Project,
	auth FireAuthority,
	labels map[string]string,
) bool {
	if s == nil || s.store == nil || project == nil {
		return false
	}
	disp, ok := s.GetDispatcher().(*HTTPAgentDispatcher)
	if !ok || disp == nil || disp.githubAppMinter == nil {
		return false
	}
	ownerID, ancestry, err := s.resolveHeadlessOwnerAndAncestry(ctx, HeadlessCreate{Authority: auth})
	if err != nil {
		return false
	}
	prospective := &store.Agent{
		ProjectID: project.ID,
		CreatedBy: auth.PrincipalID,
		OwnerID:   ownerID,
		Ancestry:  ancestry,
		Labels:    labels,
	}
	return resolveGitHubMintProject(ctx, s.store, disp.githubAppMinter, disp.log, prospective, project) != nil
}

// stripNonInstallationGitHubMaterial removes all non-installation GitHub token
// material (GITHUB_TOKEN, GH_* aliases, SCION_USER_GITHUB_TOKEN) from env,
// classifications, resolved secrets, and available as-needed keys so that user
// PATs are dropped (never merely shadowed) before installation token delivery.
func stripNonInstallationGitHubMaterial(
	resolvedEnv map[string]string,
	envClassifications *map[string]api.EnvKind,
	resolvedSecrets []ResolvedSecret,
	asNeededKeys []string,
) ([]ResolvedSecret, []string) {
	for k := range resolvedEnv {
		if isBlockedInstallationGitHubEnvKey(k) {
			delete(resolvedEnv, k)
			if envClassifications != nil && *envClassifications != nil {
				delete(*envClassifications, k)
			}
		}
	}
	if envClassifications != nil && *envClassifications != nil {
		for k := range *envClassifications {
			if isBlockedInstallationGitHubEnvKey(k) {
				delete(*envClassifications, k)
			}
		}
	}

	if len(resolvedSecrets) > 0 {
		kept := resolvedSecrets[:0:len(resolvedSecrets)]
		for _, s := range resolvedSecrets {
			if isBlockedInstallationGitHubSecret(s.Type, s.Name, s.Target) {
				continue
			}
			kept = append(kept, s)
		}
		if len(kept) == 0 {
			resolvedSecrets = nil
		} else {
			resolvedSecrets = kept
		}
	}

	if len(asNeededKeys) > 0 {
		keptKeys := make([]string, 0, len(asNeededKeys))
		for _, k := range asNeededKeys {
			if isBlockedInstallationGitHubEnvKey(k) {
				continue
			}
			keptKeys = append(keptKeys, k)
		}
		asNeededKeys = keptKeys
	}

	return resolvedSecrets, asNeededKeys
}

// deliverGitHubToken is the single auditable decision point for GitHub token
// delivery across create, provision, reprovision, start, and restart
// (Stage 2 design §5.1a).
//
// When agent.AppliedConfig.GitHubTokenPreference == "installation":
//   - All non-installation GITHUB_TOKEN and GH_* aliases (env, secrets, and
//     as-needed keys) are dropped before minting.
//   - SCION_USER_GITHUB_TOKEN is never set.
//   - If the project has a GitHub App installation and minting succeeds, the
//     installation token is injected exactly as today.
//   - If the project has no installation or minting fails, the agent receives
//     no GitHub token at all (never falling back to a PAT).
//
// When GitHubTokenPreference is "" or "user_preferred", today's precedence is
// preserved: an existing GITHUB_TOKEN wins for gh CLI
// (SCION_USER_GITHUB_TOKEN=true) while the GitHub App remains enabled for git.
func (d *HTTPAgentDispatcher) deliverGitHubToken(
	ctx context.Context,
	agent *store.Agent,
	resolvedEnv map[string]string,
	envClassifications *map[string]api.EnvKind,
	resolvedSecrets []ResolvedSecret,
	asNeededKeys []string,
	operation string,
) (filteredSecrets []ResolvedSecret, filteredAsNeeded []string, ownAppToken string) {
	// Execution containers (shell harness) receive only the owning user's
	// user-scope env/secrets plus reserved SCION_* control variables, never
	// project GitHub App credentials or SCION_GITHUB_* / SCION_USER_GITHUB_TOKEN
	// stamping (ptone/scion#8812 §14).
	if isShellAgent(agent) {
		return resolvedSecrets, asNeededKeys, ""
	}

	installationOnly := usesInstallationGitHubToken(agent)
	if installationOnly {
		resolvedSecrets, asNeededKeys = stripNonInstallationGitHubMaterial(
			resolvedEnv, envClassifications, resolvedSecrets, asNeededKeys,
		)
	}

	if d.githubAppMinter == nil || agent == nil || agent.ProjectID == "" {
		return resolvedSecrets, asNeededKeys, ""
	}

	project, err := d.store.GetProject(ctx, agent.ProjectID)
	if err != nil {
		return resolvedSecrets, asNeededKeys, ""
	}

	mintProject := d.githubMintProject(ctx, agent, project)
	if mintProject == nil {
		return resolvedSecrets, asNeededKeys, ""
	}

	if !installationOnly && resolvedEnv != nil && resolvedEnv["GITHUB_TOKEN"] != "" {
		d.log.Warn(operation+": user GITHUB_TOKEN takes precedence over GitHub App token — user token will be used for gh CLI, GitHub App for git credential helper",
			"project_id", agent.ProjectID)
		resolvedEnv["SCION_USER_GITHUB_TOKEN"] = "true"
		classifyEnv(envClassifications, "SCION_USER_GITHUB_TOKEN", api.EnvKindPlain)
		resolvedEnv["SCION_GITHUB_APP_ENABLED"] = "true"
		classifyEnv(envClassifications, "SCION_GITHUB_APP_ENABLED", api.EnvKindPlain)
		return resolvedSecrets, asNeededKeys, ""
	}

	token, expiry, mintErr := d.githubAppMinter.MintGitHubAppTokenForProject(ctx, mintProject)
	if mintErr != nil {
		if d.debug || installationOnly {
			d.log.Warn(operation+": GitHub App token minting failed",
				"error", mintErr, "project_id", agent.ProjectID, "installation_only", installationOnly)
		}
		return resolvedSecrets, asNeededKeys, ""
	}
	if token == "" {
		return resolvedSecrets, asNeededKeys, ""
	}

	if resolvedEnv != nil {
		// GitHub App minted token — ephemeral, not in secret store.
		// This MUST run AFTER the secret-store GITHUB_TOKEN injection so the
		// classification overwrites secret-fetchable -> secret-injected when
		// the App token wins.
		resolvedEnv["GITHUB_TOKEN"] = token
		classifyEnv(envClassifications, "GITHUB_TOKEN", api.EnvKindSecretInjected)
		resolvedEnv["SCION_GITHUB_APP_ENABLED"] = "true"
		classifyEnv(envClassifications, "SCION_GITHUB_APP_ENABLED", api.EnvKindPlain)
		resolvedEnv["SCION_GITHUB_TOKEN_EXPIRY"] = expiry
		classifyEnv(envClassifications, "SCION_GITHUB_TOKEN_EXPIRY", api.EnvKindPlain)
		resolvedEnv["SCION_GITHUB_TOKEN_PATH"] = "/tmp/.github-token"
		classifyEnv(envClassifications, "SCION_GITHUB_TOKEN_PATH", api.EnvKindPlain)
		if d.debug && mintProject.GitHubInstallationID != nil {
			d.log.Debug(operation+": injected GitHub App token",
				"project_id", agent.ProjectID,
				"installationID", *mintProject.GitHubInstallationID,
				"expiry", expiry)
		}
	}

	// ownAppToken is only a token minted for the agent's own project; a
	// token-source project's token is not project-bound for this agent
	// (gitTokenProjectBound).
	if mintProject.ID != project.ID {
		return resolvedSecrets, asNeededKeys, ""
	}
	return resolvedSecrets, asNeededKeys, token
}
