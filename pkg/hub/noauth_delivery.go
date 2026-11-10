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
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Launch material for an agent without credentials.
//
// An agent whose applied config records NoAuth (role none, the "none" auth
// choice, or the create-time fallback when no credential was found) gets the
// same launch material on create, start, resume and restart. The rules are
// stated once, here, and both buildCreateRequestFor and buildStartEnv (with
// the start and restart selection in material_delivery.go) apply them:
//
//   - no type-aware secret (environment, file or variable) from any scope,
//     except the git token, GITHUB_TOKEN, chosen from the project scope
//     first and then the owner's user scope (pickNoAuthGitToken) and written
//     into the env over any existing value (applyNoAuthGitToken);
//   - no stored env setting marked secret and none whose name a harness
//     declares as a credential (noAuthKeepsStoredEnv);
//   - no key of the agent's saved config env whose name a harness declares
//     as a credential, unless the requester set it for this agent at create
//     (noAuthKeepsConfigEnv).
//
// Everything else (plain settings, the agent's hub token, the transport
// token, a GitHub App token) is delivered as for any agent.

// noAuthGitTokenKey is the one stored item an agent without credentials
// still receives: the git token, for clone and pull.
const noAuthGitTokenKey = "GITHUB_TOKEN"

// noAuthHarnessConfigPageSize and noAuthHarnessConfigMaxPages bound the
// harness-config scan of noAuthCredentialEnvNames. A scan that does not end
// within the bound is an error, never a partial set.
const (
	noAuthHarnessConfigPageSize = 200
	noAuthHarnessConfigMaxPages = 50
)

// launchesWithoutCredentials reports the agent's stored no-credential
// intent.
func launchesWithoutCredentials(agent *store.Agent) bool {
	return agent != nil && agent.AppliedConfig != nil && agent.AppliedConfig.NoAuth
}

// credentialEnvNames is a set of env names harness auth metadata declares
// as credentials. Names match exactly.
type credentialEnvNames map[string]struct{}

func (c credentialEnvNames) has(name string) bool {
	_, ok := c[name]
	return ok
}

func (c credentialEnvNames) add(names []string) {
	for _, name := range names {
		c[name] = struct{}{}
	}
}

// builtinCredentialEnvNameSet is the set from the built-in harness configs
// alone.
func builtinCredentialEnvNameSet() credentialEnvNames {
	names := credentialEnvNames{}
	names.add(harness.BuiltinCredentialEnvNames())
	return names
}

// noAuthCredentialEnvNames returns the credential env names for a dispatch
// of agent: the built-in harness configs' names plus those of every stored
// harness config the agent can resolve (the global ones, its project's, and
// its own by ID). The names a config declares are harness.CredentialEnvNames
// of its auth metadata.
//
// A store error is returned with a nil set, so the caller fails the
// dispatch (or, where the dispatch continues on a read error, falls back to
// builtinCredentialEnvNameSet); the set is never silently empty.
func (d *HTTPAgentDispatcher) noAuthCredentialEnvNames(ctx context.Context, agent *store.Agent) (credentialEnvNames, error) {
	names := builtinCredentialEnvNameSet()
	if d.store == nil || agent == nil {
		return names, nil
	}
	addConfig := func(hc *store.HarnessConfig) {
		if hc != nil && hc.Config != nil {
			names.add(harness.CredentialEnvNames(hc.Config.AuthMeta))
		}
	}

	filter := store.HarnessConfigFilter{Scope: store.HarnessConfigScopeGlobal}
	if agent.ProjectID != "" {
		// Global plus this project's configs.
		filter = store.HarnessConfigFilter{ProjectID: agent.ProjectID}
	}
	cursor := ""
	for page := 0; ; page++ {
		if page == noAuthHarnessConfigMaxPages {
			return nil, fmt.Errorf("list harness configs: more than %d pages", noAuthHarnessConfigMaxPages)
		}
		result, err := d.store.ListHarnessConfigs(ctx, filter, store.ListOptions{
			Limit: noAuthHarnessConfigPageSize, Cursor: cursor, SkipTotalCount: true,
		})
		if err != nil {
			return nil, fmt.Errorf("list harness configs: %w", err)
		}
		if result == nil {
			break
		}
		for i := range result.Items {
			addConfig(&result.Items[i])
		}
		if result.NextCursor == "" || len(result.Items) == 0 {
			break
		}
		cursor = result.NextCursor
	}

	if agent.AppliedConfig != nil && agent.AppliedConfig.HarnessConfigID != "" {
		hc, err := d.store.GetHarnessConfig(ctx, agent.AppliedConfig.HarnessConfigID)
		switch {
		case err == nil:
			addConfig(hc)
		case !errors.Is(err, store.ErrNotFound):
			return nil, fmt.Errorf("get harness config: %w", err)
		}
	}
	return names, nil
}

// noAuthKeepsStoredEnv reports whether a stored env setting reaches an agent
// without credentials. The git token is kept. Any other setting is withheld
// when it is marked secret or when its name is a credential name; other
// plain settings are kept.
func noAuthKeepsStoredEnv(key string, secretRow bool, credNames credentialEnvNames) bool {
	if key == noAuthGitTokenKey {
		return true
	}
	if secretRow {
		return false
	}
	return !credNames.has(key)
}

// noAuthKeepsConfigEnv reports whether a key of the agent's saved config env
// (AppliedConfig.Env) reaches an agent without credentials. A key the
// requester set for this agent at create (requesterExplicit: present in the
// applied InlineConfig's env) and the git token are kept; any other key is
// withheld when its name is a credential name. Saved config env holds
// settings that earlier dispatches merged back (see the post-dispatch merge
// in dispatchProvision), so this keeps a credential setting saved from an
// earlier run from being applied.
func noAuthKeepsConfigEnv(key string, requesterExplicit bool, credNames credentialEnvNames) bool {
	if key == noAuthGitTokenKey || requesterExplicit {
		return true
	}
	return !credNames.has(key)
}

// requesterSetEnvKey reports whether the requester set key for agent at
// create: the applied InlineConfig's env holds the requester's explicit keys
// only.
func requesterSetEnvKey(agent *store.Agent, key string) bool {
	if agent == nil || agent.AppliedConfig == nil || agent.AppliedConfig.InlineConfig == nil {
		return false
	}
	_, ok := agent.AppliedConfig.InlineConfig.Env[key]
	return ok
}

// noAuthConfigEnv returns the saved config env keys that reach agent, a
// copy; for an agent with credentials (credNames nil) every key.
func noAuthConfigEnv(agent *store.Agent, credNames credentialEnvNames) map[string]string {
	if agent == nil || agent.AppliedConfig == nil || agent.AppliedConfig.Env == nil {
		return nil
	}
	out := make(map[string]string, len(agent.AppliedConfig.Env))
	for k, v := range agent.AppliedConfig.Env {
		if credNames != nil && !noAuthKeepsConfigEnv(k, requesterSetEnvKey(agent, k), credNames) {
			continue
		}
		out[k] = v
	}
	return out
}

// noAuthKeepsSecretCandidate reports whether a stored secret of scope and
// name is a git token candidate for an agent without credentials: the
// GITHUB_TOKEN record of the project scope or of the owner's user scope.
// No other secret is selected for such an agent, and no value is read for
// one.
func noAuthKeepsSecretCandidate(scope, name string) bool {
	return name == noAuthGitTokenKey && (scope == store.ScopeProject || scope == store.ScopeUser)
}

// noAuthGitToken is the git token picked for an agent without credentials.
type noAuthGitToken struct {
	value string
	scope string // store.ScopeProject or store.ScopeUser; "" when none
}

// projectBound returns the token's value when it is the project's own
// token, the one restrictPersonalTokenRemote treats as project-bound; ""
// otherwise.
func (t noAuthGitToken) projectBound() string {
	if t.scope == store.ScopeProject {
		return t.value
	}
	return ""
}

// pickNoAuthGitToken applies the order create has always used for an agent
// without credentials: the project's token when it has a value, otherwise
// the owner's user-scope token. This is the one place the user scope does
// not outrank the project scope.
func pickNoAuthGitToken(projectValue, userValue string) noAuthGitToken {
	switch {
	case projectValue != "":
		return noAuthGitToken{value: projectValue, scope: store.ScopeProject}
	case userValue != "":
		return noAuthGitToken{value: userValue, scope: store.ScopeUser}
	}
	return noAuthGitToken{}
}

// pickNoAuthGitTokenFrom is pickNoAuthGitToken over the candidates the
// start and restart selection returned for an agent without credentials.
func pickNoAuthGitTokenFrom(candidates []ResolvedSecret) noAuthGitToken {
	var projectValue, userValue string
	for _, c := range candidates {
		if c.Name != noAuthGitTokenKey || c.Value == "" {
			continue
		}
		switch c.Source {
		case store.ScopeProject:
			if projectValue == "" {
				projectValue = c.Value
			}
		case store.ScopeUser:
			if userValue == "" {
				userValue = c.Value
			}
		}
	}
	return pickNoAuthGitToken(projectValue, userValue)
}

// applyNoAuthGitToken writes the picked token into env, over any existing
// value, classified secret-fetchable (it comes from the secret store). It
// never goes into the resolved secrets. A GitHub App token written later
// still replaces it.
func applyNoAuthGitToken(env map[string]string, classifications *map[string]api.EnvKind, tok noAuthGitToken) {
	if tok.value == "" || env == nil {
		return
	}
	env[noAuthGitTokenKey] = tok.value
	classifyEnv(classifications, noAuthGitTokenKey, api.EnvKindSecretFetchable)
}

// noAuthCreateGitToken reads the GITHUB_TOKEN record of scope (project or
// the owner's user scope) by name for create, returning its value, or ""
// when there is none or it cannot be read (logged at debug, without the
// value).
func (d *HTTPAgentDispatcher) noAuthCreateGitToken(ctx context.Context, agent *store.Agent, scope, scopeID string) string {
	ghSecret, err := d.secretBackend.Get(ctx, noAuthGitTokenKey, scope, scopeID)
	if err != nil {
		if d.debug {
			d.log.Debug("NoAuth: failed to resolve GITHUB_TOKEN", "agent_id", agent.ID, "scope", scope, "error", err)
		}
		return ""
	}
	if ghSecret == nil || ghSecret.Value == "" {
		return ""
	}
	if d.debug {
		d.log.Debug("NoAuth: resolved GITHUB_TOKEN for git operations", "agent_id", agent.ID, "scope", scope)
	}
	return ghSecret.Value
}
