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
	"errors"
	"log/slog"
	"net/http"
	"testing"

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

// externalBearerTestPlatformAuthSA is a realistic-shaped configured
// transport service account email, matching the allowed_gcp_projects entry
// used below (project "example"). Defined locally (rather than reusing the
// identical constant in reserved_platform_identity_test.go) because that
// file carries the !no_sqlite build tag and this one does not.
const externalBearerTestPlatformAuthSA = "transport-sa@example.iam.gserviceaccount.com"

// TestExternalBearer_PlatformAuthSA_Denied exercises the configured
// transport service account end to end on the external-bearer path: a
// service-account ID token is admitted under allowed_gcp_projects with
// PreAuthorized=true (see
// TestExternalBearer_ServiceAccountIDToken_ProjectListed_Authenticates
// above), which skips the Hub sign-in policy entirely. This is the path
// GoogleIdentityResolver.Resolve's isReservedPlatformIdentity check actually
// protects — unlike the GE exchange endpoint, nothing upstream of Resolve
// rejects a service-account credential here.
func TestExternalBearer_PlatformAuthSA_Denied(t *testing.T) {
	kp := newGCVTestKeyPair("test-kid-1")
	endpoints := newSAJWKSEndpoints(kp)
	defer endpoints.close()

	userStore := newFakeUserStore()
	extStore := newMemExtIDStore()
	// authorize always allows: proves the denial comes from the
	// platformAuthSA guard, not from the sign-in policy PreAuthorized skips.
	resolver := NewGoogleIdentityResolver(userStore, extStore, alwaysAuthorized, nil, slog.Default())
	resolver.SetPlatformAuthSA(externalBearerTestPlatformAuthSA)
	cfg := newExternalBearerConfigWithSA(t, newTestValidator(endpoints), resolver, []string{"example"})

	claims := serviceAccountIDTokenClaims(externalBearerTestPlatformAuthSA)
	token := signIDToken(kp, claims)

	w, result := doExternalBearerRequest(cfg, token)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: body=%s", w.Code, w.Body.String())
	}
	if result.reached {
		t.Fatal("handler must not be reached for the configured service account")
	}
	if _, err := userStore.GetUserByEmail(context.Background(), externalBearerTestPlatformAuthSA); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected no user row for the configured service account, lookup returned err=%v", err)
	}
}

// ---------------------------------------------------------------------------
// allowed_emails on the Google issuer also narrows service-account
// admission: with it set, an SA must be in an allowed_gcp_projects project
// AND match allowed_emails (exact, case-insensitive, or "*@suffix", as on the
// federation path). Empty allowed_emails keeps project-only admission.
// ---------------------------------------------------------------------------

// TestExternalBearer_ServiceAccountIDToken_EmailNotListed_Unauthorized: an
// SA in an allowed project but not in allowed_emails gets the same 401 as an
// unlisted project, and no user row or binding is created.
func TestExternalBearer_ServiceAccountIDToken_EmailNotListed_Unauthorized(t *testing.T) {
	kp := newGCVTestKeyPair("test-kid-1")
	endpoints := newSAJWKSEndpoints(kp)
	defer endpoints.close()

	userStore := newFakeUserStore()
	extStore := newMemExtIDStore()
	resolver := NewGoogleIdentityResolver(userStore, extStore, alwaysAuthorized, nil, slog.Default())
	cfg := newExternalBearerConfigWithSAEmails(t, newTestValidator(endpoints), resolver,
		[]string{"my-a2a-project"}, []string{"canary@my-a2a-project.iam.gserviceaccount.com"})

	claims := serviceAccountIDTokenClaims("worker@my-a2a-project.iam.gserviceaccount.com")
	token := signIDToken(kp, claims)

	w, result := doExternalBearerRequest(cfg, token)
	if result.reached {
		t.Fatal("handler must not be reached: SA email is not in allowed_emails")
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
	if _, err := extStore.GetExternalIdentity(context.Background(), "google", googleCanonicalIssuer, saNumericSub); err == nil {
		t.Error("expected no external identity binding for an SA outside allowed_emails")
	}
}

// TestExternalBearer_ServiceAccountIDToken_EmailListed_Authenticates covers
// the admitted side: an exact entry (in a different case from the token's
// email, so the match must be case-insensitive) and a "*@suffix" entry.
// neverAuthorized proves admission still bypasses the sign-in policy.
func TestExternalBearer_ServiceAccountIDToken_EmailListed_Authenticates(t *testing.T) {
	cases := []struct {
		name          string
		allowedEmails []string
	}{
		{"exact_case_insensitive", []string{"other@my-a2a-project.iam.gserviceaccount.com", "Worker@My-A2A-Project.IAM.gserviceaccount.com"}},
		{"suffix_wildcard", []string{"*@my-a2a-project.iam.gserviceaccount.com"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kp := newGCVTestKeyPair("test-kid-1")
			endpoints := newSAJWKSEndpoints(kp)
			defer endpoints.close()

			userStore := newFakeUserStore()
			extStore := newMemExtIDStore()
			resolver := NewGoogleIdentityResolver(userStore, extStore, neverAuthorized, nil, slog.Default())
			cfg := newExternalBearerConfigWithSAEmails(t, newTestValidator(endpoints), resolver,
				[]string{"my-a2a-project"}, tc.allowedEmails)

			claims := serviceAccountIDTokenClaims("worker@my-a2a-project.iam.gserviceaccount.com")
			token := signIDToken(kp, claims)

			w, result := doExternalBearerRequest(cfg, token)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200: body=%s", w.Code, w.Body.String())
			}
			if !result.reached || result.identity == nil || result.identity.Email() != "worker@my-a2a-project.iam.gserviceaccount.com" {
				t.Fatalf("expected the listed SA to be admitted, got reached=%v identity=%+v", result.reached, result.identity)
			}
			if len(userStore.users) != 1 {
				t.Fatalf("expected exactly 1 user provisioned, got %d", len(userStore.users))
			}
		})
	}
}

// TestExternalBearer_ServiceAccountIDToken_EmailWildcardOtherSuffix_Unauthorized:
// a "*@suffix" entry for another project's SA domain does not admit this SA,
// and a listed email never widens allowed_gcp_projects (the email check
// narrows admission; it is not an alternative to the project check).
func TestExternalBearer_ServiceAccountIDToken_EmailWildcardOtherSuffix_Unauthorized(t *testing.T) {
	cases := []struct {
		name          string
		allowedGCP    []string
		allowedEmails []string
	}{
		{"wildcard_other_project", []string{"my-a2a-project"}, []string{"*@other-project.iam.gserviceaccount.com"}},
		{"email_listed_project_not", []string{"some-other-project"}, []string{"worker@my-a2a-project.iam.gserviceaccount.com"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kp := newGCVTestKeyPair("test-kid-1")
			endpoints := newSAJWKSEndpoints(kp)
			defer endpoints.close()

			userStore := newFakeUserStore()
			extStore := newMemExtIDStore()
			resolver := NewGoogleIdentityResolver(userStore, extStore, alwaysAuthorized, nil, slog.Default())
			cfg := newExternalBearerConfigWithSAEmails(t, newTestValidator(endpoints), resolver, tc.allowedGCP, tc.allowedEmails)

			claims := serviceAccountIDTokenClaims("worker@my-a2a-project.iam.gserviceaccount.com")
			token := signIDToken(kp, claims)

			w, result := doExternalBearerRequest(cfg, token)
			if result.reached || w.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d reached=%v, want 401 and not reached: body=%s", w.Code, result.reached, w.Body.String())
			}
			if len(userStore.users) != 0 {
				t.Errorf("expected no users created, got %d", len(userStore.users))
			}
		})
	}
}

// TestExternalBearer_ServiceAccountIDToken_EmptyAllowedEmails_ProjectOnly
// pins the unchanged behaviour: with allowed_emails empty, any SA in an
// allowed project is still admitted.
func TestExternalBearer_ServiceAccountIDToken_EmptyAllowedEmails_ProjectOnly(t *testing.T) {
	kp := newGCVTestKeyPair("test-kid-1")
	endpoints := newSAJWKSEndpoints(kp)
	defer endpoints.close()

	userStore := newFakeUserStore()
	extStore := newMemExtIDStore()
	resolver := NewGoogleIdentityResolver(userStore, extStore, neverAuthorized, nil, slog.Default())
	cfg := newExternalBearerConfigWithSAEmails(t, newTestValidator(endpoints), resolver,
		[]string{"my-a2a-project"}, []string{})

	claims := serviceAccountIDTokenClaims("worker@my-a2a-project.iam.gserviceaccount.com")
	token := signIDToken(kp, claims)

	w, result := doExternalBearerRequest(cfg, token)
	if w.Code != http.StatusOK || !result.reached {
		t.Fatalf("status = %d reached=%v, want 200: body=%s", w.Code, result.reached, w.Body.String())
	}
}

// TestExternalBearer_ServiceAccountIDToken_RemovedFromAllowedEmails_CachedTokenRefused:
// an SA provisioned while listed is refused once it is removed from
// allowed_emails (a live federation config swap, as an operational-settings
// reload does), even when its token is served from the production credential
// cache. The cache holds only the verified identity; the allowed_emails
// check runs on every request, before Resolve, so the existing user row and
// binding do not let the SA back in.
func TestExternalBearer_ServiceAccountIDToken_RemovedFromAllowedEmails_CachedTokenRefused(t *testing.T) {
	kp := newGCVTestKeyPair("test-kid-1")
	endpoints := newSAJWKSEndpoints(kp)
	defer endpoints.close()

	userStore := newFakeUserStore()
	extStore := newMemExtIDStore()
	resolver := NewGoogleIdentityResolver(userStore, extStore, alwaysAuthorized, nil, slog.Default())
	validator := NewCachingGoogleCredentialValidator(newTestValidator(endpoints))
	cfg := newExternalBearerConfigWithSAEmails(t, validator, resolver,
		[]string{"my-a2a-project"}, []string{"worker@my-a2a-project.iam.gserviceaccount.com"})

	claims := serviceAccountIDTokenClaims("worker@my-a2a-project.iam.gserviceaccount.com")
	token := signIDToken(kp, claims)

	w1, first := doExternalBearerRequest(cfg, token)
	if w1.Code != http.StatusOK || !first.reached {
		t.Fatalf("first request status = %d reached=%v, want 200: body=%s", w1.Code, first.reached, w1.Body.String())
	}
	if len(userStore.users) != 1 {
		t.Fatalf("expected the listed SA to be provisioned, got %d users", len(userStore.users))
	}

	probe, ok := validator.(externalBearerCacheProbe)
	if !ok || !probe.Cached(token, []string{externalBearerTestAudience}) {
		t.Fatal("expected the SA token to be in the credential cache after the first request")
	}

	cfg.FederationAuth.Store(newGoogleTrustFederationAuthWithSAEmails(t, externalBearerTestAudience,
		[]string{"my-a2a-project"}, []string{"canary@my-a2a-project.iam.gserviceaccount.com"}))

	w2, second := doExternalBearerRequest(cfg, token)
	if second.reached {
		t.Fatal("handler must not be reached: the SA was removed from allowed_emails")
	}
	if w2.Code != http.StatusUnauthorized {
		t.Fatalf("second request status = %d, want 401: body=%s", w2.Code, w2.Body.String())
	}
}

// TestExternalBearer_UserIDToken_NotInAllowedEmails_Authenticates: on the
// external-bearer path allowed_emails narrows service accounts only. A user
// principal whose email is not listed is still admitted (here with no
// allowed_domains and a sign-in policy that allows), so an operator who lists
// only service accounts there does not lock users out of this path.
func TestExternalBearer_UserIDToken_NotInAllowedEmails_Authenticates(t *testing.T) {
	kp := newGCVTestKeyPair("test-kid-1")
	endpoints := newSAJWKSEndpoints(kp)
	defer endpoints.close()

	userStore := newFakeUserStore()
	extStore := newMemExtIDStore()
	resolver := NewGoogleIdentityResolver(userStore, extStore, alwaysAuthorized, nil, slog.Default())
	cfg := newExternalBearerConfigWithSAEmails(t, newTestValidator(endpoints), resolver,
		[]string{"my-a2a-project"}, []string{"worker@my-a2a-project.iam.gserviceaccount.com"})

	claims := validIDTokenClaims()
	claims["aud"] = externalBearerTestAudience
	claims["email"] = "user@example.com"
	claims["hd"] = "example.com" // authoritative domain, so Resolve may provision the user
	token := signIDToken(kp, claims)

	w, result := doExternalBearerRequest(cfg, token)
	if w.Code != http.StatusOK || !result.reached {
		t.Fatalf("status = %d reached=%v, want 200: body=%s", w.Code, result.reached, w.Body.String())
	}
	if result.identity == nil || result.identity.Email() != "user@example.com" {
		t.Fatalf("unexpected identity: %+v", result.identity)
	}
}
