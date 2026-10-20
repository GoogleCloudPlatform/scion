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
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Revoking a project visitor grant through the membership service
// (ProjectMembershipService.RemoveVisitor), and the conduit authorization
// change the revoke and window routes publish (ptone/scion#4603).

// visitorRevokeRecorderStore counts the project membership locks and
// membership loss checks taken through it, inside transactions too. When
// rewrite is set, ListVisitorRoleBindings passes every binding it returns
// through it, so a test can hand the service a binding the store would not
// return.
type visitorRevokeRecorderStore struct {
	store.Store
	locks   *atomic.Int32
	losses  *atomic.Int32
	rewrite func(rb *store.RoleBinding) *store.RoleBinding
}

func newVisitorRevokeRecorderStore(inner store.Store) *visitorRevokeRecorderStore {
	return &visitorRevokeRecorderStore{Store: inner, locks: &atomic.Int32{}, losses: &atomic.Int32{}}
}

func (s *visitorRevokeRecorderStore) WithTx(ctx context.Context, fn func(tx store.Store) error) error {
	return s.Store.WithTx(ctx, func(tx store.Store) error {
		return fn(&visitorRevokeRecorderStore{Store: tx, locks: s.locks, losses: s.losses, rewrite: s.rewrite})
	})
}

func (s *visitorRevokeRecorderStore) LockProjectForMembership(ctx context.Context, projectID string) error {
	s.locks.Add(1)
	return s.Store.LockProjectForMembership(ctx, projectID)
}

func (s *visitorRevokeRecorderStore) EnqueueMembershipLossCheck(ctx context.Context, check *store.MembershipLossCheck) error {
	s.losses.Add(1)
	return s.Store.EnqueueMembershipLossCheck(ctx, check)
}

func (s *visitorRevokeRecorderStore) ListVisitorRoleBindings(ctx context.Context, projectID string) ([]*store.RoleBinding, error) {
	bindings, err := s.Store.ListVisitorRoleBindings(ctx, projectID)
	if err != nil || s.rewrite == nil {
		return bindings, err
	}
	out := make([]*store.RoleBinding, 0, len(bindings))
	for _, b := range bindings {
		out = append(out, s.rewrite(b))
	}
	return out, nil
}

// installVisitorRevokeRecorder installs a visitorRevokeRecorderStore as
// the membership service's store, right after the server is constructed,
// and returns it.
func installVisitorRevokeRecorder(srv *Server) *visitorRevokeRecorderStore {
	rec := newVisitorRevokeRecorderStore(srv.membershipService.store)
	srv.membershipService.store = rec
	return rec
}

// visitorBindingIDs returns the IDs of the project's visitor bindings.
func (f *visitorGrantFixture) visitorBindingIDs(t *testing.T) []string {
	t.Helper()
	bindings, err := f.st.ListVisitorRoleBindings(context.Background(), f.projectID)
	require.NoError(t, err)
	ids := make([]string, 0, len(bindings))
	for _, b := range bindings {
		ids = append(ids, b.ID)
	}
	return ids
}

// A project admin revokes a grant through the membership service: the
// binding is deleted, the project_visitor_revoke record names the visitor,
// its issuer, email and linked subject, and neither a project membership
// lock nor a membership loss check is taken. A project member is refused.
func TestProjectVisitors_RemoveVisitorByProjectAdmin(t *testing.T) {
	var rec *visitorRevokeRecorderStore
	f := newVisitorGrantFixture(t, func(srv *Server) {
		rec = installVisitorRevokeRecorder(srv)
	})

	v := f.grantOK(t, grantTestIssuer, "rita@corp.example")
	_, _, code := f.identityThroughMiddleware(t, f.token(t, grantTestIssuer, "sub-rita", "rita@corp.example", boolPtr(true)))
	require.Equal(t, http.StatusOK, code, "first contact links the subject")

	resp := doRequestAsUser(t, f.srv, f.member, http.MethodDelete, f.visitorsPath()+"/"+v.ID, nil)
	assert.Equal(t, http.StatusForbidden, resp.Code, "a project member may not revoke: %s", resp.Body.String())
	assert.Contains(t, f.visitorBindingIDs(t), v.BindingID)

	locks, losses := rec.locks.Load(), rec.losses.Load()
	resp = doRequestAsUser(t, f.srv, f.admin, http.MethodDelete, f.visitorsPath()+"/"+v.ID, nil)
	require.Equal(t, http.StatusNoContent, resp.Code, "a project admin may revoke: %s", resp.Body.String())
	assert.NotContains(t, f.visitorBindingIDs(t), v.BindingID)

	revokes := f.audits(t, mutationTypeProjectVisitorRevoke)
	require.Len(t, revokes, 1)
	assert.Equal(t, "project", revokes[0].TargetType)
	assert.Equal(t, f.projectID, revokes[0].TargetID)
	assert.Equal(t, f.admin.ID, revokes[0].ActorPrincipalID)
	var before map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(revokes[0].BeforeSummary), &before))
	assert.Equal(t, v.ID, before["visitor_id"])
	assert.Equal(t, v.BindingID, before["binding_id"])
	assert.Equal(t, grantTestIssuer, before["issuer"])
	assert.Equal(t, "rita@corp.example", before["email"])
	assert.Equal(t, "sub-rita", before["subject"])
	assert.Empty(t, revokes[0].AfterSummary)
	assert.Empty(t, f.audits(t, "project_member_remove"), "a revoke is not a member removal")

	assert.Equal(t, locks, rec.locks.Load(), "a revoke takes no project membership lock")
	assert.Equal(t, losses, rec.losses.Load(), "a revoke enqueues no membership loss check")
}

// RemoveVisitor removes only a project-visitor binding of the project: an
// account with no visitor grant there is not found, and a binding of any
// other role, scope or principal type is refused. Neither deletes anything
// or writes an audit record.
func TestProjectVisitors_RemoveVisitorRefusesOtherBindings(t *testing.T) {
	var rec *visitorRevokeRecorderStore
	f := newVisitorGrantFixture(t, func(srv *Server) {
		rec = installVisitorRevokeRecorder(srv)
	})
	ctx := context.Background()
	svc := f.srv.membershipService
	v := f.grantOK(t, grantTestIssuer, "sam@corp.example")

	otherOwner := tid(t.Name() + "-other-owner")
	otherProject := tid(t.Name() + "-other-project")
	createRS1Project(t, f.st, otherProject, otherOwner)

	memberBindings, err := f.st.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, f.member.ID)
	require.NoError(t, err)
	require.NotEmpty(t, memberBindings)

	t.Run("project member", func(t *testing.T) {
		_, err := svc.RemoveVisitor(ctx, f.projectID, f.member.ID, f.admin.ID)
		assert.ErrorIs(t, err, store.ErrNotFound)
		after, err := f.st.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, f.member.ID)
		require.NoError(t, err)
		assert.Len(t, after, len(memberBindings), "the member's bindings remain")
	})

	t.Run("visitor of another project", func(t *testing.T) {
		_, err := svc.RemoveVisitor(ctx, otherProject, v.ID, f.admin.ID)
		assert.ErrorIs(t, err, store.ErrNotFound)
		assert.Contains(t, f.visitorBindingIDs(t), v.BindingID)
	})

	memberRD, err := f.st.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)
	for _, tc := range []struct {
		name    string
		rewrite func(rb *store.RoleBinding) *store.RoleBinding
	}{
		{"another role", func(rb *store.RoleBinding) *store.RoleBinding {
			c := *rb
			c.RoleDefinitionID = memberRD.ID
			return &c
		}},
		{"another project", func(rb *store.RoleBinding) *store.RoleBinding {
			c := *rb
			c.ScopeID = otherProject
			return &c
		}},
		{"system scope", func(rb *store.RoleBinding) *store.RoleBinding {
			c := *rb
			c.ScopeType, c.ScopeID = store.RoleScopeSystem, ""
			return &c
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec.rewrite = tc.rewrite
			t.Cleanup(func() { rec.rewrite = nil })

			_, err := svc.RemoveVisitor(ctx, f.projectID, v.ID, f.admin.ID)
			assert.ErrorIs(t, err, store.ErrVisitorBindingRefused)
			assert.Contains(t, f.visitorBindingIDs(t), v.BindingID, "the binding remains")
			assert.Empty(t, f.audits(t, mutationTypeProjectVisitorRevoke))
		})
	}
}

func TestRequireProjectVisitorBinding(t *testing.T) {
	_, st := testServer(t)
	ctx := context.Background()
	visitorRD, err := st.GetRoleDefinitionByName(ctx, store.ProjectRoleVisitor, store.RoleScopeProject)
	require.NoError(t, err)
	memberRD, err := st.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)
	valid := store.RoleBinding{
		RoleDefinitionID: visitorRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      "visitor",
		ScopeType:        store.RoleScopeProject,
		ScopeID:          "p1",
	}
	with := func(change func(rb *store.RoleBinding)) *store.RoleBinding {
		c := valid
		change(&c)
		return &c
	}
	assert.NoError(t, requireProjectVisitorBinding(ctx, st, &valid, "p1"))
	for name, rb := range map[string]*store.RoleBinding{
		"nil binding":     nil,
		"another role":    with(func(rb *store.RoleBinding) { rb.RoleDefinitionID = memberRD.ID }),
		"another project": with(func(rb *store.RoleBinding) { rb.ScopeID = "p2" }),
		"system scope":    with(func(rb *store.RoleBinding) { rb.ScopeType, rb.ScopeID = store.RoleScopeSystem, "" }),
		"group principal": with(func(rb *store.RoleBinding) { rb.PrincipalType = store.RoleBindingPrincipalGroup }),
	} {
		t.Run(name, func(t *testing.T) {
			assert.ErrorIs(t, requireProjectVisitorBinding(ctx, st, rb, "p1"), store.ErrVisitorBindingRefused)
		})
	}
}

// conduitAuthzEvents returns the data of the conduit authorization
// change events buffered on ch, without waiting: the routes publish before
// they answer.
func conduitAuthzEvents(ch <-chan Event) []string {
	var got []string
	for {
		select {
		case evt := <-ch:
			got = append(got, string(evt.Data))
		default:
			return got
		}
	}
}

// After a revoke or a window change that narrows the grant has committed,
// the routes publish a conduit authorization change for the visitor on the
// project. A widened window, a refused request and a transaction that
// rolls back publish nothing.
func TestProjectVisitors_ConduitAuthzChangedAfterCommit(t *testing.T) {
	var fault *storeFaultSwitch
	f := newVisitorGrantFixture(t, func(srv *Server) {
		fault = installVisitorAuditFault(t, srv)
	})
	setConduitExperiment(t, f.srv, true)
	pub := NewChannelEventPublisher()
	f.srv.SetEventPublisher(pub)
	ch, unsubscribe := pub.Subscribe(conduitAuthzChangedSubject)
	defer unsubscribe()

	expiry := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Second)
	v := f.grantWithExpiry(t, grantTestIssuer, "tess@corp.example", expiry)
	want := `{"userId":"` + v.ID + `","projectId":"` + f.projectID + `"}`
	conduitAuthzEvents(ch)
	patch := func(t *testing.T, body map[string]interface{}) int {
		t.Helper()
		return doRequestAsUser(t, f.srv, f.owner, http.MethodPatch, f.visitorsPath()+"/"+v.ID, body).Code
	}

	t.Run("widened window", func(t *testing.T) {
		expiry = expiry.Add(24 * time.Hour)
		require.Equal(t, http.StatusOK, patch(t, map[string]interface{}{"expiresAt": expiry}))
		assert.Empty(t, conduitAuthzEvents(ch))
	})

	t.Run("earlier expiry", func(t *testing.T) {
		expiry = expiry.Add(-36 * time.Hour)
		require.Equal(t, http.StatusOK, patch(t, map[string]interface{}{"expiresAt": expiry}))
		got := conduitAuthzEvents(ch)
		require.Len(t, got, 1)
		assert.JSONEq(t, want, got[0])
	})

	t.Run("later start", func(t *testing.T) {
		start := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
		require.Equal(t, http.StatusOK, patch(t, map[string]interface{}{"notBefore": start, "expiresAt": expiry}))
		got := conduitAuthzEvents(ch)
		require.Len(t, got, 1)
		assert.JSONEq(t, want, got[0])
	})

	t.Run("refused requests", func(t *testing.T) {
		rec := doRequestAsUser(t, f.srv, f.member, http.MethodDelete, f.visitorsPath()+"/"+v.ID, nil)
		require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		rec = doRequestAsUser(t, f.srv, f.owner, http.MethodDelete, f.visitorsPath()+"/"+f.member.ID, nil)
		require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
		assert.Empty(t, conduitAuthzEvents(ch))
	})

	t.Run("revoke", func(t *testing.T) {
		w := f.grantOK(t, grantTestIssuer, "uma@corp.example")
		conduitAuthzEvents(ch)
		rec := doRequestAsUser(t, f.srv, f.owner, http.MethodDelete, f.visitorsPath()+"/"+w.ID, nil)
		require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
		got := conduitAuthzEvents(ch)
		require.Len(t, got, 1)
		assert.JSONEq(t, `{"userId":"`+w.ID+`","projectId":"`+f.projectID+`"}`, got[0])
	})

	// The switch stays armed: this subtest runs last.
	t.Run("rolled back", func(t *testing.T) {
		fault.Arm()
		assert.Equal(t, http.StatusInternalServerError, patch(t, map[string]interface{}{"expiresAt": expiry.Add(-time.Hour)}))
		rec := doRequestAsUser(t, f.srv, f.owner, http.MethodDelete, f.visitorsPath()+"/"+v.ID, nil)
		assert.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
		assert.Empty(t, conduitAuthzEvents(ch))
		listed, ok := f.listedVisitor(t, v.ID)
		require.True(t, ok, "the grant remains")
		require.NotNil(t, listed.ExpiresAt)
		assert.WithinDuration(t, expiry, *listed.ExpiresAt, time.Second, "the window is unchanged")
	})
}
