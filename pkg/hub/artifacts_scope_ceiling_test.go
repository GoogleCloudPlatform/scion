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

//go:build !no_sqlite

package hub

import (
	"context"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// preArtifactReadCeiling is a bounded ceiling frozen before artifacts
// existed: exactly project:read's coverage at that time, plus agent.create,
// as a UAT minted with the read selectors and agent:create carries.
func preArtifactReadCeiling() store.EffectCeiling {
	return boundedCeiling(
		"harness_config.list", "harness_config.read", "project.read",
		"skill.list", "skill.read", "template.list", "template.read",
		"agent.create",
	)
}

// TestPreArtifactCeilingKeepsProjectRead is the regression test for adding
// an artifact permission to the agent roles: a ceiling frozen before it must
// keep admitting the scopes and roles it admitted, and must not gain the new
// capability.
func TestPreArtifactCeilingKeepsProjectRead(t *testing.T) {
	c := preArtifactReadCeiling()

	assert.True(t, ceilingAllowsScope(c, ScopeProjectRead), "project:read must still be issuable")
	assert.False(t, ceilingAllowsScope(c, ScopeProjectArtifactRead), "project:artifact:read must not be issuable")
	assert.False(t, EffectCeilingAllows(c, "artifact.read", false), "artifact.read stays denied at use")

	role, cause, ok := childRoleWithinCeiling(c, AgentRoleFull, false)
	require.True(t, ok, "deny cause %q", cause)
	assert.Equal(t, AgentRoleBaseline, role, "a defaulted full role still caps to baseline")
	for _, explicit := range []AgentRole{AgentRoleReadOnly, AgentRoleBaseline} {
		got, cause, ok := childRoleWithinCeiling(c, explicit, true)
		assert.True(t, ok, "explicit %s must still fit (cause %q)", explicit, cause)
		assert.Equal(t, explicit, got)
	}

	issued := filterScopes(ScopesForRole(AgentRoleBaseline), c, ScopeCeilings{})
	assert.Contains(t, issued, ScopeProjectRead)
	assert.NotContains(t, issued, ScopeProjectArtifactRead, "the mint filter drops the ceiling-optional scope")
}

// TestPreArtifactCeilingChildCannotReadArtifacts follows the token a child
// under a pre-artifact ceiling is issued through to the artifact host: it
// keeps project reads but is not served by the artifact service.
func TestPreArtifactCeilingChildCannotReadArtifacts(t *testing.T) {
	srv, s := testServer(t)
	host := newArtifactHost(srv)
	agent := createTestAgent(t, s)

	issued := filterScopes(ScopesForRole(AgentRoleBaseline), preArtifactReadCeiling(), ScopeCeilings{})
	ctx := contextWithIdentity(context.Background(), artifactTestAgent(agent.ID, agent.ProjectID, issued...))

	_, _, _, ok := host.Principal(ctx)
	assert.False(t, ok, "the artifact service must not serve the child")
	assert.False(t, host.Authorize(ctx, agent.ProjectID, artifacts.PermissionRead))
}

// TestCeilingOptionalScopesDoNotDecideDelegation: an agent without the
// artifact scopes may still delegate a role that carries them (the child's
// mint drops them), but asking for one explicitly still requires holding it.
func TestCeilingOptionalScopesDoNotDecideDelegation(t *testing.T) {
	authz, _ := authzTestSetup(t)
	required := []AgentTokenScope{}
	for _, sc := range ScopesForRole(AgentRoleBaseline) {
		if !ceilingOptionalRoleScopes[sc] {
			required = append(required, sc)
		}
	}
	actor := artifactTestAgent(tid("optional-delegator"), tid("optional-project"), required...)

	byRole := authz.canAgentDelegateToAgent(actor, GrantDescriptor{
		Type: GrantTypeAgentDelegation, AgentRole: string(AgentRoleBaseline), ProjectID: tid("optional-project"),
	})
	assert.True(t, byRole.Allowed, "delegating baseline must not require the optional scopes: %q", byRole.Reason)

	explicit := authz.canAgentDelegateToAgent(actor, GrantDescriptor{
		Type: GrantTypeAgentDelegation, AgentRole: string(AgentRoleBaseline), ProjectID: tid("optional-project"),
		AgentScopes: []AgentTokenScope{ScopeProjectArtifactRead},
	})
	assert.False(t, explicit.Allowed, "an explicitly requested optional scope stays required")
}
