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
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

// P4 item E. Deleting a project must not delete hub-scoped service accounts.
//
// This passes with no production change: the cleanup in deleteProject already
// filters on Scope: ScopeProject / ScopeID: id, and warnManagedGCPServiceAccounts
// does the same. The test exists precisely because it costs nothing today and
// the failure it guards is expensive.
//
// Once hub-scoped SAs exist, the cascade acquires a blast radius it did not
// have when every SA belonged to exactly one project. It enumerates by filter,
// and the natural-sounding widening — drop the Scope term so cleanup "gets
// everything reachable from the project" — is now wrong in a way that produces
// no error at the point of the change: deleting one project would revoke a
// credential every other project on the hub depends on. Scope-blindness is the
// same defect the handler guards fixed, one layer down.
//
// A project-scoped SA in the same project serves as the control, so a green
// result means "the cascade ran and spared hub scope" rather than the much
// weaker "the cascade did not run".
func TestGCPSA_ProjectDelete_DoesNotCascadeHubScoped(t *testing.T) {
	srv, s, _, _, _, project := setupGCPAuthzTest(t)
	ctx := context.Background()

	admin := &store.User{
		ID:          tid("user-cascade-admin"),
		Email:       "cascade-admin@test.com",
		DisplayName: "Cascade Admin",
		Role:        store.UserRoleAdmin,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, admin))
	ensureHubMembership(ctx, s, admin.ID)

	hubSA := newHubScopedSA("sa-hub-survives", "hub-survives@hub.iam.gserviceaccount.com")
	require.NoError(t, s.CreateGCPServiceAccount(ctx, hubSA))

	projectSA := &store.GCPServiceAccount{
		ID:        tid("sa-project-cascades"),
		Scope:     store.ScopeProject,
		ScopeID:   project.ID,
		Email:     "cascades@proj.iam.gserviceaccount.com",
		ProjectID: "gcp-proj",
		CreatedBy: admin.ID,
		CreatedAt: time.Now(),
	}
	require.NoError(t, s.CreateGCPServiceAccount(ctx, projectSA))

	rec := doRequestAsUser(t, srv, admin, http.MethodDelete,
		fmt.Sprintf("/api/v1/projects/%s", project.ID), nil)
	require.Contains(t, []int{http.StatusOK, http.StatusNoContent, http.StatusAccepted}, rec.Code,
		"admin should be able to delete the project; got: %s", rec.Body.String())

	// The hub-scoped SA belongs to the hub and must outlive any one project.
	survived, err := s.GetGCPServiceAccount(ctx, hubSA.ID)
	require.NoError(t, err, "hub-scoped SA must survive deletion of an unrelated project")
	require.Equal(t, hubSA.ID, survived.ID)

	// Control: the project's own SA is gone, so the cascade demonstrably ran.
	_, err = s.GetGCPServiceAccount(ctx, projectSA.ID)
	require.Error(t, err, "project-scoped SA should have been cleaned up with its project")
}
