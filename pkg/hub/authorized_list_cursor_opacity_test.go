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
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ============================================================================
// ptone/scion#2124 / ptone/scion#2151: opaque authorizedList cursor coverage.
//
// list_cursor_seal_test.go covers listCursorSealer as a primitive. This file
// covers the two things that need the full authorizedList machinery (a real
// excluded-candidate scenario, and a real HTTP endpoint) to demonstrate:
//   - a sealed cursor contains no candidate ID or created time, and still
//     resumes at the next item the caller may see, both mid-batch and at
//     the scan-budget stop;
//   - the HTTP endpoints reject tampered, truncated, cross-query,
//     cross-caller and legacy-format cursors uniformly, while a fresh
//     first-page request is always unaffected;
//   - a cursor resumed after the caller's access changed returns no newly
//     denied items.
// ============================================================================

// ----------------------------------------------------------------------------
// Low-level: a sealed cursor contains no candidate ID or created time, and
// resumes at the next visible item.
// ----------------------------------------------------------------------------

// authorizedListOpacityTestItem is a minimal candidate carrying just enough
// to exercise the real authorizedListCursor encoding and a real per-ID
// visibility check.
type authorizedListOpacityTestItem struct {
	id      string
	created time.Time
	visible bool
}

// TestAuthorizedListCursor_MidBatchFill_SealedCursorIsOpaqueAndResumes covers
// a page that fills on a visible item followed immediately by excluded
// candidates: the sealed cursor carries no candidate ID or created time, and
// resumes at the next visible item with no duplicate and no gap.
func TestAuthorizedListCursor_MidBatchFill_SealedCursorIsOpaqueAndResumes(t *testing.T) {
	const binding = "templates:test-binding"
	sealer := mustNewListCursorSealer(t)
	base := time.Now()
	items := []authorizedListOpacityTestItem{
		{id: "11111111-1111-1111-1111-111111111111", created: base, visible: true},                        // V1 -- fills the page
		{id: "22222222-2222-2222-2222-222222222222", created: base.Add(-time.Second), visible: false},     // E1 -- excluded
		{id: "33333333-3333-3333-3333-333333333333", created: base.Add(-2 * time.Second), visible: false}, // E2 -- excluded, immediately precedes the overflow check
		{id: "44444444-4444-4444-4444-444444444444", created: base.Add(-3 * time.Second), visible: true},  // V2 -- next visible item
	}
	byID := make(map[string]authorizedListOpacityTestItem, len(items))
	for _, it := range items {
		byID[it.id] = it
	}
	fetch := authorizedListOpacityFetch(t, items, binding)
	resource := func(it *authorizedListOpacityTestItem) Resource { return Resource{Type: "test", ID: it.id} }
	cursorFor := func(it *authorizedListOpacityTestItem) string {
		return authorizedListCursor(it.created, it.id, binding)
	}
	read := func(_ context.Context, _ Identity, resources []Resource) ([]bool, error) {
		allowed := make([]bool, len(resources))
		for i, r := range resources {
			allowed[i] = byID[r.ID].visible
		}
		return allowed, nil
	}

	result, err := authorizedList(context.Background(), nil, "", 1, fetch, resource, cursorFor, read)
	require.NoError(t, err)
	require.Len(t, result.Items, 1)
	assert.Equal(t, items[0].id, result.Items[0].id)
	require.NotEmpty(t, result.NextCursor)

	sealed, err := sealer.Seal(result.NextCursor, binding)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(sealed, listCursorPrefix))
	assert.NotContains(t, sealed, items[2].id)
	assert.NotContains(t, sealed, items[2].created.Format(time.RFC3339Nano))
	rawSealed, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(sealed, listCursorPrefix))
	require.NoError(t, err)
	assert.NotContains(t, string(rawSealed), items[2].id)
	assertNoRecoverableCursorPayload(t, sealed, result.NextCursor)

	opened, err := sealer.Open(sealed, binding)
	require.NoError(t, err)
	assert.Equal(t, result.NextCursor, opened)

	// Resuming with the opened cursor must not re-return V1, must not
	// surface either excluded candidate, and must return exactly V2.
	resumed, err := authorizedList(context.Background(), nil, opened, 1, fetch, resource, cursorFor, read)
	require.NoError(t, err)
	require.Len(t, resumed.Items, 1)
	assert.Equal(t, items[3].id, resumed.Items[0].id)
}

// TestAuthorizedListCursor_BudgetStopEmptyPage_SealedCursorIsOpaqueAndResumes
// is the budget-stop twin: when every candidate up to
// authorizedListMaxCandidates is excluded, the visible page is empty, but a
// resume cursor is still returned so the caller can continue past the
// budget. The sealed cursor carries no candidate ID or created time, and
// resuming still continues correctly into the one visible item just past
// the cap.
func TestAuthorizedListCursor_BudgetStopEmptyPage_SealedCursorIsOpaqueAndResumes(t *testing.T) {
	const binding = "harness-configs:test-budget-binding"
	sealer := mustNewListCursorSealer(t)
	base := time.Now()
	n := authorizedListMaxCandidates + 1
	items := make([]authorizedListOpacityTestItem, n)
	for i := 0; i < n; i++ {
		items[i] = authorizedListOpacityTestItem{
			id:      fmt.Sprintf("99999999-0000-0000-0000-%012d", i),
			created: base.Add(-time.Duration(i) * time.Millisecond),
			visible: i == n-1, // only the very last (past the cap) item is visible
		}
	}
	byID := make(map[string]authorizedListOpacityTestItem, len(items))
	for _, it := range items {
		byID[it.id] = it
	}
	fetch := authorizedListOpacityFetch(t, items, binding)
	resource := func(it *authorizedListOpacityTestItem) Resource { return Resource{Type: "test", ID: it.id} }
	cursorFor := func(it *authorizedListOpacityTestItem) string {
		return authorizedListCursor(it.created, it.id, binding)
	}
	read := func(_ context.Context, _ Identity, resources []Resource) ([]bool, error) {
		allowed := make([]bool, len(resources))
		for i, r := range resources {
			allowed[i] = byID[r.ID].visible
		}
		return allowed, nil
	}

	result, err := authorizedList(context.Background(), nil, "", 50, fetch, resource, cursorFor, read)
	require.NoError(t, err, "crossing the scan budget must not fail the request")
	assert.Empty(t, result.Items, "every candidate up to the scan budget is denied, so the visible page is empty")
	require.NotEmpty(t, result.NextCursor, "a resume cursor is still returned so the caller can continue past the budget")

	lastScannedID := items[authorizedListMaxCandidates-1].id
	sealed, err := sealer.Seal(result.NextCursor, binding)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(sealed, listCursorPrefix))
	assert.NotContains(t, sealed, lastScannedID)
	assertNoRecoverableCursorPayload(t, sealed, result.NextCursor)

	opened, err := sealer.Open(sealed, binding)
	require.NoError(t, err)
	assert.Equal(t, result.NextCursor, opened)

	resumed, err := authorizedList(context.Background(), nil, opened, 50, fetch, resource, cursorFor, read)
	require.NoError(t, err)
	require.Len(t, resumed.Items, 1)
	assert.Equal(t, items[n-1].id, resumed.Items[0].id)
}

// authorizedListOpacityFetch builds a fetch closure over a fixed, ordered
// item slice: it resumes from a real authorizedListCursor-encoded cursor by
// decoding the ID it carries and looking up that item's position, exactly
// as the real store's keyset pagination would from the (created, id) pair
// -- but without a database.
func authorizedListOpacityFetch(t *testing.T, items []authorizedListOpacityTestItem, binding string) func(context.Context, string, int) (authorizedCandidatePage[authorizedListOpacityTestItem], error) {
	t.Helper()
	position := make(map[string]int, len(items))
	for i, it := range items {
		position[it.id] = i
	}
	return func(_ context.Context, cursor string, limit int) (authorizedCandidatePage[authorizedListOpacityTestItem], error) {
		start := 0
		if cursor != "" {
			raw, err := base64.URLEncoding.DecodeString(cursor)
			if err != nil {
				return authorizedCandidatePage[authorizedListOpacityTestItem]{}, err
			}
			parts := strings.SplitN(string(raw), ",", 3)
			require.Len(t, parts, 3)
			require.Equal(t, binding, parts[2])
			start = position[parts[1]] + 1
		}
		end := start + limit
		if end > len(items) {
			end = len(items)
		}
		next := ""
		if end < len(items) {
			last := items[end-1]
			next = authorizedListCursor(last.created, last.id, binding)
		}
		return authorizedCandidatePage[authorizedListOpacityTestItem]{Items: items[start:end], NextCursor: next}, nil
	}
}

// ----------------------------------------------------------------------------
// HTTP-level: tampering, truncation, legacy cursors and first-page requests.
// ----------------------------------------------------------------------------

// setupCursorOpacityTemplates seeds two user-scoped templates owned by carol
// (always visible to her, exercising authorizedList's per-item scan rather
// than the wide-access shortcut) with distinct Created timestamps so listing
// order is pinned.
func setupCursorOpacityTemplates(t *testing.T, s store.Store, carolID string) {
	t.Helper()
	base := time.Now()
	for i, name := range []string{"cursor-opacity-a", "cursor-opacity-b"} {
		tpl := &store.Template{
			ID:          api.NewUUID(),
			Name:        name,
			Slug:        api.Slugify(name),
			Scope:       store.TemplateScopeUser,
			ScopeID:     carolID,
			OwnerID:     carolID,
			Status:      "active",
			StoragePath: "templates/user/" + api.Slugify(name),
			Created:     base.Add(-time.Duration(i) * time.Second),
			Updated:     base.Add(-time.Duration(i) * time.Second),
		}
		require.NoError(t, s.CreateTemplate(context.Background(), tpl))
	}
}

// getTemplatesPage requests /api/v1/templates with the given raw query
// string as user, and returns the decoded response alongside the raw
// recorder (for status-code assertions).
func getTemplatesPage(t *testing.T, srv *Server, user *store.User, rawQuery string) (*httptest.ResponseRecorder, ListTemplatesResponse) {
	t.Helper()
	rec := doRequestAsUser(t, srv, user, http.MethodGet, "/api/v1/templates?"+rawQuery, nil)
	var resp ListTemplatesResponse
	if rec.Code == http.StatusOK {
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	}
	return rec, resp
}

func assertInvalidCursor(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	var errResp ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errResp))
	assert.Equal(t, ErrCodeInvalidCursor, errResp.Error.Code)
}

// A fresh first-page request (no cursor at all) always succeeds, regardless
// of anything that might be wrong with some other cursor -- the baseline
// every tampering/legacy test below is contrasted against.
func TestListTemplatesCursor_FreshFirstPageAlwaysWorks(t *testing.T) {
	srv, s, _, carol, _ := setupTemplateScopeTest(t)
	setupCursorOpacityTemplates(t, s, carol.ID)

	rec, resp := getTemplatesPage(t, srv, carol, "scope=user&limit=1")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Len(t, resp.Templates, 1)
	require.NotEmpty(t, resp.NextCursor, "two visible items at limit=1 must produce a resume cursor")
	assert.True(t, strings.HasPrefix(resp.NextCursor, listCursorPrefix),
		"a real HTTP-endpoint nextCursor must start with the sealed-cursor version prefix %q", listCursorPrefix)
}

// TestListTemplatesCursor_SealedCursorContainsNoRecoverablePayload is the
// HTTP-level form of assertNoRecoverableCursorPayload: it recovers the real
// inner cursor a live endpoint sealed (by opening the real nextCursor with
// the endpoint's own binding), then asserts that inner's payload -- not just
// its individually-known fields -- is unrecoverable from the raw sealed
// bytes. A signature-only (authenticated but unencrypted) cursor format
// would still pass every other assertion in this file, since none of them
// checks for the payload as a whole.
func TestListTemplatesCursor_SealedCursorContainsNoRecoverablePayload(t *testing.T) {
	srv, s, _, carol, _ := setupTemplateScopeTest(t)
	setupCursorOpacityTemplates(t, s, carol.ID)

	_, first := getTemplatesPage(t, srv, carol, "scope=user&limit=1")
	require.NotEmpty(t, first.NextCursor)

	binding := scopedCursorBindingForTemplatesTest(carol, "user", carol.ID)
	inner, err := srv.listCursorSealer.Open(first.NextCursor, binding)
	require.NoError(t, err)

	assertNoRecoverableCursorPayload(t, first.NextCursor, inner)
}

// TestListTemplatesCursor_SameUserNewSessionResumes proves the identity-bound
// behaviour session users are meant to keep: scopedCursorBinding's default
// arm keys on the user's type and ID alone, not a per-login/session/token
// identifier, so a cursor minted under one session for carol still resumes
// under an entirely separate session for the same carol -- a new login, a
// new tab, or a cookie refresh. getTemplatesPage/doRequestAsUser mints a
// fresh JWT per call, so the two calls below already are two independent
// sessions, exactly as every other multi-call test in this file relies on
// implicitly; this test makes that property explicit.
func TestListTemplatesCursor_SameUserNewSessionResumes(t *testing.T) {
	srv, s, _, carol, _ := setupTemplateScopeTest(t)
	setupCursorOpacityTemplates(t, s, carol.ID)

	_, first := getTemplatesPage(t, srv, carol, "scope=user&limit=1")
	require.NotEmpty(t, first.NextCursor)

	rec, second := getTemplatesPage(t, srv, carol, "scope=user&limit=1&cursor="+first.NextCursor)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String(),
		"a cursor must resume under a brand-new session for the same user")
	require.Len(t, second.Templates, 1)
}

func TestListTemplatesCursor_TamperedByteRejected(t *testing.T) {
	srv, s, _, carol, _ := setupTemplateScopeTest(t)
	setupCursorOpacityTemplates(t, s, carol.ID)
	_, first := getTemplatesPage(t, srv, carol, "scope=user&limit=1")
	require.NotEmpty(t, first.NextCursor)

	require.True(t, strings.HasPrefix(first.NextCursor, listCursorPrefix))
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(first.NextCursor, listCursorPrefix))
	require.NoError(t, err)
	tampered := append([]byte{}, raw...)
	tampered[len(tampered)-1] ^= 0x01
	tamperedCursor := listCursorPrefix + base64.RawURLEncoding.EncodeToString(tampered)

	rec, _ := getTemplatesPage(t, srv, carol, "scope=user&limit=1&cursor="+tamperedCursor)
	assertInvalidCursor(t, rec)
}

func TestListTemplatesCursor_TruncatedRejected(t *testing.T) {
	srv, s, _, carol, _ := setupTemplateScopeTest(t)
	setupCursorOpacityTemplates(t, s, carol.ID)
	_, first := getTemplatesPage(t, srv, carol, "scope=user&limit=1")
	require.NotEmpty(t, first.NextCursor)

	truncated := first.NextCursor[:len(first.NextCursor)-4]
	rec, _ := getTemplatesPage(t, srv, carol, "scope=user&limit=1&cursor="+truncated)
	assertInvalidCursor(t, rec)
}

// A cursor sealed under a different key than the hub currently holds (a
// stand-in for a rotated key, or a replica that has not converged) is
// rejected the same way as a tampered one.
func TestListTemplatesCursor_WrongKeyRejected(t *testing.T) {
	srv, s, _, carol, _ := setupTemplateScopeTest(t)
	setupCursorOpacityTemplates(t, s, carol.ID)
	_, first := getTemplatesPage(t, srv, carol, "scope=user&limit=1")
	require.NotEmpty(t, first.NextCursor)

	// Recover the plaintext with the real key, then reseal it under a
	// different one: the resulting cursor is well-formed opaque cursor
	// shape, but authenticated under a key the hub does not hold.
	binding := scopedCursorBindingForTemplatesTest(carol, "user", carol.ID)
	inner, err := srv.listCursorSealer.Open(first.NextCursor, binding)
	require.NoError(t, err)
	otherSealer := mustNewListCursorSealer(t)
	forged, err := otherSealer.Seal(inner, binding)
	require.NoError(t, err)

	rec, _ := getTemplatesPage(t, srv, carol, "scope=user&limit=1&cursor="+forged)
	assertInvalidCursor(t, rec)
}

// A cursor sealed under a different domain/version marker (see
// listCursorSealDomain) is rejected the same way a key rotation is.
func TestListTemplatesCursor_WrongVersionRejected(t *testing.T) {
	srv, s, _, carol, _ := setupTemplateScopeTest(t)
	setupCursorOpacityTemplates(t, s, carol.ID)
	_, first := getTemplatesPage(t, srv, carol, "scope=user&limit=1")
	require.NotEmpty(t, first.NextCursor)

	binding := scopedCursorBindingForTemplatesTest(carol, "user", carol.ID)
	inner, err := srv.listCursorSealer.Open(first.NextCursor, binding)
	require.NoError(t, err)

	nonce := make([]byte, srv.listCursorSealer.aead.NonceSize())
	ciphertext := srv.listCursorSealer.aead.Seal(nil, nonce, []byte(inner), []byte("scion-list-cursor-v2:"+binding))
	forged := listCursorPrefix + base64.RawURLEncoding.EncodeToString(append(nonce, ciphertext...))

	rec, _ := getTemplatesPage(t, srv, carol, "scope=user&limit=1&cursor="+forged)
	assertInvalidCursor(t, rec)
}

// A legacy (pre-opaque-cursor) plaintext cursor is rejected uniformly, not
// crashed on or silently accepted -- and a fresh first page still works
// right alongside it.
func TestListTemplatesCursor_LegacyPlaintextCursorRejectedButFirstPageStillWorks(t *testing.T) {
	srv, s, _, carol, _ := setupTemplateScopeTest(t)
	setupCursorOpacityTemplates(t, s, carol.ID)

	legacy := authorizedListCursor(time.Now(), api.NewUUID(), "some-legacy-binding")
	rec, _ := getTemplatesPage(t, srv, carol, "scope=user&limit=1&cursor="+legacy)
	assertInvalidCursor(t, rec)

	// A fresh first page (no cursor) alongside the rejected legacy one still
	// works normally.
	rec2, resp2 := getTemplatesPage(t, srv, carol, "scope=user&limit=1")
	require.Equal(t, http.StatusOK, rec2.Code, rec2.Body.String())
	require.Len(t, resp2.Templates, 1)
}

// TestListTemplatesCursor_LegacyPlaintextCursorWithRealBindingAndIDRejected
// uses the real binding the endpoint actually computes and a real,
// currently-listable template's ID, so the only reason for rejection is the
// missing sealed-cursor format, not an unrelated binding or ID mismatch.
func TestListTemplatesCursor_LegacyPlaintextCursorWithRealBindingAndIDRejected(t *testing.T) {
	srv, s, _, carol, _ := setupTemplateScopeTest(t)
	setupCursorOpacityTemplates(t, s, carol.ID)

	_, page := getTemplatesPage(t, srv, carol, "scope=user&limit=2")
	require.Len(t, page.Templates, 2)
	existingID := page.Templates[0].ID

	realBinding := scopedCursorBindingForTemplatesTest(carol, "user", carol.ID)
	legacy := authorizedListCursor(time.Now(), existingID, realBinding)
	rec, _ := getTemplatesPage(t, srv, carol, "scope=user&limit=1&cursor="+legacy)
	assertInvalidCursor(t, rec)
}

// Cross-query reuse: a cursor minted for one filter (scope=user) is
// rejected when replayed against a different filter (no scope) on the same
// endpoint, even for the same caller.
func TestListTemplatesCursor_CrossQueryReuseRejected(t *testing.T) {
	srv, s, _, carol, _ := setupTemplateScopeTest(t)
	setupCursorOpacityTemplates(t, s, carol.ID)
	_, first := getTemplatesPage(t, srv, carol, "scope=user&limit=1")
	require.NotEmpty(t, first.NextCursor)

	rec, _ := getTemplatesPage(t, srv, carol, "limit=1&cursor="+first.NextCursor)
	assertInvalidCursor(t, rec)
}

// Cross-caller reuse: a cursor minted for carol is rejected when replayed by
// alice on the identical query. Note this does not by itself isolate
// scopedCursorBinding's identity component: scope=user also forces
// filter.ScopeID to the caller's own ID, so the filter differs between
// carol and alice too. TestListCursorSealer_SessionAndScopedUATBindingsForSameUserDiffer
// isolates the identity component directly, on a filter held constant.
func TestListTemplatesCursor_CrossCallerReuseRejected(t *testing.T) {
	srv, s, alice, carol, _ := setupTemplateScopeTest(t)
	setupCursorOpacityTemplates(t, s, carol.ID)
	_, first := getTemplatesPage(t, srv, carol, "scope=user&limit=1")
	require.NotEmpty(t, first.NextCursor)

	rec, _ := getTemplatesPage(t, srv, alice, "scope=user&limit=1&cursor="+first.NextCursor)
	assertInvalidCursor(t, rec)

	// Sanity: the same cursor still works for the caller it was minted for.
	rec2, resp2 := getTemplatesPage(t, srv, carol, "scope=user&limit=1&cursor="+first.NextCursor)
	require.Equal(t, http.StatusOK, rec2.Code, rec2.Body.String())
	require.Len(t, resp2.Templates, 1)
}

// scopedCursorBindingForTemplatesTest reconstructs the exact binding
// listTemplatesV2 computes for a scope=user request, so tests can Open a
// real cursor directly. Kept in lockstep with listTemplatesV2's filter
// construction; if that handler's filter shape changes, update this too.
func scopedCursorBindingForTemplatesTest(user *store.User, scope, scopeID string) string {
	filter := store.TemplateFilter{Scope: scope, ScopeID: scopeID, Status: store.TemplateStatusActive}
	identity := NewAuthenticatedUser(user.ID, user.Email, user.DisplayName, user.Role, "api")
	return scopedCursorBinding("templates", filter, identity)
}

// ----------------------------------------------------------------------------
// Resumed cursor after access loss: no newly denied item is ever returned.
// ----------------------------------------------------------------------------

// TestListGroupsCursor_ResumedAfterAccessLossReturnsNoNewlyDeniedItems seeds
// two groups visible to alice only through a project-scoped role binding,
// fetches page 1 (limit=1, leaving one group for a resumed page 2), revokes
// the role binding entirely, and confirms the page-2 resume returns no
// items: authorizedList re-authorizes every candidate live on every page,
// so a cursor is only ever a position, never a standing grant.
func TestListGroupsCursor_ResumedAfterAccessLossReturnsNoNewlyDeniedItems(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// alice is deliberately NOT given baseline hub membership here: the
	// seeded hub-member system role includes group.read/group.list (seed.go),
	// which would confound this test by granting alice visibility
	// independent of the project-scoped role binding under test below.
	alice := NewAuthenticatedUser(tid("cursor-access-loss-alice"), "cursor-access-loss-alice@test.com", "Alice", store.UserRoleMember, "api")
	require.NoError(t, s.CreateUser(ctx, &store.User{ID: alice.ID(), Email: alice.Email(), DisplayName: alice.DisplayName(), Role: store.UserRoleMember, Status: "active"}))

	otherOwner := &store.User{ID: tid("cursor-access-loss-owner"), Email: "cursor-access-loss-owner@test.com", DisplayName: "Owner", Role: store.UserRoleMember, Status: "active"}
	require.NoError(t, s.CreateUser(ctx, otherOwner))

	project := &store.Project{ID: tid("cursor-access-loss-project"), Name: "Access Loss Project", Slug: "cursor-access-loss-project"}
	require.NoError(t, s.CreateProject(ctx, project))

	rd, err := s.CreateRoleDefinition(ctx, &store.RoleDefinition{
		Name:        "cursor-access-loss-reader",
		ScopeType:   store.RoleScopeProject,
		Permissions: []string{"group.read", "group.list"},
	})
	require.NoError(t, err)
	rb, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      alice.ID(),
		ScopeType:        store.RoleScopeProject,
		ScopeID:          project.ID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	// Owned by someone other than alice: the authz kernel grants a resource
	// owner implicit read access (authz.go's OwnerID shortcut) regardless of
	// role bindings, which would confound this test -- alice's visibility
	// here must come only from the project-scoped role binding revoked
	// below.
	otherOwnerID := otherOwner.ID
	base := time.Now()
	var groupIDs []string
	for i, name := range []string{"access-loss-a", "access-loss-b"} {
		g := &store.Group{
			ID:        api.NewUUID(),
			Name:      name,
			Slug:      api.Slugify(name) + "-" + fmt.Sprint(i),
			GroupType: store.GroupTypeExplicit,
			ProjectID: project.ID,
			OwnerID:   otherOwnerID,
			Created:   base.Add(-time.Duration(i) * time.Second),
			Updated:   base.Add(-time.Duration(i) * time.Second),
		}
		require.NoError(t, s.CreateGroup(ctx, g))
		groupIDs = append(groupIDs, g.ID)
	}

	getGroupsPage := func(query string) (*httptest.ResponseRecorder, ListGroupsResponse) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/groups?"+query, nil).WithContext(contextWithIdentity(ctx, alice))
		rec := httptest.NewRecorder()
		srv.listGroups(rec, req)
		var resp ListGroupsResponse
		if rec.Code == http.StatusOK {
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		}
		return rec, resp
	}

	rec1, page1 := getGroupsPage(fmt.Sprintf("projectId=%s&limit=1", project.ID))
	require.Equal(t, http.StatusOK, rec1.Code, rec1.Body.String())
	require.Len(t, page1.Groups, 1)
	assert.Contains(t, groupIDs, page1.Groups[0].ID, "the returned group must be one of the seeded groups")
	require.NotEmpty(t, page1.NextCursor, "a second group remains, so a resume cursor must be returned")

	// Revoke alice's only access to this project's groups.
	require.NoError(t, s.DeleteRoleBinding(ctx, rb.ID))

	rec2, page2 := getGroupsPage(fmt.Sprintf("projectId=%s&limit=1&cursor=%s", project.ID, page1.NextCursor))
	require.Equal(t, http.StatusOK, rec2.Code, rec2.Body.String(), "a resumed cursor from a now-inaccessible scope must not error the request")
	assert.Empty(t, page2.Groups, "no newly denied item may be returned once access to the scope is revoked")
}

// ----------------------------------------------------------------------------
// Cross-query / cross-caller reuse on the groups endpoint, whose filter
// (unlike templates') never embeds the caller's identity -- isolating the
// scopedCursorBinding identity component cleanly.
// ----------------------------------------------------------------------------

func setupCursorOpacityGroupReaders(t *testing.T) (srv *Server, s store.Store, alice, carol *store.User, project *store.Project) {
	t.Helper()
	srv, s = testServer(t)
	ctx := context.Background()

	alice = &store.User{ID: tid("cursor-opacity-alice"), Email: "cursor-opacity-alice@test.com", DisplayName: "Alice", Role: store.UserRoleMember, Status: "active"}
	require.NoError(t, s.CreateUser(ctx, alice))
	ensureHubMembership(ctx, s, alice.ID)
	carol = &store.User{ID: tid("cursor-opacity-carol"), Email: "cursor-opacity-carol@test.com", DisplayName: "Carol", Role: store.UserRoleMember, Status: "active"}
	require.NoError(t, s.CreateUser(ctx, carol))
	ensureHubMembership(ctx, s, carol.ID)

	project = &store.Project{ID: tid("cursor-opacity-project"), Name: "Cursor Opacity Project", Slug: "cursor-opacity-project"}
	require.NoError(t, s.CreateProject(ctx, project))
	otherProject := &store.Project{ID: tid("cursor-opacity-other-project"), Name: "Other Project", Slug: "cursor-opacity-other-project"}
	require.NoError(t, s.CreateProject(ctx, otherProject))

	rd, err := s.CreateRoleDefinition(ctx, &store.RoleDefinition{
		Name:        "cursor-opacity-reader",
		ScopeType:   store.RoleScopeProject,
		Permissions: []string{"group.read", "group.list"},
	})
	require.NoError(t, err)
	for _, u := range []*store.User{alice, carol} {
		_, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
			RoleDefinitionID: rd.ID,
			PrincipalType:    store.RoleBindingPrincipalUser,
			PrincipalID:      u.ID,
			ScopeType:        store.RoleScopeProject,
			ScopeID:          project.ID,
			CreatedBy:        "test",
		})
		require.NoError(t, err)
	}

	base := time.Now()
	for i, name := range []string{"opacity-group-a", "opacity-group-b"} {
		g := &store.Group{
			ID:        api.NewUUID(),
			Name:      name,
			Slug:      api.Slugify(name) + "-" + fmt.Sprint(i),
			GroupType: store.GroupTypeExplicit,
			ProjectID: project.ID,
			OwnerID:   alice.ID,
			Created:   base.Add(-time.Duration(i) * time.Second),
			Updated:   base.Add(-time.Duration(i) * time.Second),
		}
		require.NoError(t, s.CreateGroup(ctx, g))
	}
	return srv, s, alice, carol, project
}

func getGroupsPageAsUser(t *testing.T, srv *Server, user *store.User, query string) (*httptest.ResponseRecorder, ListGroupsResponse) {
	t.Helper()
	identity := NewAuthenticatedUser(user.ID, user.Email, user.DisplayName, user.Role, "api")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/groups?"+query, nil).WithContext(contextWithIdentity(context.Background(), identity))
	rec := httptest.NewRecorder()
	srv.listGroups(rec, req)
	var resp ListGroupsResponse
	if rec.Code == http.StatusOK {
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	}
	return rec, resp
}

func TestListGroupsCursor_CrossCallerReuseRejected(t *testing.T) {
	srv, _, alice, carol, project := setupCursorOpacityGroupReaders(t)
	query := fmt.Sprintf("projectId=%s&limit=1", project.ID)

	_, first := getGroupsPageAsUser(t, srv, alice, query)
	require.NotEmpty(t, first.NextCursor)

	rec, _ := getGroupsPageAsUser(t, srv, carol, query+"&cursor="+first.NextCursor)
	assertInvalidCursorGroups(t, rec)

	// Sanity: the cursor still works for the caller it was minted for.
	rec2, page2 := getGroupsPageAsUser(t, srv, alice, query+"&cursor="+first.NextCursor)
	require.Equal(t, http.StatusOK, rec2.Code, rec2.Body.String())
	require.Len(t, page2.Groups, 1)
}

func TestListGroupsCursor_CrossQueryReuseRejected(t *testing.T) {
	srv, _, alice, _, project := setupCursorOpacityGroupReaders(t)
	_, first := getGroupsPageAsUser(t, srv, alice, fmt.Sprintf("projectId=%s&limit=1", project.ID))
	require.NotEmpty(t, first.NextCursor)

	rec, _ := getGroupsPageAsUser(t, srv, alice, "limit=1&cursor="+first.NextCursor)
	assertInvalidCursorGroups(t, rec)
}

func assertInvalidCursorGroups(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	var errResp ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errResp))
	assert.Equal(t, ErrCodeInvalidCursor, errResp.Error.Code)
}
