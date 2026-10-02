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

//go:build !no_sqlite

package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// GET …/members?groupBy=principal (ptone/scion#2529 P2).
// =============================================================================

type groupedMembersBody struct {
	Items        []projectMemberGroup    `json:"items"`
	TotalCount   int                     `json:"totalCount"`
	Capabilities *MembershipCapabilities `json:"_capabilities"`
}

func getGroupedMembers(t *testing.T, f *mmrFixture, actor *store.User, query string) groupedMembersBody {
	t.Helper()
	rec := doRequestAsUser(t, f.srv, actor, http.MethodGet, "/api/v1/projects/"+f.projectID+"/members?groupBy=principal"+query, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var body groupedMembersBody
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	return body
}

// grpBind writes a project-scope binding directly to the store (fixture
// setup only; the tests below exercise the read path).
func grpBind(t *testing.T, s store.Store, principalType, principalID, roleDefID, projectID string) {
	t.Helper()
	_, err := s.CreateRoleBinding(context.Background(), &store.RoleBinding{
		RoleDefinitionID: roleDefID,
		PrincipalType:    principalType,
		PrincipalID:      principalID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)
}

// grpUser creates a hub user with the given display name.
func grpUser(t *testing.T, s store.Store, name, displayName string) *store.User {
	t.Helper()
	ctx := context.Background()
	id := tid(name)
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: id, Email: id + "@test.com", DisplayName: displayName, Role: "member", Status: "active",
	}))
	ensureHubMembership(ctx, s, id)
	u, err := s.GetUser(ctx, id)
	require.NoError(t, err)
	return u
}

func findGroup(items []projectMemberGroup, principalType, principalID string) *projectMemberGroup {
	for i := range items {
		if items[i].PrincipalType == principalType && items[i].PrincipalID == principalID {
			return &items[i]
		}
	}
	return nil
}

func TestProjectMembersGrouped_PrincipalWithThreeBindingsAppearsOnce(t *testing.T) {
	f := setupMMRFixture(t)
	ctx := context.Background()
	extra, err := f.store.CreateRoleDefinition(ctx, &store.RoleDefinition{
		Name: "aaa-grp-extra-" + tid(t.Name())[:8], ScopeType: store.RoleScopeProject, Permissions: []string{"project.read"},
	})
	require.NoError(t, err)
	grpBind(t, f.store, "user", f.member.ID, f.withinCeiling.ID, f.projectID)
	grpBind(t, f.store, "user", f.member.ID, extra.ID, f.projectID)

	body := getGroupedMembers(t, f, f.owner, "")

	// owner, admin, member: 3 principals, though 5 bindings.
	assert.Equal(t, 3, body.TotalCount, "totalCount counts principals, not bindings")
	require.Len(t, body.Items, 3)
	count := 0
	for _, g := range body.Items {
		if g.PrincipalType == "user" && g.PrincipalID == f.member.ID {
			count++
		}
	}
	assert.Equal(t, 1, count, "a principal with several bindings appears exactly once")

	g := findGroup(body.Items, "user", f.member.ID)
	require.NotNil(t, g)
	assert.Equal(t, store.ProjectRoleMember, g.BuiltInRoleName)
	require.Len(t, g.Bindings, 3)
	// Built-in first, then custom by role name.
	assert.Equal(t, store.ProjectRoleMember, g.Bindings[0].RoleName)
	assert.Equal(t, roleKindBuiltIn, g.Bindings[0].RoleKind)
	assert.Equal(t, extra.Name, g.Bindings[1].RoleName)
	assert.Equal(t, f.withinCeiling.Name, g.Bindings[2].RoleName)
	for _, b := range g.Bindings[1:] {
		assert.Equal(t, roleKindCustom, b.RoleKind)
		assert.Equal(t, "direct", b.Source)
		assert.Equal(t, f.member.ID, b.PrincipalID)
	}

	// The flat list still counts bindings.
	rec := doRequestAsUser(t, f.srv, f.owner, http.MethodGet, "/api/v1/projects/"+f.projectID+"/members", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var flat listProjectMembersResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &flat))
	assert.Equal(t, 5, flat.TotalCount)
}

func TestProjectMembersGrouped_PaginationNeverSplitsPrincipal(t *testing.T) {
	f := setupMMRFixture(t)
	const n = 150
	for i := 0; i < n; i++ {
		u := grpUser(t, f.store, fmt.Sprintf("%s-u%03d", t.Name(), i), fmt.Sprintf("User %03d", i))
		grpBind(t, f.store, "user", u.ID, f.memberRD.ID, f.projectID)
		grpBind(t, f.store, "user", u.ID, f.withinCeiling.ID, f.projectID)
	}
	// Plus the fixture's owner, admin and member.
	const wantPrincipals = n + 3

	page1 := getGroupedMembers(t, f, f.owner, "&limit=100")
	page2 := getGroupedMembers(t, f, f.owner, "&limit=100&offset=100")
	assert.Equal(t, wantPrincipals, page1.TotalCount)
	assert.Equal(t, wantPrincipals, page2.TotalCount)
	require.Len(t, page1.Items, 100)
	require.Len(t, page2.Items, wantPrincipals-100)

	seen := map[string]int{}
	for _, page := range [][]projectMemberGroup{page1.Items, page2.Items} {
		for _, g := range page {
			seen[g.PrincipalType+":"+g.PrincipalID]++
			want := 2
			if g.PrincipalID == f.owner.ID || g.PrincipalID == f.admin.ID || g.PrincipalID == f.member.ID {
				want = 1
			}
			assert.Len(t, g.Bindings, want, "principal %s must carry all its bindings on one page", g.PrincipalID)
			for _, b := range g.Bindings {
				assert.Equal(t, g.PrincipalID, b.PrincipalID)
			}
		}
	}
	assert.Len(t, seen, wantPrincipals, "every principal appears across the two pages")
	for k, c := range seen {
		assert.Equal(t, 1, c, "principal %s appears on exactly one page", k)
	}

	// Default limit is 100 principals.
	def := getGroupedMembers(t, f, f.owner, "")
	assert.Len(t, def.Items, 100)
	// An offset past the end yields an empty page, not an error.
	past := getGroupedMembers(t, f, f.owner, "&offset=1000")
	assert.Empty(t, past.Items)
	assert.NotNil(t, past.Items, "items is [] not null")
	assert.Equal(t, wantPrincipals, past.TotalCount)
}

func TestProjectMembersGrouped_DeterministicOrder(t *testing.T) {
	f := setupMMRFixture(t)
	// Members with display names out of order and mixed case.
	bob := grpUser(t, f.store, t.Name()+"-bob", "bob")
	alice := grpUser(t, f.store, t.Name()+"-alice", "Alice")
	twinA := grpUser(t, f.store, t.Name()+"-twin-a", "Twin")
	twinB := grpUser(t, f.store, t.Name()+"-twin-b", "Twin")
	customOnly := grpUser(t, f.store, t.Name()+"-custom", "Aardvark")
	for _, u := range []*store.User{bob, alice, twinA, twinB} {
		grpBind(t, f.store, "user", u.ID, f.memberRD.ID, f.projectID)
	}
	grpBind(t, f.store, "user", customOnly.ID, f.withinCeiling.ID, f.projectID)

	body := getGroupedMembers(t, f, f.owner, "")
	require.Len(t, body.Items, 8)

	// Tier: owner, admin, members..., custom-only last, despite "Aardvark"
	// sorting first by name.
	assert.Equal(t, f.owner.ID, body.Items[0].PrincipalID)
	assert.Equal(t, f.admin.ID, body.Items[1].PrincipalID)
	last := body.Items[len(body.Items)-1]
	assert.Equal(t, customOnly.ID, last.PrincipalID)
	assert.Equal(t, "", last.BuiltInRoleName, "a custom-only principal has no built-in role")

	// Within the member tier: case-insensitive display name, then ID.
	var memberTier []projectMemberGroup
	for _, g := range body.Items {
		if g.BuiltInRoleName == store.ProjectRoleMember {
			memberTier = append(memberTier, g)
		}
	}
	require.Len(t, memberTier, 5) // Alice, bob, Twin x2, f.member ("User")
	assert.True(t, sort.SliceIsSorted(memberTier, func(i, j int) bool {
		a, b := memberTier[i], memberTier[j]
		if la, lb := toLowerASCII(a.PrincipalDisplayName), toLowerASCII(b.PrincipalDisplayName); la != lb {
			return la < lb
		}
		return a.PrincipalID < b.PrincipalID
	}), "member tier is ordered by display name then principal ID: %+v", memberTier)
	assert.Equal(t, alice.ID, memberTier[0].PrincipalID, "Alice sorts before bob regardless of case")
	assert.Equal(t, bob.ID, memberTier[1].PrincipalID)
	assert.Equal(t, f.member.ID, memberTier[4].PrincipalID)
	twinIDs := []string{twinA.ID, twinB.ID}
	sort.Strings(twinIDs)
	assert.Equal(t, twinIDs, []string{memberTier[2].PrincipalID, memberTier[3].PrincipalID}, "equal display names tie-break on principal ID")

	// Stable across calls.
	for i := 0; i < 3; i++ {
		again := getGroupedMembers(t, f, f.owner, "")
		require.Len(t, again.Items, len(body.Items))
		for k := range body.Items {
			assert.Equal(t, body.Items[k].PrincipalID, again.Items[k].PrincipalID)
		}
	}
}

func toLowerASCII(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}

// legacyProjectMemberInfo is the per-binding list item shape from before
// ptone/scion#2529: P2 must leave that list byte-compatible, adding only
// roleKind.
type legacyProjectMemberInfo struct {
	store.RoleBinding
	RoleName             string `json:"roleName"`
	Source               string `json:"source"`
	PrincipalDisplayName string `json:"principalDisplayName,omitempty"`
	CreatedByDisplayName string `json:"createdByDisplayName,omitempty"`
}

func TestProjectMembersGrouped_NoGroupByIsByteCompatiblePlusRoleKind(t *testing.T) {
	f := setupMMRFixture(t)
	grpBind(t, f.store, "user", f.member.ID, f.withinCeiling.ID, f.projectID)

	rec := doRequestAsUser(t, f.srv, f.owner, http.MethodGet, "/api/v1/projects/"+f.projectID+"/members", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
	keys := make([]string, 0, len(raw))
	for k := range raw {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	assert.Equal(t, []string{"_capabilities", "items", "totalCount"}, keys)

	var total int
	require.NoError(t, json.Unmarshal(raw["totalCount"], &total))
	assert.Equal(t, 4, total, "flat list totalCount counts bindings")

	var items []map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw["items"], &items))
	require.Len(t, items, 4)

	ctx := context.Background()
	for _, item := range items {
		var kind string
		require.NoError(t, json.Unmarshal(item["roleKind"], &kind))
		assert.Contains(t, []string{roleKindBuiltIn, roleKindCustom}, kind)
		delete(item, "roleKind")
		withoutRoleKind, err := json.Marshal(item)
		require.NoError(t, err)

		var id string
		require.NoError(t, json.Unmarshal(item["id"], &id))
		b, err := f.store.GetRoleBinding(ctx, id)
		require.NoError(t, err)
		rd, err := f.store.GetRoleDefinition(ctx, b.RoleDefinitionID)
		require.NoError(t, err)
		legacy := legacyProjectMemberInfo{
			RoleBinding:          *b,
			RoleName:             rd.Name,
			Source:               "direct",
			PrincipalDisplayName: f.srv.resolveGroupMemberDisplayName(ctx, b.PrincipalType, b.PrincipalID),
			CreatedByDisplayName: f.srv.resolveGroupMemberDisplayName(ctx, store.GroupMemberTypeUser, b.CreatedBy),
		}
		want, err := json.Marshal(legacy)
		require.NoError(t, err)
		// Re-marshal both through a map so key order does not matter.
		assert.JSONEq(t, string(want), string(withoutRoleKind), "flat item minus roleKind equals the legacy item")
		assert.Equal(t, projectRoleKind(rd.Name), kind)
	}
}

func TestProjectMembersGrouped_InvalidGroupByRejected(t *testing.T) {
	f := setupMMRFixture(t)
	rec := doRequestAsUser(t, f.srv, f.owner, http.MethodGet, "/api/v1/projects/"+f.projectID+"/members?groupBy=role", nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), ErrCodeInvalidRequest)
}

func TestProjectMembersGrouped_ReadableByMemberWithCapabilities(t *testing.T) {
	f := setupMMRFixture(t)
	body := getGroupedMembers(t, f, f.member, "")
	assert.Equal(t, 3, body.TotalCount)
	require.NotNil(t, body.Capabilities)
	assert.False(t, body.Capabilities.CanManageMembers)
	assert.False(t, body.Capabilities.CanManageCustomRoles)

	ownerBody := getGroupedMembers(t, f, f.owner, "")
	require.NotNil(t, ownerBody.Capabilities)
	assert.True(t, ownerBody.Capabilities.CanManageCustomRoles)
}

// TestProjectMembersGrouped_HugeLimitDoesNotOverflow: a limit near
// math.MaxInt must not overflow offset+limit and panic the request.
func TestProjectMembersGrouped_HugeLimitDoesNotOverflow(t *testing.T) {
	f := setupMMRFixture(t)
	all := getGroupedMembers(t, f, f.owner, "")
	require.Greater(t, all.TotalCount, 1)

	body := getGroupedMembers(t, f, f.owner, "&limit=9223372036854775807&offset=1")
	assert.Equal(t, all.TotalCount, body.TotalCount)
	assert.Len(t, body.Items, all.TotalCount-1, "the page runs from offset to the end")
	assert.Equal(t, all.Items[1:], body.Items)
}

// TestProjectMembersGrouped_MatchesPutResponseGroup: the grouped GET and the
// PUT principal endpoint build a principal's group the same way.
func TestProjectMembersGrouped_MatchesPutResponseGroup(t *testing.T) {
	f := setupMMRFixture(t)
	target := grpUser(t, f.store, t.Name()+"-target", "Target")
	rec := putMemberRoles(t, f.srv, f.owner, f.projectID, "user", target.ID, []string{f.withinCeiling.ID, f.adminRD.ID}, &[]string{})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var put projectMemberGroupMutationResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &put))

	got := findGroup(getGroupedMembers(t, f, f.owner, "&limit=500").Items, "user", target.ID)
	require.NotNil(t, got)
	assert.Equal(t, put.BuiltInRoleName, got.BuiltInRoleName)
	assert.Equal(t, store.ProjectRoleAdmin, got.BuiltInRoleName)
	want, err := json.Marshal(put.projectMemberGroup)
	require.NoError(t, err)
	gotJSON, err := json.Marshal(got)
	require.NoError(t, err)
	assert.JSONEq(t, string(want), string(gotJSON))
}

// TestProjectMembersGrouped_BindingsCarryFlatListEnrichment: every binding
// in every grouped item, and every binding in the PUT principal response,
// is the flat-list item with the same id. The flat list is pinned to the
// legacy shape independently (NoGroupByIsByteCompatiblePlusRoleKind), so
// this guards the shared group builder's per-binding enrichment
// (principalDisplayName, createdByDisplayName, roleName, source, roleKind)
// for both the grouped GET and the PUT.
func TestProjectMembersGrouped_BindingsCarryFlatListEnrichment(t *testing.T) {
	f := setupMMRFixture(t)
	target := grpUser(t, f.store, t.Name()+"-target", "Target")
	rec := putMemberRoles(t, f.srv, f.owner, f.projectID, "user", target.ID, []string{f.withinCeiling.ID, f.adminRD.ID}, &[]string{})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var put projectMemberGroupMutationResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &put))

	rec = doRequestAsUser(t, f.srv, f.owner, http.MethodGet, "/api/v1/projects/"+f.projectID+"/members?limit=500", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var flat struct {
		Items []json.RawMessage `json:"items"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &flat))
	flatByID := make(map[string]string, len(flat.Items))
	for _, raw := range flat.Items {
		var item struct {
			ID string `json:"id"`
		}
		require.NoError(t, json.Unmarshal(raw, &item))
		flatByID[item.ID] = string(raw)
	}

	assertMatchesFlat := func(where string, bindings []projectMemberInfo) int {
		for _, b := range bindings {
			want, ok := flatByID[b.ID]
			require.True(t, ok, "%s: binding %s is in the flat list", where, b.ID)
			got, err := json.Marshal(b)
			require.NoError(t, err)
			assert.JSONEq(t, want, string(got), "%s: binding %s equals its flat-list item", where, b.ID)
		}
		return len(bindings)
	}

	grouped := getGroupedMembers(t, f, f.owner, "&limit=500")
	seen := 0
	for _, g := range grouped.Items {
		seen += assertMatchesFlat("grouped "+g.PrincipalType+":"+g.PrincipalID, g.Bindings)
	}
	assert.Equal(t, len(flat.Items), seen, "grouped bindings cover the flat list exactly")

	require.Len(t, put.Bindings, 2)
	assertMatchesFlat("PUT response", put.Bindings)

	// The enrichment under test is non-empty here, so a dropped field
	// cannot pass as an omitted empty one.
	for _, b := range put.Bindings {
		assert.Equal(t, "Target", b.PrincipalDisplayName)
		assert.Equal(t, "Owner", b.CreatedByDisplayName)
	}
}

// TestProjectMembersFlat_HugeLimitDoesNotOverflow: on the flat (per-binding)
// members list, a limit near math.MaxInt must not overflow offset+limit and
// fail the request (ptone/scion#2529, review r2 L-OVF).
func TestProjectMembersFlat_HugeLimitDoesNotOverflow(t *testing.T) {
	f := setupMMRFixture(t)
	path := "/api/v1/projects/" + f.projectID + "/members"
	getFlat := func(query string) listProjectMembersResponse {
		t.Helper()
		rec := doRequestAsUser(t, f.srv, f.owner, http.MethodGet, path+query, nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var body listProjectMembersResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		return body
	}

	all := getFlat("")
	require.Greater(t, all.TotalCount, 1)

	body := getFlat("?limit=9223372036854775807&offset=1")
	assert.Equal(t, all.TotalCount, body.TotalCount)
	assert.Len(t, body.Items, all.TotalCount-1, "the page runs from offset to the end")
	assert.Equal(t, all.Items[1:], body.Items)
}
