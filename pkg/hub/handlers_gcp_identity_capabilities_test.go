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
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type projectSAListCaps struct {
	scope []string
	items map[string][]string
}

func listProjectSACapsAs(t *testing.T, srv *Server, user *store.User, projectID string) projectSAListCaps {
	t.Helper()
	return listSACapsAt(t, srv, user, fmt.Sprintf("/api/v1/projects/%s/gcp-service-accounts", projectID))
}

func listSACapsAt(t *testing.T, srv *Server, user *store.User, path string) projectSAListCaps {
	t.Helper()
	rec := doRequestAsUser(t, srv, user, http.MethodGet, path, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp struct {
		Items []struct {
			ID  string        `json:"id"`
			Cap *Capabilities `json:"_capabilities"`
		} `json:"items"`
		Cap *Capabilities `json:"_capabilities"`
	}
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	out := projectSAListCaps{items: map[string][]string{}}
	if resp.Cap != nil {
		out.scope = resp.Cap.Actions
	}
	for _, it := range resp.Items {
		if it.Cap != nil {
			out.items[it.ID] = it.Cap.Actions
		} else {
			out.items[it.ID] = nil
		}
	}
	return out
}

// TestListProjectGCPServiceAccounts_CapabilitiesMatchHandlers checks the
// register (create), mint, delete and verify capabilities the project list
// reports for a project owner, a project admin and plain members, on
// accounts the caller did and did not register, and that each reported value
// agrees with what the matching action handler does.
func TestListProjectGCPServiceAccounts_CapabilitiesMatchHandlers(t *testing.T) {
	srv, s, _ := testServerWithMinting(t)
	owner, _, project := setupDemoPolicyOn(t, srv, s)
	ctx := context.Background()

	admin := makeProjectMemberUser(t, s, project, tid("sa-caps-admin"), "Admin", store.GroupMemberRoleAdmin)
	member := makeProjectMemberUser(t, s, project, tid("sa-caps-member"), "Member", store.GroupMemberRoleMember)
	registrar := makeProjectMemberUser(t, s, project, tid("sa-caps-registrar"), "Registrar", store.GroupMemberRoleMember)

	// One account registered by a plain member, one by someone else entirely,
	// so neither the owner nor the admin registered any of them.
	byRegistrar := &store.GCPServiceAccount{
		ID: tid("sa-caps-by-registrar"), Scope: store.ScopeProject, ScopeID: project.ID,
		Email: "by-registrar@gcp-proj.iam.gserviceaccount.com", ProjectID: "gcp-proj",
		CreatedBy: registrar.ID,
	}
	byOther := &store.GCPServiceAccount{
		ID: tid("sa-caps-by-other"), Scope: store.ScopeProject, ScopeID: project.ID,
		Email: "by-other@gcp-proj.iam.gserviceaccount.com", ProjectID: "gcp-proj",
		CreatedBy: tid("sa-caps-someone-else"),
	}
	require.NoError(t, s.CreateGCPServiceAccount(ctx, byRegistrar))
	require.NoError(t, s.CreateGCPServiceAccount(ctx, byOther))

	type want struct {
		create, mint, delete, verify bool
	}
	cases := []struct {
		name string
		user *store.User
		sa   map[string]want // per account: delete/verify only
		col  want            // collection: create/mint only
	}{
		{"owner", owner,
			map[string]want{byRegistrar.ID: {delete: true, verify: true}, byOther.ID: {delete: true, verify: true}},
			want{create: true, mint: true}},
		{"admin", admin,
			map[string]want{byRegistrar.ID: {delete: true, verify: true}, byOther.ID: {delete: true, verify: true}},
			want{create: true, mint: true}},
		{"member", member,
			map[string]want{byRegistrar.ID: {}, byOther.ID: {}},
			want{}},
		{"member_who_registered", registrar,
			map[string]want{byRegistrar.ID: {}, byOther.ID: {}},
			want{}},
	}

	paths := map[string]string{
		"nested": fmt.Sprintf("/api/v1/projects/%s/gcp-service-accounts", project.ID),
		"flat":   fmt.Sprintf("/api/v1/gcp-service-accounts?scope=project&scopeId=%s", project.ID),
	}
	for _, tc := range cases {
		for route, path := range paths {
			t.Run(tc.name+"/"+route, func(t *testing.T) {
				got := listSACapsAt(t, srv, tc.user, path)
				assert.Equal(t, tc.col.create, contains(got.scope, "create"), "reported create")
				assert.Equal(t, tc.col.mint, contains(got.scope, "mint"), "reported mint")
				for id, w := range tc.sa {
					caps, ok := got.items[id]
					require.True(t, ok, "account %s listed", id)
					assert.Equal(t, w.delete, contains(caps, "delete"), "reported delete on %s", id)
					assert.Equal(t, w.verify, contains(caps, "verify"), "reported verify on %s", id)
					// Read is reported as before.
					assert.Contains(t, caps, "read", "reported read on %s", id)
				}
			})
		}
	}

	// Where an action is not reported, its handler refuses it.
	for _, u := range []*store.User{member, registrar} {
		rec := doRequestAsUser(t, srv, u, http.MethodPost,
			fmt.Sprintf("/api/v1/projects/%s/gcp-service-accounts/mint", project.ID), map[string]string{})
		assert.Equal(t, http.StatusForbidden, rec.Code, "mint as %s", u.DisplayName)

		rec = doRequestAsUser(t, srv, u, http.MethodPost,
			fmt.Sprintf("/api/v1/projects/%s/gcp-service-accounts", project.ID),
			map[string]string{"email": "new@gcp-proj.iam.gserviceaccount.com"})
		assert.Equal(t, http.StatusForbidden, rec.Code, "create as %s", u.DisplayName)

		for _, id := range []string{byRegistrar.ID, byOther.ID} {
			rec = doRequestAsUser(t, srv, u, http.MethodPost,
				fmt.Sprintf("/api/v1/projects/%s/gcp-service-accounts/%s/verify", project.ID, id), nil)
			assert.Equal(t, http.StatusForbidden, rec.Code, "verify %s as %s", id, u.DisplayName)

			rec = doRequestAsUser(t, srv, u, http.MethodDelete,
				fmt.Sprintf("/api/v1/projects/%s/gcp-service-accounts/%s", project.ID, id), nil)
			assert.Equal(t, http.StatusForbidden, rec.Code, "delete %s as %s", id, u.DisplayName)
		}
	}

	// Where an action is reported, its handler allows it.
	rec := doRequestAsUser(t, srv, admin, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/gcp-service-accounts/%s/verify", project.ID, byOther.ID), nil)
	assert.Equal(t, http.StatusOK, rec.Code, "verify as admin: %s", rec.Body.String())
	rec = doRequestAsUser(t, srv, admin, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/gcp-service-accounts/mint", project.ID), map[string]string{})
	assert.Equal(t, http.StatusCreated, rec.Code, "mint as admin: %s", rec.Body.String())
	rec = doRequestAsUser(t, srv, owner, http.MethodDelete,
		fmt.Sprintf("/api/v1/projects/%s/gcp-service-accounts/%s", project.ID, byRegistrar.ID), nil)
	assert.Equal(t, http.StatusNoContent, rec.Code, "delete as owner: %s", rec.Body.String())
}

// TestListProjectGCPServiceAccounts_MintNotReportedWhenUnconfigured checks
// that an owner is not offered mint when the mint handler would refuse it
// because minting is not configured, while register is still offered.
func TestListProjectGCPServiceAccounts_MintNotReportedWhenUnconfigured(t *testing.T) {
	srv, s := testServer(t)
	owner, _, project := setupDemoPolicyOn(t, srv, s)

	got := listProjectSACapsAs(t, srv, owner, project.ID)
	assert.True(t, contains(got.scope, "create"), "reported create")
	assert.False(t, contains(got.scope, "mint"), "reported mint")

	rec := doRequestAsUser(t, srv, owner, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/gcp-service-accounts/mint", project.ID), map[string]string{})
	assert.NotEqual(t, http.StatusCreated, rec.Code)
}
