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
	"strconv"
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
	{OperationID: "inbox.message.read", Method: "GET", Pattern: "/api/v1/messages"}: {
		Reason: "the message list filters rows to the token boundary instead of refusing the request, so a project token for another project gets 200 with no rows",
		Pin:    "TestInboxToken_ProjectBoundaryFiltersMessages",
	},
	{OperationID: "inbox.message.write", Method: "POST", Pattern: "/api/v1/messages/read-all"}: {
		Reason: "mark-all-read touches only rows inside the token boundary instead of refusing the request, so a project token for another project gets 200 and marks nothing of the fixture project",
		Pin:    "TestInboxToken_MarkAllReadTouchesOnlyVisibleRows",
	},
	{OperationID: "inbox.conversation.list", Method: "GET", Pattern: "/api/v1/conversations"}: {
		Reason: "the conversation list filters rows to the token boundary instead of refusing the request, so a project token for another project gets 200 with no rows",
		Pin:    "TestConversationListToken_FilteredToBoundary",
	},
	{OperationID: "agent.message.target.resolve", Method: "GET", Pattern: "/api/v1/messaging/targets/resolve"}: {
		Reason: "target resolution answers 404 for every caller while cross-project messaging is off, which it is in the matrix server; the pin enables it",
		Pin:    "TestMessagingTargetsResolve_TokenNeedsAgentMessage",
	},
	{OperationID: "inbox.notification.read", Method: "GET", Pattern: "/api/v1/notifications"}: {
		Reason: "the notification list filters rows to the token boundary instead of refusing the request, so a project token for another project gets 200 with no rows",
		Pin:    "TestNotificationToken_RowsFilteredToBoundary",
	},
	{OperationID: "inbox.notification.ack", Method: "POST", Pattern: "/api/v1/notifications/ack-all"}: {
		Reason: "ack-all touches only rows inside the token boundary instead of refusing the request, so a project token for another project gets 200",
		Pin:    "TestNotificationToken_RowsFilteredToBoundary",
	},
	{OperationID: "inbox.notification.subscription.create", Method: "POST", Pattern: "/api/v1/notifications/subscriptions/bulk"}: {
		Reason: "bulk create takes a JSON array, which the matrix body overrides cannot express; with an object body every token gets 400 after the selector check",
		Pin:    "TestNotificationSubscription_RequiresProjectAndAgentRead",
	},
	{OperationID: "inbox.notification.subscription.read", Method: "GET", Pattern: "/api/v1/notifications/subscriptions"}: {
		Reason: "the subscription list filters rows to the token boundary instead of refusing the request, so a project token for another project gets 200 with no rows",
		Pin:    "TestNotificationToken_RowsFilteredToBoundary",
	},
	{OperationID: "inbox.notification.subscription.write", Method: "POST", Pattern: "/api/v1/notifications/subscriptions/bulk-delete"}: {
		Reason: "bulk delete skips rows outside the token boundary instead of refusing the request, so a project token for another project gets 200 with nothing deleted",
		Pin:    "TestNotificationToken_RowsFilteredToBoundary",
	},
	{OperationID: "inbox.notification.template.read", Method: "GET", Pattern: "/api/v1/notifications/templates"}: {
		Reason: "the template list filters rows to readable projects inside the token boundary instead of refusing the request, so a project token for another project gets 200 with no rows",
		Pin:    "TestNotificationTemplates_ListedOnlyForReadableProjects",
	},
	{OperationID: "artifact.list", Method: "GET", Pattern: "/api/v1/artifacts"}: {
		Reason: "the artifact list filters each row by the artifact.read check instead of refusing the request, so a token without the selector, or bounded to another project, gets 200 with no rows rather than 403 or 404 (no existence oracle)",
		Pin:    "TestArtifactsListUserAccessTokensAreBounded",
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
	// The inbox routes act on the caller's own records, so the matrix
	// addresses the super-admin's records in the fixture project.
	ids.inbox = seedInboxRecords(t, ctx, s, adminID, ids.project, ids.agent)

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
		params = bearerMatrixPatternOverrides(m.ids)[ep.Pattern]
	}
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

// bearerMatrixPatternOverrides holds path parameters for entry points the
// live method inventory does not probe (it covers HTTP routes only), so
// that the matrix addresses a seeded record.
func bearerMatrixPatternOverrides(f idFixtures) map[string]map[string]string {
	return map[string]map[string]string{
		"/api/v1/agents/{id}/pty": {"id": f.agent},
	}
}

// bearerMatrixGuardSelector returns the selector of the permission the
// hub-admin route guard checks on the entry point's route, or "" when the
// route has no such guard. A write behind a guard on a read permission
// needs both selectors, so the admitting token carries this one too.
func bearerMatrixGuardSelector(ep authzop.EntryPoint) string {
	best := ""
	var guard RouteMetadata
	for key, meta := range routeMetadataTable {
		_, path := splitRouteKey(key)
		if path == ep.Pattern || (strings.HasSuffix(path, "/") && strings.HasPrefix(ep.Pattern, path)) {
			if len(path) > len(best) {
				best, guard = path, meta
			}
		}
	}
	if best == "" || guard.Classification != RouteHubAdmin || guard.Permission == "" {
		return ""
	}
	return bearerMatrixSelector(guard.Permission)
}

// bearerMatrixCompanionSelectors lists, per operation, the selectors an
// admitting token carries besides the operation's own: the operation also
// checks project:read or agent:read on a target the record names.
var bearerMatrixCompanionSelectors = map[authzop.OperationID][]string{
	"inbox.conversation.create":              {"project:read"},
	"inbox.conversation.direct.read":         {"agent:read"},
	"inbox.conversation.defaultagent.set":    {"project:read"},
	"inbox.conversation.participant.add":     {"project:read"},
	"inbox.conversation.resolve":             {"project:read"},
	"inbox.notification.subscription.create": {"project:read"},
	"inbox.notification.template.create":     {"project:read"},
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
		// Inbox writes name the fixture project and agent.
		{"inbox.conversation.create", "/api/v1/conversations"}:                              {"displayName": f.inbox.createConversations + "-a", "projectId": f.project},
		{"inbox.conversation.create", "/api/v1/conversations/"}:                             {"displayName": f.inbox.createConversations + "-b", "projectId": f.project},
		{"inbox.conversation.defaultagent.set", "/api/v1/conversations/{id}/default-agent"}: {"agentId": f.agent},
		{"inbox.conversation.participant.add", "/api/v1/conversations/{id}/participants"}:   {"principalKind": "agent", "principalId": f.agent},
		{"inbox.notification.subscription.create", "/api/v1/notifications/subscriptions"}: {
			"projectId": f.project, "scope": "project", "triggerActivities": []string{"COMPLETED"},
		},
		{"inbox.notification.subscription.write", "/api/v1/notifications/subscriptions/{id}"}: {"triggerActivities": []string{"FAILED"}},
		{"inbox.notification.template.create", "/api/v1/notifications/templates"}: {
			"name": f.inbox.createConversations + "-template", "triggerActivities": []string{"COMPLETED"}, "projectId": f.project,
		},
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
//   - admit: a hub token with the selector (plus the route guard's
//     selector when the hub-admin guard checks a different permission, and
//     the operation's bearerMatrixCompanionSelectors) and live authority is not refused and reaches the seeded target (no 401, 403 or 404; a 5xx
//     only on a row listed in bearerMatrixPositiveServerErrors); a token
//     of the same user with an unrelated selector, and a project token for
//     another project, are refused (403, or 404 on a GET, where read
//     handlers hide the record); each row's three statuses are logged;
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
			ceilingRec := m.request(t, e, unrelated)
			if !bearerMatrixRefused(ep.Method, ceilingRec.Code) {
				t.Errorf("%s: a token without %s got %d, want %s: %s", label, sel, ceilingRec.Code, bearerMatrixRefusal(ep.Method), ceilingRec.Body.String())
			} else if ceilingRec.Code == http.StatusNotFound {
				refusedWith404 = true
			}

			// (c) boundary: a project token for another project, or for a
			// hub-only permission, a project token for the fixture project.
			// The super-admin owns the other project, so that token must
			// mint; only a hub-only selector may be refused at mint, which
			// enforces the boundary there.
			hubOnly := !bearerMatrixHasBoundary(d, authzop.BearerBoundaryProject)
			boundProject := m.otherProject
			if hubOnly {
				boundProject = m.ids.project
			}
			boundaryStatus := "not minted"
			if key := m.tryMint(projectBoundary(boundProject), []string{sel}); key != "" {
				rec := m.request(t, e, key)
				boundaryStatus = strconv.Itoa(rec.Code)
				if rec.Code == http.StatusNotFound {
					refusedWith404 = true
				}
				if !bearerMatrixRefused(ep.Method, rec.Code) {
					t.Errorf("%s: a project token for %s got %d, want %s: %s", label, boundProject, rec.Code, bearerMatrixRefusal(ep.Method), rec.Body.String())
				}
			} else if hubOnly {
				t.Logf("%s: a project token for %s with %s cannot be minted; the boundary is enforced at mint", label, boundProject, sel)
			} else {
				t.Errorf("%s: a project token for %s with %s must mint for the boundary probe", label, boundProject, sel)
			}

			// (a) positive: the selector on an allowed boundary.
			boundary := hubBoundary()
			if !bearerMatrixHasBoundary(d, authzop.BearerBoundaryHub) {
				boundary = projectBoundary(m.ids.project)
			}
			positive := []string{sel}
			if guardSel := bearerMatrixGuardSelector(ep); guardSel != "" && guardSel != sel {
				positive = append(positive, guardSel)
			}
			positive = append(positive, bearerMatrixCompanionSelectors[e.Spec.ID]...)
			sort.Strings(positive)
			rec := m.request(t, e, m.mint(t, boundary, positive))
			t.Logf("admit row %s: ceiling=%d boundary=%s positive=%d with %v", label, ceilingRec.Code, boundaryStatus, rec.Code, positive)
			if rec.Code == http.StatusUnauthorized || rec.Code == http.StatusForbidden {
				t.Errorf("%s: a %s token with %s got %d, want neither 401 nor 403: %s", label, boundary.Kind, sel, rec.Code, rec.Body.String())
			}
			// The admitting token must reach the seeded target, so a 404
			// fails every row. A WebSocket upgrade cannot return 2xx to a
			// plain request, so for a WebSocket row a status other than
			// 401, 403 or 404 shows both authorization and existence; the
			// refusing tokens above got 403 or 404 on the same target.
			if rec.Code == http.StatusNotFound {
				t.Errorf("%s: the admitting token got 404, so the seeded target is not reached: %s", label, rec.Body.String())
			}
			// A positive 5xx is expected only on the rows listed in
			// bearerMatrixPositiveServerErrors, with exactly that status,
			// and never carries an authorization or credential error code.
			if want, ok := bearerMatrixPositiveServerErrors[e.key()]; ok {
				if rec.Code != want.Status {
					t.Errorf("%s: the admitting token got %d, want %d (%s): %s", label, rec.Code, want.Status, want.Reason, rec.Body.String())
				}
			} else if rec.Code >= 500 {
				t.Errorf("%s: the admitting token got %d, and the row is not in bearerMatrixPositiveServerErrors: %s", label, rec.Code, rec.Body.String())
			}
			if rec.Code >= 500 {
				if code := bearerMatrixErrorCode(rec); bearerMatrixAuthzErrorCodes[code] {
					t.Errorf("%s: the admitting token got %d with authorization or credential code %q: %s", label, rec.Code, code, rec.Body.String())
				}
			}
			// A 404 counts as a refusal only when the admitting token
			// proves the same target exists: a 2xx, or for a WebSocket
			// entry point (which cannot answer 2xx to a plain request) any
			// status other than 404, since its handler answers 404 only
			// when the target is missing and any later status (no upgrade,
			// no runtime broker) follows the lookup.
			if refusedWith404 {
				found := rec.Code >= 200 && rec.Code < 300 ||
					(ep.Kind == authzop.EntryPointWebSocket && rec.Code != http.StatusNotFound)
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

// bearerMatrixPositiveServerError is the status an admitting token gets on
// a row whose handler fails after authorization in the test server.
type bearerMatrixPositiveServerError struct {
	Status int
	Reason string
}

// bearerMatrixPositiveServerErrors lists every admit row whose admitting
// token gets a 5xx in the test server, with the status and the reason the
// handler fails after its authorization check.
var bearerMatrixPositiveServerErrors = map[liveInventoryKey]bearerMatrixPositiveServerError{
	{"agent.lifecycle.resetauth", http.MethodPost, "/api/v1/agents/{id}/reset-auth"}: {
		http.StatusInternalServerError, "the test server configures no agent dispatcher"},
	{"agent.lifecycle.resetauth", http.MethodPost, "/api/v1/projects/{projectId}/agents/{id}/reset-auth"}: {
		http.StatusInternalServerError, "the test server configures no agent dispatcher"},
	{"agent.lifecycle.exec", http.MethodPost, "/api/v1/agents/{id}/exec"}: {
		http.StatusServiceUnavailable, "the test server configures no agent dispatcher"},
	{"agent.lifecycle.exec", http.MethodPost, "/api/v1/projects/{projectId}/agents/{id}/exec"}: {
		http.StatusServiceUnavailable, "the test server configures no agent dispatcher"},
	{"template.update", http.MethodPut, "/api/v1/templates/{id}"}: {
		http.StatusInternalServerError, "the empty update body fails at the store write"},
	{"harnessconfig.update", http.MethodPut, "/api/v1/harness-configs/{id}"}: {
		http.StatusInternalServerError, "the empty update body fails at the store write"},
	{"hub.config.update", http.MethodPut, "/api/v1/admin/server-config"}: {
		http.StatusInternalServerError, "the test server has no writable settings file"},
	{"hub.config.update", http.MethodPatch, "/api/v1/admin/server-config"}: {
		http.StatusInternalServerError, "the test server has no writable settings file"},
	{"hub.config.update", http.MethodPost, "/api/v1/admin/server-config"}: {
		http.StatusInternalServerError, "the test server has no writable settings file"},
	{"hub.messaging.update", http.MethodPut, "/api/v1/admin/messaging"}: {
		http.StatusNotImplemented, "the test server configures no operational settings"},
	{"hub.experiments.update", http.MethodGet, "/api/v1/admin/experiments"}: {
		http.StatusServiceUnavailable, "the test server configures no operational settings"},
	{"hub.experiments.update", http.MethodPut, "/api/v1/admin/experiments"}: {
		http.StatusServiceUnavailable, "the test server configures no operational settings"},
	{"hub.experiments.update", http.MethodDelete, "/api/v1/admin/experiments"}: {
		http.StatusServiceUnavailable, "the test server configures no operational settings"},
	{"hub.projectdefaults.update", http.MethodPut, "/api/v1/admin/project-defaults"}: {
		http.StatusNotImplemented, "the test server configures no operational settings"},
	{"hub.projectdefaults.update", http.MethodPatch, "/api/v1/admin/project-defaults"}: {
		http.StatusNotImplemented, "the test server configures no operational settings"},
	{"hub.projectdefaults.update", http.MethodPost, "/api/v1/admin/project-defaults"}: {
		http.StatusNotImplemented, "the test server configures no operational settings"},
}

// bearerMatrixAuthzErrorCodes are error codes that report an authorization
// or credential refusal; an admitting token's 5xx never carries one.
var bearerMatrixAuthzErrorCodes = map[string]bool{
	ErrCodeUnauthorized:                     true,
	ErrCodeForbidden:                        true,
	ErrCodeMembershipCredentialInsufficient: true,
	ErrCodeRoleAssignmentForbidden:          true,
	ErrCodeMutationPermissionLost:           true,
	ErrCodeSecretScopeRestricted:            true,
	ErrCodeMessageDenied:                    true,
	ErrCodePrincipalIneligible:              true,
}

// bearerMatrixErrorCode returns error.code of an error response, or "".
func bearerMatrixErrorCode(rec *httptest.ResponseRecorder) string {
	var resp ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		return ""
	}
	return resp.Error.Code
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
// declared test. Every expected positive server error names an admit row,
// a 5xx status and a reason.
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
	admitRows := map[liveInventoryKey]bool{}
	for _, e := range bearerMatrixEntries() {
		if e.Spec.Bearer.Kind == authzop.BearerAdmit {
			admitRows[e.key()] = true
		}
	}
	for key, want := range bearerMatrixPositiveServerErrors {
		if !admitRows[key] {
			t.Errorf("stale positive server error row %v: no catalog entry point with an admit disposition", key)
		}
		if want.Status < 500 || strings.TrimSpace(want.Reason) == "" {
			t.Errorf("positive server error row %v needs a 5xx status and a reason", key)
		}
	}
	for _, spec := range authzop.Catalog {
		if pin := spec.Bearer.Pin; pin != "" && !declared[pin] {
			t.Errorf("operation %s: bearer Pin %q is not a declared test in pkg/hub", spec.ID, pin)
		}
	}
}
