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
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// HeaderStripeSignature is the Stripe webhook HMAC signature header (Stage 2 §7.2).
const HeaderStripeSignature = "Stripe-Signature"

// computeStripeV1MAC computes the raw 32-byte HMAC-SHA256 digest over the
// Stripe v1 canonical payload "<t>.<body>" without allocating a concatenated
// body buffer (Stage 2 §7.2).
func computeStripeV1MAC(secret, tsRaw string, body []byte) []byte {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(tsRaw))
	_, _ = mac.Write([]byte("."))
	_, _ = mac.Write(body)
	return mac.Sum(nil)
}

// SignStripeV1 formats a valid Stripe-Signature header value ("t=<ts>,v1=<hex>")
// for the given secret, unix timestamp (seconds), and raw body (Stage 2 §7.2).
func SignStripeV1(secret string, ts int64, body []byte) string {
	tsRaw := strconv.FormatInt(ts, 10)
	sum := computeStripeV1MAC(secret, tsRaw, body)
	return fmt.Sprintf("t=%s,v1=%s", tsRaw, hex.EncodeToString(sum))
}

// VerifyStripeV1SignatureWithWindow verifies a Stripe-Signature header
// ("t=<unix>,v1=<hex>[,v1=<hex>...]") against secret, now (±window clamped to
// MinReplayWindow..MaxReplayWindow), and raw body (Stage 2 §7.2, §7.3).
func VerifyStripeV1SignatureWithWindow(secret string, now time.Time, window time.Duration, sigHeader string, body []byte) bool {
	if secret == "" || sigHeader == "" {
		return false
	}
	if window <= 0 {
		window = ReplayWindow
	} else if window < MinReplayWindow {
		window = MinReplayWindow
	} else if window > MaxReplayWindow {
		window = MaxReplayWindow
	}

	var (
		tsRaw   string
		hasTS   bool
		v1Hexes []string
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
				// Reject ambiguous multiple t= parameters.
				return false
			}
			hasTS = true
			tsRaw = v
		case "v1":
			if len(v1Hexes) >= maxSignatureV1Candidates {
				return false
			}
			v1Hexes = append(v1Hexes, v)
		default:
			// Ignore unknown tags (e.g. Stripe test-mode v0=), provided k is a simple token.
			if strings.ContainsAny(k, " \t\r\n") {
				return false
			}
		}
	}
	if !hasTS || len(v1Hexes) == 0 {
		return false
	}

	ts, err := strconv.ParseInt(tsRaw, 10, 64)
	if err != nil || strconv.FormatInt(ts, 10) != tsRaw {
		return false
	}
	windowSec := int64(window / time.Second)
	diff := now.Unix() - ts
	if diff < -windowSec || diff > windowSec {
		return false
	}

	expected := computeStripeV1MAC(secret, tsRaw, body)
	matched := false
	for _, candHex := range v1Hexes {
		if len(candHex) != sha256.Size*2 {
			continue
		}
		cand, decErr := hex.DecodeString(candHex)
		if decErr != nil {
			continue
		}
		if hmac.Equal(cand, expected) {
			matched = true
		}
	}
	return matched
}

// StripeV1Verifier verifies Stripe webhook deliveries using Stripe-Signature
// ("t=<unix>,v1=<hex>", signed payload "<t>.<body>") within ±ReplayWindow
// (Stage 2 §7.2, §7.3). Type, subject, and dedup are extracted from the signed
// body via routing hints (with Stripe defaults /type, /data/object/id, /id).
type StripeV1Verifier struct {
	Now          time.Time
	ReplayWindow time.Duration
}

// Verify checks Stripe-Signature against each candidate secret in secrets.
func (v *StripeV1Verifier) Verify(r *http.Request, body []byte, secrets []string) (VerifiedHeaders, error) {
	if r == nil || len(secrets) == 0 {
		return VerifiedHeaders{}, ErrInvalidSignature
	}
	sigVals := r.Header.Values(HeaderStripeSignature)
	if len(sigVals) != 1 {
		return VerifiedHeaders{}, ErrInvalidSignature
	}

	now := v.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}

	for _, secret := range secrets {
		if VerifyStripeV1SignatureWithWindow(secret, now, v.ReplayWindow, sigVals[0], body) {
			return VerifiedHeaders{}, nil
		}
	}
	return VerifiedHeaders{}, ErrInvalidSignature
}
