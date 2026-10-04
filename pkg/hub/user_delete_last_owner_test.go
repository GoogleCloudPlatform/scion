// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
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
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for ptone/scion#2598: deleting a user must not orphan a project, and
// a deleted user's role bindings must be removed with the user.

// requireLastOwnerDenial asserts a 409 last_owner response whose
// details.projects lists exactly the given project.
func requireLastOwnerDenial(t *testing.T, rec *httptest.ResponseRecorder, project *store.Project) {
	t.Helper()
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	var resp struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Details struct {
				Projects []lastOwnerProjectRef `json:"projects"`
			} `json:"details"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp), rec.Body.String())
	assert.Equal(t, ErrCodeLastOwner, resp.Error.Code)
	assert.NotEmpty(t, resp.Error.Message)
	assert.Equal(t, []lastOwnerProjectRef{{ID: project.ID, Name: project.Name}}, resp.Error.Details.Projects)
}

// allBindingsFor returns every role binding (any scope) held by the user.
func allBindingsFor(t *testing.T, s store.Store, userID string) []*store.RoleBinding {
	t.Helper()
	got, err := s.ListRoleBindingsForPrincipal(context.Background(), store.RoleBindingPrincipalUser, userID)
	require.NoError(t, err)
	return got
}

// grantSystemRole gives the user a system-scoped binding for the named
// system role directly in the store.
func grantSystemRole(t *testing.T, s store.Store, userID, roleName string) {
	t.Helper()
	ctx := context.Background()
	rd, err := s.GetRoleDefinitionByName(ctx, roleName, store.RoleScopeSystem)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      userID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        "test",
	})
	require.NoError(t, err)
}

func TestDeleteUser_SoleProjectOwnerDenied(t *testing.T) {
	srv, s, alice, _, project := setupDemoPolicyTest(t)
	ctx := context.Background()
	requireSingleOwnerBinding(t, s, project.ID, alice.ID)
	before := allBindingsFor(t, s, alice.ID)
	require.NotEmpty(t, before)

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/users/"+alice.ID, nil)
	requireLastOwnerDenial(t, rec, project)

	_, err := s.GetUser(ctx, alice.ID)
	require.NoError(t, err, "denied delete must keep the user")
	assert.Len(t, allBindingsFor(t, s, alice.ID), len(before), "denied delete must keep the bindings")
	requireSingleOwnerBinding(t, s, project.ID, alice.ID)
}

func TestDeleteUser_CoOwnerAllowedAndBindingsCascaded(t *testing.T) {
	srv, s, alice, bob, project := setupDemoPolicyTest(t)
	ctx := context.Background()
	addProjectOwner(t, srv, s, alice, bob, project.ID)
	grantSystemRole(t, s, alice.ID, store.SystemRoleHubViewer)
	grantSystemRole(t, s, alice.ID, store.SystemRoleGlobalCatalogAuthor)

	var scopes = map[string]bool{}
	for _, b := range allBindingsFor(t, s, alice.ID) {
		scopes[b.ScopeType] = true
	}
	require.True(t, scopes[store.RoleScopeProject] && scopes[store.RoleScopeSystem],
		"precondition: alice holds project and system/hub bindings")

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/users/"+alice.ID, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	_, err := s.GetUser(ctx, alice.ID)
	require.Error(t, err, "user should be deleted")
	assert.Empty(t, allBindingsFor(t, s, alice.ID), "deleted user's project, hub and system bindings must be removed")
	requireSingleOwnerBinding(t, s, project.ID, bob.ID)
}

func TestDeleteUser_ExpiredOwnerBindingOnOwnerlessProjectDenied(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	bob := &store.User{ID: tid("user-bob"), Email: "bob@test.com", DisplayName: "Bob",
		Role: store.UserRoleMember, Status: "active", Created: time.Now()}
	require.NoError(t, s.CreateUser(ctx, bob))
	project := &store.Project{ID: tid("project-expired"), Name: "Expired Owner Project", Slug: "expired-owner-project",
		Created: time.Now(), Updated: time.Now()}
	require.NoError(t, s.CreateProject(ctx, project))

	ownerRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleOwner, store.RoleScopeProject)
	require.NoError(t, err)
	expired := time.Now().Add(-time.Hour)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: ownerRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      bob.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          project.ID,
		ExpiresAt:        &expired,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	// Deleting the expired binding would take the project to zero owner
	// bindings, which re-arms the startup backfill: deny.
	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/users/"+bob.ID, nil)
	requireLastOwnerDenial(t, rec, project)

	_, err = s.GetUser(ctx, bob.ID)
	require.NoError(t, err)
	assert.Len(t, projectBindingsFor(t, s, project.ID, bob.ID), 1)
}

// Regression lock for the interaction between ptone/scion#2598's two
// problems (from the investigator's TestRepro2598): the delete of a sole
// owner is denied, so the owner binding survives and a restart backfill does
// not re-grant the removed creator.
func TestDeleteUser_SoleOwnerThenRestartDoesNotRegrantCreator(t *testing.T) {
	srv, s, alice, bob, project := setupDemoPolicyTest(t)
	ctx := context.Background()
	transferAndRemoveCreator(t, srv, s, alice, bob, project)
	requireSingleOwnerBinding(t, s, project.ID, bob.ID)

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/users/"+bob.ID, nil)
	requireLastOwnerDenial(t, rec, project)
	_, err := s.GetUser(ctx, bob.ID)
	require.NoError(t, err)

	// Restart.
	require.NoError(t, BackfillRoleBindings(ctx, s))
	assert.Empty(t, projectBindingsFor(t, s, project.ID, alice.ID),
		"backfill must not re-grant the removed creator")
	requireSingleOwnerBinding(t, s, project.ID, bob.ID)
}

// newInvitedUser creates a user in invited status, as the allow-list
// endpoints manage.
func newInvitedUser(t *testing.T, s store.Store, id, email string) *store.User {
	t.Helper()
	u := &store.User{ID: tid(id), Email: email, DisplayName: id,
		Role: store.UserRoleMember, Status: store.UserStatusInvited, Created: time.Now()}
	require.NoError(t, s.CreateUser(context.Background(), u))
	return u
}

func TestDeprecatedAllowListDelete_CascadesRoleBindings(t *testing.T) {
	srv, s, alice, _, project := setupDemoPolicyTest(t)
	ctx := context.Background()
	carol := newInvitedUser(t, s, "user-carol", "carol@test.com")

	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: memberRD.ID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: carol.ID,
		ScopeType: store.RoleScopeProject, ScopeID: project.ID, CreatedBy: alice.ID,
	})
	require.NoError(t, err)
	require.NotEmpty(t, allBindingsFor(t, s, carol.ID))

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/admin/allow-list/"+carol.Email, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	_, err = s.GetUser(ctx, carol.ID)
	require.Error(t, err, "invited user should be deleted")
	assert.Empty(t, allBindingsFor(t, s, carol.ID), "invited user's bindings must be removed")
	requireSingleOwnerBinding(t, s, project.ID, alice.ID)
}

func TestDeprecatedAllowListDelete_SoleProjectOwnerDenied(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	carol := newInvitedUser(t, s, "user-carol", "carol@test.com")
	project := &store.Project{ID: tid("project-carol"), Name: "Carol Project", Slug: "carol-project",
		CreatedBy: carol.ID, OwnerID: carol.ID, Created: time.Now(), Updated: time.Now()}
	require.NoError(t, s.CreateProject(ctx, project))
	srv.seedProjectCreatorMembership(ctx, project)
	requireSingleOwnerBinding(t, s, project.ID, carol.ID)

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/admin/allow-list/"+carol.Email, nil)
	requireLastOwnerDenial(t, rec, project)

	_, err := s.GetUser(ctx, carol.ID)
	require.NoError(t, err, "denied delete must keep the invited user")
	requireSingleOwnerBinding(t, s, project.ID, carol.ID)
}

// createOwnerBinding gives the principal a project-owner binding directly in
// the store, optionally pending (notBefore) or expired (expiresAt).
func createOwnerBinding(t *testing.T, s store.Store, userID, projectID string, notBefore, expiresAt *time.Time) {
	t.Helper()
	ctx := context.Background()
	ownerRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleOwner, store.RoleScopeProject)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: ownerRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      userID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		NotBefore:        notBefore,
		ExpiresAt:        expiresAt,
		CreatedBy:        "test",
	})
	require.NoError(t, err)
}

// newTestProject creates a bare project directly in the store.
func newTestProject(t *testing.T, s store.Store, id, name string) *store.Project {
	t.Helper()
	p := &store.Project{ID: tid(id), Name: name, Slug: id, Created: time.Now(), Updated: time.Now()}
	require.NoError(t, s.CreateProject(context.Background(), p))
	return p
}

// Sole owner of P1 and co-owner of P2: the denial lists only P1, and nothing
// changes on either project.
func TestDeleteUser_SoleOnOneCoOwnerOnAnotherDeniesListingOnlyOrphan(t *testing.T) {
	srv, s, alice, bob, p1 := setupDemoPolicyTest(t)
	ctx := context.Background()
	p2 := newTestProject(t, s, "project-coowned", "Co-owned Project")
	createOwnerBinding(t, s, alice.ID, p2.ID, nil, nil)
	createOwnerBinding(t, s, bob.ID, p2.ID, nil, nil)
	before := allBindingsFor(t, s, alice.ID)

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/users/"+alice.ID, nil)
	requireLastOwnerDenial(t, rec, p1) // details.projects == [p1] exactly

	_, err := s.GetUser(ctx, alice.ID)
	require.NoError(t, err)
	assert.Len(t, allBindingsFor(t, s, alice.ID), len(before), "denied delete must keep every binding")
	assert.Len(t, projectBindingsFor(t, s, p2.ID, alice.ID), 1, "co-owned project's binding must survive")
	assert.Len(t, projectBindingsFor(t, s, p2.ID, bob.ID), 1)
	requireSingleOwnerBinding(t, s, p1.ID, alice.ID)
}

// The only other owner binding is pending (NotBefore in the future) or
// expired: it is not an active owner, so the delete is denied.
func TestDeleteUser_OtherOwnerInactiveDenied(t *testing.T) {
	for _, tc := range []string{"pending", "expired"} {
		t.Run(tc, func(t *testing.T) {
			srv, s, alice, bob, project := setupDemoPolicyTest(t)
			var notBefore, expiresAt *time.Time
			if tc == "pending" {
				ts := time.Now().Add(time.Hour)
				notBefore = &ts
			} else {
				ts := time.Now().Add(-time.Hour)
				expiresAt = &ts
			}
			createOwnerBinding(t, s, bob.ID, project.ID, notBefore, expiresAt)

			rec := doRequest(t, srv, http.MethodDelete, "/api/v1/users/"+alice.ID, nil)
			requireLastOwnerDenial(t, rec, project)

			_, err := s.GetUser(context.Background(), alice.ID)
			require.NoError(t, err)
			assert.Len(t, projectBindingsFor(t, s, project.ID, alice.ID), 1)
			assert.Len(t, projectBindingsFor(t, s, project.ID, bob.ID), 1)
		})
	}
}

// concurrentGrantStore simulates a members-API grant to the target user that
// commits between the guard's binding list and the cascade delete
// (ptone/scion#2770 review M1): inside the delete transaction, right before
// DeleteRoleBindingsForPrincipal runs, it inserts a new project-owner binding
// for the target on grantProjectID, a project the guard never checked.
type concurrentGrantStore struct {
	store.Store
	targetUserID   string
	grantProjectID string
	injected       bool
}

func (c *concurrentGrantStore) WithTx(ctx context.Context, fn func(tx store.Store) error) error {
	return c.Store.WithTx(ctx, func(tx store.Store) error {
		return fn(&concurrentGrantTx{Store: tx, parent: c})
	})
}

type concurrentGrantTx struct {
	store.Store
	parent *concurrentGrantStore
}

func (c *concurrentGrantTx) DeleteRoleBindingsForPrincipal(ctx context.Context, principalType, principalID string) (int, error) {
	if principalID == c.parent.targetUserID && !c.parent.injected {
		c.parent.injected = true
		ownerRD, err := c.GetRoleDefinitionByName(ctx, store.ProjectRoleOwner, store.RoleScopeProject)
		if err != nil {
			return 0, err
		}
		if _, err := c.CreateRoleBinding(ctx, &store.RoleBinding{
			RoleDefinitionID: ownerRD.ID,
			PrincipalType:    store.RoleBindingPrincipalUser,
			PrincipalID:      principalID,
			ScopeType:        store.RoleScopeProject,
			ScopeID:          c.parent.grantProjectID,
			CreatedBy:        "concurrent-grant",
		}); err != nil {
			return 0, err
		}
	}
	return c.Store.DeleteRoleBindingsForPrincipal(ctx, principalType, principalID)
}

// requireConflictRetry asserts the 409 conflict response for a delete that
// raced a change to the user's role bindings.
func requireConflictRetry(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	var resp struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp), rec.Body.String())
	assert.Equal(t, ErrCodeConflict, resp.Error.Code)
	assert.Contains(t, resp.Error.Message, "changed concurrently")
}

// A binding granted to the target between the guard's list and the cascade
// must abort the delete with 409 conflict and roll everything back, rather
// than silently deleting an owner binding the guard never checked.
func TestDeleteUser_ConcurrentBindingChangeAbortsWithConflict(t *testing.T) {
	cases := []struct {
		name   string
		target func(alice, carol *store.User) *store.User
		path   func(u *store.User) string
	}{
		{"users", func(a, _ *store.User) *store.User { return a },
			func(u *store.User) string { return "/api/v1/users/" + u.ID }},
		{"allow-list", func(_, c *store.User) *store.User { return c },
			func(u *store.User) string { return "/api/v1/admin/allow-list/" + u.Email }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, s, alice, bob, project := setupDemoPolicyTest(t)
			ctx := context.Background()
			// alice co-owns project with bob, so the guard itself allows
			// her delete; carol (invited) holds a member binding.
			addProjectOwner(t, srv, s, alice, bob, project.ID)
			carol := newInvitedUser(t, s, "user-carol", "carol@test.com")
			memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
			require.NoError(t, err)
			_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
				RoleDefinitionID: memberRD.ID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: carol.ID,
				ScopeType: store.RoleScopeProject, ScopeID: project.ID, CreatedBy: alice.ID,
			})
			require.NoError(t, err)

			target := tc.target(alice, carol)
			// The concurrent grant lands on a project the target does not
			// own, so the guard never locks or checks it.
			other := newTestProject(t, s, "project-other", "Other Project")
			createOwnerBinding(t, s, bob.ID, other.ID, nil, nil)

			before := allBindingsFor(t, s, target.ID)
			require.NotEmpty(t, before)

			raced := &concurrentGrantStore{Store: s, targetUserID: target.ID, grantProjectID: other.ID}
			srv.store = raced

			rec := doRequest(t, srv, http.MethodDelete, tc.path(target), nil)
			srv.store = s
			require.True(t, raced.injected, "precondition: the concurrent grant was injected")
			requireConflictRetry(t, rec)

			_, err = s.GetUser(ctx, target.ID)
			require.NoError(t, err, "aborted delete must keep the user")
			after := allBindingsFor(t, s, target.ID)
			beforeIDs := make([]string, 0, len(before))
			for _, b := range before {
				beforeIDs = append(beforeIDs, b.ID)
			}
			afterIDs := make([]string, 0, len(after))
			for _, b := range after {
				afterIDs = append(afterIDs, b.ID)
			}
			assert.ElementsMatch(t, beforeIDs, afterIDs, "aborted delete must leave the bindings untouched")
		})
	}
}

// bindingSwapStore simulates a members-API role change on project `other`
// that commits between the guard's binding list and the cascade
// (ptone/scion#2770 review r2 M1/L2, PG READ COMMITTED). It fires inside
// the delete transaction on the first by-ID re-read (GetRoleBinding) or
// delete of a binding whose principal is the target, or on the first
// delete by principal for the target, whichever comes first, so it models a
// commit before the cascade starts on the count-only, the set-check and the
// re-read implementations.
//   - mode "transfer": TransferOwnership(other -> target). The target's
//     member binding is replaced by an owner binding, and the previous
//     owner's (bob's) owner binding is replaced by a member binding. The
//     target's binding count is unchanged.
//   - mode "revoke": the target's member binding on `other` is removed.
//   - mode "sameid": like "transfer", but the target's new owner binding
//     reuses the ID of its member binding, modelling an in-place role
//     UPDATE under the same ID (ptone/scion#2770 review r3 L1).
type bindingSwapStore struct {
	store.Store
	target, bob, other string
	mode               string
	injected           bool
}

func (c *bindingSwapStore) WithTx(ctx context.Context, fn func(tx store.Store) error) error {
	return c.Store.WithTx(ctx, func(tx store.Store) error {
		return fn(&bindingSwapTx{Store: tx, parent: c})
	})
}

type bindingSwapTx struct {
	store.Store
	parent *bindingSwapStore
}

func (c *bindingSwapTx) inject(ctx context.Context) error {
	p := c.parent
	if p.injected {
		return nil
	}
	p.injected = true
	ownerRD, err := c.GetRoleDefinitionByName(ctx, store.ProjectRoleOwner, store.RoleScopeProject)
	if err != nil {
		return err
	}
	memberRD, err := c.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	if err != nil {
		return err
	}
	bs, err := c.ListRoleBindingsForScope(ctx, store.RoleScopeProject, p.other)
	if err != nil {
		return err
	}
	swap := p.mode == "transfer" || p.mode == "sameid"
	var targetBindingID string
	for _, b := range bs {
		if b.PrincipalID == p.target || (swap && b.PrincipalID == p.bob) {
			if b.PrincipalID == p.target {
				targetBindingID = b.ID
			}
			if err := c.Store.DeleteRoleBinding(ctx, b.ID); err != nil {
				return err
			}
		}
	}
	if !swap {
		return nil
	}
	newTargetID := ""
	if p.mode == "sameid" {
		newTargetID = targetBindingID
	}
	for _, nb := range []struct{ id, who, rd string }{{newTargetID, p.target, ownerRD.ID}, {"", p.bob, memberRD.ID}} {
		if _, err := c.CreateRoleBinding(ctx, &store.RoleBinding{
			ID: nb.id, RoleDefinitionID: nb.rd, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: nb.who,
			ScopeType: store.RoleScopeProject, ScopeID: p.other, CreatedBy: "concurrent-transfer",
		}); err != nil {
			return err
		}
	}
	return nil
}

// injectIfTargetBinding fires the injection only when binding id belongs
// to the target, so a by-ID call for any other principal is not mistaken
// for the start of the target's cascade.
func (c *bindingSwapTx) injectIfTargetBinding(ctx context.Context, id string) error {
	if c.parent.injected {
		return nil
	}
	// A lookup error (for example, the binding is gone) is left for the
	// real call to report.
	if b, err := c.Store.GetRoleBinding(ctx, id); err == nil && b.PrincipalID == c.parent.target {
		return c.inject(ctx)
	}
	return nil
}

func (c *bindingSwapTx) GetRoleBinding(ctx context.Context, id string) (*store.RoleBinding, error) {
	if err := c.injectIfTargetBinding(ctx, id); err != nil {
		return nil, err
	}
	return c.Store.GetRoleBinding(ctx, id)
}

func (c *bindingSwapTx) DeleteRoleBinding(ctx context.Context, id string) error {
	if err := c.injectIfTargetBinding(ctx, id); err != nil {
		return err
	}
	return c.Store.DeleteRoleBinding(ctx, id)
}

func (c *bindingSwapTx) DeleteRoleBindingsForPrincipal(ctx context.Context, principalType, principalID string) (int, error) {
	if principalID == c.parent.target {
		if err := c.inject(ctx); err != nil {
			return 0, err
		}
	}
	return c.Store.DeleteRoleBindingsForPrincipal(ctx, principalType, principalID)
}

// projectOwnerIDs returns the principal IDs of every project-owner binding
// on projectID.
func projectOwnerIDs(t *testing.T, s store.Store, projectID string) []string {
	t.Helper()
	ctx := context.Background()
	ownerRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleOwner, store.RoleScopeProject)
	require.NoError(t, err)
	bs, err := s.ListRoleBindingsForScope(ctx, store.RoleScopeProject, projectID)
	require.NoError(t, err)
	var ids []string
	for _, b := range bs {
		if b.RoleDefinitionID == ownerRD.ID {
			ids = append(ids, b.PrincipalID)
		}
	}
	return ids
}

// setupBindingSwap prepares, for each delete path, a target that co-owns
// `project` with bob (users path) or is an invited member of it
// (allow-list path), plus a member binding on `other`, which bob solely
// owns and the guard therefore never locks or checks.
func setupBindingSwap(t *testing.T, path string) (srv *Server, s store.Store, target, bob *store.User, other *store.Project, url string) {
	t.Helper()
	srv, s, alice, bob, project := setupDemoPolicyTest(t)
	ctx := context.Background()
	addProjectOwner(t, srv, s, alice, bob, project.ID)
	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)
	target, url = alice, "/api/v1/users/"+alice.ID
	if path == "allow-list" {
		target = newInvitedUser(t, s, "user-carol", "carol@test.com")
		url = "/api/v1/admin/allow-list/" + target.Email
	}
	other = newTestProject(t, s, "project-other", "Other Project")
	createOwnerBinding(t, s, bob.ID, other.ID, nil, nil)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: memberRD.ID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: target.ID,
		ScopeType: store.RoleScopeProject, ScopeID: other.ID, CreatedBy: "test",
	})
	require.NoError(t, err)
	return srv, s, target, bob, other, url
}

// A concurrent TransferOwnership of an unchecked project to the target swaps
// one of its bindings for another, keeping the count equal. The delete must
// abort with 409 and change nothing, rather than delete the new owner binding
// and leave the project with no owner (ptone/scion#2770 review r2 M1/L2).
func TestDeleteUser_ConcurrentBindingSwapAbortsWithConflict(t *testing.T) {
	for _, path := range []string{"users", "allow-list"} {
		t.Run(path, func(t *testing.T) {
			srv, s, target, bob, other, url := setupBindingSwap(t, path)
			ctx := context.Background()

			raced := &bindingSwapStore{Store: s, target: target.ID, bob: bob.ID, other: other.ID, mode: "transfer"}
			srv.store = raced
			rec := doRequest(t, srv, http.MethodDelete, url, nil)
			srv.store = s
			require.True(t, raced.injected, "precondition: the concurrent transfer was injected")
			requireConflictRetry(t, rec)

			_, err := s.GetUser(ctx, target.ID)
			require.NoError(t, err, "aborted delete must keep the user")
			// The rollback also undoes the injected transfer: the target
			// keeps its member binding and bob stays the sole owner.
			assert.Equal(t, []string{bob.ID}, projectOwnerIDs(t, s, other.ID),
				"aborted delete must not leave the project without an owner")
			after := allBindingsFor(t, s, target.ID)
			assert.NotEmpty(t, after, "aborted delete must keep the target's bindings")
		})
	}
}

// A listed binding changed in place under the same ID (its member role on an
// unchecked project replaced by project-owner, with bob demoted) must abort
// the delete with 409 and change nothing. No production path does this today
// (role bindings are immutable), but without the by-ID re-read the delete
// would succeed and leave `other` with no owner (ptone/scion#2770 review r3
// L1).
func TestDeleteUser_ConcurrentBindingInPlaceChangeAbortsWithConflict(t *testing.T) {
	for _, path := range []string{"users", "allow-list"} {
		t.Run(path, func(t *testing.T) {
			srv, s, target, bob, other, url := setupBindingSwap(t, path)
			ctx := context.Background()

			raced := &bindingSwapStore{Store: s, target: target.ID, bob: bob.ID, other: other.ID, mode: "sameid"}
			srv.store = raced
			rec := doRequest(t, srv, http.MethodDelete, url, nil)
			srv.store = s
			require.True(t, raced.injected, "precondition: the in-place change was injected")
			requireConflictRetry(t, rec)

			_, err := s.GetUser(ctx, target.ID)
			require.NoError(t, err, "aborted delete must keep the user")
			assert.Equal(t, []string{bob.ID}, projectOwnerIDs(t, s, other.ID),
				"rollback must restore bob as the sole owner")
			assert.NotEmpty(t, allBindingsFor(t, s, target.ID), "aborted delete must keep the target's bindings")
		})
	}
}

// A listed binding revoked concurrently (only possible on a project the
// target does not own) is ignored by the cascade: the delete succeeds instead
// of returning a false conflict (ptone/scion#2770 review r2 N3).
func TestDeleteUser_ConcurrentBindingRevokeStillDeletes(t *testing.T) {
	for _, path := range []string{"users", "allow-list"} {
		t.Run(path, func(t *testing.T) {
			srv, s, target, bob, other, url := setupBindingSwap(t, path)
			ctx := context.Background()

			raced := &bindingSwapStore{Store: s, target: target.ID, bob: bob.ID, other: other.ID, mode: "revoke"}
			srv.store = raced
			rec := doRequest(t, srv, http.MethodDelete, url, nil)
			srv.store = s
			require.True(t, raced.injected, "precondition: the concurrent revoke was injected")
			require.Less(t, rec.Code, 300, rec.Body.String())

			_, err := s.GetUser(ctx, target.ID)
			require.ErrorIs(t, err, store.ErrNotFound)
			assert.Empty(t, allBindingsFor(t, s, target.ID))
			assert.Equal(t, []string{bob.ID}, projectOwnerIDs(t, s, other.ID))
		})
	}
}

// missingOwnerRoleTxStore makes the guard's project-owner role lookup return
// store.ErrNotFound inside the delete transaction, or (nilNil) a nil role
// definition with a nil error, which the store interface does not rule out
// (GoogleCloudPlatform/scion#2414 review).
type missingOwnerRoleTxStore struct {
	store.Store
	nilNil bool
}

func (m *missingOwnerRoleTxStore) WithTx(ctx context.Context, fn func(tx store.Store) error) error {
	return m.Store.WithTx(ctx, func(tx store.Store) error {
		return fn(&missingOwnerRoleTx{Store: tx, nilNil: m.nilNil})
	})
}

type missingOwnerRoleTx struct {
	store.Store
	nilNil bool
}

func (m *missingOwnerRoleTx) GetRoleDefinitionByName(ctx context.Context, name, scope string) (*store.RoleDefinition, error) {
	if name == store.ProjectRoleOwner {
		if m.nilNil {
			return nil, nil
		}
		return nil, store.ErrNotFound
	}
	return m.Store.GetRoleDefinitionByName(ctx, name, scope)
}

// Only a not-found from DeleteUser maps to 404 on the allow-list delete; a
// not-found inside the guard is a server error (ptone/scion#2770 review L3).
// A nil, nil role lookup must also fail closed rather than dereference nil
// (GoogleCloudPlatform/scion#2414 review).
func TestDeprecatedAllowListDelete_GuardNotFoundIsInternalError(t *testing.T) {
	for _, tc := range []struct {
		name   string
		nilNil bool
	}{{"err-not-found", false}, {"nil-nil", true}} {
		t.Run(tc.name, func(t *testing.T) {
			srv, s, alice, _, project := setupDemoPolicyTest(t)
			ctx := context.Background()
			carol := newInvitedUser(t, s, "user-carol", "carol@test.com")
			memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
			require.NoError(t, err)
			_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
				RoleDefinitionID: memberRD.ID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: carol.ID,
				ScopeType: store.RoleScopeProject, ScopeID: project.ID, CreatedBy: alice.ID,
			})
			require.NoError(t, err)

			srv.store = &missingOwnerRoleTxStore{Store: s, nilNil: tc.nilNil}
			logs := captureSpaceMembersLogs(t)
			rec := doRequest(t, srv, http.MethodDelete, "/api/v1/admin/allow-list/"+carol.Email, nil)
			srv.store = s
			require.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
			// The panic-recovery middleware also answers 500, so check the
			// guard failed closed instead of dereferencing a nil role.
			assert.NotContains(t, logs.String(), "Panic recovered")

			_, err = s.GetUser(ctx, carol.ID)
			require.NoError(t, err, "failed delete must keep the invited user")
			assert.NotEmpty(t, allBindingsFor(t, s, carol.ID))
		})
	}
}
