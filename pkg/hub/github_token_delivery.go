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

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// isGitHubTokenEnvKey reports whether key is a GitHub token environment
// variable that git's credential helper, gh CLI, or sciontool gh-wrapper
// would pick up instead of a minted GitHub App installation token
// (Stage 2 design §5.1a).
func isGitHubTokenEnvKey(key string) bool {
	switch key {
	case "GITHUB_TOKEN", "GH_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN":
		return true
	default:
		return false
	}
}

// isBlockedInstallationGitHubEnvKey reports whether key must never be
// delivered from user/project/config storage when an agent's
// GitHubTokenPreference is "installation".
func isBlockedInstallationGitHubEnvKey(key string) bool {
	return isGitHubTokenEnvKey(key) || key == "SCION_USER_GITHUB_TOKEN"
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
// with ID agentID carries GitHubTokenPreference == "installation". Used by
// child-create paths so descendants of an installation-preference agent inherit
// the preference (fail-closed on lookup error is not needed here because
// standing checks already refuse unreadable callers, and a missing row has no
// preference).
func (s *Server) agentHasInstallationGitHubTokenPreference(ctx context.Context, agentID string) bool {
	if s == nil || s.store == nil || agentID == "" {
		return false
	}
	ag, err := s.store.GetAgent(ctx, agentID)
	if err != nil {
		return false
	}
	return usesInstallationGitHubToken(ag)
}

// parentAgentUsesInstallationGitHubToken reports whether a headless create's
// agent principal carries GitHubTokenPreference == "installation".
func (s *Server) parentAgentUsesInstallationGitHubToken(ctx context.Context, auth FireAuthority) bool {
	if auth.PrincipalKind != store.DelegationPrincipalAgent {
		return false
	}
	if auth.agent != nil {
		return usesInstallationGitHubToken(auth.agent)
	}
	return s.agentHasInstallationGitHubTokenPreference(ctx, auth.PrincipalID)
}

// hasGitHubMintProjectForHeadless reports whether the hub dispatcher can
// resolve a GitHub App mint project for a prospective headless agent using the
// exact same resolver as deliverGitHubToken (resolveGitHubMintProject). Returns
// false when no HTTP dispatcher or GitHub App minter is configured, or when
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
			if isBlockedInstallationGitHubEnvKey(s.Target) || isBlockedInstallationGitHubEnvKey(s.Name) {
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
		if d.debug {
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
