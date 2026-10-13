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
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// Sign-in equivalence characterization matrix.
//
// An existing record can be found by five different mechanisms, and they do
// not all share one implementation:
//
//   - provisionUser: web login, API login, and device flow all call
//     (*Server).provisionUser directly (handlers_auth.go :229,:345,:1069,
//     :1297), as does the API-server's own proxy-trust provisioner
//     (auth.go :890, MakeProxyUserProvisioner) — one function, so
//     exercising it once exercises all four call sites.
//   - GoogleIdentityResolver.Resolve, email-match branch: GE exchange and
//     external bearer, the first time an identity links to a pre-existing
//     record by email.
//   - GoogleIdentityResolver.Resolve, existing-binding branch: GE exchange
//     and external bearer on every later issuance for an already-linked
//     identity.
//   - WebServer.handleOAuthCallback (web OAuth) and WebServer.
//     proxyAuthMiddleware (the proxy-header path) each carry their own,
//     separate implementation of the same policy/suspension/activation/
//     role/grant logic — neither calls provisionUser or
//     applyLiveSignInPolicy. That duplication is tracked at its source
//     (web.go, TODO(NG4)); this file does not consolidate it, only proves
//     the two copies behave the same as the other three mechanisms today,
//     so a future consolidation has a test to keep green.
//
// The first three call the same shared helper, applyLiveSignInPolicy
// (sign_in_policy.go); the last two are independent implementations. This
// file asserts all five produce the same outcome for the same starting
// record, so none of them can drift apart again without a test catching it.
//
// The provider-verified-email requirement is a separate, earlier gate
// ahead of these mechanisms (the OAuth userinfo functions in oauth.go for
// provisionUser's and the WebServer paths' callers; the Google credential
// validator for Resolve's callers) and is covered exhaustively in
// oauth_email_verification_test.go and google_credential_validator_test.go
// respectively — both reject before any of the five mechanisms here are
// ever reached, so an equivalence assertion at this layer would not add
// coverage; what this file adds is the presented-email handling once a
// verified identity does reach one of the mechanisms (see the
// "policy-denied" case below, and
// TestGoogleIdentityResolver_ExistingBinding_EmailChange_NoMutationOnDenialThenSynced
// for the architect-pinned presented-email behavior in full).
// ---------------------------------------------------------------------------

// signInMechanism drives one of the mechanisms above for an already-seeded
// record. userID is the seeded record's ID (used only by the
// existing-binding mechanism, to pre-link that exact record); sub, email,
// and displayName describe the incoming identity.
type signInMechanism struct {
	name string
	run  func(t *testing.T, h *signInPolicyHarness, userID, sub, email, displayName string) (*store.User, error)
	// checkNoLink additionally asserts that a denial created no new
	// identity link. Optional: nil for mechanisms with no identity-link
	// concept (provisionUser, the two WebServer paths — all keyed by email
	// only, no separate binding table) or where a link already exists
	// before the call (existing-binding, so "no NEW link" is not a
	// meaningful distinct assertion from "the existing binding is
	// untouched", which the caller checks separately where it matters).
	checkNoLink func(t *testing.T, h *signInPolicyHarness, sub string)
}

var signInMechanisms = []signInMechanism{
	{
		name: "provisionUser (web login, API login, device)",
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
		checkNoLink: func(t *testing.T, h *signInPolicyHarness, sub string) {
			t.Helper()
			if _, err := h.extStore.GetExternalIdentity(context.Background(), "google", googleCanonicalIssuer, sub); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("expected no external identity binding to be created on denial, lookup returned err=%v", err)
			}
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
	{
		name: "WebServer.handleOAuthCallback (web OAuth)",
		run:  runViaOAuthCallback,
	},
	{
		name: "WebServer.proxyAuthMiddleware (proxy)",
		run:  runViaProxyAuthMiddleware,
	},
}

// runViaOAuthCallback drives WebServer.handleOAuthCallback end to end
// through ws.Handler(), the way the web login redirect actually arrives:
// seed OAuth state in a session, then hit the callback URL with a mocked
// Google token/userinfo exchange. userID and sub are unused — this
// mechanism has no separate binding concept, only email-keyed lookup.
func runViaOAuthCallback(t *testing.T, h *signInPolicyHarness, userID, sub, email, displayName string) (*store.User, error) {
	t.Helper()
	ws := newTestWebServer(t, WebServerConfig{BaseURL: "http://localhost:8080"})
	ws.SetStore(h.store)
	ws.SetAccessSettingsProvider(h.srv)
	tokenSvc, err := NewUserTokenService(UserTokenConfig{AccessTokenDuration: DefaultGETokenTTL})
	if err != nil {
		t.Fatalf("new user token service: %v", err)
	}
	ws.SetUserTokenService(tokenSvc)
	ws.SetOAuthService(NewOAuthService(OAuthConfig{
		Web: OAuthClientConfig{
			Google: OAuthProviderConfig{ClientID: "test-client-id", ClientSecret: "test-client-secret"},
		},
	}, nil))
	ws.oauthService.httpClient = &http.Client{
		Transport: &mockOAuthTransport{
			tokenJSON:    `{"access_token":"mock-token","token_type":"Bearer","expires_in":3600}`,
			userinfoJSON: `{"id":"` + sub + `","email":"` + email + `","verified_email":true,"name":"` + displayName + `"}`,
		},
	}

	reqSetup := httptest.NewRequest(http.MethodGet, "/auth/login/google", nil)
	recSetup := httptest.NewRecorder()
	sess, err := ws.sessionStore.Get(reqSetup, webSessionName)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	oauthState := "state-" + sub
	sess.Values[sessKeyOAuthState] = oauthState
	if err := sess.Save(reqSetup, recSetup); err != nil {
		t.Fatalf("save session: %v", err)
	}
	cookies := recSetup.Result().Cookies()

	callbackURL := "/auth/callback/google?code=test-code&state=" + oauthState
	reqCallback := httptest.NewRequest(http.MethodGet, callbackURL, nil)
	for _, c := range cookies {
		reqCallback.AddCookie(c)
	}
	recCallback := httptest.NewRecorder()
	ws.Handler().ServeHTTP(recCallback, reqCallback)

	switch loc := recCallback.Result().Header.Get("Location"); loc {
	case "/":
		u, err := h.store.GetUserByEmail(context.Background(), email)
		if err != nil {
			t.Fatalf("get user after callback: %v", err)
		}
		return u, nil
	case "/login?error=suspended":
		return nil, ErrUserSuspended
	case "/login?error=unauthorized_domain":
		return nil, ErrAccessDenied
	case "/login?error=invite_only":
		return nil, ErrInviteRequired
	default:
		t.Fatalf("unexpected callback redirect: %q", loc)
		return nil, nil
	}
}

// runViaProxyAuthMiddleware drives WebServer.proxyAuthMiddleware end to end
// through ws.Handler(), the way a proxy-header request actually arrives.
// userID and sub are unused — this mechanism has no separate binding
// concept, only email-keyed lookup.
func runViaProxyAuthMiddleware(t *testing.T, h *signInPolicyHarness, userID, sub, email, displayName string) (*store.User, error) {
	t.Helper()
	ws := newTestWebServer(t, WebServerConfig{
		AuthMode: "proxy",
		ProxyAuthenticator: &mockProxyAuthenticator{
			user: &ProxyUserInfo{Subject: sub, Email: email, DisplayName: displayName},
		},
	})
	ws.SetStore(h.store)
	ws.SetAccessSettingsProvider(h.srv)
	tokenSvc, err := NewUserTokenService(UserTokenConfig{AccessTokenDuration: DefaultGETokenTTL})
	if err != nil {
		t.Fatalf("new user token service: %v", err)
	}
	ws.SetUserTokenService(tokenSvc)

	req := httptest.NewRequest(http.MethodGet, "/projects", nil)
	rec := httptest.NewRecorder()
	ws.Handler().ServeHTTP(rec, req)

	resp := rec.Result()
	if resp.StatusCode == http.StatusForbidden {
		body, _ := io.ReadAll(resp.Body)
		switch {
		case strings.Contains(string(body), "user_suspended"):
			return nil, ErrUserSuspended
		case strings.Contains(string(body), "not authorized"):
			return nil, ErrAccessDenied
		default:
			t.Fatalf("unexpected 403 body: %s", body)
		}
	}

	u, err := h.store.GetUserByEmail(context.Background(), email)
	if err != nil {
		t.Fatalf("get user after proxy auth: %v", err)
	}
	return u, nil
}

// TestSignInEquivalence_ProvisionedVsInvited_AcrossPaths is the
// characterization matrix: a provisioned (already-active) record and an
// invite-created record must reach the identical outcome — the same
// record reused, activated where applicable, the same role, and the same
// hub-members grant — regardless of which mechanism finds it.
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
					// depend on the mechanism under test. This means the
					// hub-members assertion below, for a provisioned row,
					// is satisfied by this seeding, not by the mechanism
					// under test re-syncing grants — that re-sync behavior
					// itself is pinned separately (the always-persist
					// callers' tests and the collision-winner grant test).
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
// on every mechanism.
func TestSignInEquivalence_SuspendedDeniedAcrossPaths(t *testing.T) {
	// A fixed, obviously-not-"now" timestamp: if any mechanism's suspended
	// check ran after a LastLogin bump instead of before it, this would
	// catch it (a coincidental match with time.Now() cannot happen here).
	fixedLastLogin := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)

	for _, mech := range signInMechanisms {
		t.Run(mech.name, func(t *testing.T) {
			h := newSignInPolicyHarness(t, ServerConfig{}, &fakeGoogleValidator{})
			ctx := context.Background()

			userID := uuid.New().String()
			sub := "sign-in-equivalence-suspended-" + userID
			email := "user@gmail.com"

			if err := h.store.CreateUser(ctx, &store.User{
				ID: userID, Email: email, Role: store.UserRoleMember, Status: store.UserStatusSuspended,
				Created: time.Now(), LastLogin: fixedLastLogin,
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
			if !stored.LastLogin.Equal(fixedLastLogin) {
				t.Errorf("expected LastLogin to be unchanged at %v, got %v", fixedLastLogin, stored.LastLogin)
			}
			if isHubMember(t, h.store, userID) {
				t.Error("expected a suspended record to gain no hub-members grant")
			}
		})
	}
}

// TestSignInEquivalence_PolicyDeniedNoMutationAcrossPaths asserts the same
// fail-closed outcome — denial, no state mutation, and (where the mechanism
// has an identity-link concept at all) no new identity link — for a record
// whose email fails the live sign-in policy, on every mechanism.
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
			if mech.checkNoLink != nil {
				mech.checkNoLink(t, h, sub)
			}
		})
	}
}
