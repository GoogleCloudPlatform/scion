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

package store

import (
	"fmt"
	"net/url"
	"strings"
)

// FederationIdentityProvider is the ExternalIdentityBinding.Provider of the
// binding that links a (trusted issuer, subject) pair to a federated
// account (UserKindFederated).
const FederationIdentityProvider = "federation"

// federatedEmailLocalPrefix and federatedEmailTLD shape the synthetic email
// of a federated account: fed+<user ID>@<issuer host>.invalid. The .invalid
// TLD is reserved (RFC 2606), so the address never belongs to anyone and no
// sign-in or email-keyed lookup can produce it.
const (
	federatedEmailLocalPrefix = "fed+"
	federatedEmailTLD         = ".invalid"
)

// ErrFederatedAccountRefused is returned when a path other than the
// federated account path tries to create a federated account or an address
// of its synthetic shape, or when a general path tries to give a federated
// account a role binding, a group membership or a user access credential. It
// wraps ErrInvalidInput.
var ErrFederatedAccountRefused = fmt.Errorf("%w: federated accounts are managed only by the federated account path", ErrInvalidInput)

// NormalizeFederatedIssuer returns issuer with surrounding white space and
// trailing slashes removed, the form federated accounts and their identity
// bindings store (it matches the federation authenticator's normalization).
func NormalizeFederatedIssuer(issuer string) string {
	return strings.TrimRight(strings.TrimSpace(issuer), "/")
}

// NormalizeFederatedEmail returns email trimmed and lower-cased, the form
// UserKindFederated rows store in FederatedEmail.
func NormalizeFederatedEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// FederatedSyntheticEmail returns the synthetic email of the federated
// account userID whose home issuer is issuerURL:
// fed+<userID>@<issuer host>.invalid, lower case. It returns ErrInvalidInput
// when the user ID is empty or the issuer URL has no host.
func FederatedSyntheticEmail(userID, issuerURL string) (string, error) {
	id := strings.ToLower(strings.TrimSpace(userID))
	if id == "" {
		return "", fmt.Errorf("%w: a federated account requires a user ID", ErrInvalidInput)
	}
	u, err := url.Parse(NormalizeFederatedIssuer(issuerURL))
	if err != nil {
		return "", fmt.Errorf("%w: invalid issuer URL: %v", ErrInvalidInput, err)
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host == "" {
		return "", fmt.Errorf("%w: issuer URL %q has no host", ErrInvalidInput, issuerURL)
	}
	return federatedEmailLocalPrefix + id + "@" + host + federatedEmailTLD, nil
}

// IsFederatedSyntheticEmail reports whether email has the shape of a
// federated account's synthetic address: a local part starting with
// "fed+" in a domain under the .invalid TLD. The comparison trims
// surrounding white space and ignores case. Only the federated account
// path writes such an address; every other user create or update refuses it.
func IsFederatedSyntheticEmail(email string) bool {
	e := strings.ToLower(strings.TrimSpace(email))
	at := strings.LastIndex(e, "@")
	if at < 0 {
		return false
	}
	domain := strings.TrimSuffix(e[at+1:], ".")
	return strings.HasPrefix(e[:at], federatedEmailLocalPrefix) && strings.HasSuffix(domain, federatedEmailTLD)
}
