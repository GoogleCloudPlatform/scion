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
	"fmt"
	"strings"
)

// ---------------------------------------------------------------------------
// Shared provider email-ownership invariant.
//
// Pre-registration user records are keyed by email, so associating a login
// with one by email must rest on the provider having actually proven the
// caller owns that address — not merely reported it. Google, GitHub, and
// OIDC web-login userinfo handling all decide this the same way, through
// requireVerifiedEmail, so Google and GitHub apply the same rule as OIDC's
// original reference implementation — and none of the three can drift apart
// from the others on this again.
// ---------------------------------------------------------------------------

// requireVerifiedEmail returns email when the provider has verified it, or
// an error otherwise. provider is used only to label the error message.
func requireVerifiedEmail(provider, email string, verified bool) (string, error) {
	email = strings.TrimSpace(email)
	if email == "" {
		return "", fmt.Errorf("%s did not return an email address", provider)
	}
	if !verified {
		return "", fmt.Errorf("%s returned an unverified email address %q; the user must verify their email with the provider before logging in", provider, email)
	}
	return email, nil
}
