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
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// Regression coverage: the resolver's existing-BINDING branch and the
// unique-email collision-winner handback in provisionNewUser must apply the
// same live sign-in policy and account-state handling as the email-match
// branch (see sign_in_policy.go), so an already-linked identity is
// re-checked on every issuance, not just at first link, and a collision
// winner is handed back policy-checked too.
// ---------------------------------------------------------------------------

// TestGoogleIdentityResolver_ExistingBinding_InvitedActivatesConsistently is
// an end-to-end regression: a record already bound to a Google identity, but
// still invited, must be activated and granted hub access identically to
// the email-match branch and to interactive login.
func TestGoogleIdentityResolver_ExistingBinding_InvitedActivatesConsistently(t *testing.T) {
	identity := validGmailIdentity()
	h := newSignInPolicyHarness(t, ServerConfig{}, &fakeGoogleValidator{idTokenResult: identity})

	ctx := context.Background()
	userID := uuid.New().String()
	invitedBy := "admin@example.com"
	if err := h.store.CreateUser(ctx, &store.User{
		ID:        userID,
		Email:     identity.Email,
		Role:      store.UserRoleMember,
		Status:    store.UserStatusInvited,
		InvitedBy: &invitedBy,
		Created:   time.Now(),
	}); err != nil {
		t.Fatalf("seed invited user: %v", err)
	}
	now := time.Now()
	if err := h.extStore.CreateExternalIdentity(ctx, &ExternalIdentityBinding{
		ID:        uuid.New().String(),
		Provider:  "google",
		Issuer:    googleCanonicalIssuer,
		Subject:   identity.Subject,
		UserID:    userID,
		Email:     identity.Email,
		CreatedAt: now,
		UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed binding: %v", err)
	}

	user, err := h.resolver.Resolve(ctx, identity, ResolvePolicy{})
	if err != nil {
		t.Fatalf("resolve failed: %v", err)
	}
	if user.ID != userID {
		t.Fatalf("expected the bound record to be reused, got %q", user.ID)
	}
	if user.Status != store.UserStatusActive {
		t.Fatalf("expected the bound invited record to activate, got status=%q", user.Status)
	}
	if !isHubMember(t, h.store, userID) {
		t.Error("expected the activated user to be granted hub-members access")
	}
}

// TestGoogleIdentityResolver_ExistingBinding_NotAuthorized_DeniedNoMutation
// is a negative regression: an already-bound identity whose email now fails
// the live sign-in policy must be denied on the next issuance — with no
// state mutation — even though it was already linked.
func TestGoogleIdentityResolver_ExistingBinding_NotAuthorized_DeniedNoMutation(t *testing.T) {
	identity := validWorkspaceIdentity() // user@company.com
	h := newSignInPolicyHarness(t, ServerConfig{
		UserAccessMode:    "domain_restricted",
		AuthorizedDomains: []string{"not-company.com"},
	}, &fakeGoogleValidator{idTokenResult: identity})

	ctx := context.Background()
	userID := uuid.New().String()
	if err := h.store.CreateUser(ctx, &store.User{
		ID:      userID,
		Email:   identity.Email,
		Role:    store.UserRoleMember,
		Status:  store.UserStatusActive,
		Created: time.Now(),
	}); err != nil {
		t.Fatalf("seed active user: %v", err)
	}
	now := time.Now()
	if err := h.extStore.CreateExternalIdentity(ctx, &ExternalIdentityBinding{
		ID:        uuid.New().String(),
		Provider:  "google",
		Issuer:    googleCanonicalIssuer,
		Subject:   identity.Subject,
		UserID:    userID,
		Email:     identity.Email,
		CreatedAt: now,
		UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed binding: %v", err)
	}

	_, err := h.resolver.Resolve(ctx, identity, ResolvePolicy{})
	if !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("expected ErrAccessDenied, got %v", err)
	}

	stored, getErr := h.store.GetUser(ctx, userID)
	if getErr != nil {
		t.Fatalf("get user: %v", getErr)
	}
	if stored.Role != store.UserRoleMember || stored.Status != store.UserStatusActive {
		t.Fatalf("expected the denied record to be unchanged, got role=%q status=%q", stored.Role, stored.Status)
	}
	binding, bindErr := h.extStore.GetExternalIdentity(ctx, "google", googleCanonicalIssuer, identity.Subject)
	if bindErr != nil {
		t.Fatalf("expected the existing binding to remain, lookup failed: %v", bindErr)
	}
	if binding.UserID != userID {
		t.Fatalf("expected the binding to still point at the original user, got %q", binding.UserID)
	}
}

// countingUserStore wraps a real store.UserStore, counting UpdateUser calls
// so a test can assert that a repeat resolution with no state change
// performs zero persistence — the bound branch must not turn every request
// into a write.
type countingUserStore struct {
	store.UserStore
	updateUserCalls int
}

func (s *countingUserStore) UpdateUser(ctx context.Context, user *store.User) error {
	s.updateUserCalls++
	return s.UserStore.UpdateUser(ctx, user)
}

// TestGoogleIdentityResolver_ExistingBinding_NoChange_ZeroUpdateUser is a
// repeat-issuance regression: resolving an already-bound, already-active
// identity whose profile has nothing to backfill and whose role does not
// change must not write the user row at all. This branch runs on every
// external-bearer request for an already-linked identity, so an
// unconditional per-request write would risk overwriting a concurrent admin
// change (status or role) with the stale value this request read.
func TestGoogleIdentityResolver_ExistingBinding_NoChange_ZeroUpdateUser(t *testing.T) {
	identity := validGmailIdentity()
	h := newSignInPolicyHarness(t, ServerConfig{}, &fakeGoogleValidator{idTokenResult: identity})

	ctx := context.Background()
	userID := uuid.New().String()
	if err := h.store.CreateUser(ctx, &store.User{
		ID:          userID,
		Email:       identity.Email,
		DisplayName: identity.DisplayName, // already matches: no profile backfill
		Role:        store.UserRoleMember,
		Status:      store.UserStatusActive,
		Created:     time.Now(),
	}); err != nil {
		t.Fatalf("seed active user: %v", err)
	}
	now := time.Now()
	if err := h.extStore.CreateExternalIdentity(ctx, &ExternalIdentityBinding{
		ID:        uuid.New().String(),
		Provider:  "google",
		Issuer:    googleCanonicalIssuer,
		Subject:   identity.Subject,
		UserID:    userID,
		Email:     identity.Email, // already matches: no email drift
		CreatedAt: now,
		UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed binding: %v", err)
	}

	counting := &countingUserStore{UserStore: h.store}
	resolver := NewGoogleIdentityResolver(counting, h.extStore, h.srv.isUserAuthorized, nil, nil)

	// Wire the real production role-evaluation and grant-sync callbacks
	// (unlike a bare resolver, so a regression that makes production
	// roleFor/syncGrants report a spurious change on every call is actually
	// caught here), but keep persistence routed through the counting
	// wrapper so both counters stay observable.
	deps := h.srv.signInPolicyDeps()
	deps.UpdateUser = counting.UpdateUser
	syncGrantsCalls := 0
	realSyncGrants := deps.syncGrants
	deps.syncGrants = func(ctx context.Context, userID, role string) error {
		syncGrantsCalls++
		return realSyncGrants(ctx, userID, role)
	}
	resolver.SetSignInPolicyDeps(deps)

	if _, err := resolver.Resolve(ctx, identity, ResolvePolicy{}); err != nil {
		t.Fatalf("resolve failed: %v", err)
	}
	if counting.updateUserCalls != 0 {
		t.Fatalf("expected zero UpdateUser calls when nothing changed, got %d", counting.updateUserCalls)
	}
	if syncGrantsCalls != 0 {
		t.Fatalf("expected zero syncGrants calls when nothing changed, got %d", syncGrantsCalls)
	}
}

// TestGoogleIdentityResolver_ExistingBinding_EmailChange_NoMutationOnDenialThenSynced
// is a regression on the "no mutation on denial" invariant for a presented-
// email change: a denied re-issuance must leave both the profile email and
// the binding's recorded email unchanged (not just the profile email), and
// a later admitted re-issuance with the same presented email must update
// both together.
func TestGoogleIdentityResolver_ExistingBinding_EmailChange_NoMutationOnDenialThenSynced(t *testing.T) {
	const oldEmail = "user@company.com"
	const newEmail = "user@other.com"
	const sub = "google-sub-email-change-1"

	h := newSignInPolicyHarness(t, ServerConfig{
		UserAccessMode:    "domain_restricted",
		AuthorizedDomains: []string{"company.com"},
	}, &fakeGoogleValidator{})

	ctx := context.Background()
	userID := uuid.New().String()
	if err := h.store.CreateUser(ctx, &store.User{
		ID: userID, Email: oldEmail, Role: store.UserRoleMember, Status: store.UserStatusActive, Created: time.Now(),
	}); err != nil {
		t.Fatalf("seed active user: %v", err)
	}
	now := time.Now()
	if err := h.extStore.CreateExternalIdentity(ctx, &ExternalIdentityBinding{
		ID: uuid.New().String(), Provider: "google", Issuer: googleCanonicalIssuer, Subject: sub,
		UserID: userID, Email: oldEmail, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed binding: %v", err)
	}

	presentedIdentity := &ValidatedGoogleIdentity{
		Subject: sub, Email: newEmail, EmailVerified: true, Issuer: googleCanonicalIssuer,
		UpstreamExpiry: time.Now().Add(time.Hour),
	}

	// The new email fails the domain_restricted policy: denied.
	if _, err := h.resolver.Resolve(ctx, presentedIdentity, ResolvePolicy{}); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("expected ErrAccessDenied, got %v", err)
	}

	stored, err := h.store.GetUser(ctx, userID)
	if err != nil {
		t.Fatalf("get user: %v", err)
	}
	if stored.Email != oldEmail {
		t.Fatalf("expected profile email unchanged after denial, got %q", stored.Email)
	}
	binding, err := h.extStore.GetExternalIdentity(ctx, "google", googleCanonicalIssuer, sub)
	if err != nil {
		t.Fatalf("get binding: %v", err)
	}
	if binding.Email != oldEmail {
		t.Fatalf("expected binding email unchanged after denial, got %q", binding.Email)
	}

	// The same presented email is admitted later (PreAuthorized skips the
	// still-failing domain policy) — both records must now update together.
	user, err := h.resolver.Resolve(ctx, presentedIdentity, ResolvePolicy{PreAuthorized: true})
	if err != nil {
		t.Fatalf("resolve failed: %v", err)
	}
	if user.Email != newEmail {
		t.Fatalf("expected the returned user email to be %q, got %q", newEmail, user.Email)
	}
	stored, err = h.store.GetUser(ctx, userID)
	if err != nil {
		t.Fatalf("get user: %v", err)
	}
	if stored.Email != newEmail {
		t.Fatalf("expected profile email updated after admission, got %q", stored.Email)
	}
	binding, err = h.extStore.GetExternalIdentity(ctx, "google", googleCanonicalIssuer, sub)
	if err != nil {
		t.Fatalf("get binding: %v", err)
	}
	if binding.Email != newEmail {
		t.Fatalf("expected binding email updated after admission, got %q", binding.Email)
	}
}

// collisionUserStore wraps a real store.UserStore, deterministically
// simulating the unique-email-collision window that a concurrent resolution
// can hit in production without needing an actual race: the first
// GetUserByEmail call (matching Resolve's own pre-check before it ever
// reaches provisionNewUser) reports not-found, CreateUser always reports an
// existing row, and every call after the first answers from the real store
// — so the "winner" provisionNewUser hands back is a genuine record.
type collisionUserStore struct {
	store.UserStore
	getByEmailCalls int
}

func (s *collisionUserStore) GetUserByEmail(ctx context.Context, email string) (*store.User, error) {
	s.getByEmailCalls++
	if s.getByEmailCalls == 1 {
		return nil, store.ErrNotFound
	}
	return s.UserStore.GetUserByEmail(ctx, email)
}

func (s *collisionUserStore) CreateUser(context.Context, *store.User) error {
	return store.ErrAlreadyExists
}

// TestGoogleIdentityResolver_CollisionWinner_DeniedFailClosed is a negative
// regression on the unique-email collision-winner handback: a suspended
// winner must still be denied (fail-closed), the same guarantee as every
// other existing-record path, now applied through the shared helper instead
// of a standalone suspension check.
func TestGoogleIdentityResolver_CollisionWinner_DeniedFailClosed(t *testing.T) {
	identity := validGmailIdentity()
	h := newSignInPolicyHarness(t, ServerConfig{}, &fakeGoogleValidator{idTokenResult: identity})

	ctx := context.Background()
	winnerID := uuid.New().String()
	if err := h.store.CreateUser(ctx, &store.User{
		ID:      winnerID,
		Email:   identity.Email,
		Role:    store.UserRoleMember,
		Status:  store.UserStatusSuspended,
		Created: time.Now(),
	}); err != nil {
		t.Fatalf("seed suspended winner: %v", err)
	}

	resolver := NewGoogleIdentityResolver(&collisionUserStore{UserStore: h.store}, h.extStore, h.srv.isUserAuthorized, nil, nil)
	resolver.SetSignInPolicyDeps(h.srv.signInPolicyDeps())

	_, err := resolver.Resolve(ctx, identity, ResolvePolicy{})
	if !errors.Is(err, ErrUserSuspended) {
		t.Fatalf("expected ErrUserSuspended for the collision winner, got %v", err)
	}
	if _, lookupErr := h.extStore.GetExternalIdentity(ctx, "google", googleCanonicalIssuer, identity.Subject); !errors.Is(lookupErr, store.ErrNotFound) {
		t.Fatalf("expected no external identity binding to be created on denial, lookup returned err=%v", lookupErr)
	}
}

// TestGoogleIdentityResolver_CollisionWinner_InvitedActivatesConsistently is
// an end-to-end regression on the unique-email collision-winner handback: an
// invited winner must activate, be granted the correct role and hub-members
// access, and have a binding created for it — the same outcome as every
// other existing-record path, not just a suspension check.
func TestGoogleIdentityResolver_CollisionWinner_InvitedActivatesConsistently(t *testing.T) {
	identity := validGmailIdentity()
	h := newSignInPolicyHarness(t, ServerConfig{}, &fakeGoogleValidator{idTokenResult: identity})

	ctx := context.Background()
	winnerID := uuid.New().String()
	invitedBy := "admin@example.com"
	if err := h.store.CreateUser(ctx, &store.User{
		ID:        winnerID,
		Email:     identity.Email,
		Role:      store.UserRoleMember, // placeholder role on an invited row
		Status:    store.UserStatusInvited,
		InvitedBy: &invitedBy,
		Created:   time.Now(),
	}); err != nil {
		t.Fatalf("seed invited winner: %v", err)
	}

	resolver := NewGoogleIdentityResolver(&collisionUserStore{UserStore: h.store}, h.extStore, h.srv.isUserAuthorized, nil, nil)
	resolver.SetSignInPolicyDeps(h.srv.signInPolicyDeps())

	user, err := resolver.Resolve(ctx, identity, ResolvePolicy{})
	if err != nil {
		t.Fatalf("resolve failed: %v", err)
	}
	if user.ID != winnerID {
		t.Fatalf("expected the collision winner to be reused, got %q", user.ID)
	}
	if user.Status != store.UserStatusActive {
		t.Fatalf("expected the invited winner to activate, got status=%q", user.Status)
	}
	if user.Role != store.UserRoleMember {
		t.Fatalf("expected role %q, got %q", store.UserRoleMember, user.Role)
	}
	if !isHubMember(t, h.store, winnerID) {
		t.Error("expected the activated winner to be granted hub-members access")
	}
	if _, lookupErr := h.extStore.GetExternalIdentity(ctx, "google", googleCanonicalIssuer, identity.Subject); lookupErr != nil {
		t.Errorf("expected a binding to be created for the collision winner: %v", lookupErr)
	}
}

// TestGoogleIdentityResolver_CollisionWinner_ActiveWinner_GrantsRestored
// covers the collision-winner handback's AlwaysPersist setting for an
// already-active winner whose role does not change: with AlwaysPersist,
// grants are synced on every handback, not only on activation or a role
// change, so a winner whose hub-members membership is missing (for example,
// from an earlier grant-sync failure) gets it restored on this call alone.
func TestGoogleIdentityResolver_CollisionWinner_ActiveWinner_GrantsRestored(t *testing.T) {
	identity := validGmailIdentity()
	h := newSignInPolicyHarness(t, ServerConfig{}, &fakeGoogleValidator{idTokenResult: identity})

	ctx := context.Background()
	winnerID := uuid.New().String()
	if err := h.store.CreateUser(ctx, &store.User{
		ID:      winnerID,
		Email:   identity.Email,
		Role:    store.UserRoleMember,
		Status:  store.UserStatusActive,
		Created: time.Now(),
	}); err != nil {
		t.Fatalf("seed active winner: %v", err)
	}
	// Deliberately do not add the winner to hub-members: simulates a
	// membership that was never synced (or was lost), which only an
	// unconditional grant sync on this handback — not one gated on a role
	// change — would repair.
	if isHubMember(t, h.store, winnerID) {
		t.Fatal("test setup: winner must not already be a hub member")
	}

	resolver := NewGoogleIdentityResolver(&collisionUserStore{UserStore: h.store}, h.extStore, h.srv.isUserAuthorized, nil, nil)
	resolver.SetSignInPolicyDeps(h.srv.signInPolicyDeps())

	user, err := resolver.Resolve(ctx, identity, ResolvePolicy{})
	if err != nil {
		t.Fatalf("resolve failed: %v", err)
	}
	if user.ID != winnerID {
		t.Fatalf("expected the collision winner to be reused, got %q", user.ID)
	}
	if user.Role != store.UserRoleMember {
		t.Fatalf("expected role to stay %q, got %q", store.UserRoleMember, user.Role)
	}
	if !isHubMember(t, h.store, winnerID) {
		t.Error("expected the winner's missing hub-members grant to be restored on this handback")
	}
}

// ---------------------------------------------------------------------------
// Fail-closed nil-record guards. store.UserStore is an interface — a real
// implementation returning (nil, nil) instead of (nil, store.ErrNotFound)
// would be a contract violation, but callers here are auth paths, so they
// guard against it anyway rather than trusting every implementation
// (including test doubles) to honor the contract.
// ---------------------------------------------------------------------------

// nilOnGetUserStore wraps a real store.UserStore, forcing GetUser to return
// (nil, nil) so tests can exercise the fail-closed guard deterministically.
type nilOnGetUserStore struct {
	store.UserStore
}

func (s *nilOnGetUserStore) GetUser(context.Context, string) (*store.User, error) {
	return nil, nil
}

// nilOnGetUserByEmailStore wraps a real store.UserStore, forcing
// GetUserByEmail to return (nil, nil).
type nilOnGetUserByEmailStore struct {
	store.UserStore
}

func (s *nilOnGetUserByEmailStore) GetUserByEmail(context.Context, string) (*store.User, error) {
	return nil, nil
}

// raceExternalIdentityStore wraps a real store.ExternalIdentityStore,
// deterministically simulating the unique-binding race resolveAfterConflict
// exists to handle: the first GetExternalIdentity call (Resolve's own Step-1
// check) reports not-found, CreateExternalIdentity always reports a
// conflict, and every GetExternalIdentity call after the first answers from
// the real store — so resolveAfterConflict's own lookup finds a genuine
// "winning" binding, the way a concurrent resolution's would look from here.
type raceExternalIdentityStore struct {
	store.ExternalIdentityStore
	getCalls int
}

func (s *raceExternalIdentityStore) GetExternalIdentity(ctx context.Context, provider, issuer, subject string) (*store.ExternalIdentityBinding, error) {
	s.getCalls++
	if s.getCalls == 1 {
		return nil, store.ErrNotFound
	}
	return s.ExternalIdentityStore.GetExternalIdentity(ctx, provider, issuer, subject)
}

func (s *raceExternalIdentityStore) CreateExternalIdentity(context.Context, *ExternalIdentityBinding) error {
	return store.ErrAlreadyExists
}

// TestGoogleIdentityResolver_ExistingBinding_NilUser_FailsClosed is the
// bound-branch regression: a GetUser call that returns (nil, nil) for an
// existing binding must be rejected with a clean error, not a nil-pointer
// fault.
func TestGoogleIdentityResolver_ExistingBinding_NilUser_FailsClosed(t *testing.T) {
	identity := validGmailIdentity()
	h := newSignInPolicyHarness(t, ServerConfig{}, &fakeGoogleValidator{idTokenResult: identity})

	ctx := context.Background()
	userID := uuid.New().String()
	// The binding's UserID must reference a real row (the store enforces
	// this as a foreign key); the wrapper below is what actually makes
	// GetUser return nil despite that row existing.
	if err := h.store.CreateUser(ctx, &store.User{
		ID: userID, Email: identity.Email, Role: store.UserRoleMember, Status: store.UserStatusActive, Created: time.Now(),
	}); err != nil {
		t.Fatalf("seed bound user: %v", err)
	}
	now := time.Now()
	if err := h.extStore.CreateExternalIdentity(ctx, &ExternalIdentityBinding{
		ID: uuid.New().String(), Provider: "google", Issuer: googleCanonicalIssuer, Subject: identity.Subject,
		UserID: userID, Email: identity.Email, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed binding: %v", err)
	}

	resolver := NewGoogleIdentityResolver(&nilOnGetUserStore{UserStore: h.store}, h.extStore, h.srv.isUserAuthorized, nil, nil)

	user, err := resolver.Resolve(ctx, identity, ResolvePolicy{})
	if err == nil {
		t.Fatalf("expected an error for a nil bound-user record, got user=%+v", user)
	}
}

// TestGoogleIdentityResolver_EmailMatch_NilUser_FailsClosed is the
// email-match-branch regression: a GetUserByEmail call that returns
// (nil, nil) must be rejected with a clean error, not a nil-pointer fault.
func TestGoogleIdentityResolver_EmailMatch_NilUser_FailsClosed(t *testing.T) {
	identity := validGmailIdentity()
	h := newSignInPolicyHarness(t, ServerConfig{}, &fakeGoogleValidator{idTokenResult: identity})

	resolver := NewGoogleIdentityResolver(&nilOnGetUserByEmailStore{UserStore: h.store}, h.extStore, h.srv.isUserAuthorized, nil, nil)

	user, err := resolver.Resolve(context.Background(), identity, ResolvePolicy{})
	if err == nil {
		t.Fatalf("expected an error for a nil existing-by-email user record, got user=%+v", user)
	}
}

// TestGoogleIdentityResolver_ResolveAfterConflict_NilUser_FailsClosed covers
// the third sibling of the same pattern found while scanning for it:
// resolveAfterConflict's own GetUser call, reached when
// CreateExternalIdentity reports a concurrent-creation conflict during the
// email-match branch. A (nil, nil) return there must also be rejected
// cleanly.
func TestGoogleIdentityResolver_ResolveAfterConflict_NilUser_FailsClosed(t *testing.T) {
	identity := validGmailIdentity()
	h := newSignInPolicyHarness(t, ServerConfig{}, &fakeGoogleValidator{idTokenResult: identity})

	ctx := context.Background()
	userID := uuid.New().String()
	if err := h.store.CreateUser(ctx, &store.User{
		ID: userID, Email: identity.Email, Role: store.UserRoleMember, Status: store.UserStatusActive, Created: time.Now(),
	}); err != nil {
		t.Fatalf("seed active user: %v", err)
	}
	now := time.Now()
	// The "winning" binding a concurrent resolution would have created —
	// present so resolveAfterConflict's own GetExternalIdentity lookup
	// succeeds and reaches its GetUser call.
	if err := h.extStore.CreateExternalIdentity(ctx, &ExternalIdentityBinding{
		ID: uuid.New().String(), Provider: "google", Issuer: googleCanonicalIssuer, Subject: identity.Subject,
		UserID: userID, Email: identity.Email, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed winning binding: %v", err)
	}

	resolver := NewGoogleIdentityResolver(
		&nilOnGetUserStore{UserStore: h.store},
		&raceExternalIdentityStore{ExternalIdentityStore: h.extStore},
		h.srv.isUserAuthorized, nil, nil,
	)

	user, err := resolver.Resolve(ctx, identity, ResolvePolicy{})
	if err == nil {
		t.Fatalf("expected an error for a nil resolved-after-conflict user record, got user=%+v", user)
	}
}
