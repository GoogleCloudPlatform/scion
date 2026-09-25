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
	"testing"
)

// TestGoogleIdentityResolver_Resolve_NilIdentity_ReturnsError covers a
// GoogleCredentialValidator that returns (nil, nil) upstream of Resolve — a
// contract violation, since ValidateIDToken/ValidateAccessToken must return
// a non-nil identity whenever err is nil. Resolve must not panic on
// identity.Issuer and must return an error rather than a user. Both callers
// (ge_exchange.go's Exchange and auth_external_bearer.go's
// authenticateExternalBearer via classifyResolveError) already map an
// unrecognized Resolve error to a 5xx: see
// TestGEExchange_NilIdentityFromValidator_InternalError and
// TestExternalBearer_AccessToken_NilIdentityFromValidator_ServiceUnavailable
// for those two callers' end-to-end coverage. This test exercises Resolve
// itself, directly.
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
