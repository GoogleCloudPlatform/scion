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
	"fmt"
	"slices"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
)

func TestMintCandidateScopes_ShellAgentCapped(t *testing.T) {
	roles := []AgentRole{
		AgentRoleNone,
		AgentRoleReadOnly,
		AgentRoleBaseline,
		AgentRoleFull,
	}
	signals := []struct {
		name              string
		colHarnessConfig  string
		appliedHarnessCfg string
	}{
		{name: "column_only", colHarnessConfig: config.ShellHarnessConfigName, appliedHarnessCfg: ""},
		{name: "applied_config_only", colHarnessConfig: "", appliedHarnessCfg: config.ShellHarnessConfigName},
		{name: "both", colHarnessConfig: config.ShellHarnessConfigName, appliedHarnessCfg: config.ShellHarnessConfigName},
	}

	for _, role := range roles {
		for _, devAuth := range []bool{false, true} {
			for _, gcpAssign := range []bool{false, true} {
				for _, sig := range signals {
					name := fmt.Sprintf("role=%s/devAuth=%v/gcpAssign=%v/signal=%s", role, devAuth, gcpAssign, sig.name)
					t.Run(name, func(t *testing.T) {
						applied := &store.AgentAppliedConfig{
							HarnessConfig: sig.appliedHarnessCfg,
							AgentRole:     string(role),
						}
						if gcpAssign {
							applied.GCPIdentity = &store.GCPIdentityConfig{
								MetadataMode:     store.GCPMetadataModeAssign,
								ServiceAccountID: "sa-shell-test",
							}
						}
						agent := &store.Agent{
							ID:            "shell-cap-agent",
							HarnessConfig: sig.colHarnessConfig,
							AppliedConfig: applied,
						}
						authz := &AuthzService{mintDevAuthOverride: devAuth}
						got := authz.mintCandidateScopes(agent)

						// Invariant 1/2/3/4: result is always a subset of shellRuntimeScopes.
						assert.True(t, scopesSubsetOf(got, shellRuntimeScopes),
							"shell scopes %v must be subset of %v", got, shellRuntimeScopes)

						// Expected: ScopesForRole(effectiveRole) ∩ shellRuntimeScopes,
						// preserving candidate order, with GCP token scopes dropped.
						effectiveRole := role
						if devAuth && CompareRoles(effectiveRole, AgentRoleFull) < 0 {
							effectiveRole = AgentRoleFull
						}
						var want []AgentTokenScope
						for _, s := range ScopesForRole(effectiveRole) {
							if slices.Contains(shellRuntimeScopes, s) {
								want = append(want, s)
							}
						}
						assert.Equal(t, want, got)

						// Invariant 6: lower roles stay lower when dev-auth override is off.
						if !devAuth {
							switch role {
							case AgentRoleNone:
								assert.Empty(t, got)
							case AgentRoleReadOnly:
								assert.Equal(t, []AgentTokenScope{ScopeAgentStatusUpdate}, got)
							case AgentRoleBaseline, AgentRoleFull:
								assert.Equal(t, shellRuntimeScopes, got)
							}
						}

						// Invariant 7: stored role is unchanged.
						assert.Equal(t, string(role), agent.AppliedConfig.AgentRole)
					})
				}
			}
		}
	}

	// Column-only shell agent with nil AppliedConfig fails closed (invariant 4).
	t.Run("column_only_nil_applied_config", func(t *testing.T) {
		agent := &store.Agent{
			ID:            "shell-nil-applied",
			HarnessConfig: config.ShellHarnessConfigName,
		}
		gotNoDev := (&AuthzService{mintDevAuthOverride: false}).mintCandidateScopes(agent)
		assert.Empty(t, gotNoDev)

		gotDev := (&AuthzService{mintDevAuthOverride: true}).mintCandidateScopes(agent)
		assert.Equal(t, shellRuntimeScopes, gotDev)
	})
}

func TestMintCandidateScopes_NonShellUnchanged(t *testing.T) {
	roles := []AgentRole{
		AgentRoleNone,
		AgentRoleReadOnly,
		AgentRoleBaseline,
		AgentRoleFull,
	}
	harnessConfigs := []string{"", "claude", "gemini"}

	for _, hc := range harnessConfigs {
		for _, role := range roles {
			for _, devAuth := range []bool{false, true} {
				for _, gcpAssign := range []bool{false, true} {
					name := fmt.Sprintf("hc=%s/role=%s/devAuth=%v/gcpAssign=%v", hc, role, devAuth, gcpAssign)
					t.Run(name, func(t *testing.T) {
						applied := &store.AgentAppliedConfig{
							HarnessConfig: hc,
							AgentRole:     string(role),
						}
						if gcpAssign {
							applied.GCPIdentity = &store.GCPIdentityConfig{
								MetadataMode:     store.GCPMetadataModeAssign,
								ServiceAccountID: "sa-nonshell-test",
							}
						}
						agent := &store.Agent{
							ID:            "nonshell-agent",
							HarnessConfig: hc,
							AppliedConfig: applied,
						}
						authz := &AuthzService{mintDevAuthOverride: devAuth}
						got := authz.mintCandidateScopes(agent)

						effectiveRole := role
						if devAuth && CompareRoles(effectiveRole, AgentRoleFull) < 0 {
							effectiveRole = AgentRoleFull
						}
						want := append([]AgentTokenScope(nil), ScopesForRole(effectiveRole)...)
						if gcpAssign {
							want = append(want, GCPTokenScopeForSA("sa-nonshell-test"))
						}
						assert.Equal(t, want, got)
					})
				}
			}
		}
	}
}
