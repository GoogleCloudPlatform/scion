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

package hub

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

// Handler-level coverage for hub-scoped GCP service accounts (Goal 2 / P4).
//
// These tests exercise the per-project routes against an SA that the project
// does NOT own, because that is the case the pre-Goal-2 code could not express:
// the guards compared sa.ScopeID against the project ID and so rejected every
// hub-scoped SA, and the authorization checks hung on the project resource and
// so would have handed a hub-wide credential to whichever project the caller
// happened to be looking at it through.

// newHubScopedSA builds a hub-scoped SA owned by nobody in particular. CreatedBy
// is deliberately a stranger: the owner short-circuit in checkAccessForUser
// would otherwise mask which branch of the authorization actually fired.
func newHubScopedSA(idName, email string) *store.GCPServiceAccount {
	return &store.GCPServiceAccount{
		ID:                 tid(idName),
		Scope:              store.ScopeHub,
		ScopeID:            "hub-instance-1",
		Email:              email,
		ProjectID:          "hub-gcp-project",
		DisplayName:        "Hub-wide SA",
		Verified:           true,
		VerifiedAt:         time.Now(),
		VerificationStatus: store.GCPVerificationVerified,
		CreatedBy:          tid("user-somebody-else"),
		CreatedAt:          time.Now(),
	}
}

// A hub member can read a hub-scoped SA through a project that does not own it.
//
// This asserts an ACCEPTED EXPOSURE, not a designed permission, and the
// distinction is the reason the test exists. seed.go:51 seeds
// hub-member-read-all as ScopeType "hub" / ResourceType "*" / [read, list] /
// allow, bound to the hub-members group that every user joins on login; and
// matchesResource has no case "hub" arm, so a hub-scoped policy falls straight
// through its switch. The consequence is that every authenticated user has
// read+list on every hub-scoped resource, service accounts included. sa-arch
// ruled on 2026-07-28 to accept this rather than carve service accounts out of
// a seeded hub-wide policy, on the grounds that it is read+list only and
// assignment stays gated on ActionAssign + CanActAs.
//
// It is pinned here because it is currently load-bearing — the P5 service
// account picker depends on ordinary members being able to list hub-scoped SAs
// — and because nothing else asserts it. If someone later narrows
// hub-member-read-all, or adds a case "hub" arm to matchesResource, this test
// fails and that consequence becomes visible rather than silent.
//
// Note the inverse test is deliberately ABSENT. Asserting that a project owner
// is DENIED read here would fail today, and the cheapest way to make such a
// test pass would be to give hub-scoped SAs a project parent — which is
// precisely the #595 defect, arrived at through a green test.
func TestGCPSA_Get_HubScoped_HubMemberCanRead(t *testing.T) {
	srv, s, _, member, _, project := setupGCPAuthzTest(t)
	ctx := context.Background()

	sa := newHubScopedSA("sa-hub-read", "hub-read@hub.iam.gserviceaccount.com")
	require.NoError(t, s.CreateGCPServiceAccount(ctx, sa))

	rec := doRequestAsUser(t, srv, member, http.MethodGet,
		fmt.Sprintf("/api/v1/projects/%s/gcp-service-accounts/%s", project.ID, sa.ID), nil)
	require.Equal(t, http.StatusOK, rec.Code,
		"a hub member must be able to read a hub-scoped SA through any project (hub-member-read-all); got: %s",
		rec.Body.String())
}

// The security property of the phase: reaching a hub-scoped SA through a
// project route must not confer the project's management rights over it.
//
// The project OWNER is the sharp case. Before P0.2 the SA resource carried
// ParentType "project" unconditionally, so the project owner/admin bypass in
// checkAccessForUser fired on any SA the owner could name. The owner is
// therefore the principal that a scope-blind implementation would wrongly
// allow, and an ordinary member would not distinguish the two.
func TestGCPSA_Delete_HubScoped_ProjectOwnerDenied(t *testing.T) {
	srv, s, owner, _, _, project := setupGCPAuthzTest(t)
	ctx := context.Background()

	sa := newHubScopedSA("sa-hub-del", "hub-del@hub.iam.gserviceaccount.com")
	require.NoError(t, s.CreateGCPServiceAccount(ctx, sa))

	rec := doRequestAsUser(t, srv, owner, http.MethodDelete,
		fmt.Sprintf("/api/v1/projects/%s/gcp-service-accounts/%s", project.ID, sa.ID), nil)
	require.Equal(t, http.StatusForbidden, rec.Code,
		"a project owner must NOT delete a hub-scoped SA; got: %s", rec.Body.String())

	// The denial must be real, not merely a status code: the row survives.
	still, err := s.GetGCPServiceAccount(ctx, sa.ID)
	require.NoError(t, err, "hub-scoped SA must still exist after a denied delete")
	require.Equal(t, sa.ID, still.ID)
}

// Same boundary on the verify path. Verify is a mutation — it rewrites
// verified / verified_at / verification_status — so it must not be reachable
// on a hub-wide credential by a project owner either. Asserted at the handler
// rather than by reasoning from the action set, because read+list comes from
// the seeded policy and the absence of a delete/verify grant is a property of
// that seeded set rather than of the code under test.
func TestGCPSA_Verify_HubScoped_ProjectOwnerDenied(t *testing.T) {
	srv, s, owner, _, _, project := setupGCPAuthzTest(t)
	ctx := context.Background()

	sa := newHubScopedSA("sa-hub-verify", "hub-verify@hub.iam.gserviceaccount.com")
	require.NoError(t, s.CreateGCPServiceAccount(ctx, sa))

	rec := doRequestAsUser(t, srv, owner, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/gcp-service-accounts/%s/verify", project.ID, sa.ID), nil)
	require.Equal(t, http.StatusForbidden, rec.Code,
		"a project owner must NOT verify a hub-scoped SA; got: %s", rec.Body.String())
}

// The positive half of the delete boundary: a hub admin can. Without this the
// denial test above is satisfiable by a handler that rejects everyone, which
// would pass while making hub-scoped SAs undeletable.
func TestGCPSA_Delete_HubScoped_AdminAllowed(t *testing.T) {
	srv, s, _, _, _, project := setupGCPAuthzTest(t)
	ctx := context.Background()

	admin := &store.User{
		ID:          tid("user-hub-admin"),
		Email:       "hub-admin@test.com",
		DisplayName: "Hub Admin",
		Role:        store.UserRoleAdmin,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, admin))
	ensureHubMembership(ctx, s, admin.ID)

	sa := newHubScopedSA("sa-hub-del-admin", "hub-del-admin@hub.iam.gserviceaccount.com")
	require.NoError(t, s.CreateGCPServiceAccount(ctx, sa))

	rec := doRequestAsUser(t, srv, admin, http.MethodDelete,
		fmt.Sprintf("/api/v1/projects/%s/gcp-service-accounts/%s", project.ID, sa.ID), nil)
	require.Equal(t, http.StatusNoContent, rec.Code,
		"a hub admin must be able to delete a hub-scoped SA; got: %s", rec.Body.String())
}

// Regression guard on the other half of gcpSAReachableFromProject. Opening the
// guard to hub scope must not open it to somebody else's project: the rewrite
// went from "ScopeID must equal this project" to a switch on Scope, and the
// project arm has to keep the equality it replaced.
func TestGCPSA_Get_ProjectScoped_CrossProjectNotFound(t *testing.T) {
	srv, s, owner, _, _, project := setupGCPAuthzTest(t)
	ctx := context.Background()

	other := &store.Project{
		ID:        tid("project-gcp-other"),
		Name:      "Other Project",
		Slug:      "other-project",
		OwnerID:   owner.ID,
		CreatedBy: owner.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, other))

	sa := &store.GCPServiceAccount{
		ID:        tid("sa-other-project"),
		Scope:     store.ScopeProject,
		ScopeID:   other.ID,
		Email:     "other@proj.iam.gserviceaccount.com",
		ProjectID: "gcp-proj",
		CreatedBy: owner.ID,
		CreatedAt: time.Now(),
	}
	require.NoError(t, s.CreateGCPServiceAccount(ctx, sa))

	rec := doRequestAsUser(t, srv, owner, http.MethodGet,
		fmt.Sprintf("/api/v1/projects/%s/gcp-service-accounts/%s", project.ID, sa.ID), nil)
	require.Equal(t, http.StatusNotFound, rec.Code,
		"a project-scoped SA must not be reachable through a different project; got: %s", rec.Body.String())
}
