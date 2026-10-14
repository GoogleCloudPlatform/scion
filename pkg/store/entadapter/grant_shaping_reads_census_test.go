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
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// group_memberships.expires_at read side (ptone/scion#4240, admin tier A3;
// advisor requirement 3). Every read of group memberships in the module
// lives in this package's group store and drops expired memberships
// (activeMemberships, groupMembershipActiveAt). This census parses every
// non-test, non-generated Go file of the module (go/ast) and finds every
// function that reads memberships:
//
//   - an ent query <x>.GroupMembership.Query(), however line-broken;
//   - a memberships edge traversal, eager load or edge predicate on any
//     builder (QueryMemberships, QueryGroupMemberships, WithMemberships,
//     WithGroupMemberships, HasMemberships, HasMembershipsWith);
//   - SQL that reads group_memberships (FROM or JOIN, quoted or
//     schema-qualified), or FROM/JOIN with the table assembled at runtime
//     in a function that names it.
//
// Each such function must call activeMemberships or groupMembershipActiveAt
// (an actual call; a comment does not count), or be classified below. The
// per-read behaviour is pinned by TestGrantShaping_GroupMembershipExpiry.
var membershipReadCensusExempt = map[string]string{
	"pkg/store/entadapter/group_store.go:UpdateGroupMemberRole":               "reads the row only to check its issuance stamp before a role change; confers nothing",
	"pkg/store/entadapter/issuance_grants_store.go:ListIssuanceStampedGrants": "the purge and residue listing of stamped rows, expired ones included; confers nothing",
	"pkg/ent/entc/migrate_grove_to_project.go:mergeDuplicateGroups":           "one-time pre-project database migration that copies memberships between groups, before any request is served",
}

var (
	membershipEdgeReads = map[string]bool{
		"QueryMemberships": true, "QueryGroupMemberships": true, "WithMemberships": true, "WithGroupMemberships": true,
		"HasMemberships": true, "HasMembershipsWith": true,
	}
	membershipSQLReadRE    = regexp.MustCompile(`(?i)\b(?:from|join)\s+"?(?:\w+"?\.)?"?group_memberships\b`)
	membershipSQLKeywordRE = regexp.MustCompile(`(?i)\b(?:from|join)\s*(?:%[sv]|"?\s*$)`)
	membershipSQLTableRE   = regexp.MustCompile(`(?i)^"?(?:\w+"?\.)?"?group_memberships"?$`)
)

// membershipRead is one function that reads group memberships.
type membershipRead struct {
	key      string
	filtered bool // calls activeMemberships or groupMembershipActiveAt
}

// membershipReadsInFile returns the membership-reading functions of one
// source file.
func membershipReadsInFile(rel string, src []byte) ([]membershipRead, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, rel, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	var out []membershipRead
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		reads, filtered, keyword, namesTable := false, false, false, false
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.CallExpr:
				name := callName(x)
				if name == "activeMemberships" || name == "groupMembershipActiveAt" {
					filtered = true
				}
				if membershipEdgeReads[name] {
					reads = true
				}
				if sel, ok := x.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Query" {
					if inner, ok := sel.X.(*ast.SelectorExpr); ok && inner.Sel.Name == "GroupMembership" {
						reads = true
					}
				}
			case *ast.SelectorExpr:
				if pkg, ok := x.X.(*ast.Ident); ok && pkg.Name == "groupmembership" && x.Sel.Name == "Table" {
					namesTable = true
				}
			case *ast.BasicLit:
				if v, ok := stringLit(x); ok {
					if membershipSQLReadRE.MatchString(v) {
						reads = true
					}
					if membershipSQLKeywordRE.MatchString(v) {
						keyword = true
					}
					if membershipSQLTableRE.MatchString(strings.TrimSpace(v)) {
						namesTable = true
					}
				}
			}
			return true
		})
		if keyword && namesTable {
			reads = true
		}
		if reads {
			out = append(out, membershipRead{key: rel + ":" + fn.Name.Name, filtered: filtered})
		}
	}
	return out, nil
}

func TestGroupMembershipReadCensus(t *testing.T) {
	root := moduleRoot(t)
	var unfiltered []string
	used := map[string]bool{}
	found := map[string]bool{}
	walkModuleGo(t, root, func(rel string, src []byte) {
		reads, err := membershipReadsInFile(rel, src)
		require.NoError(t, err, rel)
		for _, r := range reads {
			found[r.key] = true
			if r.filtered {
				continue
			}
			if _, ok := membershipReadCensusExempt[r.key]; ok {
				used[r.key] = true
				continue
			}
			unfiltered = append(unfiltered, r.key)
		}
	})
	sort.Strings(unfiltered)
	assert.Empty(t, unfiltered, "group membership reads without the expiry filter: apply activeMemberships, or classify the read in membershipReadCensusExempt")
	for key := range membershipReadCensusExempt {
		assert.True(t, used[key], "stale membership read exemption %s", key)
	}
	for _, want := range []string{
		"pkg/store/entadapter/group_store.go:GetEffectiveGroups",
		"pkg/store/entadapter/group_store.go:GetEffectiveGroupsForAgent",
		"pkg/store/entadapter/group_store.go:GetUserGroups",
		"pkg/store/entadapter/group_store.go:GetGroupMembers",
		"pkg/store/entadapter/group_store.go:GetGroupMembership",
		"pkg/store/entadapter/group_store.go:CountGroupMembersByRole",
	} {
		assert.True(t, found[want], "membership read %s found", want)
	}
}

// TestGroupMembershipReadCensus_Detects plants each read form the census
// must find (review R1) and checks that it is reported, and unfiltered
// unless the function really calls activeMemberships.
func TestGroupMembershipReadCensus_Detects(t *testing.T) {
	cases := map[string]struct {
		body     string
		filtered bool
	}{
		"edge predicate":            {body: `c.Group.Query().Where(group.HasMembershipsWith(groupmembership.UserIDEQ(uid))).All(ctx)`},
		"edge predicate, no args":   {body: `c.User.Query().Where(user.HasMemberships()).All(ctx)`},
		"edge traversal":            {body: `c.Group.Query().QueryMemberships().All(ctx)`},
		"eager load":                {body: `c.Group.Query().WithMemberships().All(ctx)`},
		"line-broken query":         {body: "c.GroupMembership.\n\t\tQuery().\n\t\tAll(ctx)"},
		"comment does not filter":   {body: "// activeMemberships(ms) is applied by the caller\n\tc.GroupMembership.Query().All(ctx)"},
		"sql join":                  {body: "db.Query(`SELECT g.id FROM groups g JOIN group_memberships m ON m.group_id = g.id`)"},
		"sql from, quoted":          {body: "db.Query(`SELECT 1 FROM \"group_memberships\" WHERE user_id = ?`)"},
		"table assembled":           {body: `db.Query(fmt.Sprintf("SELECT 1 FROM %s", groupmembership.Table))`},
		"really filtered":           {body: "ms, _ := c.GroupMembership.Query().All(ctx)\n\t_ = s.activeMemberships(ms)", filtered: true},
		"filtered by the predicate": {body: "m, _ := c.GroupMembership.Query().Only(ctx)\n\t_ = groupMembershipActiveAt(m, now)", filtered: true},
	}
	for name, tc := range cases {
		src := "package x\n\nfunc planted() {\n\t" + tc.body + "\n}\n"
		reads, err := membershipReadsInFile("planted.go", []byte(src))
		require.NoError(t, err, name)
		require.Len(t, reads, 1, "%s: the planted read is found", name)
		assert.Equal(t, tc.filtered, reads[0].filtered, name)
	}
	reads, err := membershipReadsInFile("planted.go", []byte("package x\n\nfunc planted() {\n\tdb.Exec(`DELETE FROM agents WHERE id = ?`)\n}\n"))
	require.NoError(t, err)
	assert.Empty(t, reads, "another table is not a membership read")
}
