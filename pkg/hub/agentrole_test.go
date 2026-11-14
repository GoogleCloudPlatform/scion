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

	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestScopesForRole_None(t *testing.T) {
	scopes := ScopesForRole(AgentRoleNone)
	assert.Nil(t, scopes)
}

func TestScopesForRole_ReadOnly(t *testing.T) {
	scopes := ScopesForRole(AgentRoleReadOnly)
	require.Len(t, scopes, 3)
	assert.Equal(t, ScopeProjectRead, scopes[0])
	// Self-only: updateAgentStatus admits only the bearer's own ID.
	assert.Equal(t, ScopeAgentStatusUpdate, scopes[1])
	// Ceiling-optional: see ceilingOptionalRoleScopes.
	assert.Equal(t, ScopeProjectArtifactRead, scopes[2])
}

func TestScopesForRole_Baseline(t *testing.T) {
	scopes := ScopesForRole(AgentRoleBaseline)
	require.Len(t, scopes, 7)

	// Must include these scopes
	assert.Contains(t, scopes, ScopeProjectRead)
	assert.Contains(t, scopes, ScopeAgentStatusUpdate)
	assert.Contains(t, scopes, ScopeAgentTokenRefresh)
	assert.Contains(t, scopes, ScopeAgentNotify)
	assert.Contains(t, scopes, ScopeAgentPortForward)
	assert.Contains(t, scopes, ScopeProjectArtifactRead)
	assert.Contains(t, scopes, ScopeProjectArtifactWrite)

	// Must NOT include elevated scopes
	assert.NotContains(t, scopes, ScopeAgentCreate)
	assert.NotContains(t, scopes, ScopeAgentLifecycle)
	assert.NotContains(t, scopes, ScopeProjectSecretRead)
	assert.NotContains(t, scopes, ScopeProjectTemplateWrite)
}

func TestScopesForRole_Full(t *testing.T) {
	scopes := ScopesForRole(AgentRoleFull)
	require.Len(t, scopes, 13)
	assert.Contains(t, scopes, ScopeProjectArtifactWrite)

	// Must include everything in baseline
	assert.Contains(t, scopes, ScopeProjectRead)
	assert.Contains(t, scopes, ScopeAgentStatusUpdate)
	assert.Contains(t, scopes, ScopeAgentTokenRefresh)
	assert.Contains(t, scopes, ScopeAgentNotify)
	assert.Contains(t, scopes, ScopeAgentPortForward)

	// Plus elevated scopes
	assert.Contains(t, scopes, ScopeAgentCreate)
	// ptone/scion#2339: gcp_service_account.assign has its own agent scope,
	// distinct from ScopeAgentCreate. AgentRoleFull carries both, so role=full
	// keeps authorizing both agent creation and service-account assignment.
	assert.Contains(t, scopes, ScopeAgentSAAssign)
	assert.Contains(t, scopes, ScopeAgentLifecycle)
	assert.Contains(t, scopes, ScopeProjectSecretRead)
	assert.Contains(t, scopes, ScopeProjectTemplateWrite)
	assert.Contains(t, scopes, ScopeAgentSetMessageMode)
	assert.Contains(t, scopes, ScopeProjectArtifactRead)
}

func TestScopesForRole_InvalidDefault(t *testing.T) {
	// Unknown role strings should fail closed to none scopes
	scopes := ScopesForRole(AgentRole("unknown-role"))
	assert.Nil(t, scopes)
}

func TestScopesForRole_EmptyStringDefaultsToNone(t *testing.T) {
	// Empty string (missing stored role) fails closed to no scopes.
	scopes := ScopesForRole(AgentRole(""))
	assert.Nil(t, scopes)
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

	// Missing role data is least-privileged.
	assert.Equal(t, 0, CompareRoles(AgentRole(""), AgentRoleNone))
	assert.Less(t, CompareRoles(AgentRole(""), AgentRoleReadOnly), 0)
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

func TestScopesForRole_RoleNoneMapToNoAuth(t *testing.T) {
	// role=none produces nil scopes — caller should set NoAuth=true
	scopes := ScopesForRole(AgentRoleNone)
	assert.Nil(t, scopes)
}

func TestScopesForRole_RoleReadOnlyHasOnlyReadAndSelfStatus(t *testing.T) {
	scopes := ScopesForRole(AgentRoleReadOnly)
	assert.ElementsMatch(t, []AgentTokenScope{ScopeProjectRead, ScopeAgentStatusUpdate, ScopeProjectArtifactRead}, scopes)
	// Must NOT have elevated scopes
	for _, s := range []AgentTokenScope{
		ScopeAgentCreate, ScopeAgentLifecycle, ScopeAgentNotify, ScopeAgentPortForward,
		ScopeAgentTokenRefresh, ScopeAgentSAAssign, ScopeProjectSecretRead,
		ScopeProjectTemplateWrite, ScopeAgentSetMessageMode, ScopeProjectArtifactWrite,
	} {
		assert.NotContains(t, scopes, s)
	}
}

// The status scope covers only self operations, so adding it to readonly
// does not change which bounded ceilings readonly fits: a ceiling holding
// just project.read still fits it (ptone/scion#7926).
func TestReadonlyRole_StatusScopeIsSelfOperationOnly(t *testing.T) {
	for _, perm := range agentScopeCoverage([]AgentTokenScope{ScopeAgentStatusUpdate}) {
		assert.True(t, selfOperationSet[perm], "%s must be a self operation", perm)
	}
	c := store.EffectCeiling{Kind: store.EffectCeilingBounded, Version: permissions.CeilingVersionV1, PermissionIDs: []string{"project.read"}}
	assert.True(t, roleFitsCeiling(c, AgentRoleReadOnly))
}
