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

import "testing"

// AgentRole represents a named authorization tier for agents.
// Each role maps to a fixed bundle of JWT scopes.
type AgentRole string

const (
	AgentRoleNone     AgentRole = "none"
	AgentRoleReadOnly AgentRole = "readonly"
	AgentRoleBaseline AgentRole = "baseline"
	AgentRoleFull     AgentRole = "full"
)

// ValidAgentRole returns true if r is one of the four stock roles.
func ValidAgentRole(r AgentRole) bool {
	switch r {
	case AgentRoleNone, AgentRoleReadOnly, AgentRoleBaseline, AgentRoleFull:
		return true
	}
	return false
}

// ScopesForRole returns the JWT scopes granted by a named agent role.
//
// Additive-scope rule (ptone/scion#8324): every scope added to an existing
// role bundle MUST also be listed in ceilingOptionalRoleScopes
// (authz_effect_ceiling.go), enforced by
// TestScopesForRole_NonOptionalScopesAreFrozen. Adding a non-optional scope to
// an existing role causes canAgentDelegateToAgent to reject delegation with
// 403 for running agents whose pre-deploy JWTs lack the scope, and permanently
// blocks agent-created chains whose frozen EffectCeilingBounded edge predates
// the scope until a hub super-admin runs 'scion reset-auth --reissue-scopes'
// top-down (see docs-site/src/content/docs/reference/permissions-policy.md).
//
// Scopes in ceilingOptionalRoleScopes are part of the bundle but never decide
// whether a role fits a ceiling (roleFitsCeiling) or may be delegated
// (canAgentDelegateToAgent); a mint drops them with droppedScopesWarning when
// the source ceiling lacks their permission.
//
// Returns nil for AgentRoleNone (caller should set NoAuth=true instead).
// GCP token scopes and identity token scopes are NOT included here —
// they are appended dynamically at dispatch time based on agent config.
func ScopesForRole(role AgentRole) []AgentTokenScope {
	return filterWithheldRoleScopesForTest(testing.Testing(), scopesForRole(role))
}

// roleScopesWithheldForTest holds scopes omitted by a test simulating a
// pre-deploy binary whose role bundle predates an additive scope
// (ptone/scion#8324). Only _test.go code writes it, and
// filterWithheldRoleScopesForTest ignores it outside a test binary
// (testing.Testing()), so no production path can alter role bundles.
var roleScopesWithheldForTest map[AgentTokenScope]bool

func filterWithheldRoleScopesForTest(inTestBinary bool, scopes []AgentTokenScope) []AgentTokenScope {
	if !inTestBinary || len(roleScopesWithheldForTest) == 0 || len(scopes) == 0 {
		return scopes
	}
	out := make([]AgentTokenScope, 0, len(scopes))
	for _, s := range scopes {
		if !roleScopesWithheldForTest[s] {
			out = append(out, s)
		}
	}
	return out
}

func scopesForRole(role AgentRole) []AgentTokenScope {
	switch role {
	case AgentRoleNone:
		return nil
	case AgentRoleReadOnly:
		return []AgentTokenScope{
			ScopeProjectRead,
			// Reporting its own status is a control-plane concern, not a
			// write to project state: updateAgentStatus admits an agent
			// token only for the bearer's own agent ID (ptone/scion#7926).
			ScopeAgentStatusUpdate,
			ScopeProjectArtifactRead,
		}
	case AgentRoleBaseline:
		return []AgentTokenScope{
			ScopeProjectRead,
			ScopeAgentStatusUpdate,
			ScopeAgentTokenRefresh,
			ScopeAgentNotify,
			ScopeAgentPortForward,
			ScopeProjectArtifactRead,
			// Publishing artifacts is how an agent hands a document to
			// readers on other brokers or in the web UI; it only writes
			// artifacts homed in the agent's own project.
			ScopeProjectArtifactWrite,
		}
	case AgentRoleFull:
		return []AgentTokenScope{
			ScopeProjectRead,
			ScopeAgentStatusUpdate,
			ScopeAgentTokenRefresh,
			ScopeAgentNotify,
			ScopeAgentPortForward,
			ScopeAgentCreate,
			ScopeAgentSAAssign,
			ScopeAgentLifecycle,
			ScopeProjectSecretRead,
			ScopeProjectTemplateWrite,
			ScopeAgentSetMessageMode,
			ScopeProjectArtifactRead,
			ScopeProjectArtifactWrite,
		}
	case "":
		return nil
	default:
		return nil
	}
}

// roleOrdinal returns the numeric ordering for role comparison.
// none(0) < readonly(1) < baseline(2) < full(3).
func roleOrdinal(r AgentRole) int {
	switch r {
	case AgentRoleNone:
		return 0
	case AgentRoleReadOnly:
		return 1
	case AgentRoleBaseline:
		return 2
	case AgentRoleFull:
		return 3
	case "":
		return 0 // fail-closed for missing roles
	default:
		return 0 // fail-closed for unknown/invalid roles
	}
}

// CompareRoles returns negative if a < b, 0 if equal, positive if a > b.
func CompareRoles(a, b AgentRole) int {
	return roleOrdinal(a) - roleOrdinal(b)
}

// minRole returns the least-privileged role from the given set.
func minRole(roles ...AgentRole) AgentRole {
	if len(roles) == 0 {
		return AgentRoleFull
	}
	min := roles[0]
	for _, r := range roles[1:] {
		if roleOrdinal(r) < roleOrdinal(min) {
			min = r
		}
	}
	return min
}
