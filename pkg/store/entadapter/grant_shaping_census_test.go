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

package entadapter

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Grant shaping census (ptone/scion#4240, admin tier A3).
//
// TestGrantShapingBuilderCensus parses every non-test, non-generated Go file
// of the module (go/ast, not text) and finds every function that writes a
// grant table:
//
//   - an ent builder call <x>.<Table>.<Create|CreateBulk|MapCreateBulk|
//     Update|UpdateOne|UpdateOneID>(...) on RoleBinding, GroupMembership,
//     RoleDefinition, InviteCode or BrokerJoinToken, however the chain is
//     line-broken;
//   - an ent edge setter that attaches or re-points grant rows or
//     child-group edges (AddChildGroup(s|IDs), AddParentGroup(s|IDs),
//     AddMembership(s|IDs), AddRoleBinding(s|IDs)) on any builder;
//   - a SQL string literal that inserts, replaces or updates one of those
//     tables or group_child_groups (INSERT [OR x] INTO, REPLACE INTO,
//     UPDATE [ONLY], schema-qualified or quoted), or a SQL write keyword in a
//     function that also names such a table (a literal or the ent Table
//     constant: a table name assembled at runtime).
//
// Each such function must call grantShapingFor or shapeJoinToken (an actual
// call; a comment does not count), where the store applies the shaping and
// the fail-closed refusal together, or be listed in
// grantShapingCensusExempt with the reason it is not a grant an admin-kind
// test identity can make. A stale exemption fails. The reflective offline
// copy of migration beta (MethodByName("CreateBulk")) is outside any source
// census; it copies every column and is not a request path.

// grantShapingCensusExempt lists the functions ("<path from the module
// root>:<func>") that write a grant table without grant shaping, each with
// its reason.
var grantShapingCensusExempt = map[string]string{
	"pkg/store/entadapter/allowlist_store.go:IncrementInviteUseCount": "redeeming an invite counts a use; it confers nothing new",
	"pkg/store/entadapter/allowlist_store.go:RevokeInviteCode":        "revocation removes authority",
	"pkg/hub/server.go:runMembershipMigration":                        "startup migration of membership_kind markers, before any request is served",
	"pkg/ent/entc/migrate_beta.go:copyGroupChildEdges":                "offline database copy (migration beta): copies rows as they are, not a request path",
	"pkg/ent/entc/migrate_grove_to_project.go:mergeDuplicateGroups":   "one-time pre-project database migration, before any request is served; such databases predate issuance stamps",
	"pkg/ent/entc/migrate_alpha.go:copyGroupMemberships":              "one-time legacy database migration (migration alpha), before any request is served; legacy rows predate issuance stamps",
	"pkg/ent/entc/migrate_alpha.go:copyGroupChildEdgesSQL":            "one-time legacy database migration (migration alpha), before any request is served",
}

var (
	grantTables     = map[string]bool{"RoleBinding": true, "GroupMembership": true, "RoleDefinition": true, "InviteCode": true, "BrokerJoinToken": true}
	grantBuilderOps = map[string]bool{"Create": true, "CreateBulk": true, "MapCreateBulk": true, "Update": true, "UpdateOne": true, "UpdateOneID": true}
	grantEdgeOps    = map[string]bool{
		"AddChildGroupIDs": true, "AddChildGroups": true, "AddParentGroupIDs": true, "AddParentGroups": true,
		"AddMembershipIDs": true, "AddMemberships": true, "AddRoleBindingIDs": true, "AddRoleBindings": true,
	}
	// grantTablePkgs are the ent packages whose Table constants name a grant
	// table (group.ChildGroupsTable for the child-group edges).
	grantTablePkgs = map[string]bool{"rolebinding": true, "groupmembership": true, "roledefinition": true, "invitecode": true, "brokerjointoken": true}

	grantSQLTables  = `(role_bindings|group_memberships|role_definitions|invite_codes|broker_join_tokens|group_child_groups)`
	grantSQLWriteRE = regexp.MustCompile(`(?i)\b(?:(?:insert|replace)\s+(?:or\s+\w+\s+)?into|update(?:\s+only)?)\s+"?(?:\w+"?\.)?"?` + grantSQLTables + `\b`)
	// grantSQLKeywordRE is a SQL write keyword whose table is not a literal
	// in the same string (assembled at runtime).
	grantSQLKeywordRE = regexp.MustCompile(`(?i)\b(?:(?:insert|replace)\s+(?:or\s+\w+\s+)?into|update(?:\s+only)?)\s*(?:%[sv]|"?\s*$)`)
	grantSQLTableRE   = regexp.MustCompile(`(?i)^"?(?:\w+"?\.)?"?` + grantSQLTables + `"?$`)
)

// callName returns the called function or method name of call.
func callName(call *ast.CallExpr) string {
	switch f := call.Fun.(type) {
	case *ast.Ident:
		return f.Name
	case *ast.SelectorExpr:
		return f.Sel.Name
	}
	return ""
}

// stringLit returns the value of a string literal.
func stringLit(n ast.Node) (string, bool) {
	lit, ok := n.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	v, err := strconv.Unquote(lit.Value)
	if err != nil {
		return "", false
	}
	return v, true
}

// grantSite is one function that writes a grant table.
type grantSite struct {
	key    string // path:func
	shaped bool   // calls grantShapingFor or shapeJoinToken
}

// grantSitesInFile returns the grant-writing functions of one source file.
func grantSitesInFile(rel string, src []byte) ([]grantSite, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, rel, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	var out []grantSite
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		writes, shaped, keyword, namesTable := false, false, false, false
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.CallExpr:
				name := callName(x)
				if name == "grantShapingFor" || name == "shapeJoinToken" {
					shaped = true
				}
				if sel, ok := x.Fun.(*ast.SelectorExpr); ok {
					if grantBuilderOps[sel.Sel.Name] {
						if inner, ok := sel.X.(*ast.SelectorExpr); ok && grantTables[inner.Sel.Name] {
							writes = true
						}
					}
					if grantEdgeOps[sel.Sel.Name] {
						writes = true
					}
				}
			case *ast.SelectorExpr:
				if pkg, ok := x.X.(*ast.Ident); ok {
					if (grantTablePkgs[pkg.Name] && x.Sel.Name == "Table") || (pkg.Name == "group" && x.Sel.Name == "ChildGroupsTable") {
						namesTable = true
					}
				}
			case *ast.BasicLit:
				if v, ok := stringLit(x); ok {
					if grantSQLWriteRE.MatchString(v) {
						writes = true
					}
					if grantSQLKeywordRE.MatchString(v) {
						keyword = true
					}
					if grantSQLTableRE.MatchString(strings.TrimSpace(v)) {
						namesTable = true
					}
				}
			}
			return true
		})
		if keyword && namesTable {
			writes = true
		}
		if writes {
			out = append(out, grantSite{key: rel + ":" + fn.Name.Name, shaped: shaped})
		}
	}
	return out, nil
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	require.NoError(t, err)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		require.NotEqual(t, parent, dir, "module root not found")
		dir = parent
	}
}

// walkModuleGo calls visit for every non-test, non-generated Go file of the
// module (the entc hooks under pkg/ent are hand-written and included).
func walkModuleGo(t *testing.T, root string, visit func(rel string, src []byte)) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if rel == ".git" || rel == "web" || rel == "docs-site" || rel == "extras" || strings.HasSuffix(rel, "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(rel, ".go") || strings.HasSuffix(rel, "_test.go") {
			return nil
		}
		if strings.HasPrefix(rel, "pkg/ent/") && !strings.HasPrefix(rel, "pkg/ent/entc/") {
			return nil // generated
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		visit(rel, src)
		return nil
	})
	require.NoError(t, err)
}

func TestGrantShapingBuilderCensus(t *testing.T) {
	root := moduleRoot(t)
	var sites []grantSite
	walkModuleGo(t, root, func(rel string, src []byte) {
		got, err := grantSitesInFile(rel, src)
		require.NoError(t, err, rel)
		sites = append(sites, got...)
	})
	require.NotEmpty(t, sites, "the scan finds the grant writes")

	var unshaped []string
	used := map[string]bool{}
	shaped := map[string]bool{}
	for _, s := range sites {
		if s.shaped {
			shaped[s.key] = true
			continue
		}
		if _, ok := grantShapingCensusExempt[s.key]; ok {
			used[s.key] = true
			continue
		}
		unshaped = append(unshaped, s.key)
	}
	sort.Strings(unshaped)
	assert.Empty(t, unshaped, "grant table writes without grant shaping: call grantShapingFor in the function, or classify it in grantShapingCensusExempt")

	for _, want := range []string{
		"pkg/store/entadapter/role_store.go:createRoleBinding",
		"pkg/store/entadapter/role_store.go:CreateRoleDefinition",
		"pkg/store/entadapter/role_store.go:updateRoleDefinition",
		"pkg/store/entadapter/role_store.go:UpdateVisitorRoleBindingWindow",
		"pkg/store/entadapter/role_store.go:UpdateSystemRoleDefinitionPermissions",
		"pkg/store/entadapter/group_store.go:AddGroupMember",
		"pkg/store/entadapter/group_store.go:UpdateGroupMemberRole",
		"pkg/store/entadapter/group_store.go:CreateGroup",
		"pkg/store/entadapter/allowlist_store.go:CreateInviteCode",
		"pkg/store/entadapter/brokersecret_store.go:CreateJoinToken",
		"pkg/store/entadapter/brokersecret_store.go:upsertJoinToken",
	} {
		assert.True(t, shaped[want], "shaped grant write %s found", want)
	}
	for key := range grantShapingCensusExempt {
		assert.True(t, used[key], "stale grant shaping exemption %q", key)
	}
}

// TestGrantShapingBuilderCensus_Detects plants each write form the census
// must find (review R1) and checks that it is reported, and unshaped unless
// the function really calls grantShapingFor.
func TestGrantShapingBuilderCensus_Detects(t *testing.T) {
	cases := map[string]struct {
		body   string
		shaped bool
	}{
		"membership edge setter":          {body: `c.User.UpdateOneID(u).AddMembershipIDs(m).Exec(ctx)`},
		"membership edge entity setter":   {body: `c.Group.UpdateOneID(g).AddMemberships(m).Exec(ctx)`},
		"role binding edge setter":        {body: `c.RoleDefinition.UpdateOneID(r).AddRoleBindingIDs(b).Exec(ctx)`},
		"child group edge":                {body: `c.Group.UpdateOneID(g).AddChildGroupIDs(m).Exec(ctx)`},
		"line-broken builder chain":       {body: "c.RoleBinding.\n\t\tCreate().\n\t\tSetPrincipalID(x).\n\t\tExec(ctx)"},
		"bulk create":                     {body: `c.GroupMembership.CreateBulk(b...).Exec(ctx)`},
		"update one":                      {body: `c.InviteCode.UpdateOneID(id).SetNote(x).Exec(ctx)`},
		"comment does not shape":          {body: "// grantShapingFor(ctx, c) is called elsewhere\n\tc.BrokerJoinToken.Create().Exec(ctx)"},
		"insert into":                     {body: "db.Exec(`INSERT INTO role_bindings (id) VALUES (?)`)"},
		"insert or ignore, schema prefix": {body: "db.Exec(`INSERT OR IGNORE INTO main.group_memberships (id) VALUES (?)`)"},
		"replace into":                    {body: "db.Exec(`REPLACE INTO invite_codes (id) VALUES (?)`)"},
		"update only":                     {body: "db.Exec(`UPDATE ONLY role_definitions SET name = ?`)"},
		"quoted table":                    {body: "db.Exec(`UPDATE \"broker_join_tokens\" SET created_by = ?`)"},
		"table assembled at runtime":      {body: `db.Exec(fmt.Sprintf("INSERT INTO %s (id) VALUES (?)", rolebinding.Table))`},
		"table name in a variable":        {body: "t := \"group_child_groups\"\n\tdb.Exec(\"UPDATE \" + t + \" SET x = 1\")"},
		"really shaped":                   {body: "if _, err := grantShapingFor(ctx, c); err != nil {\n\t\treturn\n\t}\n\tc.RoleBinding.Create().Exec(ctx)", shaped: true},
	}
	for name, tc := range cases {
		src := "package x\n\nfunc planted() {\n\t" + tc.body + "\n}\n"
		sites, err := grantSitesInFile("planted.go", []byte(src))
		require.NoError(t, err, name)
		require.Len(t, sites, 1, "%s: the planted write is found", name)
		assert.Equal(t, tc.shaped, sites[0].shaped, name)
	}
	for name, body := range map[string]string{
		"delete":            "db.Exec(`DELETE FROM role_bindings WHERE id = ?`)",
		"read":              `c.RoleBinding.Query().All(ctx)`,
		"other table write": "db.Exec(`INSERT INTO agents (id) VALUES (?)`)",
	} {
		sites, err := grantSitesInFile("planted.go", []byte("package x\n\nfunc planted() {\n\t"+body+"\n}\n"))
		require.NoError(t, err, name)
		assert.Empty(t, sites, "%s confers nothing", name)
	}
}

// issuance_id is Immutable at the ORM layer: no update builder of a grant
// table can set or clear it, and group_memberships.expires_at cannot be
// changed after the row is written.
func TestGrantShaping_IssuanceIDImmutable(t *testing.T) {
	for _, typ := range []any{
		&ent.RoleBindingUpdate{}, &ent.RoleBindingUpdateOne{},
		&ent.GroupMembershipUpdate{}, &ent.GroupMembershipUpdateOne{},
		&ent.RoleDefinitionUpdate{}, &ent.RoleDefinitionUpdateOne{},
		&ent.InviteCodeUpdate{}, &ent.InviteCodeUpdateOne{},
		&ent.BrokerJoinTokenUpdate{}, &ent.BrokerJoinTokenUpdateOne{},
	} {
		rt := reflect.TypeOf(typ)
		for _, m := range []string{"SetIssuanceID", "SetNillableIssuanceID", "ClearIssuanceID"} {
			_, ok := rt.MethodByName(m)
			assert.False(t, ok, "%s has %s: issuance_id must be Immutable", rt, m)
		}
	}
	for _, typ := range []any{&ent.GroupMembershipUpdate{}, &ent.GroupMembershipUpdateOne{}} {
		rt := reflect.TypeOf(typ)
		for _, m := range []string{"SetExpiresAt", "SetNillableExpiresAt", "ClearExpiresAt"} {
			_, ok := rt.MethodByName(m)
			assert.False(t, ok, "%s has %s: a membership's expiry must be Immutable", rt, m)
		}
	}
	// Positive control: the create builders do have the setter.
	_, ok := reflect.TypeOf(&ent.RoleBindingCreate{}).MethodByName("SetNillableIssuanceID")
	assert.True(t, ok)
}
