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
	"bytes"
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// Regression coverage, through the external-bearer HTTP surface, against a
// real store: the live sign-in policy re-check now applies to an already
// Google-bound identity on every issuance (not just at first link), and a
// pre-authorized service-account principal still only skips the policy
// check, never suspension.
// ---------------------------------------------------------------------------

// TestExternalBearer_UserIDToken_ExistingBinding_PolicyAppliesOnReissuance
// pre-links a user principal (an existing Google binding), then tightens the
// Hub sign-in policy so it would deny that email. The external-bearer path's
// own AllowedDomains gate still passes (the identity's domain is listed
// there), but the resolver must still re-check the Hub policy on this
// re-issuance and deny — proving the existing-binding branch is not
// exempted the way it was before.
func TestExternalBearer_UserIDToken_ExistingBinding_PolicyAppliesOnReissuance(t *testing.T) {
	kp := newGCVTestKeyPair("test-kid-1")
	endpoints := newSAJWKSEndpoints(kp)
	defer endpoints.close()

	h := newSignInPolicyHarness(t, ServerConfig{
		UserAccessMode:    "domain_restricted",
		AuthorizedDomains: []string{"not-company.com"},
	}, newTestValidator(endpoints))

	ctx := context.Background()
	const email = "user@company.com"
	const sub = "google-sub-existing-binding-1"
	userID := uuid.New().String()
	if err := h.store.CreateUser(ctx, &store.User{
		ID: userID, Email: email, Role: store.UserRoleMember, Status: store.UserStatusActive, Created: time.Now(),
	}); err != nil {
		t.Fatalf("seed active user: %v", err)
	}
	now := time.Now()
	if err := h.extStore.CreateExternalIdentity(ctx, &ExternalIdentityBinding{
		ID: uuid.New().String(), Provider: "google", Issuer: googleCanonicalIssuer, Subject: sub,
		UserID: userID, Email: email, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed binding: %v", err)
	}

	cfg := newExternalBearerConfigWithDomains(t, newTestValidator(endpoints), h.resolver, []string{"company.com"}, nil)

	claims := validIDTokenClaims()
	claims["aud"] = externalBearerTestAudience
	claims["sub"] = sub
	claims["email"] = email
	claims["hd"] = "company.com"
	token := signIDToken(kp, claims)

	w, result := doExternalBearerRequest(cfg, token)
	if result.reached {
		t.Fatal("handler must not be reached: the Hub sign-in policy denies this identity")
	}
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: body=%s", w.Code, w.Body.String())
	}
	wantBody := wantErrorBody(t, ErrCodeForbidden, "access denied")
	if !bytes.Equal(w.Body.Bytes(), wantBody) {
		t.Errorf("body = %s, want %s", w.Body.Bytes(), wantBody)
	}

	stored, err := h.store.GetUser(ctx, userID)
	if err != nil {
		t.Fatalf("get user: %v", err)
	}
	if stored.Role != store.UserRoleMember || stored.Status != store.UserStatusActive {
		t.Fatalf("expected the denied record to be unchanged, got role=%q status=%q", stored.Role, stored.Status)
	}
}

// TestExternalBearer_ServiceAccountIDToken_ExistingBinding_PreAuthorized_SuspendedStillRejected
// mirrors TestExternalBearer_ServiceAccountIDToken_Suspended_Forbidden against
// a real store: a service-account principal admitted by allowed_gcp_projects
// stays PreAuthorized on the existing-binding branch (a deny-all Hub policy
// does not block it), but a suspended account is still rejected.
func TestExternalBearer_ServiceAccountIDToken_ExistingBinding_PreAuthorized_SuspendedStillRejected(t *testing.T) {
	kp := newGCVTestKeyPair("test-kid-1")
	endpoints := newSAJWKSEndpoints(kp)
	defer endpoints.close()

	// A deny-all Hub sign-in policy (domain_restricted with no matching
	// domain): PreAuthorized must still admit the SA principal.
	h := newSignInPolicyHarness(t, ServerConfig{
		UserAccessMode:    "domain_restricted",
		AuthorizedDomains: []string{"not-this-project.example.com"},
	}, newTestValidator(endpoints))

	const email = "worker@my-a2a-project.iam.gserviceaccount.com"
	const sub = "111122223333444455556"
	ctx := context.Background()
	userID := uuid.New().String()
	if err := h.store.CreateUser(ctx, &store.User{
		ID: userID, Email: email, Role: store.UserRoleMember, Status: store.UserStatusSuspended, Created: time.Now(),
	}); err != nil {
		t.Fatalf("seed suspended SA user: %v", err)
	}
	now := time.Now()
	if err := h.extStore.CreateExternalIdentity(ctx, &ExternalIdentityBinding{
		ID: uuid.New().String(), Provider: "google", Issuer: googleCanonicalIssuer, Subject: sub,
		UserID: userID, Email: email, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed binding: %v", err)
	}

	cfg := newExternalBearerConfigWithSA(t, newTestValidator(endpoints), h.resolver, []string{"my-a2a-project"})

	claims := serviceAccountIDTokenClaims(email)
	claims["sub"] = sub
	claims["azp"] = sub
	token := signIDToken(kp, claims)

	w, result := doExternalBearerRequest(cfg, token)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: body=%s", w.Code, w.Body.String())
	}
	if result.reached {
		t.Fatal("handler must not be reached for a suspended SA user")
	}
	wantBody := wantErrorBody(t, "user_suspended", "access denied: user account is suspended")
	if !bytes.Equal(w.Body.Bytes(), wantBody) {
		t.Errorf("body = %s, want %s", w.Body.Bytes(), wantBody)
	}
}
