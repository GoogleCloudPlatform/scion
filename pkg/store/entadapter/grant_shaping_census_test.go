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
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Grant shaping census (ptone/scion#4240, admin tier A3).
//
// TestGrantShapingBuilderCensus scans every non-test, non-generated Go file
// of the module for writes to a grant table: an ent create, update or
// upsert builder on RoleBinding, GroupMembership, RoleDefinition,
// InviteCode or BrokerJoinToken, a child-group edge write (AddChildGroupIDs,
// AddParentGroupIDs and their entity forms), and raw SQL that inserts into
// or updates one of those tables or group_child_groups. Every function
// holding one must call grantShapingFor, where the store applies the
// shaping and the fail-closed refusal together, or be listed in
// grantShapingCensusExempt with the reason it is not a grant an admin-kind
// test identity can make. The sites are discovered, not counted: a new
// write path fails until it is shaped or classified, and a stale exemption
// fails too.

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

// grantShapingTables are the entity names of the grant tables.
var grantShapingTables = []string{"RoleBinding", "GroupMembership", "RoleDefinition", "InviteCode", "BrokerJoinToken"}

var (
	grantBuilderRE = regexp.MustCompile(`\.(` + strings.Join(grantShapingTables, "|") + `)\.(Create|CreateBulk|MapCreateBulk|Update|UpdateOne|UpdateOneID)\(`)
	grantEdgeRE    = regexp.MustCompile(`\.(AddChildGroupIDs|AddChildGroups|AddParentGroupIDs|AddParentGroups)\(`)
	grantSQLRE     = regexp.MustCompile(`(?i)(insert\s+(or\s+\w+\s+)?into|update)\s+"?(\w+\.)?(role_bindings|group_memberships|role_definitions|invite_codes|broker_join_tokens|group_child_groups)\b`)
)

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

// grantWriteSite is one function that writes a grant table.
type grantWriteSite struct {
	key    string // path:func
	shaped bool   // calls grantShapingFor
}

func scanGrantWriteSites(t *testing.T, root string) []grantWriteSite {
	t.Helper()
	var sites []grantWriteSite
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
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		if strings.HasPrefix(rel, "pkg/ent/") && !strings.HasPrefix(rel, "pkg/ent/entc/") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !grantBuilderRE.Match(src) && !grantEdgeRE.Match(src) && !grantSQLRE.Match(src) {
			return nil
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, src, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			body := string(src[fset.Position(fn.Body.Pos()).Offset:fset.Position(fn.Body.End()).Offset])
			if !grantBuilderRE.MatchString(body) && !grantEdgeRE.MatchString(body) && !grantSQLRE.MatchString(body) {
				continue
			}
			sites = append(sites, grantWriteSite{
				key:    rel + ":" + fn.Name.Name,
				shaped: strings.Contains(body, "grantShapingFor(") || strings.Contains(body, "shapeJoinToken("),
			})
		}
		return nil
	})
	require.NoError(t, err)
	return sites
}

func grantCensusExempt(key string) (string, bool) {
	if r, ok := grantShapingCensusExempt[key]; ok {
		return r, true
	}
	for pat, r := range grantShapingCensusExempt {
		if strings.HasSuffix(pat, "*") && strings.HasPrefix(key, strings.TrimSuffix(pat, "*")) {
			return r, true
		}
	}
	return "", false
}

func TestGrantShapingBuilderCensus(t *testing.T) {
	root := moduleRoot(t)
	sites := scanGrantWriteSites(t, root)
	require.NotEmpty(t, sites, "the scan finds the grant writes")

	var unshaped []string
	usedExempt := map[string]bool{}
	shaped := map[string]bool{}
	for _, s := range sites {
		if s.shaped {
			shaped[s.key] = true
			continue
		}
		if _, ok := grantCensusExempt(s.key); ok {
			for pat := range grantShapingCensusExempt {
				if pat == s.key || (strings.HasSuffix(pat, "*") && strings.HasPrefix(s.key, strings.TrimSuffix(pat, "*"))) {
					usedExempt[pat] = true
				}
			}
			continue
		}
		unshaped = append(unshaped, s.key)
	}
	sort.Strings(unshaped)
	assert.Empty(t, unshaped, "grant table writes without grant shaping: call grantShapingFor in the function, or classify it in grantShapingCensusExempt")

	// The known shaped sites are found (the scan works), including every
	// write path named in grant_shaping.go.
	for _, want := range []string{
		"pkg/store/entadapter/role_store.go:createRoleBinding",
		"pkg/store/entadapter/role_store.go:CreateRoleDefinition",
		"pkg/store/entadapter/role_store.go:UpdateRoleDefinition",
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
	// No stale exemption: each names a site that still writes a grant
	// table without shaping.
	for pat := range grantShapingCensusExempt {
		assert.True(t, usedExempt[pat], "stale grant shaping exemption %q", pat)
	}
}

// The census regexes catch each form of grant write (proof the scan is
// not vacuous).
func TestGrantShapingBuilderCensus_Detects(t *testing.T) {
	for _, src := range []string{
		`c.client.RoleBinding.Create().SetPrincipalID(x)`,
		`tx.GroupMembership.UpdateOneID(id).SetRole(r)`,
		`c.client.RoleDefinition.CreateBulk(b...)`,
		`client.InviteCode.Update().Where(p)`,
		`client.BrokerJoinToken.Create()`,
	} {
		assert.True(t, grantBuilderRE.MatchString(src), src)
	}
	assert.True(t, grantEdgeRE.MatchString(`s.client.Group.UpdateOneID(g).AddChildGroupIDs(m)`))
	for _, src := range []string{
		`INSERT INTO role_bindings (id) VALUES (?)`,
		`update group_memberships set role = 'owner'`,
		`INSERT INTO group_child_groups (group_id, parent_group_id)`,
		`INSERT OR IGNORE INTO group_memberships (id, role)`,
		`INSERT INTO main.group_memberships (id, group_id)`,
	} {
		assert.True(t, grantSQLRE.MatchString(src), src)
	}
	assert.False(t, grantSQLRE.MatchString(`DELETE FROM role_bindings WHERE id = ?`), "a delete confers nothing")
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
