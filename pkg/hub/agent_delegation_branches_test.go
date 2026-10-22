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
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/authzop"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAgentDelegationHandlerBranches_MatchCatalog pins the handlers'
// explicit delegated branches (admitDelegatedHandler calls) against the
// catalog and the admission table: the set of operations named in those
// calls equals the set of catalog operations that admit the delegated
// principal and credential, which equals the set of admission table
// operations. So no handler admits the delegated kind without a catalog
// entry, and no catalog entry lacks a handler branch.
func TestAgentDelegationHandlerBranches_MatchCatalog(t *testing.T) {
	// pkgmove:scan-covers pkg/hub/apierr
	// scanned through guardScanSubdirs; finds admitDelegatedHandler calls and operation constants.
	// pkgmove:scan-covers pkg/hub/internal/principal
	// scanned through guardScanSubdirs; finds admitDelegatedHandler calls and operation constants.
	// pkgmove:scan-covers pkg/hub/maintenance
	// scanned through guardScanSubdirs once it exists; finds admitDelegatedHandler calls and operation constants.
	files, err := globGuardScanDirs(".", "*.go")
	require.NoError(t, err)
	fset := token.NewFileSet()
	consts := map[string]string{}
	type branch struct {
		where string
		arg   ast.Expr
	}
	var branches []branch
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		require.NoError(t, err)
		file, err := parser.ParseFile(fset, name, src, 0)
		require.NoError(t, err)
		ast.Inspect(file, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.ValueSpec:
				for i, id := range node.Names {
					if i >= len(node.Values) {
						continue
					}
					if lit, ok := node.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
						if v, err := strconv.Unquote(lit.Value); err == nil {
							consts[id.Name] = v
						}
					}
				}
			case *ast.CallExpr:
				if fn, ok := node.Fun.(*ast.Ident); ok && fn.Name == "admitDelegatedHandler" {
					require.Len(t, node.Args, 4, "%s: admitDelegatedHandler takes four arguments", fset.Position(node.Pos()))
					branches = append(branches, branch{where: fset.Position(node.Pos()).String(), arg: node.Args[2]})
				}
			}
			return true
		})
	}

	branchOps := map[string]bool{}
	for _, b := range branches {
		id, ok := b.arg.(*ast.Ident)
		if !assert.True(t, ok, "%s: the operation must be a named constant", b.where) {
			continue
		}
		op, ok := consts[id.Name]
		if !assert.True(t, ok, "%s: %s is not a string constant of package hub", b.where, id.Name) {
			continue
		}
		branchOps[op] = true
	}
	catalogOps := map[string]bool{}
	for _, spec := range authzop.Catalog {
		if catalogAdmitsDelegated(spec) {
			catalogOps[string(spec.ID)] = true
		}
	}
	tableOps := map[string]bool{}
	for _, entry := range agentDelegationAdmittedRoutes {
		tableOps[string(entry.Operation)] = true
	}

	keys := func(m map[string]bool) []string {
		out := make([]string, 0, len(m))
		for k := range m {
			out = append(out, k)
		}
		sort.Strings(out)
		return out
	}
	require.NotEmpty(t, branchOps, "no handler has a delegated branch")
	assert.Equal(t, keys(catalogOps), keys(branchOps), "handler delegated branches and catalog operations admitting the delegated pair differ")
	assert.Equal(t, keys(catalogOps), keys(tableOps), "admission table operations and catalog operations admitting the delegated pair differ")
}

// TestProjectResourceRoute_ResolvesOnlyTheExactPath: the project resource
// resolution the admission table matches accepts exactly
// /api/v1/projects/{id}, and only GET has an operation.
func TestProjectResourceRoute_ResolvesOnlyTheExactPath(t *testing.T) {
	route, ok := resolveProjectResourceRoute(http.MethodGet, "/api/v1/projects/p-1")
	require.True(t, ok)
	assert.Equal(t, projectResourceRouteID, route.RouteID)
	assert.Equal(t, opProjectRead, route.OperationID)
	assert.Equal(t, delegatedRouteTarget{Type: "project", ID: "p-1"}, route.Target)

	route, ok = resolveProjectResourceRoute(http.MethodGet, "/api/v1/projects/p-1__my-project")
	require.True(t, ok)
	assert.Equal(t, "p-1", route.Target.ID, "the {uuid}__{slug} form resolves to the ID")

	for _, method := range []string{http.MethodPatch, http.MethodDelete, http.MethodPost, http.MethodPut} {
		route, ok := resolveProjectResourceRoute(method, "/api/v1/projects/p-1")
		require.True(t, ok)
		assert.Empty(t, route.OperationID, "%s has no admitted operation", method)
	}
	for _, path := range []string{
		"/api/v1/projects/",
		"/api/v1/projects/p-1/",
		"/api/v1/projects/p-1/members",
		"/api/v1/projects/p-1/agents",
		"/api/v1/projects/register",
		"/api/v1/projects/.",
		"/api/v1/projects/..",
		"/api/v1/projects/p%2F1",
		"/api/v1/projects/p%5C1",
		"/api/v1/projects/p%251",
		"/api/v1/projects/p%001",
		"/api/v1/projectsX/p-1",
		"/api/v1/agents/p-1",
	} {
		_, ok := resolveProjectResourceRoute(http.MethodGet, path)
		assert.False(t, ok, "%s must not resolve", path)
	}
}

// TestResolveDelegatedRoute_OnlyOnItsMuxPatterns: a delegated request
// resolves only on the agent sub-route patterns and on the project pattern;
// the same path under any other mux route resolves to nothing.
func TestResolveDelegatedRoute_OnlyOnItsMuxPatterns(t *testing.T) {
	route, agentRoute, ok := resolveDelegatedRoute(RouteMetadata{RouteID: "agents.byId"}, http.MethodGet, "/api/v1/agents/a-1")
	require.True(t, ok)
	require.NotNil(t, agentRoute)
	assert.Equal(t, delegatedRouteID(AgentRouteRoot), route.RouteID)
	assert.Equal(t, opAgentRead, route.OperationID)
	assert.Equal(t, delegatedRouteTarget{Type: "agent", ID: "a-1"}, route.Target)

	route, agentRoute, ok = resolveDelegatedRoute(RouteMetadata{RouteID: "projects.byId"}, http.MethodGet, "/api/v1/projects/p-1")
	require.True(t, ok)
	assert.Nil(t, agentRoute)
	assert.Equal(t, projectResourceRouteID, route.RouteID)

	_, _, ok = resolveDelegatedRoute(RouteMetadata{RouteID: "projects.list"}, http.MethodGet, "/api/v1/projects/p-1")
	assert.False(t, ok)
	_, _, ok = resolveDelegatedRoute(RouteMetadata{RouteID: "agents.list"}, http.MethodGet, "/api/v1/agents/a-1")
	assert.False(t, ok)
}

// TestBearerStageAgentDelegationCode_MapsEveryStage pins the §11.2 table
// from EvaluateBearerCeiling stages to agent delegation codes; an unknown
// stage is an evaluation error.
func TestBearerStageAgentDelegationCode_MapsEveryStage(t *testing.T) {
	for stage, code := range map[string]string{
		BearerStageBoundaryInvalid:     agentDelegationCodeGrantBoundaryInvalid,
		BearerStageTargetUnknown:       agentDelegationCodeTargetUnknown,
		BearerStageOutsideBoundary:     agentDelegationCodeOutsideBoundary,
		BearerStageCeiling:             agentDelegationCodeOutsideCeiling,
		BearerStageBoundaryEligibility: agentDelegationCodeOutsideBoundaryEligibility,
		BearerStageProjectAccess:       agentDelegationCodeIssuerProjectAccess,
		BearerStageAuthority:           agentDelegationCodeIssuerAuthority,
		BearerStageError:               agentDelegationCodeEvaluationError,
		"":                             agentDelegationCodeEvaluationError,
		"no_such_stage":                agentDelegationCodeEvaluationError,
	} {
		assert.Equal(t, code, bearerStageAgentDelegationCode(stage), "stage %q", stage)
	}
}
