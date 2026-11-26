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

package trigger

import (
	"fmt"
	"net/http"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/trigger/source"
)

// GitHub webhook HTTP header names (Stage 2 §7.2).
const (
	HeaderGitHubSignature256 = "X-Hub-Signature-256"
	HeaderGitHubEvent        = "X-GitHub-Event"
	HeaderGitHubDelivery     = "X-GitHub-Delivery"
)

// GitHubV1Verifier verifies generic GitHub repository webhook deliveries using
// X-Hub-Signature-256 ("sha256=<hex>"), reusing githubapp.VerifyWebhookSignature
// via VerifySignature (or the shared source.VerifyHMACSHA256Hex implementation).
//
// Security Surface S19: GitHub signs only the raw request body (no timestamp,
// no signed headers). X-GitHub-Event and X-GitHub-Delivery are unsigned and
// shape-checked after body verification; github_v1 endpoints are therefore not
// replay-protected and serve message triggers only (Stage 2 §7.2, §7.3).
type GitHubV1Verifier struct {
	VerifySignature GitHubSignatureVerifyFunc
}

// Precheck rejects missing, duplicated, or syntactically malformed
// X-Hub-Signature-256 headers ("sha256=<64 lowercase hex>") before any secret
// lookup.
func (v *GitHubV1Verifier) Precheck(r *http.Request) error {
	if r == nil {
		return ErrInvalidSignature
	}
	sigVals := r.Header.Values(HeaderGitHubSignature256)
	if len(sigVals) != 1 {
		return ErrInvalidSignature
	}
	const prefix = "sha256="
	sig := sigVals[0]
	if len(sig) != len(prefix)+64 || sig[:len(prefix)] != prefix {
		return ErrInvalidSignature
	}
	for i := len(prefix); i < len(sig); i++ {
		c := sig[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return ErrInvalidSignature
		}
	}
	return nil
}

// Verify checks X-Hub-Signature-256 against each candidate secret in secrets
// and extracts bounded X-GitHub-Event and X-GitHub-Delivery headers.
func (v *GitHubV1Verifier) Verify(r *http.Request, body []byte, secrets []string) (VerifiedHeaders, error) {
	if r == nil || len(secrets) == 0 {
		return VerifiedHeaders{}, ErrInvalidSignature
	}
	sigVals := r.Header.Values(HeaderGitHubSignature256)
	if len(sigVals) != 1 {
		return VerifiedHeaders{}, ErrInvalidSignature
	}

	verifyFn := v.VerifySignature
	if verifyFn == nil {
		verifyFn = func(payload []byte, signature, secret string) bool {
			return source.VerifyHMACSHA256Hex(payload, signature, "sha256=", secret)
		}
	}

	matched := false
	for _, secret := range secrets {
		if verifyFn(body, sigVals[0], secret) {
			matched = true
			break
		}
	}
	if !matched {
		return VerifiedHeaders{}, ErrInvalidSignature
	}

	// Validate unsigned GitHub routing headers ONLY after body signature
	// verification passes so unauthenticated callers always receive 401.
	if len(r.Header.Values(HeaderGitHubEvent)) > 1 || len(r.Header.Values(HeaderGitHubDelivery)) > 1 {
		return VerifiedHeaders{}, fmt.Errorf("%w: duplicate X-GitHub-* header is not allowed", ErrInvalidHeader)
	}
	eventHdr := r.Header.Get(HeaderGitHubEvent)
	if eventHdr != "" {
		if len(eventHdr) > MaxEventBytes || !eventNamePattern.MatchString(eventHdr) {
			return VerifiedHeaders{}, fmt.Errorf("%w: invalid X-GitHub-Event header", ErrInvalidHeader)
		}
	}
	delivery := r.Header.Get(HeaderGitHubDelivery)
	if delivery != "" {
		if len(delivery) > MaxDeliveryBytes || !isPrintableString(delivery) {
			return VerifiedHeaders{}, fmt.Errorf("%w: invalid X-GitHub-Delivery header", ErrInvalidHeader)
		}
	}

	return VerifiedHeaders{
		Delivery: delivery,
		Event:    eventHdr,
	}, nil
}
