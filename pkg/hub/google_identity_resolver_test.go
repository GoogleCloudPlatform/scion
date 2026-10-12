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
	"reflect"
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

// emailLookupErrUserStore wraps fakeUserStore so that GetUserByEmail returns
// a fixed store error that is not store.ErrNotFound.
type emailLookupErrUserStore struct {
	*fakeUserStore
	err error
}

func (s *emailLookupErrUserStore) GetUserByEmail(context.Context, string) (*store.User, error) {
	return nil, s.err
}

// TestGoogleIdentityResolver_Resolve_EmailLookupStoreError_ReturnsError checks
// that a store error other than store.ErrNotFound from the email lookup is
// returned by Resolve, the same as for the binding lookup, and that no user
// or binding is created.
func TestGoogleIdentityResolver_Resolve_EmailLookupStoreError_ReturnsError(t *testing.T) {
	storeErr := errors.New("database is locked")
	inner := newFakeUserStore()
	userStore := &emailLookupErrUserStore{fakeUserStore: inner, err: storeErr}
	extStore := newMemExtIDStore()
	resolver := newTestResolver(userStore, extStore, alwaysAuthorized, nil)

	user, err := resolver.Resolve(context.Background(), validGmailIdentity(), ResolvePolicy{})
	if !errors.Is(err, storeErr) {
		t.Fatalf("expected Resolve to return the email lookup store error, got user=%+v err=%v", user, err)
	}
	if user != nil {
		t.Fatalf("expected no user on email lookup store error, got %+v", user)
	}
	inner.mu.Lock()
	userCount := len(inner.users)
	inner.mu.Unlock()
	if userCount != 0 {
		t.Fatalf("expected no user to be created, store has %d", userCount)
	}
	extStore.mu.Lock()
	bindingCount := len(extStore.bindings)
	extStore.mu.Unlock()
	if bindingCount != 0 {
		t.Fatalf("expected no binding to be created, store has %d", bindingCount)
	}
}

// TestGoogleIdentityResolver_Resolve_EmailNotFound_Provisions checks that
// store.ErrNotFound from the email lookup continues into new-user
// provisioning and creates one user and one binding.
func TestGoogleIdentityResolver_Resolve_EmailNotFound_Provisions(t *testing.T) {
	userStore := newFakeUserStore()
	extStore := newMemExtIDStore()
	resolver := newTestResolver(userStore, extStore, alwaysAuthorized, nil)
	identity := validGmailIdentity()

	user, err := resolver.Resolve(context.Background(), identity, ResolvePolicy{})
	if err != nil {
		t.Fatalf("unexpected error when no user has this email: %v", err)
	}
	if user == nil || user.Email != identity.Email {
		t.Fatalf("expected a provisioned user for %s, got %+v", identity.Email, user)
	}
	if _, lookupErr := userStore.GetUser(context.Background(), user.ID); lookupErr != nil {
		t.Fatalf("expected provisioned user %s in the store: %v", user.ID, lookupErr)
	}
	binding, bindErr := extStore.GetExternalIdentity(context.Background(), "google", googleCanonicalIssuer, identity.Subject)
	if bindErr != nil {
		t.Fatalf("expected a binding for the provisioned user: %v", bindErr)
	}
	if binding.UserID != user.ID {
		t.Fatalf("expected binding for user %s, got %s", user.ID, binding.UserID)
	}
	userStore.mu.Lock()
	userCount := len(userStore.users)
	userStore.mu.Unlock()
	if userCount != 1 {
		t.Fatalf("expected exactly 1 user in the store, got %d", userCount)
	}
	extStore.mu.Lock()
	bindingCount := len(extStore.bindings)
	extStore.mu.Unlock()
	if bindingCount != 1 {
		t.Fatalf("expected exactly 1 binding in the store, got %d", bindingCount)
	}
}

// identitiesLookupErrExtIDStore wraps memExtIDStore so that
// GetExternalIdentitiesByUserID returns a fixed store error.
type identitiesLookupErrExtIDStore struct {
	*memExtIDStore
	err error
}

func (s *identitiesLookupErrExtIDStore) GetExternalIdentitiesByUserID(context.Context, string) ([]*store.ExternalIdentityBinding, error) {
	return nil, s.err
}

// TestGoogleIdentityResolver_Resolve_ExistingIdentitiesLookupStoreError_ReturnsError
// checks that a store error from the lookup of an existing user's external
// identities is returned by Resolve, that the user record is unchanged, and
// that no binding is created. store.ErrNotFound is returned like any other
// error, since the store returns an empty slice for a user with no bindings.
func TestGoogleIdentityResolver_Resolve_ExistingIdentitiesLookupStoreError_ReturnsError(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{name: "store error", err: errors.New("database is locked")},
		{name: "not found", err: store.ErrNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			userStore := newFakeUserStore()
			before := *addUser(userStore, "user-1", "user@gmail.com", "member", "active")
			inner := newMemExtIDStore()
			extStore := &identitiesLookupErrExtIDStore{memExtIDStore: inner, err: tc.err}
			resolver := newTestResolver(userStore, extStore, alwaysAuthorized, nil)
			identity := validGmailIdentity()
			identity.DisplayName = "Changed Name"

			user, err := resolver.Resolve(context.Background(), identity, ResolvePolicy{})
			if !errors.Is(err, tc.err) {
				t.Fatalf("expected Resolve to return the existing identities lookup error, got user=%+v err=%v", user, err)
			}
			if user != nil {
				t.Fatalf("expected no user on existing identities lookup error, got %+v", user)
			}
			after, lookupErr := userStore.GetUser(context.Background(), before.ID)
			if lookupErr != nil {
				t.Fatalf("expected user %s in the store: %v", before.ID, lookupErr)
			}
			if !reflect.DeepEqual(before, *after) {
				t.Fatalf("expected the user record to be unchanged, before=%+v after=%+v", before, *after)
			}
			userStore.mu.Lock()
			userCount := len(userStore.users)
			userStore.mu.Unlock()
			if userCount != 1 {
				t.Fatalf("expected exactly 1 user in the store, got %d", userCount)
			}
			inner.mu.Lock()
			bindingCount := len(inner.bindings)
			inner.mu.Unlock()
			if bindingCount != 0 {
				t.Fatalf("expected no binding to be created, store has %d", bindingCount)
			}
		})
	}
}

// TestGoogleIdentityResolver_Resolve_ExistingUserByEmail_Links checks that
// when the existing identities lookup succeeds with no bindings, Resolve
// links the identity to the user found by email.
func TestGoogleIdentityResolver_Resolve_ExistingUserByEmail_Links(t *testing.T) {
	userStore := newFakeUserStore()
	existing := addUser(userStore, "user-1", "user@gmail.com", "member", "active")
	extStore := newMemExtIDStore()
	resolver := newTestResolver(userStore, extStore, alwaysAuthorized, nil)
	identity := validGmailIdentity()

	user, err := resolver.Resolve(context.Background(), identity, ResolvePolicy{})
	if err != nil {
		t.Fatalf("unexpected error linking the existing user: %v", err)
	}
	if user == nil || user.ID != existing.ID {
		t.Fatalf("expected existing user %s, got %+v", existing.ID, user)
	}
	binding, bindErr := extStore.GetExternalIdentity(context.Background(), "google", googleCanonicalIssuer, identity.Subject)
	if bindErr != nil {
		t.Fatalf("expected a binding for the existing user: %v", bindErr)
	}
	if binding.UserID != existing.ID {
		t.Fatalf("expected binding for user %s, got %s", existing.ID, binding.UserID)
	}
	userStore.mu.Lock()
	userCount := len(userStore.users)
	userStore.mu.Unlock()
	if userCount != 1 {
		t.Fatalf("expected exactly 1 user in the store, got %d", userCount)
	}
	extStore.mu.Lock()
	bindingCount := len(extStore.bindings)
	extStore.mu.Unlock()
	if bindingCount != 1 {
		t.Fatalf("expected exactly 1 binding in the store, got %d", bindingCount)
	}
}
