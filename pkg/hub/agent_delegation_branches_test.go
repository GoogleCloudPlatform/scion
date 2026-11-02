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
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/authzop"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAgentDelegationHandlerBranches_MatchCatalog pins the handlers'
// explicit delegated branches (admitDelegatedHandler,
// admitDelegatedAgentHandler and admitDelegatedListHandler calls) against the
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
				if fn, ok := node.Fun.(*ast.Ident); ok && (fn.Name == "admitDelegatedHandler" || fn.Name == "admitDelegatedAgentHandler" || fn.Name == "admitDelegatedListHandler") {
					require.Len(t, node.Args, 4, "%s: %s takes four arguments", fset.Position(node.Pos()), fn.Name)
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

	// The project form of an agent sub-route records the project, resolved
	// from its {uuid}__{slug} form, with the agent segment as given.
	pid := "11111111-2222-3333-4444-555555555555"
	route, agentRoute, ok = resolveDelegatedRoute(RouteMetadata{RouteID: "projects.byId"}, http.MethodPost, "/api/v1/projects/"+pid+"__p-slug/agents/a-slug/message")
	require.True(t, ok)
	require.NotNil(t, agentRoute)
	assert.Equal(t, delegatedRouteID(ProjectAgentRouteActionMessage), route.RouteID)
	assert.Equal(t, opAgentActionMessage, route.OperationID)
	assert.Equal(t, delegatedRouteTarget{Type: "agent", ID: "a-slug", ProjectID: pid}, route.Target)

	route, _, ok = resolveDelegatedRoute(RouteMetadata{RouteID: "agents.byId"}, http.MethodGet, "/api/v1/agents/a-1/pty")
	require.True(t, ok)
	assert.Equal(t, delegatedRouteID(AgentRoutePTY), route.RouteID)
	assert.Equal(t, opAgentAttach, route.OperationID)
	assert.Equal(t, delegatedRouteTarget{Type: "agent", ID: "a-1"}, route.Target)

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

// TestAdmitDelegatedHandler_DeniesUnlessAdmittedForThisOperationAndTarget:
// the explicit handler branch lets a delegated caller through only when
// routeGuard admitted exactly the handler's operation for exactly the
// handler's resource; every other shape answers 403
// credential_not_admitted. A non-delegated caller is never affected.
func TestAdmitDelegatedHandler_DeniesUnlessAdmittedForThisOperationAndTarget(t *testing.T) {
	agentEntry, ok := agentDelegationAdmissionFor(http.MethodGet, delegatedRouteID(AgentRouteRoot))
	require.True(t, ok)
	projectEntry, ok := agentDelegationAdmissionFor(http.MethodGet, projectResourceRouteID)
	require.True(t, ok)
	agentRes := Resource{Type: "agent", ID: "a-1"}
	delegated := adtTestIdentity("agent-x", "project-x")

	type ctxFn func(context.Context) context.Context
	withEntry := func(e agentDelegationAdmission) ctxFn {
		return func(ctx context.Context) context.Context { return contextWithDelegatedAdmission(ctx, e) }
	}
	withTarget := func(typ, id string) ctxFn {
		return func(ctx context.Context) context.Context {
			return contextWithDelegatedTarget(ctx, delegatedRouteTarget{Type: typ, ID: id})
		}
	}
	cases := []struct {
		name     string
		identity Identity
		setup    []ctxFn
		op       authzop.OperationID
		resource Resource
		allowed  bool
	}{
		{"admitted for this operation and target", delegated, []ctxFn{withEntry(agentEntry), withTarget("agent", "a-1")}, opAgentRead, agentRes, true},
		{"no admission entry", delegated, []ctxFn{withTarget("agent", "a-1")}, opAgentRead, agentRes, false},
		{"admitted for another operation", delegated, []ctxFn{withEntry(projectEntry), withTarget("agent", "a-1")}, opAgentRead, agentRes, false},
		{"no route target", delegated, []ctxFn{withEntry(agentEntry)}, opAgentRead, agentRes, false},
		{"target of another type with the same ID", delegated, []ctxFn{withEntry(agentEntry), withTarget("project", "a-1")}, opAgentRead, agentRes, false},
		{"target with another ID", delegated, []ctxFn{withEntry(agentEntry), withTarget("agent", "a-2")}, opAgentRead, agentRes, false},
		{"empty resource ID", delegated, []ctxFn{withEntry(agentEntry), withTarget("agent", "a-1")}, opAgentRead, Resource{Type: "agent"}, false},
		{"project-form target", delegated, []ctxFn{withEntry(agentEntry), func(ctx context.Context) context.Context {
			return contextWithDelegatedTarget(ctx, delegatedRouteTarget{Type: "agent", ID: "a-1", ProjectID: "p-1"})
		}}, opAgentRead, agentRes, false},
		{"non-delegated caller without any admission", NewAuthenticatedUser("u-1", "u@example.com", "U", "member", "web"), nil, opAgentRead, agentRes, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := contextWithIdentity(context.Background(), tc.identity)
			for _, fn := range tc.setup {
				ctx = fn(ctx)
			}
			req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/a-1", nil).WithContext(ctx)
			rec := httptest.NewRecorder()
			got := admitDelegatedHandler(rec, req, tc.op, tc.resource)
			assert.Equal(t, tc.allowed, got)
			if tc.allowed {
				assert.Equal(t, http.StatusOK, rec.Code, "nothing written on the allow path")
				assert.Empty(t, rec.Body.String())
				return
			}
			require.Equal(t, http.StatusForbidden, rec.Code)
			var body struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), rec.Body.String())
			assert.Equal(t, agentDelegationCodeCredentialNotAdmitted, body.Error.Code)
		})
	}
}

// TestAdmitDelegatedListHandler_DeniesUnlessAdmittedForThisListAndCollection:
// the list handler branch lets a delegated caller through only when
// routeGuard admitted exactly the handler's operation on a list route (an
// entry with an item filter) whose target is exactly the collection; every
// other shape answers 403 credential_not_admitted. A non-delegated caller is
// never affected.
func TestAdmitDelegatedListHandler_DeniesUnlessAdmittedForThisListAndCollection(t *testing.T) {
	listEntry, ok := agentDelegationAdmissionFor(http.MethodGet, agentsCollectionRouteID)
	require.True(t, ok)
	readEntry, ok := agentDelegationAdmissionFor(http.MethodGet, delegatedRouteID(AgentRouteRoot))
	require.True(t, ok)
	noFilter := listEntry
	noFilter.ItemFilter = ""
	delegated := adtTestIdentity("agent-x", "project-x")

	type ctxFn func(context.Context) context.Context
	withEntry := func(e agentDelegationAdmission) ctxFn {
		return func(ctx context.Context) context.Context { return contextWithDelegatedAdmission(ctx, e) }
	}
	withTarget := func(tg delegatedRouteTarget) ctxFn {
		return func(ctx context.Context) context.Context { return contextWithDelegatedTarget(ctx, tg) }
	}
	otherID := hubAgentCollectionTarget
	otherID.ID = "elsewhere"
	withProject := hubAgentCollectionTarget
	withProject.ProjectID = "p-1"
	cases := []struct {
		name     string
		identity Identity
		setup    []ctxFn
		allowed  bool
	}{
		{"admitted for the list operation and the collection", delegated, []ctxFn{withEntry(listEntry), withTarget(hubAgentCollectionTarget)}, true},
		{"no admission entry", delegated, []ctxFn{withTarget(hubAgentCollectionTarget)}, false},
		{"admitted for the agent read route", delegated, []ctxFn{withEntry(readEntry), withTarget(hubAgentCollectionTarget)}, false},
		{"list entry without an item filter", delegated, []ctxFn{withEntry(noFilter), withTarget(hubAgentCollectionTarget)}, false},
		{"no route target", delegated, []ctxFn{withEntry(listEntry)}, false},
		{"collection type with another ID", delegated, []ctxFn{withEntry(listEntry), withTarget(otherID)}, false},
		{"collection with a project", delegated, []ctxFn{withEntry(listEntry), withTarget(withProject)}, false},
		{"non-delegated caller without any admission", NewAuthenticatedUser("u-1", "u@example.com", "U", "member", "web"), nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := contextWithIdentity(context.Background(), tc.identity)
			for _, fn := range tc.setup {
				ctx = fn(ctx)
			}
			req := httptest.NewRequest(http.MethodGet, "/api/v1/agents", nil).WithContext(ctx)
			rec := httptest.NewRecorder()
			got := admitDelegatedListHandler(rec, req, opAgentList, hubAgentCollectionTarget)
			assert.Equal(t, tc.allowed, got)
			if tc.allowed {
				assert.Equal(t, http.StatusOK, rec.Code, "nothing written on the allow path")
				assert.Empty(t, rec.Body.String())
				return
			}
			require.Equal(t, http.StatusForbidden, rec.Code)
			var body struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), rec.Body.String())
			assert.Equal(t, agentDelegationCodeCredentialNotAdmitted, body.Error.Code)
		})
	}
}

// TestAdmitDelegatedAgentHandler_DeniesUnlessTheRouteNamesTheAgent: the
// agent handler branch lets a delegated caller through only when routeGuard
// admitted exactly the handler's operation on a route that names the loaded
// agent: by ID, or by slug inside the agent's project on the project form.
// Every other shape answers 403 credential_not_admitted, and a
// non-delegated caller is never affected.
func TestAdmitDelegatedAgentHandler_DeniesUnlessTheRouteNamesTheAgent(t *testing.T) {
	byID, ok := agentDelegationAdmissionFor(http.MethodPost, delegatedRouteID(AgentRouteActionMessage))
	require.True(t, ok)
	projectForm, ok := agentDelegationAdmissionFor(http.MethodPost, delegatedRouteID(ProjectAgentRouteActionMessage))
	require.True(t, ok)
	pty, ok := agentDelegationAdmissionFor(http.MethodGet, delegatedRouteID(AgentRoutePTY))
	require.True(t, ok)
	agent := &store.Agent{ID: "a-1", Slug: "a-slug", ProjectID: "p-1"}
	delegated := adtTestIdentity("agent-x", "project-x")

	cases := []struct {
		name     string
		identity Identity
		entry    *agentDelegationAdmission
		target   *delegatedRouteTarget
		op       authzop.OperationID
		agent    *store.Agent
		allowed  bool
	}{
		{"by ID", delegated, &byID, &delegatedRouteTarget{Type: "agent", ID: "a-1"}, opAgentActionMessage, agent, true},
		{"project form by slug", delegated, &projectForm, &delegatedRouteTarget{Type: "agent", ID: "a-slug", ProjectID: "p-1"}, opAgentActionMessage, agent, true},
		{"project form by ID", delegated, &projectForm, &delegatedRouteTarget{Type: "agent", ID: "a-1", ProjectID: "p-1"}, opAgentActionMessage, agent, true},
		{"attach handshake", delegated, &pty, &delegatedRouteTarget{Type: "agent", ID: "a-1"}, opAgentAttach, agent, true},
		{"by-ID route names the slug", delegated, &byID, &delegatedRouteTarget{Type: "agent", ID: "a-slug"}, opAgentActionMessage, agent, false},
		{"project form in another project", delegated, &projectForm, &delegatedRouteTarget{Type: "agent", ID: "a-slug", ProjectID: "p-2"}, opAgentActionMessage, agent, false},
		{"another agent", delegated, &byID, &delegatedRouteTarget{Type: "agent", ID: "a-2"}, opAgentActionMessage, agent, false},
		{"admitted for another operation", delegated, &pty, &delegatedRouteTarget{Type: "agent", ID: "a-1"}, opAgentActionMessage, agent, false},
		{"no admission entry", delegated, nil, &delegatedRouteTarget{Type: "agent", ID: "a-1"}, opAgentActionMessage, agent, false},
		{"no route target", delegated, &byID, nil, opAgentActionMessage, agent, false},
		{"project target", delegated, &byID, &delegatedRouteTarget{Type: "project", ID: "a-1"}, opAgentActionMessage, agent, false},
		{"nil agent", delegated, &byID, &delegatedRouteTarget{Type: "agent", ID: "a-1"}, opAgentActionMessage, nil, false},
		{"non-delegated caller without any admission", NewAuthenticatedUser("u-1", "u@example.com", "U", "member", "web"), nil, nil, opAgentActionMessage, agent, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := contextWithIdentity(context.Background(), tc.identity)
			if tc.entry != nil {
				ctx = contextWithDelegatedAdmission(ctx, *tc.entry)
			}
			if tc.target != nil {
				ctx = contextWithDelegatedTarget(ctx, *tc.target)
			}
			req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/a-1/message", nil).WithContext(ctx)
			rec := httptest.NewRecorder()
			got := admitDelegatedAgentHandler(rec, req, tc.op, tc.agent)
			assert.Equal(t, tc.allowed, got)
			if tc.allowed {
				assert.Equal(t, http.StatusOK, rec.Code, "nothing written on the allow path")
				assert.Empty(t, rec.Body.String())
				return
			}
			require.Equal(t, http.StatusForbidden, rec.Code)
			var body struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), rec.Body.String())
			assert.Equal(t, agentDelegationCodeCredentialNotAdmitted, body.Error.Code)
		})
	}
}
