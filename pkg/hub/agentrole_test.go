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
	// Unknown role strings should fail closed to none scopes
	scopes := ScopesForRole(AgentRole("unknown-role"))
	assert.Nil(t, scopes)
}

func TestScopesForRole_EmptyStringDefaultsToFull(t *testing.T) {
	// Empty string (legacy agents with no stored role) defaults to full scopes
	scopes := ScopesForRole(AgentRole(""))
	fullScopes := ScopesForRole(AgentRoleFull)
	assert.Equal(t, fullScopes, scopes)
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
	// Empty returns full (the default role)
	assert.Equal(t, AgentRoleFull, minRole())

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
	// Member user requesting full gets full (member ceiling is now full)
	assert.Equal(t, AgentRoleFull, ResolveEffectiveRole(AgentRoleFull, "member", AgentRoleFull))

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
	// Empty user role defaults to full ceiling
	assert.Equal(t, AgentRoleFull, ResolveEffectiveRole(AgentRoleFull, "", AgentRoleFull))
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

func TestResolveEffectiveRole_InvalidRequestedRole(t *testing.T) {
	// An unknown/invalid requested role gets ordinal 0 (none) via the default
	// case in roleOrdinal. This is the fail-closed behavior: invalid roles
	// resolve to the lowest privilege level.

	// Admin + invalid request + full project: invalid ordinal (0) is the min,
	// so the invalid role string is returned. Its scopes resolve to none (nil).
	resolved := ResolveEffectiveRole(AgentRole("superuser"), "admin", AgentRoleFull)
	assert.Equal(t, AgentRole("superuser"), resolved)
	assert.Nil(t, ScopesForRole(resolved))

	// Member + invalid request + full project: invalid ordinal (0) is still
	// the min — fail-closed.
	resolved = ResolveEffectiveRole(AgentRole("superuser"), "member", AgentRoleFull)
	assert.Nil(t, ScopesForRole(resolved))

	// Admin + invalid request + readonly project: invalid (0) < readonly (1),
	// so invalid wins — even the project cap can't elevate an invalid role.
	resolved = ResolveEffectiveRole(AgentRole("superuser"), "admin", AgentRoleReadOnly)
	assert.Equal(t, AgentRole("superuser"), resolved)
	assert.Nil(t, ScopesForRole(resolved))
}

func TestResolveEffectiveRole_ProjectMaxReadonly_AdminRequestsFull(t *testing.T) {
	// When project max is "readonly" and an admin requests full, effective is readonly.
	assert.Equal(t, AgentRoleReadOnly, ResolveEffectiveRole(AgentRoleFull, "admin", AgentRoleReadOnly))
}

func TestResolveEffectiveRole_ProjectMaxNone(t *testing.T) {
	// When project max is "none", any request resolves to none.
	assert.Equal(t, AgentRoleNone, ResolveEffectiveRole(AgentRoleFull, "admin", AgentRoleNone))
	assert.Equal(t, AgentRoleNone, ResolveEffectiveRole(AgentRoleBaseline, "member", AgentRoleNone))
	assert.Equal(t, AgentRoleNone, ResolveEffectiveRole(AgentRoleReadOnly, "admin", AgentRoleNone))
}

func TestResolveEffectiveRole_ProjectMaxFull_MemberGetsFull(t *testing.T) {
	// When project max is full and user is member, member ceiling is full so full is granted.
	assert.Equal(t, AgentRoleFull, ResolveEffectiveRole(AgentRoleFull, "member", AgentRoleFull))
}

func TestScopesForRole_RoleNoneMapToNoAuth(t *testing.T) {
	// role=none produces nil scopes — caller should set NoAuth=true
	scopes := ScopesForRole(AgentRoleNone)
	assert.Nil(t, scopes)
}

func TestScopesForRole_RoleReadOnlyHasOnlyRead(t *testing.T) {
	scopes := ScopesForRole(AgentRoleReadOnly)
	require.Len(t, scopes, 1)
	assert.Equal(t, ScopeProjectRead, scopes[0])
	// Must NOT have elevated scopes
	assert.NotContains(t, scopes, ScopeAgentCreate)
	assert.NotContains(t, scopes, ScopeAgentStatusUpdate)
}
