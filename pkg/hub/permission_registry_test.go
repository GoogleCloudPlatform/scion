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
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func TestPermissionRegistryEntriesDeclareCurrentUse(t *testing.T) {
	ev := loadNonRouteEvidence(t)
	ids := map[string]bool{}
	for _, permission := range permissions.Registry {
		if permission.ID == "" {
			t.Fatal("registry permission with empty ID")
		}
		if ids[permission.ID] {
			t.Fatalf("duplicate permission ID %q", permission.ID)
		}
		ids[permission.ID] = true
		if permission.Resource == "" {
			t.Fatalf("%s has empty resource", permission.ID)
		}
		if permission.Action == "" {
			t.Fatalf("%s has empty action", permission.ID)
		}
		_, pending := ev.Pending[permission.ID]
		if len(permission.Enforcement) == 0 && !ev.Verified[permission.ID] && !pending && !permission.IsReserved() {
			t.Fatalf("%s must declare route enforcement, a verified call site (nonRouteCallSites), or Reserved", permission.ID)
		}
		for _, enforcement := range permission.Enforcement {
			assertEnforcementReferenceExists(t, permission.ID, enforcement)
		}
	}
}

// TestPermissionRegistryRowsEnforcedOrReserved requires every registry row
// to be exactly one of: used (Enforcement, or route or call-site evidence;
// see nonRouteCallSites) or Reserved. NonRouteUse text alone is not a use:
// such a row must be in pendingNonRouteRows until it is decided
// (ptone/scion#4064). A row that is neither is a published permission
// nothing checks, and one
// that is both makes it unclear whether the check exists. Nothing may grant
// a reserved permission: no agent scope bundle, no built-in role, no manage
// alias and no scope picker list may carry it.
func TestPermissionRegistryRowsEnforcedOrReserved(t *testing.T) {
	content, err := os.ReadFile(webTokenListPath)
	if err != nil {
		t.Fatalf("read %s: %v", webTokenListPath, err)
	}
	aliases := map[string][]string{}
	for alias, resource := range permissions.UATManageAliases {
		aliases[alias] = permissions.UATManageScopesFor(resource)
	}
	surfaces := grantSurfaces{
		Roles:   BuiltInRoles(),
		Aliases: aliases,
		Pickers: map[string][]string{
			"UATScopeOptions":                     registryUATScopes(true),
			webTokenListPath + " FALLBACK_SCOPES": extractWebTokenScopes(t, webTokenListPath, string(content)),
		},
	}
	if err := checkRowsEnforcedOrReserved(permissions.Registry, surfaces, loadNonRouteEvidence(t)); err != nil {
		t.Fatal(err)
	}
}

const webTokenListPath = "../../web/src/components/shared/token-list.ts"

// grantSurfaces are the places a permission can be granted or offered:
// built-in roles (permission IDs), manage aliases (alias to UAT scopes) and
// scope picker lists (surface name to UAT scopes).
type grantSurfaces struct {
	Roles   []BuiltInRole
	Aliases map[string][]string
	Pickers map[string][]string
}

func checkRowsEnforcedOrReserved(registry []permissions.Permission, s grantSurfaces, ev nonRouteEvidence) error {
	var problems []string
	reserved := map[string]bool{}
	reservedScope := map[string]string{} // UAT scope -> reserved permission ID
	inRegistry := map[string]bool{}
	for _, p := range registry {
		inRegistry[p.ID] = true
		used := len(p.Enforcement) > 0 || ev.Verified[p.ID]
		reason, pending := ev.Pending[p.ID]
		switch {
		case p.Reserved != "" && !p.IsReserved():
			problems = append(problems, p.ID+": Reserved is blank; give the reason")
		case pending && (used || p.IsReserved()):
			problems = append(problems, p.ID+": in pendingNonRouteRows but now has Enforcement, a verified call site or Reserved; remove its pending entry")
		case pending && len(p.NonRouteUse) == 0:
			problems = append(problems, p.ID+": in pendingNonRouteRows but declares no NonRouteUse; remove its pending entry")
		case pending && !pendingReasonValid(reason):
			problems = append(problems, p.ID+": pending entry needs a reason and a ptone/scion# follow-up issue")
		case pending:
		case !used && !p.IsReserved() && len(p.NonRouteUse) > 0:
			problems = append(problems, p.ID+": declared through NonRouteUse only, without a verified call site; add it to nonRouteCallSites, mark it Reserved, or list it in pendingNonRouteRows")
		case !used && !p.IsReserved():
			problems = append(problems, p.ID+": no Enforcement or verified call site and not Reserved")
		case p.IsReserved() && (used || len(p.NonRouteUse) > 0):
			problems = append(problems, p.ID+": Reserved but also declares Enforcement, NonRouteUse or a verified call site")
		}
		if !p.IsReserved() {
			continue
		}
		reserved[p.ID] = true
		if p.UATScope != "" {
			reservedScope[p.UATScope] = p.ID
		}
		if len(p.AgentScopes) > 0 {
			problems = append(problems, fmt.Sprintf("%s: reserved permission is in agent scope bundle(s) %v", p.ID, p.AgentScopes))
		}
	}
	for _, role := range s.Roles {
		for _, id := range role.Permissions {
			if reserved[id] {
				problems = append(problems, fmt.Sprintf("role %s holds reserved permission %s", role.Name, id))
			}
		}
	}
	for _, alias := range sortedSurfaceKeys(s.Aliases) {
		for _, scope := range s.Aliases[alias] {
			if id, ok := reservedScope[scope]; ok {
				problems = append(problems, fmt.Sprintf("manage alias %s expands to %s (reserved permission %s)", alias, scope, id))
			}
		}
	}
	for _, picker := range sortedSurfaceKeys(s.Pickers) {
		for _, scope := range s.Pickers[picker] {
			if id, ok := reservedScope[scope]; ok {
				problems = append(problems, fmt.Sprintf("scope picker %s offers %s (reserved permission %s)", picker, scope, id))
			}
		}
	}
	for _, id := range sortedStringKeys(ev.Pending) {
		if !inRegistry[id] {
			problems = append(problems, id+": in pendingNonRouteRows but not in the registry; remove its pending entry")
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("permission registry rows must be enforced or reserved:\n  %s", strings.Join(problems, "\n  "))
	}
	return nil
}

// pendingReasonValid requires a pending entry to carry a reason and a
// follow-up issue reference.
func pendingReasonValid(reason string) bool {
	return strings.Contains(reason, "ptone/scion#") && strings.TrimSpace(reason) != ""
}

func sortedStringKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func sortedSurfaceKeys(m map[string][]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// TestCheckRowsEnforcedOrReserved_RejectsBadRows pins the checker against
// the mutations it exists to catch, one per rule and grant surface.
func TestCheckRowsEnforcedOrReserved_RejectsBadRows(t *testing.T) {
	enforced := permissions.Permission{ID: "x.read", UATScope: "x:read", Enforcement: []string{"pkg/hub/x.go"}}
	reserved := permissions.Permission{ID: "x.delete", UATScope: "x:delete", Reserved: "nothing checks it yet"}
	rows := []permissions.Permission{enforced, reserved}
	role := func(scope string, ids ...string) BuiltInRole {
		return BuiltInRole{Name: "role-" + scope, ScopeType: scope, Permissions: ids}
	}
	cases := []struct {
		name string
		rows []permissions.Permission
		s    grantSurfaces
		want string
	}{
		{"enforcement removed", []permissions.Permission{{ID: "x.read"}}, grantSurfaces{}, "x.read: no Enforcement"},
		{"reserved mark removed", []permissions.Permission{{ID: "x.delete"}}, grantSurfaces{}, "x.delete: no Enforcement"},
		{"blank reserved reason", []permissions.Permission{{ID: "x.delete", Reserved: "  "}}, grantSurfaces{}, "x.delete: Reserved is blank"},
		{"blank reserved reason on used row", []permissions.Permission{{ID: "x.read", Enforcement: []string{"pkg/hub/x.go"}, Reserved: " "}}, grantSurfaces{}, "x.read: Reserved is blank"},
		{"both", []permissions.Permission{{ID: "x.read", Enforcement: []string{"pkg/hub/x.go"}, Reserved: "r"}}, grantSurfaces{}, "x.read: Reserved but also"},
		{"agent scope bundle", []permissions.Permission{enforced, {ID: "x.delete", Reserved: "r", AgentScopes: []string{"project:x:write"}}}, grantSurfaces{}, "x.delete: reserved permission is in agent scope bundle"},
		{"project role", rows, grantSurfaces{Roles: []BuiltInRole{role(store.RoleScopeProject, "x.read", "x.delete")}}, "role role-project holds reserved permission x.delete"},
		{"system role", rows, grantSurfaces{Roles: []BuiltInRole{role(store.RoleScopeSystem, "x.delete")}}, "role role-system holds reserved permission x.delete"},
		{"manage alias", rows, grantSurfaces{Aliases: map[string][]string{"x:manage": {"x:read", "x:delete"}}}, "manage alias x:manage expands to x:delete"},
		{"scope picker", rows, grantSurfaces{Pickers: map[string][]string{"web": {"x:read", "x:delete"}}}, "scope picker web offers x:delete"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkRowsEnforcedOrReserved(tc.rows, tc.s, nonRouteEvidence{})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want an error containing %q", err, tc.want)
			}
		})
	}
	ok := grantSurfaces{
		Roles:   []BuiltInRole{role(store.RoleScopeProject, "x.read"), role(store.RoleScopeSystem, "x.read")},
		Aliases: map[string][]string{"x:manage": {"x:read"}},
		Pickers: map[string][]string{"web": {"x:read"}},
	}
	if err := checkRowsEnforcedOrReserved(rows, ok, nonRouteEvidence{}); err != nil {
		t.Fatalf("valid rows rejected: %v", err)
	}
}

// TestCheckRowsEnforcedOrReserved_NonRouteUseIsNotEvidence pins the
// ptone/scion#4064 rules: NonRouteUse text alone never satisfies the
// checker, a verified call site does, and the pending list is a ratchet in
// both directions.
func TestCheckRowsEnforcedOrReserved_NonRouteUseIsNotEvidence(t *testing.T) {
	const issue = "not yet checked (ptone/scion#1)"
	text := permissions.Permission{ID: "x.read", NonRouteUse: []string{"some handler"}}
	verified := map[string]bool{"x.read": true}
	pending := map[string]string{"x.read": issue}
	bad := []struct {
		name string
		rows []permissions.Permission
		ev   nonRouteEvidence
		want string
	}{
		{"free text only", []permissions.Permission{text}, nonRouteEvidence{}, "x.read: declared through NonRouteUse only"},
		{"pending row gains a call site", []permissions.Permission{text}, nonRouteEvidence{Verified: verified, Pending: pending}, "x.read: in pendingNonRouteRows but now has"},
		{"pending row gains Enforcement", []permissions.Permission{{ID: "x.read", NonRouteUse: []string{"h"}, Enforcement: []string{"pkg/hub/x.go"}}}, nonRouteEvidence{Pending: pending}, "x.read: in pendingNonRouteRows but now has"},
		{"pending row marked Reserved", []permissions.Permission{{ID: "x.read", Reserved: "r"}}, nonRouteEvidence{Pending: pending}, "x.read: in pendingNonRouteRows but now has"},
		{"pending row without NonRouteUse", []permissions.Permission{{ID: "x.read"}}, nonRouteEvidence{Pending: pending}, "x.read: in pendingNonRouteRows but declares no NonRouteUse"},
		{"pending entry without issue", []permissions.Permission{text}, nonRouteEvidence{Pending: map[string]string{"x.read": "later"}}, "x.read: pending entry needs a reason"},
		{"pending entry for unknown row", []permissions.Permission{{ID: "x.read", Enforcement: []string{"pkg/hub/x.go"}}}, nonRouteEvidence{Pending: map[string]string{"x.gone": issue}}, "x.gone: in pendingNonRouteRows but not in the registry"},
		{"reserved with free text", []permissions.Permission{{ID: "x.read", NonRouteUse: []string{"h"}, Reserved: "r"}}, nonRouteEvidence{}, "x.read: Reserved but also"},
		{"reserved with call site", []permissions.Permission{{ID: "x.read", Reserved: "r"}}, nonRouteEvidence{Verified: verified}, "x.read: Reserved but also"},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			err := checkRowsEnforcedOrReserved(tc.rows, grantSurfaces{}, tc.ev)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want an error containing %q", err, tc.want)
			}
		})
	}
	for name, ev := range map[string]nonRouteEvidence{
		"verified call site": {Verified: verified},
		"pending entry":      {Pending: pending},
	} {
		if err := checkRowsEnforcedOrReserved([]permissions.Permission{text}, grantSurfaces{}, ev); err != nil {
			t.Errorf("%s: valid row rejected: %v", name, err)
		}
	}
}

// TestArtifactPermissionsConsumedUnlessReserved closes the gap a shared
// dispatcher leaves: every artifact row names artifactHost.Authorize, which
// exists whether or not anything passes it that permission. So a non-reserved
// artifact row must be used, and a reserved row must be unused, so wiring
// one up forces clearing Reserved. A use is, in non-test files: the
// pkg/artifacts constant inside pkg/artifacts, an artifacts.Permission<X>
// selector in pkg/hub, or the permission ID as a string literal in either
// package. The constant declarations themselves do not count, nor do the
// role tables in pkg/hub/seed.go (BuiltInRoles and the *PermissionIDs
// functions): a role listing a permission is a grant, not a check, and
// TestPermissionRegistryRowsEnforcedOrReserved fails any role holding a
// reserved one. The rest of seed.go is scanned. The registry and
// applicability tables live in pkg/hub/permissions, which is not scanned.
func TestArtifactPermissionsConsumedUnlessReserved(t *testing.T) {
	constants := map[string]string{
		"PermissionRead":   artifacts.PermissionRead,
		"PermissionCreate": artifacts.PermissionCreate,
		"PermissionUpdate": artifacts.PermissionUpdate,
		"PermissionDelete": artifacts.PermissionDelete,
		"PermissionManage": artifacts.PermissionManage,
	}
	uses := artifactPermissionUses(t, constants, isSeedRoleTable)
	seen := map[string]bool{}
	for _, p := range permissions.Registry {
		if p.Resource != permissions.ResourceArtifact {
			continue
		}
		seen[p.ID] = true
		switch {
		case !p.IsReserved() && uses[p.ID] == 0:
			t.Errorf("%s is not Reserved but nothing in pkg/artifacts or pkg/hub uses it; mark it Reserved or wire the check", p.ID)
		case p.IsReserved() && uses[p.ID] > 0:
			t.Errorf("%s is Reserved but pkg/artifacts or pkg/hub uses it %d time(s); clear Reserved and record the check in Enforcement", p.ID, uses[p.ID])
		}
	}
	for name, id := range constants {
		if !seen[id] {
			t.Errorf("pkg/artifacts constant %s (%s) has no artifact row in the permission registry", name, id)
		}
	}
}

// isSeedRoleTable reports whether function fn in pkg/hub file is one of the
// built-in role tables in seed.go.
func isSeedRoleTable(file, fn string) bool {
	return file == "seed.go" && (fn == "BuiltInRoles" || strings.HasSuffix(fn, "PermissionIDs"))
}

// artifactPermissionUses counts uses of each artifact permission ID (see
// TestArtifactPermissionsConsumedUnlessReserved) across the non-test Go
// files of pkg/artifacts and pkg/hub. constants maps each pkg/artifacts
// constant name to its permission ID; skipHubFunc reports which pkg/hub
// function bodies (by file and function name) to skip.
func artifactPermissionUses(t *testing.T, constants map[string]string, skipHubFunc func(file, fn string) bool) map[string]int {
	t.Helper()
	ids := map[string]bool{}
	for _, id := range constants {
		ids[id] = true
	}
	counts := map[string]int{}
	countFile := func(file *ast.File, fileName string, inArtifacts bool) {
		// Identifiers and literals that make up a Permission constant's own
		// declaration are not uses, nor is anything in a skipped function.
		skip := map[ast.Node]bool{}
		ast.Inspect(file, func(n ast.Node) bool {
			if fd, ok := n.(*ast.FuncDecl); ok && !inArtifacts && skipHubFunc(fileName, fd.Name.Name) {
				skip[fd] = true
				return false
			}
			if vs, ok := n.(*ast.ValueSpec); ok {
				for i, name := range vs.Names {
					if _, isConst := constants[name.Name]; isConst && inArtifacts {
						skip[name] = true
						if i < len(vs.Values) {
							skip[vs.Values[i]] = true
						}
					}
				}
			}
			return true
		})
		ast.Inspect(file, func(n ast.Node) bool {
			if n == nil {
				return true
			}
			if skip[n] {
				return false
			}
			switch x := n.(type) {
			case *ast.SelectorExpr:
				if pkg, ok := x.X.(*ast.Ident); ok && pkg.Name == "artifacts" && !inArtifacts {
					if id, ok := constants[x.Sel.Name]; ok {
						counts[id]++
						return false
					}
				}
			case *ast.Ident:
				if id, ok := constants[x.Name]; ok && inArtifacts {
					counts[id]++
				}
			case *ast.BasicLit:
				if x.Kind == token.STRING {
					if v, err := strconv.Unquote(x.Value); err == nil && ids[v] {
						counts[v]++
					}
				}
			}
			return true
		})
	}
	for _, dir := range []struct {
		path        string
		inArtifacts bool
	}{{filepath.Join("..", "artifacts"), true}, {".", false}} {
		entries, err := os.ReadDir(dir.path)
		if err != nil {
			t.Fatalf("read %s: %v", dir.path, err)
		}
		fset := token.NewFileSet()
		files := 0
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			file, err := parser.ParseFile(fset, filepath.Join(dir.path, name), nil, 0)
			if err != nil {
				t.Fatalf("parse %s: %v", name, err)
			}
			files++
			countFile(file, name, dir.inArtifacts)
		}
		if files == 0 {
			t.Fatalf("no Go files found in %s", dir.path)
		}
	}
	return counts
}

func TestCapabilityActionMapsAreRegistryDerived(t *testing.T) {
	assertActionMapEqual(t, "resource actions", ResourceActions, permissions.ResourceActions())
	assertActionMapEqual(t, "scope actions", ScopeActions, permissions.ScopeActions())

	if !slices.Contains(ResourceActions["project"], ActionUpdate) {
		t.Fatal("project:update must appear in resource capabilities")
	}
	if !slices.Contains(ResourceActions["agent"], ActionPortAccess) {
		t.Fatal("agent:port_access must appear in resource capabilities")
	}
	for _, stale := range []Action{ActionStart, ActionStop, ActionMessage} {
		if slices.Contains(ResourceActions["agent"], stale) {
			t.Fatalf("agent:%s is not independently enforced and must not appear in capabilities", stale)
		}
	}
}

func TestUATScopesAreRegistryDerived(t *testing.T) {
	wantValid := permissions.UATValidScopes()
	assertStringBoolMapEqual(t, "UATValidScopes", store.UATValidScopes, wantValid)

	wantManage := permissions.UATManageScopes()
	gotManage := append([]string(nil), store.UATManageScopes...)
	sort.Strings(gotManage)
	if strings.Join(gotManage, "\n") != strings.Join(wantManage, "\n") {
		t.Fatalf("UATManageScopes drifted from registry\ngot:  %v\nwant: %v", gotManage, wantManage)
	}

	for _, stale := range []string{
		store.UATScopeAgentStart,
		store.UATScopeAgentStop,
		store.UATScopeAgentDispatch,
	} {
		if store.UATValidScopes[stale] {
			t.Fatalf("stale UAT scope %q must not be valid for new tokens", stale)
		}
		if slices.Contains(store.UATManageScopes, stale) {
			t.Fatalf("stale UAT scope %q must not be expanded by agent:manage", stale)
		}
	}
	for _, required := range []string{store.UATScopeProjectUpdate, store.UATScopeAgentPortAccess} {
		if !store.UATValidScopes[required] {
			t.Fatalf("valid UAT scope %q missing from registry-derived validation", required)
		}
	}
}

// TestAgentTokenScopesMapToRegistry pins the permission coverage of every
// agent token scope the registry uses. It is a guard, not a mirror: effect
// ceilings are frozen at write time (store.EffectCeiling) and a scope is
// issued only if the ceiling allows its whole coverage (ceilingAllowsScope),
// so adding a permission to an existing scope's list silently withdraws that
// scope from every agent and UAT ceiling frozen before the change. Give a new
// permission its own scope instead, made ceiling-optional in the role
// bundles (ceilingOptionalRoleScopes) when roles should carry it, as the
// artifact scopes are. Every scope in the registry must have a row here.
func TestAgentTokenScopesMapToRegistry(t *testing.T) {
	want := map[AgentTokenScope][]string{
		ScopeAgentStatusUpdate: {"agent.status_update"},
		ScopeAgentLogAppend:    {"agent.log_append"},
		// ptone/scion#2129 gives secret.use the same explicit AgentScopes
		// mapping as project.secret_read, so a runtime read and a project
		// secret-read decision share one token capability. No other
		// permission gains this mapping (TestMaterialPermissions_AgentScopeMappingExplicit).
		ScopeProjectSecretRead: {"project.secret_read", "secret.use"},
		ScopeAgentCreate:       {"agent.create"},
		ScopeAgentSAAssign:     {"gcp_service_account.assign"},
		ScopeAgentLifecycle:    {"agent.attach", "agent.delete", "agent.lifecycle"},
		ScopeAgentNotify:       {"agent.notify"},
		ScopeAgentTokenRefresh: {"agent.token_refresh"},
		ScopeAgentPortForward:  {"agent.port_forward"},
		ScopeIdentityToken:     {"agent.identity_token"},
		// #1494 added AgentScopes: ["project:read"] to template.read/list and
		// harness_config.read/list without updating this map, so the guard has
		// been failing on main since. The scope constant documents itself as
		// covering "agents, templates, skills, harness configs, projects", so the
		// widening is intended - it just was not recorded here.
		// ptone/scion#1968 adds skill.read/list (agents read skills).
		ScopeProjectRead: {
			"harness_config.list",
			"harness_config.read",
			"project.read",
			"skill.list",
			"skill.read",
			"template.list",
			"template.read",
		},
		// Write access to templates within the agent's own project.
		// Deliberately excludes template.delete - see the scope declaration.
		ScopeProjectTemplateWrite: {"template.create", "template.update"},
		// Publishing artifacts (and new versions) homed in the agent's own
		// project. Deliberately excludes artifact.delete and artifact.manage,
		// and the Reserved artifact.update (ptone/scion#3652).
		ScopeProjectArtifactWrite: {"artifact.create"},
		// Reading artifacts has its own ceiling-optional scope rather than
		// riding on project:read, so ceilings frozen before artifacts existed
		// keep admitting project:read (see ceilingOptionalRoleScopes).
		ScopeProjectArtifactRead: {"artifact.read"},
		ScopeAgentSetMessageMode: {"agent.set_message_mode"},
	}
	for scope, wantIDs := range want {
		gotIDs := registryPermissionIDsForAgentScope(string(scope))
		if strings.Join(gotIDs, "\n") != strings.Join(wantIDs, "\n") {
			t.Fatalf("agent token scope %q maps to wrong registry permissions\ngot:  %v\nwant: %v", scope, gotIDs, wantIDs)
		}
	}
	for _, permission := range permissions.Registry {
		for _, scope := range permission.AgentScopes {
			if _, pinned := want[AgentTokenScope(scope)]; !pinned {
				t.Fatalf("agent token scope %q (on %s) has no pinned coverage row in this test", scope, permission.ID)
			}
		}
	}
}

func TestTokenScopeSurfacesDoNotExposeStaleUATScopes(t *testing.T) {
	cliHelp := permissions.UATScopeHelp()
	for _, stale := range []string{"agent:start", "agent:stop", "agent:dispatch"} {
		if strings.Contains(cliHelp, stale) {
			t.Fatalf("generated CLI help still exposes stale UAT scope %q", stale)
		}
	}
	for _, required := range []string{"project:update", "agent:port_access"} {
		if !strings.Contains(cliHelp, required) {
			t.Fatalf("generated CLI help does not expose valid UAT scope %q", required)
		}
	}

	for _, path := range []string{webTokenListPath} {
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		got := extractWebTokenScopes(t, path, string(content))
		want := registryUATScopes(true)
		if strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Fatalf("%s FALLBACK_SCOPES drifted from registry\ngot:  %v\nwant: %v", path, got, want)
		}
	}
}

func assertActionMapEqual(t *testing.T, name string, got map[string][]Action, want map[string][]string) {
	t.Helper()
	gotString := map[string][]string{}
	for resource, actions := range got {
		for _, action := range actions {
			gotString[resource] = append(gotString[resource], string(action))
		}
	}
	for resource := range gotString {
		sort.Strings(gotString[resource])
	}
	for resource := range want {
		sort.Strings(want[resource])
	}
	if !stringSliceMapEqual(gotString, want) {
		t.Fatalf("%s drifted from registry\ngot:  %v\nwant: %v", name, gotString, want)
	}
}

func assertStringBoolMapEqual(t *testing.T, name string, got, want map[string]bool) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s length drifted from registry\ngot:  %v\nwant: %v", name, got, want)
	}
	for key, wantValue := range want {
		if got[key] != wantValue {
			t.Fatalf("%s[%q] = %v, want %v", name, key, got[key], wantValue)
		}
	}
}

func stringSliceMapEqual(a, b map[string][]string) bool {
	if len(a) != len(b) {
		return false
	}
	for key, aValues := range a {
		bValues, ok := b[key]
		if !ok || strings.Join(aValues, "\n") != strings.Join(bValues, "\n") {
			return false
		}
	}
	return true
}

func registryPermissionIDsForAgentScope(scope string) []string {
	var ids []string
	for _, permission := range permissions.Registry {
		if slices.Contains(permission.AgentScopes, scope) {
			ids = append(ids, permission.ID)
		}
	}
	sort.Strings(ids)
	return ids
}

func assertEnforcementReferenceExists(t *testing.T, permissionID, enforcement string) {
	t.Helper()

	fileRef, symbolRef, _ := strings.Cut(enforcement, ":")
	if fileRef == "" {
		t.Fatalf("%s has empty enforcement file reference %q", permissionID, enforcement)
	}
	path := filepath.Clean(filepath.Join("..", "..", fileRef))
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s enforcement reference %q points to missing file: %v", permissionID, enforcement, err)
	}
	if symbolRef != "" && !strings.Contains(string(content), symbolRef) {
		t.Fatalf("%s enforcement reference %q points to missing symbol %q", permissionID, enforcement, symbolRef)
	}
}

func extractWebTokenScopes(t *testing.T, path, content string) []string {
	t.Helper()

	start := strings.Index(content, "const FALLBACK_SCOPES: ScopeOption[] = [")
	if start < 0 {
		t.Fatalf("%s missing FALLBACK_SCOPES declaration", path)
	}
	// The array ends with "];\n" (no "as const" after the rename to typed ScopeOption[]).
	end := strings.Index(content[start:], "];")
	if end < 0 {
		t.Fatalf("%s missing FALLBACK_SCOPES terminator", path)
	}
	block := content[start : start+end]

	matches := regexp.MustCompile(`value:\s*'([^']+)'`).FindAllStringSubmatch(block, -1)
	if len(matches) == 0 {
		t.Fatalf("%s FALLBACK_SCOPES has no scope values", path)
	}
	scopes := make([]string, 0, len(matches))
	seen := map[string]bool{}
	for _, match := range matches {
		scope := match[1]
		if seen[scope] {
			t.Fatalf("%s FALLBACK_SCOPES contains duplicate scope %q", path, scope)
		}
		seen[scope] = true
		scopes = append(scopes, scope)
	}
	sort.Strings(scopes)
	return scopes
}

func registryUATScopes(includeAliases bool) []string {
	options := permissions.UATScopeOptions(includeAliases)
	scopes := make([]string, 0, len(options))
	for _, option := range options {
		scopes = append(scopes, option.UATScope)
	}
	sort.Strings(scopes)
	return scopes
}
