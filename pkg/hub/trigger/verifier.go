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
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Sentinel errors returned by Verifier implementations.
var (
	// ErrInvalidSignature indicates the request's signature was missing,
	// malformed, stale, or did not match any candidate secret (HTTP 401).
	ErrInvalidSignature = errors.New("invalid signature")

	// ErrInvalidHeader indicates the request passed signature verification but
	// carried a malformed or out-of-bounds routing header (HTTP 400).
	ErrInvalidHeader = errors.New("invalid routing header")
)

// VerifiedHeaders carries routing metadata extracted by a Verifier from an
// authenticated webhook request (Stage 2 §7.2).
type VerifiedHeaders struct {
	Delivery string
	Event    string
	Subject  string

	// Signed* flags record whether each field was covered by the cryptographic
	// signature (true for non-empty scion_v1 headers). Signed headers always
	// take precedence over routing hints (Stage 2 §7.2, §18 S20).
	SignedDelivery bool
	SignedEvent    bool
	SignedSubject  bool
}

// Verifier verifies an inbound HMAC webhook request against one or more
// candidate signing secrets (current secret plus previous secret during
// rotation grace) and extracts mode-specific routing headers (Stage 2 §7.2).
type Verifier interface {
	Verify(r *http.Request, body []byte, secrets []string) (VerifiedHeaders, error)
}

// signaturePrechecker is an optional interface implemented by Verifiers to
// reject missing, duplicated, or syntactically unparseable signature headers
// with ErrInvalidSignature (401) BEFORE any secret lookup or rotation fallback
// (review O1: unsigned/malformed requests never get 503 during secret outages
// or rotation grace).
type signaturePrechecker interface {
	Precheck(r *http.Request) error
}

// parseTimestampedSignatureHeader parses a comma-separated "t=<unix>,v1=<hex>"
// header shared by scion_v1 and stripe_v1, rejecting empty/malformed parts,
// duplicate t=, non-canonical t=, >maxSignatureV1Candidates v1= values, or
// headers with no syntactically valid 64-hex v1= candidate.
func parseTimestampedSignatureHeader(sigHeader string) bool {
	if sigHeader == "" {
		return false
	}
	var (
		tsRaw        string
		hasTS        bool
		v1Count      int
		hasValidHex1 bool
	)
	for _, part := range strings.Split(sigHeader, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			return false
		}
		k, v, ok := strings.Cut(part, "=")
		if !ok || v == "" {
			return false
		}
		switch k {
		case "t":
			if hasTS {
				return false
			}
			hasTS = true
			tsRaw = v
		case "v1":
			if v1Count >= maxSignatureV1Candidates {
				return false
			}
			v1Count++
			if len(v) == sha256.Size*2 {
				if _, err := hex.DecodeString(v); err == nil {
					hasValidHex1 = true
				}
			}
		default:
			if strings.ContainsAny(k, " \t\r\n") {
				return false
			}
		}
	}
	if !hasTS || !hasValidHex1 {
		return false
	}
	ts, err := strconv.ParseInt(tsRaw, 10, 64)
	return err == nil && strconv.FormatInt(ts, 10) == tsRaw
}

// GitHubSignatureVerifyFunc matches githubapp.VerifyWebhookSignature.
type GitHubSignatureVerifyFunc func(payload []byte, signature, secret string) bool

// NewVerifierForMode constructs the Verifier for signatureMode, returning nil
// if the mode is unsupported.
func NewVerifierForMode(
	signatureMode string,
	now time.Time,
	replayWindow time.Duration,
	githubVerifyFn GitHubSignatureVerifyFunc,
) Verifier {
	switch signatureMode {
	case store.WebhookSignatureModeScionV1:
		return &ScionV1Verifier{
			Now:          now,
			ReplayWindow: replayWindow,
		}
	case store.WebhookSignatureModeGitHubV1:
		return &GitHubV1Verifier{
			VerifySignature: githubVerifyFn,
		}
	case store.WebhookSignatureModeStripeV1:
		return &StripeV1Verifier{
			Now:          now,
			ReplayWindow: replayWindow,
		}
	default:
		return nil
	}
}
