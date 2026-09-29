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

import "fmt"

// ---------------------------------------------------------------------------
// Shared provider email-ownership invariant.
//
// Pre-registration user records are keyed by email, so associating a login
// with one by email must rest on the provider having actually proven the
// caller owns that address — not merely reported it. Google's and GitHub's
// web-login userinfo handling each decide this independently; requireVerifiedEmail
// is the one place that decision is made, so the two cannot drift apart the
// way they did before (one never checked its verified flag at all; the
// other fell back to an address it never verified). OIDC already enforces
// this invariant inline and is left as its own reference implementation.
// ---------------------------------------------------------------------------

// requireVerifiedEmail returns email when the provider has verified it, or
// an error otherwise. provider is used only to label the error message.
func requireVerifiedEmail(provider, email string, verified bool) (string, error) {
	if email == "" {
		return "", fmt.Errorf("%s did not return an email address", provider)
	}
	if !verified {
		return "", fmt.Errorf("%s returned an unverified email address %q; the user must verify their email with the provider before logging in", provider, email)
	}
	return email, nil
}
