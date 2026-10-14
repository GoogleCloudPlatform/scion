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
// (activeMemberships, groupMembershipActiveAt). This census scans every
// non-test, non-generated Go file of the module for a membership read (an
// ent GroupMembership query, a memberships edge traversal or eager load,
// raw SQL selecting from group_memberships) and fails unless its function
// applies the expiry filter or is classified below. The per-read behaviour
// is pinned by TestGrantShaping_GroupMembershipExpiry.
var membershipReadCensusExempt = map[string]string{
	"pkg/store/entadapter/group_store.go:UpdateGroupMemberRole":               "reads the row only to check its issuance stamp before a role change; confers nothing",
	"pkg/store/entadapter/issuance_grants_store.go:ListIssuanceStampedGrants": "the purge and residue listing of stamped rows, expired ones included; confers nothing",
	"pkg/ent/entc/migrate_grove_to_project.go:mergeDuplicateGroups":           "one-time pre-project database migration that copies memberships between groups, before any request is served",
}

var membershipReadRE = regexp.MustCompile(`\.GroupMembership\.Query\(|\.Query(Memberships|GroupMemberships)\(|\.With(Memberships|GroupMemberships)\(|(?i)from\s+"?group_memberships\b`)

func TestGroupMembershipReadCensus(t *testing.T) {
	root := moduleRoot(t)
	var unfiltered []string
	used := map[string]bool{}
	found := map[string]bool{}
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
		if !membershipReadRE.Match(src) {
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
			if !membershipReadRE.MatchString(body) {
				continue
			}
			key := rel + ":" + fn.Name.Name
			found[key] = true
			if strings.Contains(body, "activeMemberships(") || strings.Contains(body, "groupMembershipActiveAt(") {
				continue
			}
			if _, ok := membershipReadCensusExempt[key]; ok {
				used[key] = true
				continue
			}
			unfiltered = append(unfiltered, key)
		}
		return nil
	})
	require.NoError(t, err)
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
	assert.True(t, membershipReadRE.MatchString(`SELECT group_id FROM group_memberships WHERE user_id = $1`), "raw SQL reads are detected")
}
