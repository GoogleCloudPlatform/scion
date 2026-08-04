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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

// Coverage for the top-level /api/v1/gcp-service-accounts route (P4 item C).
//
// The validation cases carry most of the weight. Each one is a request the
// server could plausibly have repaired instead of refused, and the repair would
// have produced a plausible-looking 200 for a question the client did not ask.

func topLevelSAEmails(t *testing.T, srv *Server, user *store.User, query string) []string {
	t.Helper()
	rec := doRequestAsUser(t, srv, user, http.MethodGet, "/api/v1/gcp-service-accounts?"+query, nil)
	require.Equal(t, http.StatusOK, rec.Code, "list failed: %s", rec.Body.String())

	var resp ListGCPServiceAccountsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	emails := make([]string, 0, len(resp.Items))
	for _, item := range resp.Items {
		emails = append(emails, item.Email)
	}
	return emails
}

func TestGCPSA_TopLevel_ListByProjectScope(t *testing.T) {
	srv, s, owner, _, _, project := setupGCPAuthzTest(t)
	ctx := context.Background()

	mine, hub := seedListMix(t, ctx, s, owner, project)

	emails := topLevelSAEmails(t, srv, owner,
		fmt.Sprintf("scope=project&scopeId=%s", project.ID))
	require.ElementsMatch(t, []string{mine}, emails,
		"scope=project must return only that project's SAs; hub-scoped %q leaked", hub)
}

func TestGCPSA_TopLevel_ListByProjectScope_IncludeHubScoped(t *testing.T) {
	srv, s, owner, _, _, project := setupGCPAuthzTest(t)
	ctx := context.Background()

	mine, hub := seedListMix(t, ctx, s, owner, project)

	emails := topLevelSAEmails(t, srv, owner,
		fmt.Sprintf("scope=project&scopeId=%s&includeHubScoped=true", project.ID))
	require.ElementsMatch(t, []string{mine, hub}, emails,
		"the union must add hub-scoped SAs without reaching other projects")
}

// scope=hub carries no scopeId, which is the shape P5's picker sends. The
// assertion that matters is the negative half: no project-scoped account
// appears, so hub scope is a real filter and not a synonym for "everything".
func TestGCPSA_TopLevel_ListByHubScope_NoScopeIDRequired(t *testing.T) {
	srv, s, owner, _, _, project := setupGCPAuthzTest(t)
	ctx := context.Background()

	_, hub := seedListMix(t, ctx, s, owner, project)

	emails := topLevelSAEmails(t, srv, owner, "scope=hub")
	require.ElementsMatch(t, []string{hub}, emails,
		"scope=hub must return hub-scoped SAs and only those")
}

// An ordinary hub member, not just the project owner, can list hub-scoped
// accounts. This is the list-side counterpart of the accepted exposure pinned
// in TestGCPSA_Get_HubScoped_HubMemberCanRead: hub-member-read-all grants
// read+list at hub scope to every user. P5's picker depends on it, so a change
// that narrows it should fail here rather than surface as an empty dropdown.
func TestGCPSA_TopLevel_ListByHubScope_HubMemberCanList(t *testing.T) {
	srv, s, owner, member, _, project := setupGCPAuthzTest(t)
	ctx := context.Background()

	_, hub := seedListMix(t, ctx, s, owner, project)

	emails := topLevelSAEmails(t, srv, member, "scope=hub")
	require.ElementsMatch(t, []string{hub}, emails,
		"an ordinary hub member must be able to list hub-scoped SAs")
}

// Validation. Grouped because the shared property is what matters: each of
// these is refused rather than repaired.
func TestGCPSA_TopLevel_ScopeValidation(t *testing.T) {
	srv, s, owner, _, _, project := setupGCPAuthzTest(t)
	ctx := context.Background()

	seedListMix(t, ctx, s, owner, project)

	cases := []struct {
		name  string
		query string
		why   string
	}{
		{
			name:  "MissingScope",
			query: "",
			why: "no default scope: an unfiltered list would be a cross-project " +
				"enumeration of every SA on the hub, which no existing route offers",
		},
		{
			name:  "UnknownScope",
			query: "scope=global",
			why: "hub scope is spelled \"hub\". \"global\" is the template " +
				"vocabulary and is not silently translated",
		},
		{
			name:  "UserScopeNotSupported",
			query: "scope=user",
			why:   "a real store.Scope value, but not one this route serves",
		},
		{
			name:  "ProjectScopeWithoutScopeID",
			query: "scope=project",
			why:   "would otherwise select every project's SAs at once",
		},
		{
			name:  "HubScopeWithClientSuppliedScopeID",
			query: "scope=hub&scopeId=some-other-hub",
			why: "the server resolves the hub's ID; accepting one from the client " +
				"would let a request name a hub that is not this one",
		},
		{
			name:  "HubScopeWithEmptyScopeID",
			query: "scope=hub&scopeId=",
			why: "presence is what is checked, not emptiness -- otherwise " +
				"?scopeId= slips past a rule that ?scopeId=x does not",
		},
		{
			name:  "IncludeHubScopedWithHubScope",
			query: "scope=hub&includeHubScoped=true",
			why:   "already implied; accepting it would make a no-op look meaningful",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doRequestAsUser(t, srv, owner, http.MethodGet,
				"/api/v1/gcp-service-accounts?"+tc.query, nil)
			require.Equal(t, http.StatusBadRequest, rec.Code,
				"%s: %s (got %s)", tc.name, tc.why, rec.Body.String())
		})
	}
}

// A project that does not exist is a 404, not an empty list. The distinction is
// invisible to the caller otherwise, and a typo'd project ID reading as "this
// project has no service accounts" is the kind of answer that gets believed.
func TestGCPSA_TopLevel_UnknownProjectIs404(t *testing.T) {
	srv, _, owner, _, _, _ := setupGCPAuthzTest(t)

	rec := doRequestAsUser(t, srv, owner, http.MethodGet,
		"/api/v1/gcp-service-accounts?scope=project&scopeId="+tid("project-does-not-exist"), nil)
	require.Equal(t, http.StatusNotFound, rec.Code,
		"unknown project should 404, not return an empty list; got: %s", rec.Body.String())
}

// The top-level create for project scope must be the same operation as the
// nested one, not a parallel implementation of it. Asserted through
// authorization, since that is where a second implementation would most likely
// have diverged: the nested route requires project ActionManage, so this one
// must deny an ordinary member too.
func TestGCPSA_TopLevel_CreateProjectScope_MatchesNestedAuthz(t *testing.T) {
	srv, _, owner, member, _, project := setupGCPAuthzTest(t)

	body := map[string]any{
		"email":     "new-sa@p.iam.gserviceaccount.com",
		"projectId": "gcp-proj",
	}

	rec := doRequestAsUser(t, srv, member, http.MethodPost,
		fmt.Sprintf("/api/v1/gcp-service-accounts?scope=project&scopeId=%s", project.ID), body)
	require.Equal(t, http.StatusForbidden, rec.Code,
		"a project member must not create a project-scoped SA here, same as the nested route; got: %s",
		rec.Body.String())

	rec = doRequestAsUser(t, srv, owner, http.MethodPost,
		fmt.Sprintf("/api/v1/gcp-service-accounts?scope=project&scopeId=%s", project.ID), body)
	require.Equal(t, http.StatusCreated, rec.Code,
		"a project owner should be able to create through the top-level route; got: %s",
		rec.Body.String())

	// And it lands at project scope, not somewhere the scope parameter implied
	// but the handler ignored.
	emails := topLevelSAEmails(t, srv, owner, fmt.Sprintf("scope=project&scopeId=%s", project.ID))
	require.Contains(t, emails, "new-sa@p.iam.gserviceaccount.com")
}

// P9: TestGCPSA_TopLevel_CreateHubScope_NotEnabled removed.
// The tripwire was a deliberate hold until hub-scoped BYO registration could
// safely open. P9 completes that: hub-scoped creation is now enabled for hub
// members (BYO registration), guarded at assignment by mode coupling + actAs.

func TestGCPSA_TopLevel_MethodNotAllowed(t *testing.T) {
	srv, _, owner, _, _, _ := setupGCPAuthzTest(t)

	rec := doRequestAsUser(t, srv, owner, http.MethodDelete, "/api/v1/gcp-service-accounts?scope=hub", nil)
	require.Equal(t, http.StatusMethodNotAllowed, rec.Code, rec.Body.String())
}
