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

package hub

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// ---------------------------------------------------------------------------
// The SA branch of authenticateExternalBearer. Some related SA cases live
// alongside the validator/exchange code they exercise
// (google_credential_validator_test.go, ge_exchange_test.go);
// TestExternalBearer_ServiceAccountIDToken_AZPNotSub_Unauthorized
// (auth_external_bearer_test.go) exercises the azp/sub disagreement case at
// the middleware level.
//
// These tests use newExternalBearerConfigWithSA, which builds a real,
// validated FederationAuthenticator with AllowedGCPProjects set on the
// Google trust entry (the distinct field for SA project admission — not the
// unrelated AllowedProjects/allowed_projects).
// ---------------------------------------------------------------------------

// neverAuthorized always denies. Used to prove that a service account
// admitted via allowed_gcp_projects bypasses the Hub sign-in policy — the
// project allowlist IS the authorization decision (ResolvePolicy.PreAuthorized).
func neverAuthorized(_ context.Context, _ string) bool { return false }

// saNumericSub is a realistic Google service-account "sub"/"azp" value: a
// large numeric unique ID, matching what the metadata server and
// iamcredentials.generateIdToken actually issue. Fake sub-shaped strings can
// hide a real classification bug, so tests should use a realistic value.
const saNumericSub = "111122223333444455556"

// serviceAccountIDTokenClaims returns claims shaped like a real Google
// service-account ID token: sub == azp (both the SA's own numeric unique
// ID), aud the caller-chosen audience, email/email_verified set.
func serviceAccountIDTokenClaims(email string) map[string]interface{} {
	now := time.Now()
	return map[string]interface{}{
		"iss":            googleIssuerHTTPS,
		"sub":            saNumericSub,
		"azp":            saNumericSub,
		"aud":            externalBearerTestAudience,
		"email":          email,
		"email_verified": true,
		"exp":            now.Add(30 * time.Minute).Unix(),
		"iat":            now.Unix(),
	}
}

func newSAJWKSEndpoints(kp *gcvTestKeyPair) *testEndpoints {
	return newTestEndpoints(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(gcvJWKSJSON(kp))
		}),
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}),
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}),
	)
}

// ---------------------------------------------------------------------------
// SA ID token, aud = expected, azp == sub, project listed -> 200,
// provisioned WITHOUT the Hub sign-in policy (authorize would have denied
// it). Also an azp == "" variant.
// ---------------------------------------------------------------------------

func TestExternalBearer_ServiceAccountIDToken_ProjectListed_Authenticates(t *testing.T) {
	kp := newGCVTestKeyPair("test-kid-1")
	endpoints := newSAJWKSEndpoints(kp)
	defer endpoints.close()

	userStore := newFakeUserStore()
	extStore := newMemExtIDStore()
	// authorize always denies: if the identity were provisioned through the
	// normal sign-in policy (not PreAuthorized), this would 403. Success here
	// proves the allowlist bypassed that policy, not merely that it wasn't hit.
	resolver := NewGoogleIdentityResolver(userStore, extStore, neverAuthorized, nil, slog.Default())
	cfg := newExternalBearerConfigWithSA(t, newTestValidator(endpoints), resolver, []string{"my-a2a-project"})

	claims := serviceAccountIDTokenClaims("worker@my-a2a-project.iam.gserviceaccount.com")
	token := signIDToken(kp, claims)

	w, result := doExternalBearerRequest(cfg, token)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: body=%s", w.Code, w.Body.String())
	}
	if !result.reached || result.authType != AuthTypeExternalBearer {
		t.Fatalf("expected external-bearer auth, got reached=%v authType=%q", result.reached, result.authType)
	}
	if result.identity == nil || result.identity.Email() != "worker@my-a2a-project.iam.gserviceaccount.com" {
		t.Fatalf("unexpected identity: %+v", result.identity)
	}
	if len(userStore.users) != 1 {
		t.Fatalf("expected exactly 1 user provisioned, got %d", len(userStore.users))
	}
}

// TestExternalBearer_ServiceAccountIDToken_MixedCaseAllowedProject_Authenticates
// proves that a mixed-case operator-configured allowed_gcp_projects entry
// still matches a (lower-case) parsed SA project. The list is not rewritten
// at config load; containsFold's case-insensitive comparison is what makes
// this match.
func TestExternalBearer_ServiceAccountIDToken_MixedCaseAllowedProject_Authenticates(t *testing.T) {
	kp := newGCVTestKeyPair("test-kid-1")
	endpoints := newSAJWKSEndpoints(kp)
	defer endpoints.close()

	userStore := newFakeUserStore()
	extStore := newMemExtIDStore()
	resolver := NewGoogleIdentityResolver(userStore, extStore, alwaysAuthorized, nil, slog.Default())
	cfg := newExternalBearerConfigWithSA(t, newTestValidator(endpoints), resolver, []string{"My-A2A-Project"})

	claims := serviceAccountIDTokenClaims("worker@my-a2a-project.iam.gserviceaccount.com")
	token := signIDToken(kp, claims)

	w, result := doExternalBearerRequest(cfg, token)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: body=%s", w.Code, w.Body.String())
	}
	if !result.reached {
		t.Fatal("handler must be reached: mixed-case allowed_gcp_projects must still match case-insensitively")
	}
}

func TestExternalBearer_ServiceAccountIDToken_EmptyAZP_ProjectListed_Authenticates(t *testing.T) {
	kp := newGCVTestKeyPair("test-kid-1")
	endpoints := newSAJWKSEndpoints(kp)
	defer endpoints.close()

	userStore := newFakeUserStore()
	extStore := newMemExtIDStore()
	resolver := NewGoogleIdentityResolver(userStore, extStore, neverAuthorized, nil, slog.Default())
	cfg := newExternalBearerConfigWithSA(t, newTestValidator(endpoints), resolver, []string{"my-a2a-project"})

	claims := serviceAccountIDTokenClaims("worker@my-a2a-project.iam.gserviceaccount.com")
	delete(claims, "azp") // azp == "" variant: the azp/sub check only applies when azp is present.
	token := signIDToken(kp, claims)

	w, result := doExternalBearerRequest(cfg, token)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: body=%s", w.Code, w.Body.String())
	}
	if !result.reached {
		t.Fatal("handler must be reached")
	}
}

// ---------------------------------------------------------------------------
// Project not listed -> 401; allowed_gcp_projects unset -> 401.
// ---------------------------------------------------------------------------

func TestExternalBearer_ServiceAccountIDToken_ProjectNotListed_Unauthorized(t *testing.T) {
	kp := newGCVTestKeyPair("test-kid-1")
	endpoints := newSAJWKSEndpoints(kp)
	defer endpoints.close()

	userStore := newFakeUserStore()
	extStore := newMemExtIDStore()
	resolver := NewGoogleIdentityResolver(userStore, extStore, alwaysAuthorized, nil, slog.Default())
	cfg := newExternalBearerConfigWithSA(t, newTestValidator(endpoints), resolver, []string{"some-other-project"})

	claims := serviceAccountIDTokenClaims("worker@my-a2a-project.iam.gserviceaccount.com")
	token := signIDToken(kp, claims)

	w, result := doExternalBearerRequest(cfg, token)
	if result.reached {
		t.Fatal("handler must not be reached: project is not in allowed_gcp_projects")
	}
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401: body=%s", w.Code, w.Body.String())
	}
	wantBody := wantErrorBody(t, ErrCodeUnauthorized, "invalid external bearer token")
	if !bytes.Equal(w.Body.Bytes(), wantBody) {
		t.Errorf("body = %s, want %s", w.Body.Bytes(), wantBody)
	}
	if len(userStore.users) != 0 {
		t.Errorf("expected no users created, got %d", len(userStore.users))
	}
}

func TestExternalBearer_ServiceAccountIDToken_AllowedGCPProjectsUnset_Unauthorized(t *testing.T) {
	kp := newGCVTestKeyPair("test-kid-1")
	endpoints := newSAJWKSEndpoints(kp)
	defer endpoints.close()

	userStore := newFakeUserStore()
	extStore := newMemExtIDStore()
	resolver := NewGoogleIdentityResolver(userStore, extStore, alwaysAuthorized, nil, slog.Default())
	// allowed_gcp_projects unset entirely admits NO
	// service accounts, not "any project".
	cfg := newExternalBearerConfigWithSA(t, newTestValidator(endpoints), resolver, nil)

	claims := serviceAccountIDTokenClaims("worker@my-a2a-project.iam.gserviceaccount.com")
	token := signIDToken(kp, claims)

	w, result := doExternalBearerRequest(cfg, token)
	if result.reached {
		t.Fatal("handler must not be reached: allowed_gcp_projects is unset")
	}
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401: body=%s", w.Code, w.Body.String())
	}
	wantBody := wantErrorBody(t, ErrCodeUnauthorized, "invalid external bearer token")
	if !bytes.Equal(w.Body.Bytes(), wantBody) {
		t.Errorf("body = %s, want %s", w.Body.Bytes(), wantBody)
	}
}

// TestExternalBearer_ServiceAccountIDToken_UnparseableProject_Unauthorized
// covers googleSAProject's false-project cases (a compute default SA, in
// this case) reaching the middleware: the project can't be parsed at all, so
// it is never in allowed_gcp_projects regardless of content — proves the branch
// checks googleSAProject's ok return, not just list membership. The
// allowlist deliberately includes "" (googleSAProject's zero value on a
// false result): dropping the `!ok ||` guard and comparing only the empty
// project string against this list would otherwise still (accidentally)
// reject via containsFold, hiding that mutation.
func TestExternalBearer_ServiceAccountIDToken_UnparseableProject_Unauthorized(t *testing.T) {
	kp := newGCVTestKeyPair("test-kid-1")
	endpoints := newSAJWKSEndpoints(kp)
	defer endpoints.close()

	userStore := newFakeUserStore()
	extStore := newMemExtIDStore()
	resolver := NewGoogleIdentityResolver(userStore, extStore, alwaysAuthorized, nil, slog.Default())
	cfg := newExternalBearerConfigWithSA(t, newTestValidator(endpoints), resolver, []string{"some-project", ""})

	claims := serviceAccountIDTokenClaims("123456789012-compute@developer.gserviceaccount.com")
	token := signIDToken(kp, claims)

	w, result := doExternalBearerRequest(cfg, token)
	if result.reached {
		t.Fatal("handler must not be reached: compute default SA project is unparseable")
	}
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401: body=%s", w.Code, w.Body.String())
	}
	wantBody := wantErrorBody(t, ErrCodeUnauthorized, "invalid external bearer token")
	if !bytes.Equal(w.Body.Bytes(), wantBody) {
		t.Errorf("body = %s, want %s", w.Body.Bytes(), wantBody)
	}
}

// ---------------------------------------------------------------------------
// SA access token (listed project too) -> 401. The project is listed
// here specifically so that "reject access tokens" and "reject unlisted
// projects" can't be conflated into a single accidentally-ANDed check (which
// would admit a listed project's SA via an access token) — an empty
// allowlist would reject this token either way and couldn't tell the two
// guards apart.
// ---------------------------------------------------------------------------

func TestExternalBearer_AccessToken_ServiceAccount_ProjectListed_StillRejected(t *testing.T) {
	tokenInfo, userInfo := validAccessTokenEndpoints(externalBearerTestAudience, "sa-sub-1", "worker@my-a2a-project.iam.gserviceaccount.com", true)
	endpoints := newTestEndpoints(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}), tokenInfo, userInfo)
	defer endpoints.close()

	userStore := newFakeUserStore()
	extStore := newMemExtIDStore()
	resolver := NewGoogleIdentityResolver(userStore, extStore, alwaysAuthorized, nil, slog.Default())
	cfg := newExternalBearerConfigWithSA(t, newTestValidator(endpoints), resolver, []string{"my-a2a-project"})

	w, result := doExternalBearerRequest(cfg, "opaque-sa-access-token-listed-project")
	if result.reached {
		t.Fatal("handler must not be reached: SA access tokens are never accepted, listed project or not")
	}
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401: body=%s", w.Code, w.Body.String())
	}
	wantBody := wantErrorBody(t, ErrCodeUnauthorized, "invalid external bearer token")
	if !bytes.Equal(w.Body.Bytes(), wantBody) {
		t.Errorf("body = %s, want %s", w.Body.Bytes(), wantBody)
	}
	if len(userStore.users) != 0 {
		t.Errorf("expected no users created, got %d", len(userStore.users))
	}
}

// ---------------------------------------------------------------------------
// A suspended SA user is refused on the next request (403 user_suspended),
// with PreAuthorized still set: PreAuthorized bypasses the sign-in policy for
// first-time provisioning ONLY, never the suspension check.
// ---------------------------------------------------------------------------

func TestExternalBearer_ServiceAccountIDToken_Suspended_Forbidden(t *testing.T) {
	kp := newGCVTestKeyPair("test-kid-1")
	endpoints := newSAJWKSEndpoints(kp)
	defer endpoints.close()

	userStore := newFakeUserStore()
	extStore := newMemExtIDStore()
	resolver := NewGoogleIdentityResolver(userStore, extStore, neverAuthorized, nil, slog.Default())
	cfg := newExternalBearerConfigWithSA(t, newTestValidator(endpoints), resolver, []string{"my-a2a-project"})

	claims := serviceAccountIDTokenClaims("worker@my-a2a-project.iam.gserviceaccount.com")
	token := signIDToken(kp, claims)

	// First request provisions the SA user via PreAuthorized (authorize would
	// have denied it — see neverAuthorized above).
	w1, first := doExternalBearerRequest(cfg, token)
	if w1.Code != http.StatusOK {
		t.Fatalf("first request status = %d, want 200: body=%s", w1.Code, w1.Body.String())
	}

	u, err := userStore.GetUser(context.Background(), first.identity.ID())
	if err != nil {
		t.Fatalf("GetUser: %v", err)
	}
	u.Status = store.UserStatusSuspended
	if err := userStore.UpdateUser(context.Background(), u); err != nil {
		t.Fatalf("UpdateUser: %v", err)
	}

	w2, second := doExternalBearerRequest(cfg, token)
	if w2.Code != http.StatusForbidden {
		t.Fatalf("second request status = %d, want 403: body=%s", w2.Code, w2.Body.String())
	}
	if second.reached {
		t.Fatal("handler must not be reached for a suspended SA user")
	}
	wantBody := wantErrorBody(t, "user_suspended", "access denied: user account is suspended")
	if !bytes.Equal(w2.Body.Bytes(), wantBody) {
		t.Errorf("body = %s, want %s", w2.Body.Bytes(), wantBody)
	}
}

// TestExternalBearer_ServiceAccountIDToken_SuspendedExistingUserByEmail_Forbidden
// covers Resolve's OTHER suspension check: an existing user found by email
// (no binding yet), rather than the bound-user path
// TestExternalBearer_ServiceAccountIDToken_Suspended_Forbidden above already
// covers. An admin can pre-create or import a suspended user at the SA's
// email before the SA ever presents a token — isGoogleServiceAccount counts
// as authoritative for auto-linking, so this path is reachable for SAs.
// PreAuthorized must not exempt this check either.
func TestExternalBearer_ServiceAccountIDToken_SuspendedExistingUserByEmail_Forbidden(t *testing.T) {
	kp := newGCVTestKeyPair("test-kid-1")
	endpoints := newSAJWKSEndpoints(kp)
	defer endpoints.close()

	userStore := newFakeUserStore()
	extStore := newMemExtIDStore()
	addUser(userStore, "existing-suspended-sa-user", "worker@my-a2a-project.iam.gserviceaccount.com", "member", store.UserStatusSuspended)
	resolver := NewGoogleIdentityResolver(userStore, extStore, neverAuthorized, nil, slog.Default())
	cfg := newExternalBearerConfigWithSA(t, newTestValidator(endpoints), resolver, []string{"my-a2a-project"})

	claims := serviceAccountIDTokenClaims("worker@my-a2a-project.iam.gserviceaccount.com")
	token := signIDToken(kp, claims)

	w, result := doExternalBearerRequest(cfg, token)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: body=%s", w.Code, w.Body.String())
	}
	if result.reached {
		t.Fatal("handler must not be reached for a suspended existing user")
	}
	wantBody := wantErrorBody(t, "user_suspended", "access denied: user account is suspended")
	if !bytes.Equal(w.Body.Bytes(), wantBody) {
		t.Errorf("body = %s, want %s", w.Body.Bytes(), wantBody)
	}
	if _, err := extStore.GetExternalIdentity(context.Background(), "google", googleCanonicalIssuer, saNumericSub); err == nil {
		t.Error("expected no external identity binding to be created for a suspended user")
	}
}
