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
