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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestScopesForRole_None(t *testing.T) {
	scopes := ScopesForRole(AgentRoleNone)
	assert.Nil(t, scopes)
}

func TestScopesForRole_ReadOnly(t *testing.T) {
	scopes := ScopesForRole(AgentRoleReadOnly)
	require.Len(t, scopes, 1)
	assert.Equal(t, ScopeProjectRead, scopes[0])
}

func TestScopesForRole_Baseline(t *testing.T) {
	scopes := ScopesForRole(AgentRoleBaseline)
	require.Len(t, scopes, 5)

	// Must include these scopes
	assert.Contains(t, scopes, ScopeProjectRead)
	assert.Contains(t, scopes, ScopeAgentStatusUpdate)
	assert.Contains(t, scopes, ScopeAgentTokenRefresh)
	assert.Contains(t, scopes, ScopeAgentNotify)
	assert.Contains(t, scopes, ScopeAgentPortForward)

	// Must NOT include elevated scopes
	assert.NotContains(t, scopes, ScopeAgentCreate)
	assert.NotContains(t, scopes, ScopeAgentLifecycle)
	assert.NotContains(t, scopes, ScopeProjectSecretRead)
}

func TestScopesForRole_Full(t *testing.T) {
	scopes := ScopesForRole(AgentRoleFull)
	require.Len(t, scopes, 8)

	// Must include everything in baseline
	assert.Contains(t, scopes, ScopeProjectRead)
	assert.Contains(t, scopes, ScopeAgentStatusUpdate)
	assert.Contains(t, scopes, ScopeAgentTokenRefresh)
	assert.Contains(t, scopes, ScopeAgentNotify)
	assert.Contains(t, scopes, ScopeAgentPortForward)

	// Plus elevated scopes
	assert.Contains(t, scopes, ScopeAgentCreate)
	assert.Contains(t, scopes, ScopeAgentLifecycle)
	assert.Contains(t, scopes, ScopeProjectSecretRead)
}

func TestScopesForRole_InvalidDefault(t *testing.T) {
	// Unknown role strings should fall back to baseline scopes
	scopes := ScopesForRole(AgentRole("unknown-role"))
	baselineScopes := ScopesForRole(AgentRoleBaseline)
	assert.Equal(t, baselineScopes, scopes)
}

func TestValidAgentRole(t *testing.T) {
	// All four stock roles are valid
	assert.True(t, ValidAgentRole(AgentRoleNone))
	assert.True(t, ValidAgentRole(AgentRoleReadOnly))
	assert.True(t, ValidAgentRole(AgentRoleBaseline))
	assert.True(t, ValidAgentRole(AgentRoleFull))

	// Random strings are invalid
	assert.False(t, ValidAgentRole(AgentRole("")))
	assert.False(t, ValidAgentRole(AgentRole("admin")))
	assert.False(t, ValidAgentRole(AgentRole("superuser")))
	assert.False(t, ValidAgentRole(AgentRole("unknown")))
}

func TestCompareRoles(t *testing.T) {
	// none < readonly < baseline < full
	assert.Less(t, CompareRoles(AgentRoleNone, AgentRoleReadOnly), 0)
	assert.Less(t, CompareRoles(AgentRoleReadOnly, AgentRoleBaseline), 0)
	assert.Less(t, CompareRoles(AgentRoleBaseline, AgentRoleFull), 0)
	assert.Less(t, CompareRoles(AgentRoleNone, AgentRoleFull), 0)

	// Equal returns 0
	assert.Equal(t, 0, CompareRoles(AgentRoleNone, AgentRoleNone))
	assert.Equal(t, 0, CompareRoles(AgentRoleBaseline, AgentRoleBaseline))
	assert.Equal(t, 0, CompareRoles(AgentRoleFull, AgentRoleFull))

	// Reverse comparisons are positive
	assert.Greater(t, CompareRoles(AgentRoleFull, AgentRoleNone), 0)
	assert.Greater(t, CompareRoles(AgentRoleBaseline, AgentRoleReadOnly), 0)
}

func TestMinRole(t *testing.T) {
	// Empty returns baseline
	assert.Equal(t, AgentRoleBaseline, minRole())

	// Single role returns itself
	assert.Equal(t, AgentRoleNone, minRole(AgentRoleNone))
	assert.Equal(t, AgentRoleFull, minRole(AgentRoleFull))

	// Two roles
	assert.Equal(t, AgentRoleReadOnly, minRole(AgentRoleFull, AgentRoleReadOnly))
	assert.Equal(t, AgentRoleNone, minRole(AgentRoleBaseline, AgentRoleNone))

	// Three roles
	assert.Equal(t, AgentRoleNone, minRole(AgentRoleFull, AgentRoleBaseline, AgentRoleNone))
	assert.Equal(t, AgentRoleReadOnly, minRole(AgentRoleFull, AgentRoleReadOnly, AgentRoleBaseline))
}

func TestResolveEffectiveRole_MemberUser(t *testing.T) {
	// Member user requesting full gets capped at baseline
	assert.Equal(t, AgentRoleBaseline, ResolveEffectiveRole(AgentRoleFull, "member", AgentRoleFull))

	// Member user requesting baseline gets baseline
	assert.Equal(t, AgentRoleBaseline, ResolveEffectiveRole(AgentRoleBaseline, "member", AgentRoleFull))

	// Member user requesting readonly gets readonly
	assert.Equal(t, AgentRoleReadOnly, ResolveEffectiveRole(AgentRoleReadOnly, "member", AgentRoleFull))
}

func TestResolveEffectiveRole_AdminUser(t *testing.T) {
	// Admin requesting full in full-project gets full
	assert.Equal(t, AgentRoleFull, ResolveEffectiveRole(AgentRoleFull, "admin", AgentRoleFull))

	// Admin in baseline-project gets baseline (project cap takes effect)
	assert.Equal(t, AgentRoleBaseline, ResolveEffectiveRole(AgentRoleFull, "admin", AgentRoleBaseline))
}

func TestResolveEffectiveRole_EmptyUserRole(t *testing.T) {
	// Empty user role defaults to member ceiling (baseline)
	assert.Equal(t, AgentRoleBaseline, ResolveEffectiveRole(AgentRoleFull, "", AgentRoleFull))
	assert.Equal(t, AgentRoleReadOnly, ResolveEffectiveRole(AgentRoleReadOnly, "", AgentRoleFull))
}

func TestResolveEffectiveRole_LatticeMin(t *testing.T) {
	// Three-way min: admin + full request + readonly project = readonly
	assert.Equal(t, AgentRoleReadOnly, ResolveEffectiveRole(AgentRoleFull, "admin", AgentRoleReadOnly))

	// Three-way min: member + readonly request + full project = readonly
	assert.Equal(t, AgentRoleReadOnly, ResolveEffectiveRole(AgentRoleReadOnly, "member", AgentRoleFull))

	// Three-way min: admin + none request + full project = none
	assert.Equal(t, AgentRoleNone, ResolveEffectiveRole(AgentRoleNone, "admin", AgentRoleFull))

	// Three-way min: member + full request + none project = none
	assert.Equal(t, AgentRoleNone, ResolveEffectiveRole(AgentRoleFull, "member", AgentRoleNone))
}
