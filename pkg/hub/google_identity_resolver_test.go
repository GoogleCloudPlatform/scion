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
	"context"
	"errors"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// TestGoogleIdentityResolver_Resolve_NilIdentity_ReturnsError covers a
// GoogleCredentialValidator that returns (nil, nil) upstream of Resolve — a
// contract violation, since ValidateIDToken/ValidateAccessToken must return
// a non-nil identity whenever err is nil. Resolve must not panic on
// identity.Issuer and must return an error rather than a user. Both callers
// already map an unrecognized Resolve error to a 5xx, not the 4xx arms
// reserved for the named sentinels: ge_exchange.go's Exchange, covered by
// TestGEExchange_ProvisionNewUser_CreateError_FailsClosed ("user resolution
// failed", 500); and auth_external_bearer.go's authenticateExternalBearer
// via classifyResolveError, covered by
// TestExternalBearer_ResolveInternalError_ServiceUnavailable and
// TestExternalBearer_GetExternalIdentityFault_ServiceUnavailable (503
// store_error). This test exercises Resolve itself, directly, with a nil
// identity specifically — none of those three reach Resolve with one, since
// each drives a different Resolve fault.
func TestGoogleIdentityResolver_Resolve_NilIdentity_ReturnsError(t *testing.T) {
	userStore := newFakeUserStore()
	extStore := newMemExtIDStore()
	resolver := NewGoogleIdentityResolver(userStore, extStore, alwaysAuthorized, nil, nil)

	user, err := resolver.Resolve(context.Background(), nil, ResolvePolicy{})
	if err == nil {
		t.Fatal("expected an error when identity is nil")
	}
	if user != nil {
		t.Errorf("expected a nil user, got %+v", user)
	}
}

// TestGoogleIdentityResolver_Resolve_PlatformAuthSA_Denied exercises the
// check in Resolve directly, without going through the credential validator
// or either of Resolve's two callers. For the GE exchange endpoint this
// duplicates its existing service-account rejection (ge_exchange.go's Step
// 1.5); for the external-bearer path, which admits a service-account
// identity whose GCP project is on its allowed_gcp_projects list, this is
// the check that applies (see TestExternalBearer_PlatformAuthSA_Denied for
// that path end-to-end). This test drives Resolve with a hand-built
// ValidatedGoogleIdentity to exercise the check on its own.
func TestGoogleIdentityResolver_Resolve_PlatformAuthSA_Denied(t *testing.T) {
	userStore := newFakeUserStore()
	extStore := newMemExtIDStore()
	resolver := NewGoogleIdentityResolver(userStore, extStore, alwaysAuthorized, nil, nil)
	const platformAuthSA = "transport-sa@example.iam.gserviceaccount.com"
	resolver.SetPlatformAuthSA(platformAuthSA)

	identity := &ValidatedGoogleIdentity{
		Subject:          "111122223333",
		Email:            platformAuthSA,
		EmailVerified:    true,
		Issuer:           googleIssuerHTTPS,
		IsServiceAccount: true,
		UpstreamExpiry:   time.Now().Add(time.Hour),
	}

	user, err := resolver.Resolve(context.Background(), identity, ResolvePolicy{PreAuthorized: true})
	if !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("expected ErrAccessDenied, got %v", err)
	}
	if user != nil {
		t.Errorf("expected a nil user, got %+v", user)
	}

	if _, lookupErr := userStore.GetUserByEmail(context.Background(), platformAuthSA); !errors.Is(lookupErr, store.ErrNotFound) {
		t.Fatalf("expected no user row for the configured service account, lookup returned err=%v", lookupErr)
	}
}

// TestGoogleIdentityResolver_Resolve_PlatformAuthSA_Unconfigured_Inert is a
// regression check: with no platform auth SA configured (the default),
// Resolve's normal provisioning path for a Google identity is unaffected.
func TestGoogleIdentityResolver_Resolve_PlatformAuthSA_Unconfigured_Inert(t *testing.T) {
	userStore := newFakeUserStore()
	extStore := newMemExtIDStore()
	resolver := NewGoogleIdentityResolver(userStore, extStore, alwaysAuthorized, nil, nil)
	// resolver.platformAuthSA intentionally left unset.

	identity := &ValidatedGoogleIdentity{
		Subject:        "user-sub-1",
		Email:          "person@gmail.com",
		EmailVerified:  true,
		Issuer:         googleIssuerHTTPS,
		UpstreamExpiry: time.Now().Add(time.Hour),
	}

	user, err := resolver.Resolve(context.Background(), identity, ResolvePolicy{})
	if err != nil {
		t.Fatalf("unexpected error with no platform auth SA configured: %v", err)
	}
	if user == nil || user.Email != "person@gmail.com" {
		t.Fatalf("expected a provisioned user for person@gmail.com, got %+v", user)
	}
}
