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
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// ---------------------------------------------------------------------------
// Regression coverage: every existing-record sign-in path must apply the
// same live sign-in policy and account-state handling that interactive
// login applies (see sign_in_policy.go / google_identity_resolver.go),
// against a real store so role assignment and grant sync exercise their
// real implementation rather than a fake.
// ---------------------------------------------------------------------------

// TestGEExchange_ExistingUserByEmail_InvitedActivatesConsistently is an
// end-to-end regression: an invited record, resolved by email through the
// GE exchange path (no prior Google binding), must be activated and granted
// hub access identically to an interactive-login sign-in.
func TestGEExchange_ExistingUserByEmail_InvitedActivatesConsistently(t *testing.T) {
	identity := validGmailIdentity()
	validator := &fakeGoogleValidator{idTokenResult: identity}

	h := newSignInPolicyHarness(t, ServerConfig{UserAccessMode: "invite_only"}, validator)

	ctx := context.Background()
	invitedUserID := uuid.New().String()
	invitedBy := "admin@example.com"
	if err := h.store.CreateUser(ctx, &store.User{
		ID:        invitedUserID,
		Email:     identity.Email,
		Role:      store.UserRoleMember, // placeholder role on an invited row
		Status:    store.UserStatusInvited,
		InvitedBy: &invitedBy,
		Created:   time.Now(),
	}); err != nil {
		t.Fatalf("seed invited user: %v", err)
	}

	resp, status, err := h.svc.Exchange(ctx, &ExchangeRequest{
		Credential:     "token",
		CredentialType: "id_token",
	})
	if err != nil {
		t.Fatalf("exchange failed: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d", status)
	}
	if resp.AccessToken == "" {
		t.Fatal("expected a Hub access token to be issued")
	}
	if resp.User.ID != invitedUserID {
		t.Fatalf("expected the invited record to be reused, got user %q", resp.User.ID)
	}
	if resp.User.Role != store.UserRoleMember {
		t.Fatalf("expected role %q, got %q", store.UserRoleMember, resp.User.Role)
	}

	stored, err := h.store.GetUser(ctx, invitedUserID)
	if err != nil {
		t.Fatalf("get user: %v", err)
	}
	if stored.Status != store.UserStatusActive {
		t.Fatalf("expected invited record to be activated (status=active), got %q", stored.Status)
	}
	if !isHubMember(t, h.store, invitedUserID) {
		t.Error("expected the activated user to be granted hub-members access")
	}
}

// TestGEExchange_ExistingUserByEmail_NotAuthorized_DeniedNoBinding is a
// negative regression: a pre-existing record whose email fails the live
// sign-in policy must be denied — with no external identity binding created
// and no state mutation — even though it was found by email and not by an
// existing binding.
func TestGEExchange_ExistingUserByEmail_NotAuthorized_DeniedNoBinding(t *testing.T) {
	identity := validWorkspaceIdentity() // user@company.com
	validator := &fakeGoogleValidator{idTokenResult: identity}

	h := newSignInPolicyHarness(t, ServerConfig{
		UserAccessMode:    "domain_restricted",
		AuthorizedDomains: []string{"not-company.com"},
	}, validator)

	ctx := context.Background()
	activeUserID := uuid.New().String()
	if err := h.store.CreateUser(ctx, &store.User{
		ID:      activeUserID,
		Email:   identity.Email,
		Role:    store.UserRoleMember,
		Status:  store.UserStatusActive,
		Created: time.Now(),
	}); err != nil {
		t.Fatalf("seed active user: %v", err)
	}

	_, status, err := h.svc.Exchange(ctx, &ExchangeRequest{
		Credential:     "token",
		CredentialType: "id_token",
	})
	if err == nil {
		t.Fatal("expected an error for an unauthorized domain")
	}
	if status != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", status)
	}

	// No state mutation: the record must be untouched.
	stored, getErr := h.store.GetUser(ctx, activeUserID)
	if getErr != nil {
		t.Fatalf("get user: %v", getErr)
	}
	if stored.Role != store.UserRoleMember || stored.Status != store.UserStatusActive {
		t.Fatalf("expected the denied record to be unchanged, got role=%q status=%q", stored.Role, stored.Status)
	}

	// No identity link: a subsequent exchange under an authorized policy
	// must still resolve by email again (bootstrap), not by a binding
	// created during the denied attempt.
	if _, lookupErr := h.extStore.GetExternalIdentity(ctx, "google", googleCanonicalIssuer, identity.Subject); !errors.Is(lookupErr, store.ErrNotFound) {
		t.Fatalf("expected no external identity binding to be created on denial, lookup returned err=%v", lookupErr)
	}
}

// TestGEExchange_ExistingUserByEmail_NotAuthorized_WritesOneDenialAudit
// verifies the resolver path's denial writes exactly one audit record (the
// same failure event provisionUser's own denial path writes) — not zero
// (silently dropped) and not more than one (double-audited).
func TestGEExchange_ExistingUserByEmail_NotAuthorized_WritesOneDenialAudit(t *testing.T) {
	identity := validWorkspaceIdentity()
	validator := &fakeGoogleValidator{idTokenResult: identity}

	h := newSignInPolicyHarness(t, ServerConfig{
		UserAccessMode:    "domain_restricted",
		AuthorizedDomains: []string{"not-company.com"},
	}, validator)
	auditLog := &recordingAuditLogger{}
	h.srv.auditLogger = auditLog

	ctx := context.Background()
	activeUserID := uuid.New().String()
	if err := h.store.CreateUser(ctx, &store.User{
		ID:      activeUserID,
		Email:   identity.Email,
		Role:    store.UserRoleMember,
		Status:  store.UserStatusActive,
		Created: time.Now(),
	}); err != nil {
		t.Fatalf("seed active user: %v", err)
	}

	_, status, err := h.svc.Exchange(ctx, &ExchangeRequest{
		Credential:     "token",
		CredentialType: "id_token",
	})
	if err == nil {
		t.Fatal("expected an error for an unauthorized domain")
	}
	if status != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", status)
	}
	if got := auditLog.deniedCount(); got != 1 {
		t.Fatalf("expected exactly one denial audit record, got %d (total invite events=%d)", got, len(auditLog.invite))
	}
}

// TestGoogleIdentityResolver_EmailMatch_PreAuthorized_SkipsPolicyNotState is
// the other half of PreAuthorized coverage on the email-match branch: it
// skips the sign-in policy check only, never the account-state handling —
// an active record still gets its role re-evaluated and grants applied, and
// an invited record still activates and gets grants, even though the
// authorize callback here denies everyone.
func TestGoogleIdentityResolver_EmailMatch_PreAuthorized_SkipsPolicyNotState(t *testing.T) {
	neverAuthorized := func(context.Context, string) bool { return false }

	t.Run("active record succeeds", func(t *testing.T) {
		identity := validGmailIdentity()
		h := newSignInPolicyHarness(t, ServerConfig{}, &fakeGoogleValidator{idTokenResult: identity})
		h.resolver = NewGoogleIdentityResolver(h.store, h.extStore, neverAuthorized, nil, nil)
		h.resolver.SetSignInPolicyDeps(h.srv.signInPolicyDeps())

		ctx := context.Background()
		userID := uuid.New().String()
		if err := h.store.CreateUser(ctx, &store.User{
			ID: userID, Email: identity.Email, Role: store.UserRoleMember, Status: store.UserStatusActive, Created: time.Now(),
		}); err != nil {
			t.Fatalf("seed active user: %v", err)
		}

		user, err := h.resolver.Resolve(ctx, identity, ResolvePolicy{PreAuthorized: true})
		if err != nil {
			t.Fatalf("expected PreAuthorized to admit the record despite a deny-all policy: %v", err)
		}
		if user.ID != userID {
			t.Fatalf("expected the existing record to be reused, got %q", user.ID)
		}
		if !isHubMember(t, h.store, userID) {
			t.Error("expected the record to be granted hub-members access")
		}
	})

	t.Run("invited record activates", func(t *testing.T) {
		identity := validWorkspaceIdentity()
		h := newSignInPolicyHarness(t, ServerConfig{}, &fakeGoogleValidator{idTokenResult: identity})
		h.resolver = NewGoogleIdentityResolver(h.store, h.extStore, neverAuthorized, nil, nil)
		h.resolver.SetSignInPolicyDeps(h.srv.signInPolicyDeps())

		ctx := context.Background()
		userID := uuid.New().String()
		invitedBy := "admin@example.com"
		if err := h.store.CreateUser(ctx, &store.User{
			ID: userID, Email: identity.Email, Role: store.UserRoleMember, Status: store.UserStatusInvited, InvitedBy: &invitedBy, Created: time.Now(),
		}); err != nil {
			t.Fatalf("seed invited user: %v", err)
		}

		user, err := h.resolver.Resolve(ctx, identity, ResolvePolicy{PreAuthorized: true})
		if err != nil {
			t.Fatalf("expected PreAuthorized to admit the record despite a deny-all policy: %v", err)
		}
		if user.Status != store.UserStatusActive {
			t.Fatalf("expected the invited record to activate, got status=%q", user.Status)
		}
		if !isHubMember(t, h.store, userID) {
			t.Error("expected the activated record to be granted hub-members access")
		}
	})
}
