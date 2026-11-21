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
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"strings"
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
	assert.NotContains(t, scopes, ScopeProjectSkillWrite)
}

func TestScopesForRole_Full(t *testing.T) {
	scopes := ScopesForRole(AgentRoleFull)
	require.Len(t, scopes, 14)
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
	assert.Contains(t, scopes, ScopeProjectSkillWrite)
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
		ScopeProjectTemplateWrite, ScopeProjectSkillWrite, ScopeAgentSetMessageMode, ScopeProjectArtifactWrite,
	} {
		assert.NotContains(t, scopes, s)
	}
}

// The status scope covers only self operations, so adding it to readonly
// does not change which bounded ceilings readonly fits: a ceiling holding
// just what project:read covers still fits it (ptone/scion#7926).
func TestReadonlyRole_StatusScopeIsSelfOperationOnly(t *testing.T) {
	for _, perm := range agentScopeCoverage([]AgentTokenScope{ScopeAgentStatusUpdate}) {
		assert.True(t, selfOperationSet[perm], "%s must be a self operation", perm)
	}
	c := store.EffectCeiling{Kind: store.EffectCeilingBounded, Version: permissions.CeilingVersionV1, PermissionIDs: agentScopeCoverage([]AgentTokenScope{ScopeProjectRead})}
	assert.True(t, roleFitsCeiling(c, AgentRoleReadOnly))
}

// goldenRoleNonOptionalScopes pins the exact non-optional scopes for each
// stock agent role (ptone/scion#8324). Do NOT add a new scope here to expand
// an existing role bundle: every additive scope on an existing role MUST be
// listed in ceilingOptionalRoleScopes instead.
var goldenRoleNonOptionalScopes = map[AgentRole][]AgentTokenScope{
	AgentRoleNone: nil,
	AgentRoleReadOnly: {
		ScopeProjectRead,
		ScopeAgentStatusUpdate,
	},
	AgentRoleBaseline: {
		ScopeProjectRead,
		ScopeAgentStatusUpdate,
		ScopeAgentTokenRefresh,
		ScopeAgentNotify,
		ScopeAgentPortForward,
	},
	AgentRoleFull: {
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
	},
}

func checkRoleNonOptionalScopes(bundles func(AgentRole) []AgentTokenScope, optional map[AgentTokenScope]bool) error {
	roles := []AgentRole{AgentRoleNone, AgentRoleReadOnly, AgentRoleBaseline, AgentRoleFull}
	for _, role := range roles {
		var got []AgentTokenScope
		for _, s := range bundles(role) {
			if !optional[s] {
				got = append(got, s)
			}
		}
		want := goldenRoleNonOptionalScopes[role]
		if !slices.Equal(got, want) {
			return fmt.Errorf(
				"role %q non-optional scopes changed (got %v, want %v): "+
					"every scope added to an existing role bundle MUST be listed in ceilingOptionalRoleScopes "+
					"(pkg/hub/authz_effect_ceiling.go) so pre-deploy agent JWTs and frozen bounded delegation ceilings "+
					"do not fail delegation with 403; see ScopesForRole in pkg/hub/agentrole.go and "+
					"docs-site/src/content/docs/reference/permissions-policy.md",
				role, got, want,
			)
		}
	}
	return nil
}

// TestScopesForRole_NonOptionalScopesAreFrozen enforces that adding a scope to
// an existing role bundle without listing it in ceilingOptionalRoleScopes
// fails immediately with a pointer to the additive-scope rule
// (ptone/scion#8324).
func TestScopesForRole_NonOptionalScopesAreFrozen(t *testing.T) {
	require.NoError(t, checkRoleNonOptionalScopes(ScopesForRole, ceilingOptionalRoleScopes))

	const syntheticScope AgentTokenScope = "project:test:new_capability"
	withAdded := func(role AgentRole) []AgentTokenScope {
		base := ScopesForRole(role)
		if role == AgentRoleFull {
			return append(slices.Clone(base), syntheticScope)
		}
		return base
	}

	t.Run("non-optional addition fails with pointer to rule", func(t *testing.T) {
		err := checkRoleNonOptionalScopes(withAdded, ceilingOptionalRoleScopes)
		require.Error(t, err)
		assert.Contains(t, err.Error(), string(syntheticScope))
		assert.Contains(t, err.Error(), "ceilingOptionalRoleScopes")
		assert.Contains(t, err.Error(), "permissions-policy.md")
	})

	t.Run("ceiling-optional addition passes non-optional freeze", func(t *testing.T) {
		opt := map[AgentTokenScope]bool{syntheticScope: true}
		for k, v := range ceilingOptionalRoleScopes {
			opt[k] = v
		}
		require.NoError(t, checkRoleNonOptionalScopes(withAdded, opt))
	})
}

func isTestingTestingExpr(e ast.Expr) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok || len(call.Args) != 0 {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Testing" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "testing"
}

// TestRoleScopeWithheldForTest_UnreachableFromProductionCode proves that the
// pre-deploy simulation hook in agentrole.go is test-only: outside a test
// binary filterWithheldRoleScopesForTest is a no-op, and no non-test file
// outside agentrole.go touches roleScopesWithheldForTest or calls
// filterWithheldRoleScopesForTest.
func TestRoleScopeWithheldForTest_UnreachableFromProductionCode(t *testing.T) {
	prev := roleScopesWithheldForTest
	roleScopesWithheldForTest = map[AgentTokenScope]bool{ScopeProjectTemplateWrite: true}
	t.Cleanup(func() { roleScopesWithheldForTest = prev })

	full := scopesForRole(AgentRoleFull)
	require.Contains(t, full, ScopeProjectTemplateWrite)
	assert.Equal(t, full, filterWithheldRoleScopesForTest(false, full),
		"outside a test binary (inTestBinary=false), withheld scopes must never be filtered")
	assert.NotContains(t, filterWithheldRoleScopesForTest(true, full), ScopeProjectTemplateWrite,
		"inside a test binary (inTestBinary=true), withheld scopes are filtered for pre-deploy simulation")
	roleScopesWithheldForTest = prev

	// pkgmove:scan-covers guardScanSubdirs
	// scanned through readGuardScanDirs: ensures no production file outside agentrole.go touches roleScopesWithheldForTest.
	entries, err := readGuardScanDirs(".")
	require.NoError(t, err)
	fset := token.NewFileSet()
	varUses := map[string]int{}
	callSites := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		require.NoError(t, err, name)
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.Ident:
				if n.Name == "roleScopesWithheldForTest" {
					varUses[name]++
				}
			case *ast.CallExpr:
				if fn, ok := n.Fun.(*ast.Ident); ok && fn.Name == "filterWithheldRoleScopesForTest" {
					assert.Equal(t, "agentrole.go", name)
					assert.True(t, len(n.Args) == 2 && isTestingTestingExpr(n.Args[0]),
						"%s: filterWithheldRoleScopesForTest must pass testing.Testing() as first arg", fset.Position(n.Pos()))
					callSites++
				}
			}
			return true
		})
	}
	assert.Equal(t, map[string]int{"agentrole.go": 3}, varUses)
	assert.Equal(t, 1, callSites)
}
