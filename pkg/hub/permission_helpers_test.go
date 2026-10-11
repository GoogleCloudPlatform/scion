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
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
)

// nonRouteEvidence is the verified evidence and pending list
// checkRowsEnforcedOrReserved applies to rows without Enforcement.
type nonRouteEvidence struct {
	Verified map[string]bool   // row ID -> has route or call-site evidence
	Pending  map[string]string // row ID -> reason (pendingNonRouteRows)
}

// loadNonRouteEvidence verifies route evidence and every nonRouteCallSites
// entry against the pkg/hub sources, failing the test on a call site that
// does not reference its permission.
func loadNonRouteEvidence(t *testing.T) nonRouteEvidence {
	t.Helper()
	verified := routeGuardPermissionIDs(routeMetadataTable)
	consts := loadReferenceConstants(t)
	sites, problems := verifyCallSites(".", permissions.Registry, nonRouteCallSites, consts)
	if len(problems) > 0 {
		t.Fatalf("permission call sites not verified:\n  %s", strings.Join(problems, "\n  "))
	}
	for id := range sites {
		verified[id] = true
	}
	return nonRouteEvidence{Verified: verified, Pending: pendingNonRouteRows}
}

// loadReferenceConstants reads the package-level string constants of
// pkg/hub, pkg/hub/permissions and pkg/store from their non-test sources.
func loadReferenceConstants(t *testing.T) referenceConstants {
	t.Helper()
	out := referenceConstants{}
	for _, dir := range []struct{ path, prefix string }{
		{".", ""},
		{"permissions", "permissions."},
		{filepath.Join("..", "store"), "store."},
	} {
		files, err := parseNonTestGoFiles(dir.path)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range files {
			for _, decl := range f.Decls {
				gd, ok := decl.(*ast.GenDecl)
				if !ok || gd.Tok != token.CONST {
					continue
				}
				for _, spec := range gd.Specs {
					vs := spec.(*ast.ValueSpec)
					for i, name := range vs.Names {
						if i >= len(vs.Values) {
							continue
						}
						if v, ok := stringLit(vs.Values[i]); ok {
							out[dir.prefix+name.Name] = v
						}
					}
				}
			}
		}
	}
	return out
}

// pendingNonRouteRows lists rows declared through NonRouteUse that no call
// site checks yet, each with its reason and follow-up issue. It is a
// ratchet: a new NonRouteUse row without a verified call site fails unless
// it is added here, and an entry fails once its row gains Enforcement, a
// verified call site or a Reserved mark, so the entry must be removed.
var pendingNonRouteRows = map[string]string{
	"hub.settings.read":       "held by built-in roles; the only hub settings read route, GET /api/v1/hub/settings/injected-skills, is open to any authenticated user by design (ptone/scion#4171)",
	"hub.scheduler.update":    "held by built-in roles; no route checks it: /api/v1/admin/scheduler is GET-only (ptone/scion#4171)",
	"secret.deliver":          "decision rules exist; no production path requests it yet (ptone/scion#4171)",
	"env_var.deliver":         "decision rules exist; no production path requests it yet (ptone/scion#4171)",
	"skill_injection.deliver": "decision rules exist; no production path requests it yet (ptone/scion#4171)",
	"gcp_service_account.use": "decision rules exist; token mint checks the per-account token scope (ptone/scion#4171)",
}

// parseNonTestGoFiles parses the non-test Go files in dir.
func parseNonTestGoFiles(dir string) ([]*ast.File, error) {
	// pkgmove:scan-covers pkg/hub/apierr
	// unaffected: guards permission ID references, which errors.go and json_response.go do not contain.
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}
	fset := token.NewFileSet()
	var out []*ast.File
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", name, err)
		}
		out = append(out, f)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no Go files found in %s", dir)
	}
	return out, nil
}

// nonRouteCallSites maps a registry row ID to the functions that check it.
var nonRouteCallSites = map[string][]permissionCallSite{
	"agent.message": {
		{"authorize_message.go", "AuthzService.uatMessageGate"},
		{"authorize_message.go", "AuthzService.messageAncestorProjectAccess"},
		{"authorize_message.go", "Server.authorizeUserToAgent"},
	},
	"hub.auth_reset.execute": {{"agent_scope_reissue.go", "Server.authorizeScopeReissue"}},
	"hub.admin_mode.update":  {{"admin_mode.go", "Server.handleAdminMaintenance"}},
	"hub.allow_list.update": {
		{"admin_allow_list.go", "Server.handleAdminAllowList"},
		{"admin_allow_list.go", "Server.handleAdminAllowListByEmail"},
	},
	"hub.audit.read": {
		{"audit_authz.go", "Server.handleAuthzExplain"},
		{"handlers_admin_effective_access.go", "Server.handleAdminEffectiveAccess"},
		{"handlers_access_constraints.go", "Server.canReadConstraintAudit"},
	},
	"project.clone": {{"project_clone.go", "Server.handleProjectClone"}},
	"project.list": {
		{"handlers_projects_core.go", "Server.listProjects"},
		{"skill_handlers.go", "Server.listSkills"},
	},
	"agent.status_update": {{"handlers_agent_lifecycle.go", "Server.updateAgentStatus"}},
	"project.secret_read": {{"material_runtime.go", "Server.materialRuntimePrecheck"}},
	"agent.notify":        {{"handlers_notifications.go", "checkAgentNotifyScope"}},
	"agent.token_refresh": {
		{"handlers_agents_core.go", "Server.handleAgentTokenRefresh"},
		{"handlers_agent_messaging.go", "Server.handleAgentGitHubTokenRefresh"},
	},
	"agent.port_forward": {
		{"port_forward_handlers.go", "Server.authorizePortRegistration"},
		{"conduit_relay.go", "Server.handleConduit"},
	},
	"agent.identity_token": {{"handlers_oidc.go", "Server.handleAgentIdentityToken"}},
}

// routeGuardPermissionIDs returns the permissions the RouteHubAdmin guard
// decides for some route in table.
func routeGuardPermissionIDs(table map[string]RouteMetadata) map[string]bool {
	out := map[string]bool{}
	for _, meta := range table {
		if meta.Classification == RouteHubAdmin && meta.Permission != "" {
			out[meta.Permission] = true
		}
	}
	return out
}

// referenceConstants maps a constant as written in pkg/hub source ("Name"
// for pkg/hub, "permissions.Name" or "store.Name") to its string value.
type referenceConstants map[string]string

func stringLit(e ast.Expr) (string, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	v, err := strconv.Unquote(lit.Value)
	return v, err == nil
}

// verifyCallSites checks each call site in sites (files relative to dir)
// against registry. It returns the row IDs with at least one verified call
// site and a problem for every call site that is missing or does not
// reference its permission, and for every ID not in registry. A row is
// verified only when all of its call sites are.
func verifyCallSites(dir string, registry []permissions.Permission, sites map[string][]permissionCallSite, consts referenceConstants) (map[string]bool, []string) {
	rows := map[string]permissions.Permission{}
	for _, p := range registry {
		rows[p.ID] = p
	}
	parsed := map[string]*ast.File{}
	verified := map[string]bool{}
	var problems []string
	ids := make([]string, 0, len(sites))
	for id := range sites {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		row, ok := rows[id]
		if !ok {
			problems = append(problems, id+": call site entry for a permission not in the registry")
			continue
		}
		if len(sites[id]) == 0 {
			problems = append(problems, id+": call site entry names no function")
			continue
		}
		want := map[string]bool{row.ID: true}
		if row.UATScope != "" {
			want[row.UATScope] = true
		}
		for _, s := range row.AgentScopes {
			want[s] = true
		}
		ok = true
		for _, site := range sites[id] {
			f, seen := parsed[site.File]
			if !seen {
				var err error
				f, err = parser.ParseFile(token.NewFileSet(), filepath.Join(dir, site.File), nil, 0)
				if err != nil {
					problems = append(problems, fmt.Sprintf("%s: call site %s: %v", id, site.File, err))
					ok = false
					continue
				}
				parsed[site.File] = f
			}
			fd := findFuncDecl(f, site.Func)
			if fd == nil || fd.Body == nil {
				problems = append(problems, fmt.Sprintf("%s: call site %s:%s not found", id, site.File, site.Func))
				ok = false
				continue
			}
			if !bodyReferences(fd.Body, want, consts) {
				problems = append(problems, fmt.Sprintf("%s: call site %s:%s does not reference the permission (want one of %s)", id, site.File, site.Func, strings.Join(sortedWantKeys(want), ", ")))
				ok = false
			}
		}
		if ok {
			verified[id] = true
		}
	}
	return verified, problems
}

// bodyReferences reports whether body contains a string literal or a known
// constant whose value is in want.
func bodyReferences(body *ast.BlockStmt, want map[string]bool, consts referenceConstants) bool {
	found := false
	var visit func(n ast.Node) bool
	visit = func(n ast.Node) bool {
		if found {
			return false
		}
		switch x := n.(type) {
		case *ast.BasicLit:
			if v, ok := stringLit(x); ok && want[v] {
				found = true
			}
		case *ast.Ident:
			if v, ok := consts[x.Name]; ok && want[v] {
				found = true
			}
		case *ast.SelectorExpr:
			if pkg, ok := x.X.(*ast.Ident); ok && (pkg.Name == "permissions" || pkg.Name == "store") {
				if v, ok := consts[pkg.Name+"."+x.Sel.Name]; ok && want[v] {
					found = true
				}
				return false
			}
			// A field or method name is not a pkg/hub constant: visit
			// only the operand.
			ast.Inspect(x.X, visit)
			return false
		}
		return !found
	}
	ast.Inspect(body, visit)
	return found
}

// findFuncDecl returns the function named name ("Name" or "Recv.Name") in f.
func findFuncDecl(f *ast.File, name string) *ast.FuncDecl {
	recv, fn, isMethod := strings.Cut(name, ".")
	if !isMethod {
		fn, recv = recv, ""
	}
	for _, decl := range f.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Name.Name != fn {
			continue
		}
		if recvTypeName(fd) == recv {
			return fd
		}
	}
	return nil
}

func sortedWantKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// recvTypeName returns the receiver's type name without the pointer star
// or type parameters (T, *T, T[K] and *T[K, V] all give "T"), or "" for a
// plain function. A receiver shape it does not know gives "?", which no
// call site names, so such a method is never taken for a plain function.
func recvTypeName(fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return ""
	}
	t := fd.Recv.List[0].Type
	if star, ok := t.(*ast.StarExpr); ok {
		t = star.X
	}
	switch x := t.(type) {
	case *ast.IndexExpr:
		t = x.X
	case *ast.IndexListExpr:
		t = x.X
	}
	if id, ok := t.(*ast.Ident); ok {
		return id.Name
	}
	return "?"
}

// permissionCallSite names one function that checks a permission. File is
// relative to pkg/hub. Func is a function name, or "Recv.Name" for a method
// (Recv without the pointer star or type parameters).
type permissionCallSite struct {
	File string
	Func string
}
