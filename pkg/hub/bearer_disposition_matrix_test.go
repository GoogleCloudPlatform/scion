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
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/authzop"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

// bearerMatrixExclusion names why one catalog entry point is not exercised
// by TestBearerDispositionMatrix_CatalogEntryPoints, and the dedicated test
// that pins its disposition instead.
type bearerMatrixExclusion struct {
	Reason string
	Pin    string
}

// bearerMatrixExclusions lists catalog HTTP, SSE and WebSocket entry points
// whose recorded disposition the generic matrix cannot exercise. Each one
// carries a specific reason and the name of the test that pins it.
// TestBearerMatrixExclusions_NotStaleAndPinned keeps every key live and
// every Pin declared.
var bearerMatrixExclusions = map[liveInventoryKey]bearerMatrixExclusion{
	{OperationID: "project.lifecycle.delete", Method: "DELETE", Pattern: "/api/v1/projects/{id}"}: {
		Reason: "project.delete has no token selector, so a token is refused by the deletion service's base permission check (403 project_delete_forbidden) before its session-only credential step is reached; the session-only step and its IRREVERSIBLE_CASCADE reason are pinned at the service",
		Pin:    "TestRS3_ProjectDeleteScopedUATDenied",
	},
	{OperationID: "harnessconfig.read", Method: "GET", Pattern: "/api/v1/harness-configs"}: {
		Reason: "the collection list filters each row by harness_config.read instead of refusing the request, so a token without the selector gets 200 with no rows rather than 403; the list gets its own harness_config.list disposition in a later batch",
		Pin:    "TestScopedAdminListEndpointsFilterCrossProjectRowsAndCountAuthorizedMatches",
	},
}

// bearerMatrixEntry is one catalog entry point with a request surface.
type bearerMatrixEntry struct {
	Spec       authzop.OperationSpec
	EntryPoint authzop.EntryPoint
}

// bearerMatrixKinds are the entry point kinds a request can reach.
var bearerMatrixKinds = map[authzop.EntryPointKind]bool{
	authzop.EntryPointHTTPRoute: true,
	authzop.EntryPointSSE:       true,
	authzop.EntryPointWebSocket: true,
}

// bearerMatrixEntries returns every catalog HTTP, SSE and WebSocket entry
// point, in catalog declaration order.
func bearerMatrixEntries() []bearerMatrixEntry {
	var out []bearerMatrixEntry
	for _, spec := range authzop.Catalog {
		for _, ep := range spec.EntryPoints {
			if bearerMatrixKinds[ep.Kind] {
				out = append(out, bearerMatrixEntry{Spec: spec, EntryPoint: ep})
			}
		}
	}
	return out
}

func (e bearerMatrixEntry) key() liveInventoryKey {
	return liveInventoryKey{OperationID: string(e.Spec.ID), Method: e.EntryPoint.Method, Pattern: e.EntryPoint.Pattern}
}

// bearerMatrixSelector returns the token selector of the operation's base
// permission, from the permission registry.
func bearerMatrixSelector(permissionID string) string {
	for _, p := range permissions.Registry {
		if p.ID == permissionID {
			return p.UATScope
		}
	}
	return ""
}

// bearerMatrixUnrelatedSelector returns a mintable selector that maps to a
// different permission than sel.
func bearerMatrixUnrelatedSelector(sel string) string {
	if sel == "project:read" {
		return "agent:read"
	}
	return "project:read"
}

// bearerMatrixFixture is a live server seeded with the live-inventory
// fixtures, a super-admin who holds live authority on every fixture, and a
// second project the super-admin's project tokens can be bound to.
type bearerMatrixFixture struct {
	srv          *Server
	store        store.Store
	ids          idFixtures
	adminID      string
	otherProject string
	tokens       map[string]string
}

func newBearerMatrixFixture(t *testing.T) *bearerMatrixFixture {
	t.Helper()
	srv, s := testServer(t)
	ctx := context.Background()
	ids := seedLiveInventoryFixtures(t, ctx, srv, s)

	adminID := tid("bdm-super-admin")
	createTestUserWithRole(t, s, adminID, adminID+"@test.com", "admin", store.SystemRoleSuperAdmin)
	ensureHubMembership(ctx, s, adminID)

	other := tid("bdm-other-project")
	require.NoError(t, s.CreateProject(ctx, &store.Project{ID: other, Name: "BDM Other", Slug: "bdm-other"}))
	// Membership in the other project lets the super-admin mint tokens
	// bound to it.
	createTestUserWithProjectRole(t, s, adminID, adminID+"@test.com", other, store.ProjectRoleOwner)

	return &bearerMatrixFixture{srv: srv, store: s, ids: ids, adminID: adminID, otherProject: other, tokens: map[string]string{}}
}

// mint returns a real token for the super-admin, minted through
// UserAccessTokenService over a session, for the boundary and selectors.
// Tokens are cached per boundary and selector set.
func (m *bearerMatrixFixture) mint(t *testing.T, boundary TokenBoundary, scopes []string) string {
	t.Helper()
	cacheKey := string(boundary.Kind) + "|" + boundary.ProjectID + "|" + strings.Join(scopes, ",")
	if key, ok := m.tokens[cacheKey]; ok {
		return key
	}
	key, _, err := m.srv.uatService.CreateTokenWithParams(rs4MintContext(m.adminID), CreateTokenParams{
		UserID: m.adminID, Name: "bdm-" + tid("tok"), Boundary: boundary, Scopes: scopes,
	})
	require.NoError(t, err, "mint %s token with %v", boundary.Kind, scopes)
	m.tokens[cacheKey] = key
	return key
}

// tryMint is mint for a selector set that may not be mintable; it returns
// "" when minting fails. Results are cached like mint's.
func (m *bearerMatrixFixture) tryMint(boundary TokenBoundary, scopes []string) string {
	cacheKey := "try|" + string(boundary.Kind) + "|" + boundary.ProjectID + "|" + strings.Join(scopes, ",")
	if key, ok := m.tokens[cacheKey]; ok {
		return key
	}
	key, _, err := m.srv.uatService.CreateTokenWithParams(rs4MintContext(m.adminID), CreateTokenParams{
		UserID: m.adminID, Name: "bdm-" + tid("try"), Boundary: boundary, Scopes: scopes,
	})
	if err != nil {
		key = ""
	}
	m.tokens[cacheKey] = key
	return key
}

// canMint reports whether the super-admin can mint a token for the
// boundary and selectors. The trial token is deleted, so probing does not
// count against the per-user token limit.
func (m *bearerMatrixFixture) canMint(t *testing.T, boundary TokenBoundary, scopes []string) bool {
	t.Helper()
	ctx := rs4MintContext(m.adminID)
	_, tok, err := m.srv.uatService.CreateTokenWithParams(ctx, CreateTokenParams{
		UserID: m.adminID, Name: "bdm-" + tid("probe"), Boundary: boundary, Scopes: scopes,
	})
	if err != nil {
		require.NotErrorIs(t, err, ErrUATLimitExceeded, "probing must not exhaust the token limit")
		return false
	}
	require.NoError(t, m.srv.uatService.DeleteToken(ctx, m.adminID, tok.ID))
	return true
}

// everySelectorHubToken mints a hub token for the super-admin that carries
// every selector the super-admin can mint on the hub boundary.
func (m *bearerMatrixFixture) everySelectorHubToken(t *testing.T) (string, []string) {
	t.Helper()
	var selectors []string
	seen := map[string]bool{}
	for _, p := range permissions.Registry {
		if p.UATScope == "" || seen[p.UATScope] {
			continue
		}
		seen[p.UATScope] = true
		if m.canMint(t, hubBoundary(), []string{p.UATScope}) {
			selectors = append(selectors, p.UATScope)
		}
	}
	sort.Strings(selectors)
	require.NotEmpty(t, selectors, "the super-admin can mint at least one hub selector")
	return m.mint(t, hubBoundary(), selectors), selectors
}

// request sends method to the entry point's live path with a real token.
// A stream is ended by the request deadline.
func (m *bearerMatrixFixture) request(t *testing.T, e bearerMatrixEntry, key string) *httptest.ResponseRecorder {
	t.Helper()
	ep := e.EntryPoint
	id := string(e.Spec.ID)
	params := opPatternOverrides(m.ids)[overrideKey{id, ep.Pattern}]
	if params == nil {
		params = patternOverrides(m.ids)[ep.Pattern]
	}
	path := substituteLiveInventoryParams(ep.Pattern, params)
	if q, ok := queryOverrides(m.ids)[ep.Pattern]; ok {
		path += "?" + q
	}
	var body []byte
	switch ep.Method {
	case http.MethodPost, http.MethodPut, http.MethodPatch:
		var payload interface{} = map[string]interface{}{}
		if override, ok := bearerMatrixBodyOverrides(m.ids)[overrideKey{id, ep.Pattern}]; ok {
			payload = override
		} else if override, ok := bodyOverrides(m.ids)[overrideKey{id, ep.Pattern}]; ok {
			payload = override
		}
		var err error
		body, err = json.Marshal(payload)
		require.NoError(t, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req := httptest.NewRequest(ep.Method, path, bytes.NewReader(body)).WithContext(ctx)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	m.srv.Handler().ServeHTTP(rec, req)
	return rec
}

// bearerMatrixBodyOverrides holds request bodies the matrix sends in place
// of the live-inventory bodies, for handlers that validate the body before
// they reach the credential check the matrix observes.
func bearerMatrixBodyOverrides(f idFixtures) map[overrideKey]map[string]interface{} {
	return map[overrideKey]map[string]interface{}{
		{"project.membership.add", "/api/v1/projects/{id}/members"}: {
			"roleDefinitionId": f.projectMemberRoleID, "principalType": "user", "principalId": f.user,
		},
		{"project.membership.transfer", "/api/v1/projects/{id}/transfer-ownership"}: {"newOwnerId": f.member},
		// The delegation ceiling applies to the role an agent is granted:
		// a token carrying only agent:create covers no usable role, so the
		// probe asks for agentRole "none", which every creator may grant.
		{"agent.lifecycle.create", "/api/v1/agents"}: {"name": "bdm-created", "projectId": f.project, "agentRole": "none"},
	}
}

// sessionOnlyDetailsOf returns details.reason and details.credential of an
// error response, or "" for each when absent.
func sessionOnlyDetailsOf(rec *httptest.ResponseRecorder) (reason, credential string) {
	var resp ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		return "", ""
	}
	reason, _ = resp.Error.Details["reason"].(string)
	credential, _ = resp.Error.Details["credential"].(string)
	return reason, credential
}

// TestBearerDispositionMatrix_CatalogEntryPoints drives real tokens through
// every catalogued HTTP, SSE and WebSocket entry point and checks the
// result against the operation's recorded bearer disposition:
//   - admit: a hub token with the selector and live authority is not
//     refused (no 401/403); a token of the same user with an unrelated
//     selector, and a project token for another project, are refused
//     (403, or 404 on a GET, where read handlers hide the record);
//   - admit_self: a token with an unrelated selector is not refused, and
//     the disposition names the test that pins its result filter;
//   - session_only: a hub token of a super-admin carrying every mintable
//     selector gets 403 with details.reason equal to the recorded reason
//     and details.credential = session_required;
//   - non_user and out_of_scope: skipped and logged;
//   - an operation with no recorded disposition yet is listed in
//     authzop.PendingBearerOperations and skipped.
//
// The refusal checks run before the admitting request, so a destructive
// entry point still has its fixture when it is refused.
func TestBearerDispositionMatrix_CatalogEntryPoints(t *testing.T) {
	m := newBearerMatrixFixture(t)
	allKey, allSelectors := m.everySelectorHubToken(t)
	t.Logf("session-only probe token carries %d selectors: %v", len(allSelectors), allSelectors)

	counts := map[string]int{}
	for _, e := range bearerMatrixEntries() {
		ep := e.EntryPoint
		label := string(e.Spec.ID) + " " + ep.Method + " " + ep.Pattern
		if ex, ok := bearerMatrixExclusions[e.key()]; ok {
			t.Logf("excluded %s: %s (pinned by %s)", label, ex.Reason, ex.Pin)
			counts["excluded"]++
			continue
		}
		d := e.Spec.Bearer
		switch d.Kind {
		case "":
			if !authzop.IsPendingBearerOperation(e.Spec.ID) {
				t.Errorf("%s has no bearer disposition and is not pending", label)
			}
			counts["pending"]++

		case authzop.BearerNonUser:
			t.Logf("skipped %s: non_user", label)
			counts["skipped"]++

		case authzop.BearerOutOfScope:
			t.Logf("skipped %s: out_of_scope, owner %s", label, d.Owner)
			counts["skipped"]++

		case authzop.BearerSessionOnly:
			rec := m.request(t, e, allKey)
			reason, credential := sessionOnlyDetailsOf(rec)
			if rec.Code != http.StatusForbidden || reason != string(d.Reason) || credential != sessionRequiredCredential {
				t.Errorf("%s: session_only %s: got %d reason=%q credential=%q, want 403 reason=%q credential=%q: %s",
					label, d.Reason, rec.Code, reason, credential, d.Reason, sessionRequiredCredential, rec.Body.String())
			}
			counts["session_only"]++

		case authzop.BearerAdmit:
			sel := bearerMatrixSelector(e.Spec.BasePermission)
			if sel == "" {
				t.Errorf("%s: admit needs a selector for %s", label, e.Spec.BasePermission)
				continue
			}

			// (b) ceiling: the same user, an unrelated selector.
			refusedWith404 := false
			unrelated := m.mint(t, hubBoundary(), []string{bearerMatrixUnrelatedSelector(sel)})
			if rec := m.request(t, e, unrelated); !bearerMatrixRefused(ep.Method, rec.Code) {
				t.Errorf("%s: a token without %s got %d, want %s: %s", label, sel, rec.Code, bearerMatrixRefusal(ep.Method), rec.Body.String())
			} else if rec.Code == http.StatusNotFound {
				refusedWith404 = true
			}

			// (c) boundary: a project token for another project, or for a
			// hub-only permission, a project token for the fixture project.
			hubOnly := !bearerMatrixHasBoundary(d, authzop.BearerBoundaryProject)
			boundProject := m.otherProject
			if hubOnly {
				boundProject = m.ids.project
			}
			if key := m.tryMint(projectBoundary(boundProject), []string{sel}); key != "" {
				rec := m.request(t, e, key)
				if rec.Code == http.StatusNotFound {
					refusedWith404 = true
				}
				if !bearerMatrixRefused(ep.Method, rec.Code) {
					t.Errorf("%s: a project token for %s got %d, want %s: %s", label, boundProject, rec.Code, bearerMatrixRefusal(ep.Method), rec.Body.String())
				}
			} else {
				t.Logf("%s: a project token for %s with %s cannot be minted; the boundary is enforced at mint", label, boundProject, sel)
			}

			// (a) positive: the selector on an allowed boundary.
			boundary := hubBoundary()
			if !bearerMatrixHasBoundary(d, authzop.BearerBoundaryHub) {
				boundary = projectBoundary(m.ids.project)
			}
			rec := m.request(t, e, m.mint(t, boundary, []string{sel}))
			if rec.Code == http.StatusUnauthorized || rec.Code == http.StatusForbidden {
				t.Errorf("%s: a %s token with %s got %d, want neither 401 nor 403: %s", label, boundary.Kind, sel, rec.Code, rec.Body.String())
			}
			// A 404 counts as a refusal only when the admitting token
			// proves the same target exists: a 2xx, or for a WebSocket
			// entry point the 400 that answers a request without an
			// upgrade once the target was found.
			if refusedWith404 {
				found := rec.Code >= 200 && rec.Code < 300 ||
					(ep.Kind == authzop.EntryPointWebSocket && rec.Code == http.StatusBadRequest)
				if !found {
					t.Errorf("%s: refused with 404, but the admitting token got %d, so the target is not shown to exist: %s", label, rec.Code, rec.Body.String())
				} else {
					t.Logf("404 refusal on %s: the admitting token got %d on the same target", label, rec.Code)
				}
			}
			counts["admit"]++

		case authzop.BearerAdmitSelf:
			if d.Pin == "" {
				t.Errorf("%s: admit_self names the test that pins its result filter", label)
			}
			key := m.mint(t, hubBoundary(), []string{"project:read"})
			if rec := m.request(t, e, key); rec.Code == http.StatusUnauthorized || rec.Code == http.StatusForbidden {
				t.Errorf("%s: admit_self with an unrelated selector got %d, want neither 401 nor 403: %s", label, rec.Code, rec.Body.String())
			}
			counts["admit_self"]++

		default:
			t.Errorf("%s: unknown bearer kind %q", label, d.Kind)
		}
	}

	t.Logf("bearer disposition matrix: admit=%d admit_self=%d session_only=%d skipped=%d pending=%d excluded=%d",
		counts["admit"], counts["admit_self"], counts["session_only"], counts["skipped"], counts["pending"], counts["excluded"])
	if counts["admit"] == 0 || counts["session_only"] == 0 {
		t.Fatalf("the matrix exercised no admit or no session_only entry point: %v", counts)
	}
}

// bearerMatrixRefused reports whether status refuses a token. A refusal is
// 403; a GET entry point may answer 404 instead, because read handlers do
// not reveal whether a record exists to a caller that may not read it.
func bearerMatrixRefused(method string, status int) bool {
	return status == http.StatusForbidden || (method == http.MethodGet && status == http.StatusNotFound)
}

func bearerMatrixRefusal(method string) string {
	if method == http.MethodGet {
		return "403 or 404"
	}
	return "403"
}

func bearerMatrixHasBoundary(d authzop.BearerDisposition, b authzop.BearerBoundary) bool {
	for _, have := range d.Boundaries {
		if have == b {
			return true
		}
	}
	return false
}

// declaredTestFunctions returns the names of every top-level Test function
// declared in this package's test files.
func declaredTestFunctions(t *testing.T) map[string]bool {
	t.Helper()
	files, err := filepath.Glob("*_test.go")
	require.NoError(t, err)
	re := regexp.MustCompile(`(?m)^func (Test\w+)\(t \*testing\.T\)`)
	out := map[string]bool{}
	for _, f := range files {
		src, err := os.ReadFile(f)
		require.NoError(t, err)
		for _, m := range re.FindAllStringSubmatch(string(src), -1) {
			out[m[1]] = true
		}
	}
	return out
}

// TestBearerMatrixExclusions_NotStaleAndPinned requires every matrix
// exclusion to name a live catalog entry point whose disposition the matrix
// would otherwise exercise, to give a reason, and to name a declared test
// as its Pin. Every Pin recorded on a catalog disposition must also name a
// declared test.
func TestBearerMatrixExclusions_NotStaleAndPinned(t *testing.T) {
	declared := declaredTestFunctions(t)
	exercised := map[liveInventoryKey]bool{}
	for _, e := range bearerMatrixEntries() {
		switch e.Spec.Bearer.Kind {
		case authzop.BearerAdmit, authzop.BearerAdmitSelf, authzop.BearerSessionOnly:
			exercised[e.key()] = true
		}
	}
	for key, ex := range bearerMatrixExclusions {
		if !exercised[key] {
			t.Errorf("stale matrix exclusion %v: no catalog entry point with an admit, admit_self or session_only disposition", key)
		}
		if strings.TrimSpace(ex.Reason) == "" {
			t.Errorf("matrix exclusion %v has no reason", key)
		}
		if ex.Pin == "" || !declared[ex.Pin] {
			t.Errorf("matrix exclusion %v: Pin %q is not a declared test in pkg/hub", key, ex.Pin)
		}
	}
	for _, spec := range authzop.Catalog {
		if pin := spec.Bearer.Pin; pin != "" && !declared[pin] {
			t.Errorf("operation %s: bearer Pin %q is not a declared test in pkg/hub", spec.ID, pin)
		}
	}
}
