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
// Sign-in equivalence characterization matrix.
//
// Every sign-in surface — web login, API login, device flow, web OAuth,
// the proxy-header path, GE token exchange, and external bearer — reduces
// to one of three mechanisms for an already-existing record:
//
//   - provisionUser: web login, API login, device flow, web OAuth, and the
//     proxy path all call (*Server).provisionUser directly (handlers_auth.go
//     :229,:345,:1069,:1297; auth.go:890 via MakeProxyUserProvisioner) — one
//     function, so exercising it once exercises all five call sites.
//   - GoogleIdentityResolver.Resolve, email-match branch: GE exchange and
//     external bearer, the first time an identity links to a pre-existing
//     record by email.
//   - GoogleIdentityResolver.Resolve, existing-binding branch: GE exchange
//     and external bearer on every later issuance for an already-linked
//     identity.
//
// All three call the same shared helper, applyLiveSignInPolicy
// (sign_in_policy.go). This file asserts they produce the same outcome for
// the same starting record, so the five named surfaces cannot drift apart
// again without a test catching it — the property H.2 depends on.
//
// The provider-verified-email requirement is a separate, earlier gate on
// two of the three mechanisms (the OAuth userinfo functions in oauth.go for
// provisionUser's callers; the Google credential validator for Resolve's
// callers) and is covered exhaustively in oauth_email_verification_test.go
// and google_credential_validator_test.go respectively — both reject before
// any of the three mechanisms here are ever reached, so an equivalence
// assertion at this layer would not add coverage; what this file adds is
// the presented-email handling once a verified identity does reach one of
// the three mechanisms (see the "denied leaves no mutation" case below,
// and TestGoogleIdentityResolver_ExistingBinding_EmailChange_NoMutationOnDenialThenSynced
// for the architect-pinned presented-email behavior in full).
// ---------------------------------------------------------------------------

// signInMechanism drives one of the three mechanisms above for an
// already-seeded record. userID is the seeded record's ID (used only by the
// existing-binding mechanism, to pre-link that exact record); sub, email,
// and displayName describe the incoming identity.
type signInMechanism struct {
	name string
	run  func(t *testing.T, h *signInPolicyHarness, userID, sub, email, displayName string) (*store.User, error)
}

var signInMechanisms = []signInMechanism{
	{
		name: "provisionUser (web login, API login, device, web OAuth, proxy)",
		run: func(t *testing.T, h *signInPolicyHarness, userID, sub, email, displayName string) (*store.User, error) {
			t.Helper()
			return h.srv.provisionUser(context.Background(), &ExternalUserInfo{Email: email, DisplayName: displayName})
		},
	},
	{
		name: "resolver email-match (GE exchange / external bearer, first link)",
		run: func(t *testing.T, h *signInPolicyHarness, userID, sub, email, displayName string) (*store.User, error) {
			t.Helper()
			identity := &ValidatedGoogleIdentity{
				Subject: sub, Email: email, EmailVerified: true, DisplayName: displayName,
				Issuer: googleCanonicalIssuer, UpstreamExpiry: time.Now().Add(time.Hour),
			}
			return h.resolver.Resolve(context.Background(), identity, ResolvePolicy{})
		},
	},
	{
		name: "resolver existing-binding (GE exchange / external bearer, later issuance)",
		run: func(t *testing.T, h *signInPolicyHarness, userID, sub, email, displayName string) (*store.User, error) {
			t.Helper()
			ctx := context.Background()
			now := time.Now()
			if err := h.extStore.CreateExternalIdentity(ctx, &ExternalIdentityBinding{
				ID: uuid.New().String(), Provider: "google", Issuer: googleCanonicalIssuer, Subject: sub,
				UserID: userID, Email: email, CreatedAt: now, UpdatedAt: now,
			}); err != nil {
				t.Fatalf("seed binding: %v", err)
			}
			identity := &ValidatedGoogleIdentity{
				Subject: sub, Email: email, EmailVerified: true, DisplayName: displayName,
				Issuer: googleCanonicalIssuer, UpstreamExpiry: time.Now().Add(time.Hour),
			}
			return h.resolver.Resolve(ctx, identity, ResolvePolicy{})
		},
	},
}

// TestSignInEquivalence_ProvisionedVsInvited_AcrossPaths is the
// characterization matrix: a provisioned (already-active) record and an
// invite-created record must reach the identical outcome — the same
// record reused, activated where applicable, the same role, and the same
// hub-members grant — regardless of which of the three mechanisms finds it.
func TestSignInEquivalence_ProvisionedVsInvited_AcrossPaths(t *testing.T) {
	records := []struct {
		name   string
		status string
	}{
		{name: "provisioned", status: store.UserStatusActive},
		{name: "invited", status: store.UserStatusInvited},
	}

	for _, mech := range signInMechanisms {
		for _, rec := range records {
			t.Run(mech.name+"/"+rec.name, func(t *testing.T) {
				h := newSignInPolicyHarness(t, ServerConfig{}, &fakeGoogleValidator{})
				ctx := context.Background()

				userID := uuid.New().String()
				sub := "sign-in-equivalence-" + userID
				email := "user@gmail.com"
				displayName := "Test User"

				user := &store.User{
					ID:      userID,
					Email:   email,
					Role:    store.UserRoleMember, // placeholder on an invited row; the real starting role on a provisioned one
					Status:  rec.status,
					Created: time.Now(),
				}
				if rec.status == store.UserStatusInvited {
					invitedBy := "admin@example.com"
					user.InvitedBy = &invitedBy
				}
				if err := h.store.CreateUser(ctx, user); err != nil {
					t.Fatalf("seed %s user: %v", rec.name, err)
				}
				if rec.status == store.UserStatusActive {
					// A provisioned record, by construction, already went
					// through the always-sync creation path once (unlike an
					// invited row, which has never signed in). Seed that
					// same starting state directly rather than through
					// provisionUser, so this test's own setup does not
					// depend on the mechanism under test.
					if err := syncHubRoleGrants(ctx, h.store, userID, user.Role, store.SystemReconcileCreatedBy); err != nil {
						t.Fatalf("seed existing grants for %s user: %v", rec.name, err)
					}
				}

				got, err := mech.run(t, h, userID, sub, email, displayName)
				if err != nil {
					t.Fatalf("sign-in failed: %v", err)
				}
				if got.ID != userID {
					t.Fatalf("expected the existing record to be reused, got %q", got.ID)
				}
				if got.Status != store.UserStatusActive {
					t.Fatalf("expected status active, got %q", got.Status)
				}
				if got.Role != store.UserRoleMember {
					t.Fatalf("expected role %q, got %q", store.UserRoleMember, got.Role)
				}
				if !isHubMember(t, h.store, userID) {
					t.Error("expected the record to be granted hub-members access")
				}
			})
		}
	}
}

// TestSignInEquivalence_SuspendedDeniedAcrossPaths asserts the same
// fail-closed outcome — denial, no state mutation — for a suspended record
// on all three mechanisms.
func TestSignInEquivalence_SuspendedDeniedAcrossPaths(t *testing.T) {
	for _, mech := range signInMechanisms {
		t.Run(mech.name, func(t *testing.T) {
			h := newSignInPolicyHarness(t, ServerConfig{}, &fakeGoogleValidator{})
			ctx := context.Background()

			userID := uuid.New().String()
			sub := "sign-in-equivalence-suspended-" + userID
			email := "user@gmail.com"

			if err := h.store.CreateUser(ctx, &store.User{
				ID: userID, Email: email, Role: store.UserRoleMember, Status: store.UserStatusSuspended, Created: time.Now(),
			}); err != nil {
				t.Fatalf("seed suspended user: %v", err)
			}

			_, err := mech.run(t, h, userID, sub, email, "Test User")
			if !errors.Is(err, ErrUserSuspended) {
				t.Fatalf("expected ErrUserSuspended, got %v", err)
			}

			stored, getErr := h.store.GetUser(ctx, userID)
			if getErr != nil {
				t.Fatalf("get user: %v", getErr)
			}
			if stored.Status != store.UserStatusSuspended {
				t.Fatalf("expected the suspended record to be unchanged, got status=%q", stored.Status)
			}
		})
	}
}

// TestSignInEquivalence_PolicyDeniedNoMutationAcrossPaths asserts the same
// fail-closed outcome — denial, no state mutation, no identity link — for a
// record whose email fails the live sign-in policy, on all three
// mechanisms.
func TestSignInEquivalence_PolicyDeniedNoMutationAcrossPaths(t *testing.T) {
	for _, mech := range signInMechanisms {
		t.Run(mech.name, func(t *testing.T) {
			h := newSignInPolicyHarness(t, ServerConfig{
				UserAccessMode:    "domain_restricted",
				AuthorizedDomains: []string{"not-gmail.example"},
			}, &fakeGoogleValidator{})
			ctx := context.Background()

			userID := uuid.New().String()
			sub := "sign-in-equivalence-denied-" + userID
			email := "user@gmail.com"

			if err := h.store.CreateUser(ctx, &store.User{
				ID: userID, Email: email, Role: store.UserRoleMember, Status: store.UserStatusActive, Created: time.Now(),
			}); err != nil {
				t.Fatalf("seed active user: %v", err)
			}

			_, err := mech.run(t, h, userID, sub, email, "Test User")
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
		})
	}
}
