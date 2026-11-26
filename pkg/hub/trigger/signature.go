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

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Inbound webhook HTTP header names (§8.1a).
const (
	HeaderSignature = "X-Scion-Signature"
	HeaderDelivery  = "X-Scion-Delivery"
	HeaderEvent     = "X-Scion-Event"
	HeaderSubject   = "X-Scion-Subject"
)

// ReplayWindow is the default clock skew / replay tolerance (±300 s) accepted
// for the scion_v1 timestamp parameter (§8.1a, Stage 2 §7.3).
const (
	ReplayWindow    = time.Duration(store.DefaultWebhookReplayWindowSeconds) * time.Second
	MinReplayWindow = time.Duration(store.MinWebhookReplayWindowSeconds) * time.Second
	MaxReplayWindow = time.Duration(store.MaxWebhookReplayWindowSeconds) * time.Second
)

// EffectiveReplayWindow returns the bounded replay tolerance for an endpoint's
// configured replay_window_seconds (0 -> 300s default, clamped to 30s..900s).
func EffectiveReplayWindow(seconds int) time.Duration {
	if seconds <= 0 {
		return ReplayWindow
	}
	w := time.Duration(seconds) * time.Second
	if w < MinReplayWindow {
		return MinReplayWindow
	}
	if w > MaxReplayWindow {
		return MaxReplayWindow
	}
	return w
}

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
// now (±300 s default window), the optional routing headers, and raw body
// (§8.1 step 6, §8.1a). Multiple v1= tags are accepted so future secret
// rotation works seamlessly; any constant-time match passes.
func VerifyScionV1Signature(secret string, now time.Time, sigHeader, delivery, event, subject string, body []byte) bool {
	return VerifyScionV1SignatureWithWindow(secret, now, ReplayWindow, sigHeader, delivery, event, subject, body)
}

// VerifyScionV1SignatureWithWindow verifies an X-Scion-Signature header using
// the given replay tolerance window (clamped to MinReplayWindow..MaxReplayWindow;
// zero uses ReplayWindow = 300s, Stage 2 §7.3).
func VerifyScionV1SignatureWithWindow(secret string, now time.Time, window time.Duration, sigHeader, delivery, event, subject string, body []byte) bool {
	if secret == "" || sigHeader == "" {
		return false
	}
	if len(delivery)+len(event)+len(subject) > maxSignedHeaderBytes {
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
	windowSec := int64(window / time.Second)
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

// SourceReplayProtected reports whether a trigger source provides replay
// protection sufficient for agent-creating or command-executing actions
// (Stage 2 design §7.3).
//
// Replay-protected sources:
//   - webhook endpoints with auth_mode = hub_token (authenticated live hub
//     credential over TLS; no captured HMAC signature to replay);
//   - webhook endpoints in scion_v1 or stripe_v1 signature mode;
//   - github App sources (Stage 3 §4.4: per-project X-GitHub-Delivery dedup +
//     signed-body freshness window).
//
// Body-only / github_v1 webhook modes with hmac auth do not authenticate
// timestamps or delivery headers and therefore return false.
func SourceReplayProtected(sourceKind string, ep *store.WebhookEndpoint) bool {
	switch sourceKind {
	case store.TriggerSourceKindGitHub:
		return true
	case store.TriggerSourceKindWebhook:
		if ep == nil {
			return false
		}
		if ep.AuthMode == store.WebhookAuthModeHubToken {
			return true
		}
		switch ep.SignatureMode {
		case store.WebhookSignatureModeScionV1, store.WebhookSignatureModeStripeV1:
			return true
		default:
			return false
		}
	default:
		return false
	}
}

// Closed-set refusal reasons returned by FireReplaySafe.
const (
	FireReplayReasonSourceNotProtected = "source not replay protected"
	FireReplayReasonMissingDeliveryID  = "missing delivery id"
)

// WebhookEventVerificationMode derives the ingress verification mode for a
// webhook event (Stage 2 §7.1, §7.3, §5.8). It prefers a recognized hub-written
// event.attributes["signature_mode"] recorded at ingress ("scion_v1",
// "hub_token", "body_only", "github_v1", "stripe_v1") so a subsequent endpoint
// auth_mode flip cannot rewrite how an already-ingested event was verified,
// while unrecognized/spoofed attribute values fall back to ep.
func WebhookEventVerificationMode(attrs map[string]string, ep *store.WebhookEndpoint) string {
	if attrs != nil {
		switch recorded := strings.TrimSpace(attrs["signature_mode"]); recorded {
		case store.WebhookAuthModeHubToken,
			store.WebhookSignatureModeScionV1,
			store.WebhookSignatureModeBodyOnly,
			store.WebhookSignatureModeGitHubV1,
			store.WebhookSignatureModeStripeV1:
			return recorded
		}
	}
	if ep == nil {
		return ""
	}
	if ep.AuthMode == store.WebhookAuthModeHubToken {
		return store.WebhookAuthModeHubToken
	}
	return strings.TrimSpace(ep.SignatureMode)
}

// FireReplaySafe checks whether a trigger fire event satisfies both source-level
// replay protection (SourceReplayProtected) and per-delivery deduplication
// required by agent-creating or command-executing actions (Stage 2 §7.3,
// P1c-1 security M1, S19).
//
// Returns "" when safe to fire, or a closed-set fire refusal reason:
//   - FireReplayReasonSourceNotProtected when SourceReplayProtected is false OR
//     when a webhook event was ingested under a non-replay-protected mode
//     (e.g. github_v1 or body_only) even if the endpoint was later switched to
//     a replay-protected mode
//   - FireReplayReasonMissingDeliveryID when a webhook delivery not verified
//     as hub_token at ingress omitted a delivery / dedup ID (leaving it
//     replayable within replay_window_seconds)
func FireReplaySafe(sourceKind string, ep *store.WebhookEndpoint, attrs map[string]string) string {
	if !SourceReplayProtected(sourceKind, ep) {
		return FireReplayReasonSourceNotProtected
	}
	if sourceKind == store.TriggerSourceKindWebhook {
		mode := WebhookEventVerificationMode(attrs, ep)
		switch mode {
		case store.WebhookAuthModeHubToken,
			store.WebhookSignatureModeScionV1,
			store.WebhookSignatureModeStripeV1:
			// Replay-protected ingress verification mode.
		default:
			return FireReplayReasonSourceNotProtected
		}
		if mode != store.WebhookAuthModeHubToken && strings.TrimSpace(attrs["delivery"]) == "" {
			return FireReplayReasonMissingDeliveryID
		}
	}
	return ""
}
