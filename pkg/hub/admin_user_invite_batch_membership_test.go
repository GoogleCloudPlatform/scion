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
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Batch invite with project memberships (ptone/scion#4952, design §4-§6),
// on the members fixture (setupMMRFixture).

// batchGrantHubPerms gives userID a custom system role holding perms.
func batchGrantHubPerms(t *testing.T, s store.Store, userID string, perms ...string) {
	t.Helper()
	ctx := context.Background()
	rd, err := s.CreateRoleDefinition(ctx, &store.RoleDefinition{
		Name: "batch-hub-" + tid(t.Name() + userID)[:8], ScopeType: store.RoleScopeSystem, Permissions: perms,
	})
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: userID,
		ScopeType: store.RoleScopeSystem, CreatedBy: "test",
	})
	require.NoError(t, err)
}

// batchFixture is the members fixture with the owner and the admin able
// to call the bulk route (user.invite and user.read), and a recording
// audit logger.
type batchFixture struct {
	*mmrFixture
	audit *recordingAuditLogger
}

func newBatchFixture(t *testing.T) *batchFixture {
	t.Helper()
	f := setupMMRFixture(t)
	for _, u := range []*store.User{f.owner, f.admin} {
		batchGrantHubPerms(t, f.store, u.ID, "user.invite", "user.read")
	}
	audit := &recordingAuditLogger{}
	f.srv.SetAuditLogger(audit)
	return &batchFixture{mmrFixture: f, audit: audit}
}

func (f *batchFixture) post(t *testing.T, actor *store.User, req UserInviteBulkRequest) *httptest.ResponseRecorder {
	t.Helper()
	return doRequestAsUser(t, f.srv, actor, http.MethodPost, "/api/v1/admin/users/invite/bulk", req)
}

func (f *batchFixture) ok(t *testing.T, actor *store.User, req UserInviteBulkRequest) UserInviteBulkResponse {
	t.Helper()
	rec := f.post(t, actor, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp UserInviteBulkResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	return resp
}

func (f *batchFixture) events(typ InviteAuditEventType) []*InviteAuditEvent {
	f.audit.mu.Lock()
	defer f.audit.mu.Unlock()
	var out []*InviteAuditEvent
	for _, e := range f.audit.invite {
		if e.EventType == typ {
			out = append(out, e)
		}
	}
	return out
}

func (f *batchFixture) noUser(t *testing.T, email string) {
	t.Helper()
	_, err := f.store.GetUserByEmail(context.Background(), email)
	assert.ErrorIs(t, err, store.ErrNotFound, "%s must not exist", email)
}

func batchEmails(emails ...string) []UserInviteRequest {
	out := make([]UserInviteRequest, len(emails))
	for i, e := range emails {
		out[i] = UserInviteRequest{Email: e}
	}
	return out
}

func batchRefusal(t *testing.T, rec *httptest.ResponseRecorder) (batchID string, refusals []InviteBatchProjectRefusal) {
	t.Helper()
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	resp := parseErrorResponse(t, rec.Body.Bytes())
	require.Equal(t, inviteBatchReasonProjectsNotGrantable, resp.Error.Details["reason"], rec.Body.String())
	batchID, _ = resp.Error.Details["batchId"].(string)
	b, err := json.Marshal(resp.Error.Details["projects"])
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(b, &refusals))
	return batchID, refusals
}

// TestInviteBatch_AppliesProjectRoleToInvitedRow: the project role is
// bound to the invited row's ID through SetMemberRoles (with its
// project_member_add record), grants nothing before activation, and grants
// after it.
func TestInviteBatch_AppliesProjectRoleToInvitedRow(t *testing.T) {
	f := newBatchFixture(t)
	ctx := context.Background()
	auditBefore := len(mmrAuditRows(t, f.store, f.projectID))
	resp := f.ok(t, f.owner, UserInviteBulkRequest{
		Emails:   batchEmails("joiner@example.com"),
		Projects: []UserInviteBulkProject{{ProjectID: f.projectID, RoleDefinitionIDs: []string{f.memberRD.ID}}},
	})
	require.Len(t, resp.Results, 1)
	r := resp.Results[0]
	assert.Equal(t, inviteBatchStatusCreated, r.Status)
	require.Equal(t, []InviteBatchMembership{{ProjectID: f.projectID, Status: inviteBatchMembershipApplied}}, r.Memberships)
	assert.True(t, resp.Complete)

	bindings := mmrBindingsFor(t, f.store, store.RoleBindingPrincipalUser, r.UserID, f.projectID)
	require.Len(t, bindings, 1, "the binding is held by the invited row ID")
	assert.Equal(t, f.memberRD.ID, bindings[0].RoleDefinitionID)
	assert.Len(t, mmrAuditRows(t, f.store, f.projectID), auditBefore+1, "one project_member_add record, written by SetMemberRoles")

	project, err := f.store.GetProject(ctx, f.projectID)
	require.NoError(t, err)
	joiner, err := f.store.GetUser(ctx, r.UserID)
	require.NoError(t, err)
	before := decidePerm(f.srv.authzService, NewAuthenticatedUser(joiner.ID, joiner.Email, "", joiner.Role, string(ClientTypeWeb)),
		projectResource(project), ActionRead, "project.read", true)
	assert.False(t, before.Allowed, "the binding grants nothing before activation")
	assert.Equal(t, principalNotActiveReason, before.Reason)

	signedIn, err := f.srv.provisionUser(ctx, &ExternalUserInfo{Email: joiner.Email, DisplayName: "Joiner"})
	require.NoError(t, err)
	require.Equal(t, joiner.ID, signedIn.ID)
	after := decidePerm(f.srv.authzService, NewAuthenticatedUser(signedIn.ID, signedIn.Email, "", signedIn.Role, string(ClientTypeWeb)),
		projectResource(project), ActionRead, "project.read", true)
	assert.True(t, after.Allowed, "the binding grants after activation: %s", after.Reason)
}

// TestInviteBatch_MixedBatchRefusedCreatesNothing: one project the caller
// cannot manage refuses the whole request with 403 before any write.
func TestInviteBatch_MixedBatchRefusedCreatesNothing(t *testing.T) {
	f := newBatchFixture(t)
	rec := f.post(t, f.owner, UserInviteBulkRequest{
		Emails: batchEmails("mixed-a@example.com", "mixed-b@example.com"),
		Projects: []UserInviteBulkProject{
			{ProjectID: f.projectID, RoleDefinitionIDs: []string{f.memberRD.ID}},
			{ProjectID: f.otherProjectID, RoleDefinitionIDs: []string{f.memberRD.ID}},
		},
	})
	// Rows first, so a missing preflight is reported as the rows it let
	// through.
	f.noUser(t, "mixed-a@example.com")
	f.noUser(t, "mixed-b@example.com")
	assert.Empty(t, f.events(InviteAuditUserInvited), "no user_invited for a refused run")
	_, refusals := batchRefusal(t, rec)
	require.Equal(t, []InviteBatchProjectRefusal{{Index: 1, ProjectID: f.otherProjectID, Reason: inviteBatchReasonProjectNotManageable}}, refusals)
}

// TestInviteBatch_RoleNotGrantableRefusedWithDenialCode: a role the caller
// cannot grant (the CanDelegate ceiling, or governance) refuses the
// request with the members PUT's denial code and details.
func TestInviteBatch_RoleNotGrantableRefusedWithDenialCode(t *testing.T) {
	f := newBatchFixture(t)
	rec := f.post(t, f.owner, UserInviteBulkRequest{
		Emails:   batchEmails("ceiling@example.com"),
		Projects: []UserInviteBulkProject{{ProjectID: f.projectID, RoleDefinitionIDs: []string{f.memberRD.ID, f.beyondCeiling.ID}}},
	})
	_, refusals := batchRefusal(t, rec)
	require.Len(t, refusals, 1)
	assert.Equal(t, inviteBatchReasonRoleNotGrantable, refusals[0].Reason)
	assert.Equal(t, f.beyondCeiling.ID, refusals[0].RoleDefinitionID)
	assert.Equal(t, ErrCodeTargetRoleProtected, refusals[0].DenialCode)
	assert.Equal(t, f.beyondCeiling.ID, refusals[0].Details["roleDefinitionId"])
	f.noUser(t, "ceiling@example.com")

	rec = f.post(t, f.admin, UserInviteBulkRequest{
		Emails:   batchEmails("governed@example.com"),
		Projects: []UserInviteBulkProject{{ProjectID: f.projectID, RoleDefinitionIDs: []string{f.ownerRD.ID}}},
	})
	_, refusals = batchRefusal(t, rec)
	require.Len(t, refusals, 1)
	assert.Equal(t, ErrCodeTargetRoleProtected, refusals[0].DenialCode, "an admin cannot grant the owner role")
	f.noUser(t, "governed@example.com")
}

// TestInviteBatch_MissingAndDeniedProjectSameReason: a project that does
// not exist and one the caller cannot manage give the same refusal.
func TestInviteBatch_MissingAndDeniedProjectSameReason(t *testing.T) {
	f := newBatchFixture(t)
	missing := tid(t.Name() + "-missing")
	rec := f.post(t, f.owner, UserInviteBulkRequest{
		Emails: batchEmails("nobody@example.com"),
		Projects: []UserInviteBulkProject{
			{ProjectID: missing, RoleDefinitionIDs: []string{f.memberRD.ID}},
			{ProjectID: f.otherProjectID, RoleDefinitionIDs: []string{f.memberRD.ID}},
		},
	})
	_, refusals := batchRefusal(t, rec)
	require.Len(t, refusals, 2)
	assert.Equal(t, InviteBatchProjectRefusal{Index: 0, ProjectID: missing, Reason: inviteBatchReasonProjectNotManageable}, refusals[0])
	assert.Equal(t, InviteBatchProjectRefusal{Index: 1, ProjectID: f.otherProjectID, Reason: inviteBatchReasonProjectNotManageable}, refusals[1])
}

// TestInviteBatch_SystemScopedRoleNotAssignable: a system-scoped role ID
// (here hub-admin) is never assignable through a batch.
func TestInviteBatch_SystemScopedRoleNotAssignable(t *testing.T) {
	f := newBatchFixture(t)
	hubAdmin, err := f.store.GetRoleDefinitionByName(context.Background(), store.SystemRoleHubAdmin, store.RoleScopeSystem)
	require.NoError(t, err)
	rec := f.post(t, f.owner, UserInviteBulkRequest{
		Emails:   batchEmails("sys@example.com"),
		Projects: []UserInviteBulkProject{{ProjectID: f.projectID, RoleDefinitionIDs: []string{hubAdmin.ID}}},
	})
	_, refusals := batchRefusal(t, rec)
	require.Equal(t, []InviteBatchProjectRefusal{{Index: 0, ProjectID: f.projectID, Reason: inviteBatchReasonRoleNotAssignable, RoleDefinitionID: hubAdmin.ID}}, refusals)
	f.noUser(t, "sys@example.com")
}

// TestInviteBatch_RerunConverges: the same request again reports
// already_invited and already_member, and writes no binding, mutation
// record or user_invited event.
func TestInviteBatch_RerunConverges(t *testing.T) {
	f := newBatchFixture(t)
	req := UserInviteBulkRequest{
		Emails:   batchEmails("again-a@example.com", "again-b@example.com"),
		Projects: []UserInviteBulkProject{{ProjectID: f.projectID, RoleDefinitionIDs: []string{f.memberRD.ID}}},
	}
	first := f.ok(t, f.owner, req)
	require.Equal(t, 2, first.Counts.Created)
	audits := len(mmrAuditRows(t, f.store, f.projectID))
	invited := len(f.events(InviteAuditUserInvited))

	second := f.ok(t, f.owner, req)
	for i, r := range second.Results {
		assert.Equal(t, inviteBatchStatusAlreadyInvited, r.Status, "entry %d", i)
		assert.Equal(t, first.Results[i].UserID, r.UserID)
		assert.Equal(t, []InviteBatchMembership{{ProjectID: f.projectID, Status: inviteBatchMembershipAlreadyMember}}, r.Memberships)
		assert.Len(t, mmrBindingsFor(t, f.store, store.RoleBindingPrincipalUser, r.UserID, f.projectID), 1)
	}
	assert.True(t, second.Complete)
	assert.Len(t, mmrAuditRows(t, f.store, f.projectID), audits, "a re-run writes no mutation record")
	assert.Len(t, f.events(InviteAuditUserInvited), invited, "a re-run writes no user_invited")
}

// TestInviteBatch_ExistingMemberWithOtherRolesConflictUnchanged: an
// invited row that already holds another role set is reported as conflict
// and left unchanged.
func TestInviteBatch_ExistingMemberWithOtherRolesConflictUnchanged(t *testing.T) {
	f := newBatchFixture(t)
	pending := newInvitedUser(t, f.store, tid(t.Name()+"-pending"), "pending-admin@example.com")
	grpBind(t, f.store, store.RoleBindingPrincipalUser, pending.ID, f.adminRD.ID, f.projectID)
	resp := f.ok(t, f.owner, UserInviteBulkRequest{
		Emails:   batchEmails(pending.Email),
		Projects: []UserInviteBulkProject{{ProjectID: f.projectID, RoleDefinitionIDs: []string{f.memberRD.ID}}},
	})
	r := resp.Results[0]
	assert.Equal(t, inviteBatchStatusAlreadyInvited, r.Status)
	require.Equal(t, []InviteBatchMembership{{
		ProjectID: f.projectID, Status: inviteBatchMembershipConflict,
		Code: inviteBatchCodeMemberWithOtherRoles, CurrentRoleDefinitionIDs: []string{f.adminRD.ID},
	}}, r.Memberships)
	assert.True(t, resp.Complete, "a conflict is a final outcome")
	bindings := mmrBindingsFor(t, f.store, store.RoleBindingPrincipalUser, pending.ID, f.projectID)
	require.Len(t, bindings, 1)
	assert.Equal(t, f.adminRD.ID, bindings[0].RoleDefinitionID, "the existing membership is unchanged")
}

// TestInviteBatch_ActiveAndSuspendedAddressesSkipped: memberships are not
// attempted for an active or suspended address.
func TestInviteBatch_ActiveAndSuspendedAddressesSkipped(t *testing.T) {
	f := newBatchFixture(t)
	ctx := context.Background()
	outsider := grpUser(t, f.store, t.Name()+"-outsider", "Outsider")
	suspendedID := tid(t.Name() + "-suspended")
	require.NoError(t, f.store.CreateUser(ctx, &store.User{ID: suspendedID, Email: "suspended-batch@example.com", Role: store.UserRoleMember, Status: store.UserStatusSuspended}))
	resp := f.ok(t, f.owner, UserInviteBulkRequest{
		Emails:   batchEmails(outsider.Email, "suspended-batch@example.com", "bad"),
		Projects: []UserInviteBulkProject{{ProjectID: f.projectID, RoleDefinitionIDs: []string{f.memberRD.ID}}},
	})
	skipped := []InviteBatchMembership{{ProjectID: f.projectID, Status: inviteBatchMembershipSkipped, Code: inviteBatchCodeAddressNotInvited}}
	assert.Equal(t, inviteBatchStatusAlreadyActive, resp.Results[0].Status)
	assert.Equal(t, skipped, resp.Results[0].Memberships)
	assert.Equal(t, inviteBatchStatusSuspended, resp.Results[1].Status)
	assert.Equal(t, skipped, resp.Results[1].Memberships)
	assert.Equal(t, inviteBatchStatusInvalid, resp.Results[2].Status)
	assert.Equal(t, skipped, resp.Results[2].Memberships)
	assert.Empty(t, mmrBindingsFor(t, f.store, store.RoleBindingPrincipalUser, outsider.ID, f.projectID))
	assert.Empty(t, mmrBindingsFor(t, f.store, store.RoleBindingPrincipalUser, suspendedID, f.projectID))
}

// hookedMemberSetter delegates to the real service, running before (with
// the 1-based call number) ahead of each call, and counts calls per
// project.
type hookedMemberSetter struct {
	inner  inviteBatchMemberSetter
	before func(call int, req SetMemberRolesRequest)
	calls  map[string]int
	total  int
}

func (h *hookedMemberSetter) SetMemberRoles(ctx context.Context, req SetMemberRolesRequest) (*SetMemberRolesResult, *MembershipDecision) {
	h.total++
	if h.calls == nil {
		h.calls = map[string]int{}
	}
	h.calls[req.ProjectID]++
	if h.before != nil {
		h.before(h.total, req)
	}
	return h.inner.SetMemberRoles(ctx, req)
}

// batchActor returns the identity and a context carrying it with an
// interactive credential, as a request does, for direct runInviteBatch
// calls.
func batchActor(u *store.User) (context.Context, UserIdentity) {
	return mmrServiceCtx(u.ID, u.Email), mmrServiceIdentity(u.ID, u.Email)
}

// TestInviteBatch_StopClassDenialStopsProject: the caller loses authority
// in the project partway through; SetMemberRoles refuses with a stop-class
// code, and the later addresses get the same code without another call.
func TestInviteBatch_StopClassDenialStopsProject(t *testing.T) {
	f := newBatchFixture(t)
	ctx, actor := batchActor(f.admin)
	adminBindings := mmrBindingsFor(t, f.store, store.RoleBindingPrincipalUser, f.admin.ID, f.projectID)
	require.NotEmpty(t, adminBindings)
	setter := &hookedMemberSetter{inner: f.srv.membershipService, before: func(call int, _ SetMemberRolesRequest) {
		if call == 2 {
			for _, b := range adminBindings {
				require.NoError(t, f.store.DeleteRoleBinding(ctx, b.ID))
			}
		}
	}}
	entries := []inviteBatchEntry{{raw: "stop-a@example.com"}, {raw: "stop-b@example.com"}, {raw: "stop-c@example.com"}, {raw: "stop-d@example.com"}}
	projects := []inviteBatchProject{{ProjectID: f.projectID, RoleDefinitionIDs: []string{f.memberRD.ID}}}
	run := f.srv.runInviteBatch(ctx, actor, "b-stop", f.srv.validateInviteBatchAddresses(f.admin.ID, entries), projects, setter, true)

	assert.Equal(t, inviteBatchMembershipApplied, run.results[0].Memberships[0].Status)
	second := run.results[1].Memberships[0]
	require.Equal(t, inviteBatchMembershipFailed, second.Status)
	require.True(t, batchStopsProject(second.Code), "the refusal after the authority change is a stop-class code: %q", second.Code)
	for i := 2; i < 4; i++ {
		assert.Equal(t, inviteBatchStatusCreated, run.results[i].Status, "the invite itself still happens")
		assert.Equal(t, second, run.results[i].Memberships[0], "address %d reports the same code", i)
	}
	assert.Equal(t, 2, setter.calls[f.projectID], "no SetMemberRoles call after a stop-class refusal")
	assert.Equal(t, 3, run.counts.MembershipFailed)
}

// TestInviteBatch_ContinueClassDenialDoesNotStop: the invited row is
// removed between the user step and the membership step, so SetMemberRoles
// refuses with invalid_request (a continue code); the next address is
// still attempted and applied.
func TestInviteBatch_ContinueClassDenialDoesNotStop(t *testing.T) {
	f := newBatchFixture(t)
	ctx, actor := batchActor(f.owner)
	setter := &hookedMemberSetter{inner: f.srv.membershipService, before: func(call int, req SetMemberRolesRequest) {
		if call == 1 {
			require.NoError(t, f.store.DeleteUser(ctx, req.PrincipalID))
		}
	}}
	entries := []inviteBatchEntry{{raw: "gone@example.com"}, {raw: "stays@example.com"}}
	projects := []inviteBatchProject{{ProjectID: f.projectID, RoleDefinitionIDs: []string{f.memberRD.ID}}}
	run := f.srv.runInviteBatch(ctx, actor, "b-cont", f.srv.validateInviteBatchAddresses(f.owner.ID, entries), projects, setter, true)

	first := run.results[0].Memberships[0]
	assert.Equal(t, inviteBatchMembershipFailed, first.Status)
	assert.Equal(t, ErrCodeInvalidRequest, first.Code)
	require.False(t, batchStopsProject(first.Code))
	assert.Equal(t, inviteBatchMembershipApplied, run.results[1].Memberships[0].Status, "the next address is still attempted")
	assert.Equal(t, 2, setter.calls[f.projectID])
	assert.Equal(t, []string{f.projectID}, run.changedProjects)
}

// TestInviteBatch_ChangedProjectsOncePerProject: the run reports each
// changed project once, however many memberships it applied there; the
// handler publishes one conduit authorization change per entry.
func TestInviteBatch_ChangedProjectsOncePerProject(t *testing.T) {
	srv, _ := testServer(t)
	setter := &scriptedMemberSetter{decisions: []*MembershipDecision{
		nil, {DenialCode: ErrCodeInvalidRequest, HTTPStatus: 400},
		nil, {DenialCode: ErrCodeInvalidRequest, HTTPStatus: 400},
		nil, {DenialCode: ErrCodeInvalidRequest, HTTPStatus: 400},
	}}
	actor := NewAuthenticatedUser("actor-changed", "actor-changed@example.com", "", "admin", string(ClientTypeWeb))
	entries := []inviteBatchEntry{{raw: "ch-a@example.com"}, {raw: "ch-b@example.com"}, {raw: "ch-c@example.com"}}
	projects := []inviteBatchProject{{ProjectID: "p-applied", RoleDefinitionIDs: []string{"r"}}, {ProjectID: "p-refused", RoleDefinitionIDs: []string{"r"}}}
	run := srv.runInviteBatch(context.Background(), actor, "b-ch", srv.validateInviteBatchAddresses(actor.ID(), entries), projects, setter, true)
	assert.Equal(t, []string{"p-applied"}, run.changedProjects)
	assert.Equal(t, 3, run.membershipApplied)
	assert.Equal(t, 3, run.counts.MembershipFailed)
	assert.Equal(t, 6, setter.calls)
}

// TestInviteBatch_RefusedRunWritesOneAuditRecord: a run refused at the
// preflight writes exactly one user_invited_bulk record (Success=false)
// whose batch_id is the 403 body's details.batchId and which lists every
// refused entry; no user_invited record, user row or binding.
func TestInviteBatch_RefusedRunWritesOneAuditRecord(t *testing.T) {
	f := newBatchFixture(t)
	missing := tid(t.Name() + "-missing")
	rec := f.post(t, f.owner, UserInviteBulkRequest{
		Emails: batchEmails("ref-a@example.com", "ref-b@example.com", "ref-c@example.com"),
		Projects: []UserInviteBulkProject{
			{ProjectID: f.projectID, RoleDefinitionIDs: []string{f.memberRD.ID, f.beyondCeiling.ID}},
			{ProjectID: missing, RoleDefinitionIDs: []string{f.memberRD.ID}},
		},
	})
	batchID, refusals := batchRefusal(t, rec)
	require.NotEmpty(t, batchID)
	require.Len(t, refusals, 2)

	bulk := f.events(InviteAuditUserInvitedBulk)
	require.Len(t, bulk, 1, "exactly one record for the refused run")
	e := bulk[0]
	assert.False(t, e.Success)
	assert.Equal(t, inviteBatchReasonProjectsNotGrantable, e.FailReason)
	assert.Equal(t, 0, e.Count)
	assert.Equal(t, f.owner.ID, e.ActorID)
	assert.Equal(t, f.owner.Email, e.ActorEmail)
	assert.Equal(t, batchID, e.Details["batch_id"])
	assert.Equal(t, "3", e.Details["total"])
	assert.Equal(t, "2", e.Details["refused_entries"])
	assert.Equal(t, f.projectID+","+missing, e.Details["projects"])
	items := strings.Split(e.Details["refusals"], ";")
	assert.Equal(t, []string{
		fmt.Sprintf("0:%s:%s:%s", f.projectID, inviteBatchReasonRoleNotGrantable, ErrCodeTargetRoleProtected),
		fmt.Sprintf("1:%s:%s", missing, inviteBatchReasonProjectNotManageable),
	}, items, "one refusals item per refused entry")
	assert.Empty(t, f.events(InviteAuditUserInvited))
	for _, email := range []string{"ref-a@example.com", "ref-b@example.com", "ref-c@example.com"} {
		f.noUser(t, email)
	}
}

// TestInviteBatch_DecodeRefusalWritesNoAudit: a 400 decode or cap refusal
// writes no audit record.
func TestInviteBatch_DecodeRefusalWritesNoAudit(t *testing.T) {
	f := newBatchFixture(t)
	rec := doRequestAsUser(t, f.srv, f.owner, http.MethodPost, "/api/v1/admin/users/invite/bulk",
		map[string]interface{}{"emails": []map[string]string{{"email": "x@example.com"}}, "unexpected": true})
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	rec = f.post(t, f.owner, UserInviteBulkRequest{
		Emails:   batchEmails("y@example.com"),
		Projects: make([]UserInviteBulkProject, inviteBulkMaxProjects+1),
	})
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	f.audit.mu.Lock()
	defer f.audit.mu.Unlock()
	assert.Empty(t, f.audit.invite, "no audit record for a malformed request")
}

// TestInviteBatch_ProjectsRequireUserRead: a caller without user.read who
// names project memberships is refused before any write, in the shape of
// a preflight refusal and with its one audit record, because membership
// results would tell invited rows from active or suspended ones. Without
// projects the same caller still invites, with the collapsed rows.
func TestInviteBatch_ProjectsRequireUserRead(t *testing.T) {
	f := newBatchFixture(t)
	// Created directly, without hub-members membership: the hub-member
	// role grants user.read.
	ctx := context.Background()
	callerID := tid(t.Name() + "-inviter")
	require.NoError(t, f.store.CreateUser(ctx, &store.User{ID: callerID, Email: callerID + "@example.com", DisplayName: "Inviter", Role: store.UserRoleMember, Status: store.UserStatusActive}))
	caller, err := f.store.GetUser(ctx, callerID)
	require.NoError(t, err)
	batchGrantHubPerms(t, f.store, caller.ID, "user.invite")
	grpBind(t, f.store, store.RoleBindingPrincipalUser, caller.ID, f.ownerRD.ID, f.projectID)
	// Preconditions, so the test cannot pass or fail for the wrong reason:
	// the caller can manage the project and does not hold user.read.
	ident := mmrServiceIdentity(caller.ID, caller.Email)
	require.True(t, f.srv.authzService.CheckAccess(ctx, ident, Resource{Type: "project", ID: f.projectID}, ActionManage).Allowed,
		"precondition: the caller can manage the project")
	require.False(t, f.srv.authzService.Decide(ctx, AuthzRequest{
		Principal:  principalContextForIdentity(ident),
		Credential: credentialContextForIdentity(ident),
		Resource:   hubScopedResource("user", "hub"),
		Action:     ActionRead,
		Permission: "user.read",
	}).Allowed, "precondition: the caller must not hold user.read")
	pending := newInvitedUser(t, f.store, tid(t.Name()+"-pending"), "noread-pending@example.com")

	rec := f.post(t, caller, UserInviteBulkRequest{
		Emails:   batchEmails("noread-new@example.com", pending.Email),
		Projects: []UserInviteBulkProject{{ProjectID: f.projectID, RoleDefinitionIDs: []string{f.memberRD.ID}}},
	})
	batchID, refusals := batchRefusal(t, rec)
	assert.Equal(t, []InviteBatchProjectRefusal{{Index: 0, ProjectID: f.projectID, Reason: inviteBatchReasonUserReadRequired}}, refusals)
	f.noUser(t, "noread-new@example.com")
	assert.Empty(t, mmrBindingsFor(t, f.store, store.RoleBindingPrincipalUser, pending.ID, f.projectID), "no binding for the existing row")
	bulk := f.events(InviteAuditUserInvitedBulk)
	require.Len(t, bulk, 1, "one audit record for the refused run")
	assert.False(t, bulk[0].Success)
	assert.Equal(t, inviteBatchReasonProjectsNotGrantable, bulk[0].FailReason)
	assert.Equal(t, batchID, bulk[0].Details["batch_id"])
	assert.Equal(t, fmt.Sprintf("0:%s:%s", f.projectID, inviteBatchReasonUserReadRequired), bulk[0].Details["refusals"])
	assert.Empty(t, f.events(InviteAuditUserInvited))

	resp := f.ok(t, caller, UserInviteBulkRequest{Emails: batchEmails("noread-new@example.com", pending.Email)})
	require.Len(t, resp.Results, 2)
	assert.Equal(t, inviteBatchStatusCreated, resp.Results[0].Status)
	assert.Equal(t, inviteBatchStatusExists, resp.Results[1].Status, "collapsed row without user.read")
	assert.Empty(t, resp.Results[1].UserID)
	assert.Empty(t, resp.Results[1].Memberships)
}
