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
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// TestLeaveBindingClass_Table (R3, 6a) covers every row of the design's
// binding-kind table (3.3).
func TestLeaveBindingClass_Table(t *testing.T) {
	const (
		project = "11111111-1111-4111-8111-111111111111"
		other   = "22222222-2222-4222-8222-222222222222"
		caller  = "33333333-3333-4333-8333-333333333333"
		someone = "44444444-4444-4444-8444-444444444444"
	)
	no := false
	ruleID := "rule-1"
	issuance := "issuance-1"
	past, future := time.Now().Add(-time.Hour), time.Now().Add(time.Hour)

	sysRole := func(name string) *store.RoleDefinition {
		return &store.RoleDefinition{ID: "rd-" + name, Name: name, ScopeType: store.RoleScopeProject, System: true}
	}
	custom := &store.RoleDefinition{ID: "rd-custom", Name: "custom-deployers", ScopeType: store.RoleScopeProject}
	visitor := &store.RoleDefinition{ID: "rd-visitor", Name: store.ProjectRoleVisitor, ScopeType: store.RoleScopeProject, System: true, ConfersMembership: &no}
	nonMembership := &store.RoleDefinition{ID: "rd-nm", Name: "custom-no-membership", ScopeType: store.RoleScopeProject, ConfersMembership: &no}
	systemScoped := &store.RoleDefinition{ID: "rd-sys", Name: "custom-system-role", ScopeType: store.RoleScopeSystem}
	fakeOwner := &store.RoleDefinition{ID: "rd-fake-owner", Name: store.ProjectRoleOwner, ScopeType: store.RoleScopeProject}
	unknownSeeded := &store.RoleDefinition{ID: "rd-seeded", Name: "project-auditor", ScopeType: store.RoleScopeProject, System: true}

	direct := func(rd *store.RoleDefinition, mod func(*store.RoleBinding)) *store.RoleBinding {
		rb := &store.RoleBinding{
			ID: "rb-1", RoleDefinitionID: rd.ID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: caller,
			ScopeType: store.RoleScopeProject, ScopeID: project, CreatedBy: "test",
		}
		if mod != nil {
			mod(rb)
		}
		return rb
	}

	tests := []struct {
		name string
		rb   *store.RoleBinding
		rd   *store.RoleDefinition
		want leaveClass
	}{
		{"owner", direct(sysRole(store.ProjectRoleOwner), nil), sysRole(store.ProjectRoleOwner), leaveClassRemovable},
		{"admin", direct(sysRole(store.ProjectRoleAdmin), nil), sysRole(store.ProjectRoleAdmin), leaveClassRemovable},
		{"member", direct(sysRole(store.ProjectRoleMember), nil), sysRole(store.ProjectRoleMember), leaveClassRemovable},
		{"custom project role", direct(custom, nil), custom, leaveClassRemovable},
		{"expired", direct(custom, func(rb *store.RoleBinding) { rb.ExpiresAt = &past }), custom, leaveClassRemovable},
		{"scheduled", direct(custom, func(rb *store.RoleBinding) { rb.NotBefore = &future }), custom, leaveClassRemovable},
		{"issuance provenance", direct(custom, func(rb *store.RoleBinding) { rb.IssuanceID = &issuance }), custom, leaveClassRemovable},
		{"visitor role", direct(visitor, nil), visitor, leaveClassExcluded},
		{"role that confers no membership", direct(nonMembership, nil), nonMembership, leaveClassExcluded},
		{"rule-sourced", direct(custom, func(rb *store.RoleBinding) { rb.SourceRuleID = &ruleID }), custom, leaveClassExcluded},
		{"group principal", direct(custom, func(rb *store.RoleBinding) { rb.PrincipalType = store.RoleBindingPrincipalGroup }), custom, leaveClassExcluded},
		{"agent principal", direct(custom, func(rb *store.RoleBinding) { rb.PrincipalType = store.RoleBindingPrincipalAgent }), custom, leaveClassExcluded},
		{"system scope", direct(custom, func(rb *store.RoleBinding) { rb.ScopeType = store.RoleScopeSystem; rb.ScopeID = "" }), custom, leaveClassExcluded},
		{"another project", direct(custom, func(rb *store.RoleBinding) { rb.ScopeID = other }), custom, leaveClassExcluded},
		{"another user", direct(custom, func(rb *store.RoleBinding) { rb.PrincipalID = someone }), custom, leaveClassExcluded},
		{"unknown principal type", direct(custom, func(rb *store.RoleBinding) { rb.PrincipalType = "service" }), custom, leaveClassUnclassified},
		{"unknown scope type", direct(custom, func(rb *store.RoleBinding) { rb.ScopeType = "org" }), custom, leaveClassUnclassified},
		{"project binding of a system-scope role", direct(systemScoped, nil), systemScoped, leaveClassUnclassified},
		{"built-in name on a non-seeded role", direct(fakeOwner, nil), fakeOwner, leaveClassUnclassified},
		{"unknown seeded project role", direct(unknownSeeded, nil), unknownSeeded, leaveClassUnclassified},
		{"role definition mismatch", direct(custom, func(rb *store.RoleBinding) { rb.RoleDefinitionID = "rd-other" }), custom, leaveClassUnclassified},
		{"nil role", direct(custom, nil), nil, leaveClassUnclassified},
		{"nil binding", nil, custom, leaveClassUnclassified},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, leaveBindingClass(tc.rb, tc.rd, project, caller))
		})
	}
}

// Golden lists TestLeaveBindingClass_GuardUnclassified compares against:
// the shapes and constants leaveBindingClass was written for. A change
// here means leaveBindingClass must be reviewed for the new field,
// constant or role kind, and the golden list updated with it.
var (
	leaveClassGoldenRoleBindingFields    = []string{"CreatedAt", "CreatedBy", "ExpiresAt", "ID", "IssuanceID", "NotBefore", "PrincipalID", "PrincipalType", "RoleDefinitionID", "ScopeID", "ScopeType", "SourceRuleID"}
	leaveClassGoldenRoleDefinitionFields = []string{"ConfersMembership", "CreatedAt", "Description", "ID", "IssuanceID", "Name", "Permissions", "ScopeType", "System", "UpdatedAt"}
	leaveClassGoldenConstants            = map[string]string{
		"ProjectRoleAdmin":          store.ProjectRoleAdmin,
		"ProjectRoleMember":         store.ProjectRoleMember,
		"ProjectRoleOwner":          store.ProjectRoleOwner,
		"ProjectRoleVisitor":        store.ProjectRoleVisitor,
		"RoleBindingPrincipalAgent": store.RoleBindingPrincipalAgent,
		"RoleBindingPrincipalGroup": store.RoleBindingPrincipalGroup,
		"RoleBindingPrincipalUser":  store.RoleBindingPrincipalUser,
		"RoleScopeProject":          store.RoleScopeProject,
		"RoleScopeSystem":           store.RoleScopeSystem,
	}
	leaveClassGoldenBuiltInRoles = []string{store.ProjectRoleAdmin, store.ProjectRoleMember, store.ProjectRoleOwner}
)

// TestLeaveBindingClass_GuardUnclassified (R3, 6a) is a reflection guard:
// the fields of store.RoleBinding and store.RoleDefinition, the
// principal-type, scope-type and project-role constants of pkg/store, and
// the built-in project roles must equal the golden lists. A new field,
// constant or role kind fails this test until leaveBindingClass handles it
// and the golden list names it, so leave cannot silently drop a binding
// kind added later.
func TestLeaveBindingClass_GuardUnclassified(t *testing.T) {
	fields := func(v interface{}) []string {
		typ := reflect.TypeOf(v)
		out := make([]string, 0, typ.NumField())
		for i := 0; i < typ.NumField(); i++ {
			out = append(out, typ.Field(i).Name)
		}
		sort.Strings(out)
		return out
	}
	assert.Equal(t, leaveClassGoldenRoleBindingFields, fields(store.RoleBinding{}),
		"store.RoleBinding changed: classify the new field in leaveBindingClass, then update the golden list")
	assert.Equal(t, leaveClassGoldenRoleDefinitionFields, fields(store.RoleDefinition{}),
		"store.RoleDefinition changed: classify the new field in leaveBindingClass, then update the golden list")

	var builtIn []string
	for name := range store.BuiltInProjectMembershipRoles {
		builtIn = append(builtIn, name)
	}
	sort.Strings(builtIn)
	assert.Equal(t, leaveClassGoldenBuiltInRoles, builtIn,
		"the built-in project roles changed: classify the new role in leaveBindingClass, then update the golden list")

	// Every exported constant of pkg/store with a prefix leaveBindingClass
	// switches on.
	prefixes := []string{"RoleBindingPrincipal", "RoleScope", "ProjectRole"}
	dir := filepath.Join("..", "store")
	// scan-coverage:exempt reads pkg/store sources only, never pkg/hub.
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	fset := token.NewFileSet()
	found := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		require.NoError(t, err)
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, spec := range gd.Specs {
				vs := spec.(*ast.ValueSpec)
				for _, n := range vs.Names {
					for _, pre := range prefixes {
						if n.IsExported() && strings.HasPrefix(n.Name, pre) {
							found[n.Name] = true
						}
					}
				}
			}
		}
	}
	var got, want []string
	for n := range found {
		got = append(got, n)
	}
	for n := range leaveClassGoldenConstants {
		want = append(want, n)
	}
	sort.Strings(got)
	sort.Strings(want)
	assert.Equal(t, want, got,
		"a principal-type, scope-type or project-role constant changed in pkg/store: classify it in leaveBindingClass, then update the golden list")
}
