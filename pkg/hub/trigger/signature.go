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
	"strconv"
	"strings"
	"time"
)

// Inbound webhook HTTP header names (§8.1a).
const (
	HeaderSignature = "X-Scion-Signature"
	HeaderDelivery  = "X-Scion-Delivery"
	HeaderEvent     = "X-Scion-Event"
	HeaderSubject   = "X-Scion-Subject"
)

// ReplayWindow is the maximum clock skew / replay tolerance (±300 s) accepted
// for the scion_v1 timestamp parameter (§8.1a).
const ReplayWindow = 300 * time.Second

// maxSignatureV1Candidates bounds the number of v1= tags examined in a single
// X-Scion-Signature header during secret rotation (§8.1a).
const maxSignatureV1Candidates = 8

// maxSignedHeaderBytes caps total X-Scion-* header bytes hashed during
// pre-validation signature verification so an unauthenticated request cannot
// feed unbounded header data into HMAC-SHA256 before returning 401 (§8.1 step 7).
// Valid headers total at most 128 + 64 + 256 = 448 bytes; 16 KiB leaves ample
// room for validly-signed over-length headers to verify and receive 400.
const maxSignedHeaderBytes = 16 * 1024

// computeScionV1MAC computes the raw 32-byte HMAC-SHA256 digest over the
// scion_v1 canonical input without allocating a concatenated body buffer (§8.1a):
//
//	signed = "v1\n" + t + "\n" + delivery + "\n" + event + "\n" + subject + "\n" + body
//
// Per §8.1a rev 8.15, key is the ASCII bytes of the base64url secret string as
// shown once at endpoint creation, NOT base64-decoded bytes.
func computeScionV1MAC(secret, tsRaw, delivery, event, subject string, body []byte) []byte {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte("v1\n"))
	_, _ = mac.Write([]byte(tsRaw))
	_, _ = mac.Write([]byte("\n"))
	_, _ = mac.Write([]byte(delivery))
	_, _ = mac.Write([]byte("\n"))
	_, _ = mac.Write([]byte(event))
	_, _ = mac.Write([]byte("\n"))
	_, _ = mac.Write([]byte(subject))
	_, _ = mac.Write([]byte("\n"))
	_, _ = mac.Write(body)
	return mac.Sum(nil)
}

// SignScionV1 formats a valid X-Scion-Signature header value ("t=<ts>,v1=<hex>")
// for the given secret, unix timestamp (seconds), optional routing headers, and
// raw body (§8.1a).
func SignScionV1(secret string, ts int64, delivery, event, subject string, body []byte) string {
	tsRaw := strconv.FormatInt(ts, 10)
	sum := computeScionV1MAC(secret, tsRaw, delivery, event, subject, body)
	return fmt.Sprintf("t=%s,v1=%s", tsRaw, hex.EncodeToString(sum))
}

// VerifyScionV1Signature verifies an X-Scion-Signature header against secret,
// now (±300 s window), the optional routing headers, and raw body (§8.1 step 6,
// §8.1a). Multiple v1= tags are accepted so future secret rotation works
// seamlessly; any constant-time match passes.
func VerifyScionV1Signature(secret string, now time.Time, sigHeader, delivery, event, subject string, body []byte) bool {
	if secret == "" || sigHeader == "" {
		return false
	}
	if len(delivery)+len(event)+len(subject) > maxSignedHeaderBytes {
		return false
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
			// Ignore unknown future signature tags (e.g. v2=), provided k is a simple token.
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
	windowSec := int64(ReplayWindow / time.Second)
	diff := now.Unix() - ts
	if diff < -windowSec || diff > windowSec {
		return false
	}

	expected := computeScionV1MAC(secret, tsRaw, delivery, event, subject, body)
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
