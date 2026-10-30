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
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The tests in this file drive the real executor with the server as its
// fire-time authorizer, against a real store. Each refusal case first
// writes a hook that passes the check, then changes the writer or the
// account, then fires the hook: no token may be minted and no request may
// be sent.

// fireFixture is a server, a hub-scoped verified service account, a
// hub-admin writer created before the hook, and a counting HTTP target.
type fireFixture struct {
	srv       *Server
	store     store.Store
	sa        *store.GCPServiceAccount
	writer    *store.User
	bindingID string
	tokenGen  *mockTokenGenerator
	audit     *capturingAuditLogger
	executor  *HTTPExecutor
	target    *httptest.Server
	requests  atomic.Int32
	lastAuth  atomic.Value
}

func newFireFixture(t *testing.T) *fireFixture {
	t.Helper()
	srv, s := testServer(t)
	ctx := context.Background()
	seedRoleDefinitions(ctx, s)

	f := &fireFixture{srv: srv, store: s}
	f.writer, f.bindingID = createFireUser(t, s, "hook-writer@example.com", time.Now().Add(-time.Hour))

	f.sa = &store.GCPServiceAccount{
		ID:                 uuid.New().String(),
		Scope:              store.ScopeHub,
		ScopeID:            "test-hub-id",
		Email:              "hook-sa@gcp-project.iam.gserviceaccount.com",
		ProjectID:          "gcp-project",
		DisplayName:        "Hook SA",
		Verified:           true,
		VerifiedAt:         time.Now(),
		VerificationStatus: store.GCPVerificationVerified,
		CreatedBy:          "test",
		CreatedAt:          time.Now(),
	}
	require.NoError(t, s.CreateGCPServiceAccount(ctx, f.sa))

	f.target = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests.Add(1)
		f.lastAuth.Store(r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(f.target.Close)

	f.tokenGen = &mockTokenGenerator{accessToken: "fire-token", email: "hub@sa.com"}
	f.audit = newCapturingAuditLogger()
	f.executor = newTestExecutorWithAuthorizer(s, f.tokenGen, f.audit, slog.Default(), srv)
	setHookIdentityMode(srv, SAAssignCheckOff, nil, nil)
	return f
}

// createFireUser creates an active user with the given creation time and a
// system hub-admin binding, which carries hub.lifecycle_hooks.update. It
// returns the user and the binding ID.
func createFireUser(t *testing.T, s store.Store, email string, created time.Time) (*store.User, string) {
	t.Helper()
	ctx := context.Background()
	u := &store.User{
		ID: uuid.New().String(), Email: email, DisplayName: email,
		Role: store.UserRoleMember, Status: store.UserStatusActive, Created: created,
	}
	require.NoError(t, s.CreateUser(ctx, u))
	rd, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleHubAdmin, store.RoleScopeSystem)
	require.NoError(t, err)
	rb, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      u.ID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        "test",
	})
	require.NoError(t, err)
	return u, rb.ID
}

// setHookIdentityMode sets the hook identity check mode, checker and token
// generator the fire-time actAs step resolves through hookIdentityCheckerFor.
func setHookIdentityMode(srv *Server, mode string, checker store.CallerPermissionChecker, gen GCPTokenGenerator) {
	srv.mu.Lock()
	srv.hookIdentityCheckMode = mode
	if checker != nil {
		srv.hookIdentityChecker = checker
	}
	if gen != nil {
		srv.gcpTokenGenerator = gen
	}
	srv.mu.Unlock()
}

// hook returns a stored, enabled hub-scoped http hook written by writer
// (by email) now, after the writer's row was created.
func (f *fireFixture) hook(t *testing.T, writerEmail string) *store.LifecycleHook {
	t.Helper()
	h := &store.LifecycleHook{
		ID:                uuid.New().String(),
		Name:              "fire-check",
		ScopeType:         store.LifecycleHookScopeHub,
		Trigger:           store.LifecycleHookTriggerRunning,
		Action:            &store.LifecycleHookAction{Type: store.LifecycleHookActionHTTP, Method: "POST", URL: f.target.URL + "/hook", Body: `{}`, TimeoutSeconds: 10, OnError: store.LifecycleHookOnErrorLog},
		ExecutionIdentity: f.sa.ID,
		Enabled:           true,
		CreatedBy:         writerEmail,
		Created:           time.Now(),
		Updated:           time.Now(),
	}
	require.NoError(t, f.store.CreateLifecycleHook(context.Background(), h))
	stored, err := f.store.GetLifecycleHook(context.Background(), h.ID)
	require.NoError(t, err)
	return stored
}

// resave re-saves h through the real PUT handler as the development user (a
// hub admin on the test server), keeping its writer, and returns the stored
// hook. The PUT carries an https URL, as ValidateHook requires for an http
// action; the returned hook's action URL is pointed back at the local test
// target in memory, so a fire reaches it.
func (f *fireFixture) resave(t *testing.T, h *store.LifecycleHook, name string) *store.LifecycleHook {
	t.Helper()
	action := *h.Action
	action.URL = "https://hooks.example.com/hook"
	rec := doRequest(t, f.srv, http.MethodPut, "/api/v1/admin/lifecycle-hooks/"+h.ID, updateLifecycleHookRequest{
		Name:              name,
		Trigger:           h.Trigger,
		Action:            &action,
		ExecutionIdentity: h.ExecutionIdentity,
		Enabled:           true,
		StateVersion:      h.StateVersion,
	})
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	stored, err := f.store.GetLifecycleHook(context.Background(), h.ID)
	require.NoError(t, err)
	require.Equal(t, name, stored.Name)
	require.Equal(t, "https://hooks.example.com/hook", stored.Action.URL)
	stored.Action.URL = f.target.URL + "/hook"
	return stored
}

func (f *fireFixture) fire(t *testing.T, h *store.LifecycleHook) error {
	t.Helper()
	return f.executor.Execute(context.Background(), h, makeTestAgent(""), store.LifecycleHookTriggerRunning)
}

// assertMinted asserts the fire minted one token and sent the request with it.
func (f *fireFixture) assertMinted(t *testing.T, err error) {
	t.Helper()
	require.NoError(t, err)
	assert.Equal(t, 1, f.tokenGen.calls, "token minted")
	assert.Equal(t, int32(1), f.requests.Load(), "request sent")
	assert.Equal(t, "Bearer fire-token", f.lastAuth.Load())
}

// assertRefused asserts the fire minted nothing, sent nothing, and was
// refused with cause.
func (f *fireFixture) assertRefused(t *testing.T, err error, cause HookFireDenyCause) {
	t.Helper()
	require.Error(t, err)
	var denied *HookFireDeniedError
	require.True(t, errors.As(err, &denied), "refusal must be a HookFireDeniedError, got %v", err)
	assert.Equal(t, cause, denied.Cause)
	assert.Equal(t, 0, f.tokenGen.calls, "no token may be minted")
	assert.Equal(t, int32(0), f.requests.Load(), "no request may be sent")
	events := f.audit.getEvents()
	require.NotEmpty(t, events, "the refusal must be audited")
	last := events[len(events)-1]
	assert.False(t, last.Success)
	assert.Contains(t, last.FailReason, "hook writer no longer authorized")
	assert.Contains(t, last.FailReason, string(cause))
}

// ---------------------------------------------------------------------------
// Positive controls
// ---------------------------------------------------------------------------

func TestHookFire_AuthorizedWriter_Mints(t *testing.T) {
	f := newFireFixture(t)
	f.assertMinted(t, f.fire(t, f.hook(t, f.writer.Email)))
}

func TestHookFire_WriterEmailCaseInsensitive_Mints(t *testing.T) {
	f := newFireFixture(t)
	f.assertMinted(t, f.fire(t, f.hook(t, strings.ToUpper(f.writer.Email))))
}

func TestHookFire_EnforceMode_ActAsAllowed_Mints(t *testing.T) {
	f := newFireFixture(t)
	checker := &scriptedActAsChecker{outcome: store.ActAsAllowed}
	setHookIdentityMode(f.srv, SAAssignCheckEnforce, checker, f.tokenGen)
	f.assertMinted(t, f.fire(t, f.hook(t, f.writer.Email)))
	require.Len(t, checker.callers(), 1)
	assert.Equal(t, f.writer.ID, checker.callers()[0].ID, "actAs is checked for the writer")
	assert.Equal(t, store.PrincipalUser, checker.callers()[0].Kind)
}

// Webhook actions mint nothing, so the fire-time check does not apply: a
// webhook keeps working whatever happened to its writer.
func TestHookFire_Webhook_NotGated(t *testing.T) {
	f := newFireFixture(t)
	h := f.hook(t, "nobody@example.com")
	h.Action.Type = store.LifecycleHookActionWebhook
	require.NoError(t, f.fire(t, h))
	assert.Equal(t, 0, f.tokenGen.calls)
	assert.Equal(t, int32(1), f.requests.Load())
	assert.Equal(t, "", f.lastAuth.Load())
}

// ---------------------------------------------------------------------------
// Revocation kinds: each one passes at write, then is refused at fire.
// ---------------------------------------------------------------------------

func TestHookFire_WriterRoleRemoved_Refused(t *testing.T) {
	f := newFireFixture(t)
	h := f.hook(t, f.writer.Email)
	require.NoError(t, f.store.DeleteRoleBinding(context.Background(), f.bindingID))
	f.assertRefused(t, f.fire(t, h), HookFireWriterLacksPermission)
}

func TestHookFire_WriterSuspended_Refused(t *testing.T) {
	f := newFireFixture(t)
	h := f.hook(t, f.writer.Email)
	u, err := f.store.GetUser(context.Background(), f.writer.ID)
	require.NoError(t, err)
	u.Status = store.UserStatusSuspended
	require.NoError(t, f.store.UpdateUser(context.Background(), u))
	f.assertRefused(t, f.fire(t, h), HookFireWriterInactive)
}

func TestHookFire_WriterDeleted_Refused(t *testing.T) {
	f := newFireFixture(t)
	h := f.hook(t, f.writer.Email)
	require.NoError(t, f.store.DeleteRoleBinding(context.Background(), f.bindingID))
	require.NoError(t, f.store.DeleteUser(context.Background(), f.writer.ID))
	f.assertRefused(t, f.fire(t, h), HookFireWriterNotFound)
}

// A user re-created after the hook with the deleted writer's email, and
// given the same authority, is not the writer: user delete is a hard
// delete, so the email is free to be taken.
func TestHookFire_WriterRecreatedAfterHook_Refused(t *testing.T) {
	f := newFireFixture(t)
	h := f.hook(t, f.writer.Email)
	require.NoError(t, f.store.DeleteRoleBinding(context.Background(), f.bindingID))
	require.NoError(t, f.store.DeleteUser(context.Background(), f.writer.ID))
	createFireUser(t, f.store, f.writer.Email, time.Now().Add(time.Minute))
	f.assertRefused(t, f.fire(t, h), HookFireWriterNewerThanHook)
}

func TestHookFire_EnforceMode_WriterActAsRevoked_Refused(t *testing.T) {
	f := newFireFixture(t)
	h := f.hook(t, f.writer.Email)
	checker := &scriptedActAsChecker{outcome: store.ActAsDenied}
	setHookIdentityMode(f.srv, SAAssignCheckEnforce, checker, f.tokenGen)
	f.assertRefused(t, f.fire(t, h), HookFireWriterActAsDenied)
}

// Enforce mode with no token generator resolves to the unavailable checker,
// which denies, as it does at write time.
func TestHookFire_EnforceMode_NoGenerator_Refused(t *testing.T) {
	f := newFireFixture(t)
	h := f.hook(t, f.writer.Email)
	setHookIdentityMode(f.srv, SAAssignCheckEnforce, &scriptedActAsChecker{outcome: store.ActAsAllowed}, nil)
	f.srv.mu.Lock()
	f.srv.gcpTokenGenerator = nil
	f.srv.mu.Unlock()
	f.assertRefused(t, f.fire(t, h), HookFireWriterActAsDenied)
}

func TestHookFire_SAUnverified_Refused(t *testing.T) {
	f := newFireFixture(t)
	h := f.hook(t, f.writer.Email)
	f.sa.Verified = false
	f.sa.VerificationStatus = "failed"
	require.NoError(t, f.store.UpdateGCPServiceAccount(context.Background(), f.sa))
	f.assertRefused(t, f.fire(t, h), HookFireIdentityNotAdmissible)
}

func TestHookFire_SAOutOfScope_Refused(t *testing.T) {
	f := newFireFixture(t)
	h := f.hook(t, f.writer.Email)
	h.ScopeType = store.LifecycleHookScopeProject
	h.ScopeID = uuid.New().String()
	f.assertRefused(t, f.fire(t, h), HookFireIdentityNotAdmissible)
}

// ---------------------------------------------------------------------------
// Writer records that do not resolve to a live user: all fail closed.
// ---------------------------------------------------------------------------

func TestHookFire_EmptyWriter_Refused(t *testing.T) {
	f := newFireFixture(t)
	f.assertRefused(t, f.fire(t, f.hook(t, "")), HookFireWriterMissing)
}

// The writer's address changed (an identity-provider email change rewrites
// user.Email at sign-in): the hook's recorded email no longer resolves.
func TestHookFire_WriterEmailChanged_Refused(t *testing.T) {
	f := newFireFixture(t)
	h := f.hook(t, f.writer.Email)
	u, err := f.store.GetUser(context.Background(), f.writer.ID)
	require.NoError(t, err)
	u.Email = "hook-writer-renamed@example.com"
	require.NoError(t, f.store.UpdateUser(context.Background(), u))
	f.assertRefused(t, f.fire(t, h), HookFireWriterNotFound)
}

// A hook written by the development identity mints while the seeded row
// carries the same email.
func TestHookFire_DevWriter_Mints(t *testing.T) {
	f := newFireFixture(t)
	dev, err := f.store.GetUser(context.Background(), DevUserID)
	require.NoError(t, err)
	f.assertMinted(t, f.fire(t, f.hook(t, dev.Email)))
}

// The development user's email changed: the configured development identity
// (whose email the write path records) no longer matches the seeded row,
// which seedDevUser never refreshes. Hooks written under either address that
// no row carries do not resolve and fail closed, as ruled for a changed
// development email.
func TestHookFire_DevEmailChanged_Refused(t *testing.T) {
	f := newFireFixture(t)
	dev, err := f.store.GetUser(context.Background(), DevUserID)
	require.NoError(t, err)
	configured := NewDevUser(f.srv.config.DevUserConfig).Email()
	dev.Email = "seeded-dev@localhost"
	require.NoError(t, f.store.UpdateUser(context.Background(), dev))
	f.assertRefused(t, f.fire(t, f.hook(t, configured)), HookFireWriterNotFound)
	f.assertRefused(t, f.fire(t, f.hook(t, "old-dev@localhost")), HookFireWriterNotFound)
}

// The development identity's configured email changed after the row was
// seeded: the row keeps the old email, so a hook written under that old
// email (before the change) still resolves to the development user and
// mints. Only the email no row carries fails closed.
func TestHookFire_DevRowKeepsOldEmail_OldEmailHook_Mints(t *testing.T) {
	f := newFireFixture(t)
	dev, err := f.store.GetUser(context.Background(), DevUserID)
	require.NoError(t, err)
	dev.Email = "old-dev@localhost"
	require.NoError(t, f.store.UpdateUser(context.Background(), dev))
	require.NotEqual(t, dev.Email, NewDevUser(f.srv.config.DevUserConfig).Email(), "the configured identity has moved on")
	f.assertMinted(t, f.fire(t, f.hook(t, "old-dev@localhost")))
}

// A fire-time decision that cannot be made (a role-binding lookup fault,
// an indeterminate deny) refuses the mint.
func TestHookFire_WriterDecisionIndeterminate_Refused(t *testing.T) {
	f := newFireFixture(t)
	h := f.hook(t, f.writer.Email)
	orig := f.srv.authzService.store
	f.srv.authzService.store = &failingRoleBindingStore{Store: orig}
	t.Cleanup(func() { f.srv.authzService.store = orig })

	writer := NewAuthenticatedUser(f.writer.ID, f.writer.Email, f.writer.DisplayName, f.writer.Role, "")
	d := f.srv.authzService.Decide(maskAuthzInputs(context.Background()), AuthzRequest{
		Principal:  principalContextForIdentity(writer),
		Credential: credentialContextForIdentity(writer),
		Resource:   Resource{Type: "hub", ID: "hub"},
		Action:     Action("update"),
		Permission: hookWritePermission,
	})
	require.True(t, d.IsIndeterminate(), "the fault makes the decision indeterminate, got %+v", d)

	f.assertRefused(t, f.fire(t, h), HookFireWriterLacksPermission)
}

// The development user's hooks stop when development authority is off.
func TestHookFire_DevWriter_DevAuthorityOff_Refused(t *testing.T) {
	f := newFireFixture(t)
	dev, err := f.store.GetUser(context.Background(), DevUserID)
	require.NoError(t, err, "test server seeds the development user")
	h := f.hook(t, dev.Email)
	f.srv.authzService.setDevLocalAuthorityEnabled(false)
	f.assertRefused(t, f.fire(t, h), HookFireWriterInactive)
}

// A federated account is never resolved by email (GetUserByEmail excludes
// federated rows), so a hook naming one fails closed.
func TestHookFire_FederatedWriter_Refused(t *testing.T) {
	f := newFireFixture(t)
	issuer := "https://peer.example.com"
	fedEmail := "fed-writer@peer.example.com"
	fu := &store.User{
		Kind:           store.UserKindFederated,
		HomeIssuer:     &issuer,
		FederatedEmail: &fedEmail,
		Status:         store.UserStatusActive,
		Created:        time.Now().Add(-time.Hour),
	}
	require.NoError(t, f.store.CreateFederatedUser(context.Background(), fu))
	f.assertRefused(t, f.fire(t, f.hook(t, fu.Email)), HookFireWriterNotFound)
}

// A hook re-saved by another admin still names its creator (update does not
// change CreatedBy), so it stops when the creator loses authority.
func TestHookFire_ResavedByOtherAdmin_CreatorDemoted_Refused(t *testing.T) {
	f := newFireFixture(t)
	h := f.hook(t, f.writer.Email)
	require.NoError(t, f.store.DeleteRoleBinding(context.Background(), f.bindingID))

	// The development user (a hub admin on the test server) re-saves it.
	resaved := f.resave(t, h, h.Name+"-resaved")
	require.Equal(t, f.writer.Email, resaved.CreatedBy, "update keeps the creator")

	f.assertRefused(t, f.fire(t, resaved), HookFireWriterLacksPermission)
}

// A legitimate update does not trip the creation-time guard: the hook's
// Created is immutable, so a re-save leaves the compared timestamp alone.
func TestHookFire_UpdatedHook_KeepsCreated_Mints(t *testing.T) {
	f := newFireFixture(t)
	h := f.hook(t, f.writer.Email)
	created := h.Created

	updated := f.resave(t, h, h.Name+"-edited")
	require.True(t, updated.Created.Equal(created), "update must not change Created")
	require.Equal(t, f.writer.Email, updated.CreatedBy)

	f.assertMinted(t, f.fire(t, updated))
}

// ---------------------------------------------------------------------------
// Wiring
// ---------------------------------------------------------------------------

// An executor built without an authorizer refuses every mint.
func TestHookFire_NilAuthorizer_Refused(t *testing.T) {
	f := newFireFixture(t)
	f.executor = newTestExecutorWithAuthorizer(f.store, f.tokenGen, f.audit, slog.Default(), nil)
	f.assertRefused(t, f.fire(t, f.hook(t, f.writer.Email)), HookFireAuthorizerUnavailable)
}

// The production evaluator's executor is wired to the server.
func TestHookFire_ServerExecutorUsesServerAuthorizer(t *testing.T) {
	srv, _ := testServer(t)
	e := NewHTTPExecutor(srv.store, nil, nil, nil, srv)
	assert.Same(t, srv, e.fireAuthorizer)
	e = NewHTTPExecutor(srv.store, nil, nil, nil, nil)
	assert.IsType(t, denyingHookFireAuthorizer{}, e.fireAuthorizer)
}

// ---------------------------------------------------------------------------
// Refusal audit rate limit
// ---------------------------------------------------------------------------

func TestHookFire_RefusalAudit_RateLimitedPerHook(t *testing.T) {
	f := newFireFixture(t)
	clock := &fakeClock{now: time.Now()}
	f.executor.refusals = newHookRefusalLimiter(10*time.Minute, clock.Now)

	h := f.hook(t, "")
	other := f.hook(t, "")
	for i := 0; i < 5; i++ {
		require.Error(t, f.fire(t, h))
	}
	require.Len(t, f.audit.getEvents(), 1, "repeated refusals of one hook are recorded once per interval")

	// Another hook is limited separately.
	require.Error(t, f.fire(t, other))
	require.Len(t, f.audit.getEvents(), 2)

	// After the interval the next refusal is recorded with the count left out.
	clock.advance(10 * time.Minute)
	require.Error(t, f.fire(t, h))
	events := f.audit.getEvents()
	require.Len(t, events, 3)
	assert.Contains(t, events[2].FailReason, "4 similar refusals for this hook not recorded")

	// A different cause is recorded at once.
	h.CreatedBy = "nobody@example.com"
	require.Error(t, f.fire(t, h))
	events = f.audit.getEvents()
	require.Len(t, events, 4)
	assert.Contains(t, events[3].FailReason, string(HookFireWriterNotFound))
	assert.Equal(t, 0, f.tokenGen.calls)
}

func TestHookRefusalLimiter_PrunesStaleEntries(t *testing.T) {
	clock := &fakeClock{now: time.Now()}
	l := newHookRefusalLimiter(time.Minute, clock.Now)
	for i := 0; i < hookRefusalLimiterMaxEntries; i++ {
		rec, _ := l.admit(uuid.New().String(), "c")
		require.True(t, rec)
	}
	clock.advance(time.Minute)
	rec, _ := l.admit("new", "c")
	require.True(t, rec)
	assert.Len(t, l.entries, 1, "stale entries are pruned once the limit is reached")
}

// TestHookRefusalLimiter_PruneScanAtMostOncePerInterval: once the limit is
// reached, the prune scan runs at most once per interval. A new hook within
// an interval of the last scan is admitted without scanning (stale entries
// stay), and the next scan after the interval removes them.
func TestHookRefusalLimiter_PruneScanAtMostOncePerInterval(t *testing.T) {
	clock := &fakeClock{now: time.Now()}
	l := newHookRefusalLimiter(time.Minute, clock.Now)
	for i := 0; i < hookRefusalLimiterMaxEntries; i++ {
		rec, _ := l.admit(uuid.New().String(), "c")
		require.True(t, rec)
	}
	clock.advance(59 * time.Second)
	rec, _ := l.admit("first", "c")
	require.True(t, rec)
	require.Len(t, l.entries, hookRefusalLimiterMaxEntries+1, "a scan with nothing stale removes nothing")

	clock.advance(time.Second) // the original entries are now stale
	rec, _ = l.admit("second", "c")
	require.True(t, rec)
	assert.Len(t, l.entries, hookRefusalLimiterMaxEntries+2, "no second scan within the interval of the last one")

	clock.advance(59 * time.Second)
	rec, _ = l.admit("third", "c")
	require.True(t, rec)
	// Left: "second" (59 s old) and "third"; the originals and "first" (60 s
	// old) are stale.
	assert.Len(t, l.entries, 2, "the next scan, an interval after the last, removes the stale entries")
	assert.Contains(t, l.entries, "second")
	assert.Contains(t, l.entries, "third")
}

func TestHookFire_ActAsAuditSink_RateLimitsDenials(t *testing.T) {
	sink := &capturingSASink{}
	clock := &fakeClock{now: time.Now()}
	r := &rateLimitedActAsAuditSink{inner: sink, hookID: "h1", limiter: newHookRefusalLimiter(10*time.Minute, clock.Now)}
	denied, allowed := store.ActAsDenied, store.ActAsAllowed
	for i := 0; i < 3; i++ {
		require.NoError(t, r.RecordSAAssignment(context.Background(), &store.SAAssignmentEvent{Decision: &denied, Reason: "no"}))
		require.NoError(t, r.RecordSAAssignment(context.Background(), &store.SAAssignmentEvent{Decision: &allowed}))
	}
	require.Len(t, sink.events, 4, "one denial and every allow")
	clock.advance(10 * time.Minute)
	require.NoError(t, r.RecordSAAssignment(context.Background(), &store.SAAssignmentEvent{Decision: &denied, Reason: "no"}))
	require.Len(t, sink.events, 5)
	assert.Contains(t, sink.events[4].Reason, "2 similar refusals for this hook not recorded")
}

// ---------------------------------------------------------------------------
// Test doubles
// ---------------------------------------------------------------------------

type scriptedActAsChecker struct {
	mu      sync.Mutex
	outcome store.ActAsOutcome
	calls   []store.Principal
}

func (c *scriptedActAsChecker) CanActAs(_ context.Context, caller store.Principal, _ *store.GCPServiceAccount) (store.ActAsResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, caller)
	return store.ActAsResult{Outcome: c.outcome, Mechanism: "scripted", Reason: "scripted"}, nil
}

func (c *scriptedActAsChecker) callers() []store.Principal {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]store.Principal(nil), c.calls...)
}

type capturingSASink struct {
	events []*store.SAAssignmentEvent
}

func (c *capturingSASink) RecordSAAssignment(_ context.Context, e *store.SAAssignmentEvent) error {
	c.events = append(c.events, e)
	return nil
}

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}
