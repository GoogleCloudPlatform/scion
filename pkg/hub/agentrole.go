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
// Returns nil for AgentRoleNone (caller should set NoAuth=true instead).
// GCP token scopes and identity token scopes are NOT included here —
// they are appended dynamically at dispatch time based on agent config.
func ScopesForRole(role AgentRole) []AgentTokenScope {
	switch role {
	case AgentRoleNone:
		return nil
	case AgentRoleReadOnly:
		return []AgentTokenScope{ScopeProjectRead}
	case AgentRoleBaseline:
		return []AgentTokenScope{
			ScopeProjectRead,
			ScopeAgentStatusUpdate,
			ScopeAgentTokenRefresh,
			ScopeAgentNotify,
			ScopeAgentPortForward,
		}
	case AgentRoleFull:
		return []AgentTokenScope{
			ScopeProjectRead,
			ScopeAgentStatusUpdate,
			ScopeAgentTokenRefresh,
			ScopeAgentNotify,
			ScopeAgentPortForward,
			ScopeAgentCreate,
			ScopeAgentLifecycle,
			ScopeProjectSecretRead,
		}
	default:
		return ScopesForRole(AgentRoleBaseline)
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
	default:
		return 2 // default to baseline
	}
}

// CompareRoles returns negative if a < b, 0 if equal, positive if a > b.
func CompareRoles(a, b AgentRole) int {
	return roleOrdinal(a) - roleOrdinal(b)
}

// minRole returns the least-privileged role from the given set.
func minRole(roles ...AgentRole) AgentRole {
	if len(roles) == 0 {
		return AgentRoleBaseline
	}
	min := roles[0]
	for _, r := range roles[1:] {
		if roleOrdinal(r) < roleOrdinal(min) {
			min = r
		}
	}
	return min
}

// ResolveEffectiveRole computes the effective agent role using the two-gate
// authority lattice: min(requested, userCeiling, projectMax).
//
// userHubRole is the hub-level user role string ("admin" or "member").
// If empty, defaults to "member" ceiling (baseline).
func ResolveEffectiveRole(requested AgentRole, userHubRole string, projectMax AgentRole) AgentRole {
	userCeiling := AgentRoleBaseline
	if userHubRole == "admin" {
		userCeiling = AgentRoleFull
	}
	return minRole(requested, userCeiling, projectMax)
}
