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
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func signGitHubFixture(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestGitHubV1Verifier(t *testing.T) {
	const (
		secV1 = "gh-secret-v1"
		secV2 = "gh-secret-v2"
	)
	body := []byte(`{"action":"opened","pull_request":{"number":42},"repository":{"full_name":"acme/widgets"}}`)
	v := &GitHubV1Verifier{}

	t.Run("valid_fixture_verified_and_typed", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/hooks/00000000-0000-0000-0000-000000000001", nil)
		req.Header.Set(HeaderGitHubSignature256, signGitHubFixture(secV1, body))
		req.Header.Set(HeaderGitHubEvent, "pull_request")
		req.Header.Set(HeaderGitHubDelivery, "72d3162e-cc78-11e3-81ab-4c9367dc0958")

		vh, err := v.Verify(req, body, []string{secV1})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if vh.Event != "pull_request" || vh.Delivery != "72d3162e-cc78-11e3-81ab-4c9367dc0958" {
			t.Fatalf("unexpected headers: %+v", vh)
		}
		if vh.SignedEvent || vh.SignedDelivery || vh.SignedSubject {
			t.Fatalf("github_v1 headers must not be marked signed: %+v", vh)
		}
	})

	t.Run("rotation_accepts_previous_secret", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/hooks/00000000-0000-0000-0000-000000000001", nil)
		req.Header.Set(HeaderGitHubSignature256, signGitHubFixture(secV1, body))
		req.Header.Set(HeaderGitHubEvent, "issues")

		vh, err := v.Verify(req, body, []string{secV2, secV1})
		if err != nil {
			t.Fatalf("expected previous secret in slice to pass: %v", err)
		}
		if vh.Event != "issues" {
			t.Fatalf("got event %q, want issues", vh.Event)
		}
	})

	t.Run("bad_signature_returns_ErrInvalidSignature_even_with_bad_headers", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/hooks/00000000-0000-0000-0000-000000000001", nil)
		req.Header.Set(HeaderGitHubSignature256, "sha256=deadbeef")
		req.Header.Set(HeaderGitHubEvent, "BAD UPPERCASE EVENT!")

		_, err := v.Verify(req, body, []string{secV1})
		if !errors.Is(err, ErrInvalidSignature) {
			t.Fatalf("got %v, want ErrInvalidSignature", err)
		}
	})

	t.Run("valid_sig_invalid_event_header_returns_ErrInvalidHeader", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/hooks/00000000-0000-0000-0000-000000000001", nil)
		req.Header.Set(HeaderGitHubSignature256, signGitHubFixture(secV1, body))
		req.Header.Set(HeaderGitHubEvent, "PullRequest")

		_, err := v.Verify(req, body, []string{secV1})
		if !errors.Is(err, ErrInvalidHeader) {
			t.Fatalf("got %v, want ErrInvalidHeader", err)
		}
	})
}

func TestStripeV1Verifier(t *testing.T) {
	const (
		secV1 = "whsec_test_v1"
		secV2 = "whsec_test_v2"
	)
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	body := []byte(`{"id":"evt_123","type":"invoice.payment_succeeded","data":{"object":{"id":"in_456"}}}`)
	v := &StripeV1Verifier{Now: now, ReplayWindow: 300 * time.Second}

	t.Run("valid_fixture", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/hooks/00000000-0000-0000-0000-000000000001", nil)
		req.Header.Set(HeaderStripeSignature, SignStripeV1(secV1, now.Unix()-10, body))

		if _, err := v.Verify(req, body, []string{secV1}); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("stale_timestamp_refused", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/hooks/00000000-0000-0000-0000-000000000001", nil)
		req.Header.Set(HeaderStripeSignature, SignStripeV1(secV1, now.Unix()-301, body))

		_, err := v.Verify(req, body, []string{secV1})
		if !errors.Is(err, ErrInvalidSignature) {
			t.Fatalf("expected ErrInvalidSignature for stale t, got %v", err)
		}
	})

	t.Run("multiple_v1_and_v0_tags_and_rotation", func(t *testing.T) {
		validSig := SignStripeV1(secV1, now.Unix(), body)
		// Append an unknown v0= tag (Stripe test mode) and an extra v1= tag.
		req := httptest.NewRequest(http.MethodPost, "/api/v1/hooks/00000000-0000-0000-0000-000000000001", nil)
		req.Header.Set(HeaderStripeSignature, validSig+",v0=0000,v1="+strings.Repeat("a", 64))

		if _, err := v.Verify(req, body, []string{secV2, secV1}); err != nil {
			t.Fatalf("expected rotation + v0 tag to pass: %v", err)
		}
	})
}

func TestValidateAndResolveRoutingHints(t *testing.T) {
	t.Run("pointer_validation", func(t *testing.T) {
		if err := ValidateRoutingHints(&store.WebhookRoutingHints{
			TypePath:    "/type",
			SubjectPath: "/data/object/id",
			DedupPath:   "/meta/a~1b/c~0d",
		}); err != nil {
			t.Fatalf("valid pointers rejected: %v", err)
		}

		badCases := []*store.WebhookRoutingHints{
			{TypePath: "no-leading-slash"},
			{TypePath: "/" + strings.Repeat("a", MaxRoutingHintPathBytes)},
			{SubjectPath: "/bad~2escape"},
			{DedupPath: "/trailing~"},
			{TypePath: "/has\nnewline"},
		}
		for _, h := range badCases {
			if err := ValidateRoutingHints(h); err == nil {
				t.Errorf("expected error for invalid hints %+v", h)
			}
		}
	})

	t.Run("stripe_defaults_and_non_string_error", func(t *testing.T) {
		body := []byte(`{"id":"evt_1","type":"invoice.payment_succeeded","data":{"object":{"id":"in_9"}}}`)
		eff := EffectiveRoutingHints(store.WebhookSignatureModeStripeV1, nil)
		res, hintErr := ResolveRoutingHints(body, eff, VerifiedHeaders{})
		if hintErr != "" {
			t.Fatalf("unexpected hintErr: %q", hintErr)
		}
		if res.Event != "invoice.payment_succeeded" || res.Delivery != "evt_1" || res.Subject != "in_9" {
			t.Fatalf("unexpected resolved hints: %+v", res)
		}

		// Non-string/non-number typePath ignored with content-free routing_hint_error.
		nonStrBody := []byte(`{"type":{"nested":"obj"},"id":true,"data":{"object":{"id":["arr"]}}}`)
		res2, hintErr2 := ResolveRoutingHints(nonStrBody, eff, VerifiedHeaders{})
		if res2.Event != "" || res2.Delivery != "" || res2.Subject != "" {
			t.Fatalf("expected non-string hints to be ignored, got %+v", res2)
		}
		if hintErr2 == "" || strings.Contains(hintErr2, "nested") {
			t.Fatalf("expected content-free routing_hint_error, got %q", hintErr2)
		}
	})

	t.Run("no_lowercasing_and_signed_header_precedence", func(t *testing.T) {
		body := []byte(`{"type":"PullRequest.Opened","sub":"from-body","deliv":"body-deliv"}`)
		hints := &store.WebhookRoutingHints{
			TypePath:    "/type",
			SubjectPath: "/sub",
			DedupPath:   "/deliv",
		}
		// Signed headers win over body hints.
		res, hintErr := ResolveRoutingHints(body, hints, VerifiedHeaders{
			Event:          "deploy.ok",
			Subject:        "signed-sub",
			Delivery:       "signed-deliv",
			SignedEvent:    true,
			SignedSubject:  true,
			SignedDelivery: true,
		})
		if hintErr != "" {
			t.Fatalf("expected no hintErr when signed headers take precedence, got %q", hintErr)
		}
		if res.Event != "deploy.ok" || res.Subject != "signed-sub" || res.Delivery != "signed-deliv" {
			t.Fatalf("signed headers were overwritten: %+v", res)
		}

		// Without signed headers, uppercase type is NOT lowercased; rejected with routing_hint_error.
		res2, hintErr2 := ResolveRoutingHints(body, hints, VerifiedHeaders{})
		if res2.Event != "" {
			t.Fatalf("uppercase type must be ignored, got %q", res2.Event)
		}
		if !strings.Contains(hintErr2, "typePath") {
			t.Fatalf("expected typePath error, got %q", hintErr2)
		}
		if res2.Subject != "from-body" || res2.Delivery != "body-deliv" {
			t.Fatalf("expected valid subject/delivery to still resolve: %+v", res2)
		}
	})

	t.Run("overflow_body_only_scans_first_64KiB", func(t *testing.T) {
		// Place "early" before 64 KiB and "late" after 64 KiB of padding.
		padding := strings.Repeat("x", MaxPayloadBytes+1024)
		body := []byte(`{"early":"ok.event","pad":"` + padding + `","late":"late.event","late_sub":"sub_after_64k"}`)

		hints := &store.WebhookRoutingHints{
			TypePath:    "/early",
			SubjectPath: "/late_sub",
			DedupPath:   "/late",
		}
		res, hintErr := ResolveRoutingHints(body, hints, VerifiedHeaders{})
		if hintErr != "" {
			t.Fatalf("unexpected hintErr on overflow body: %q", hintErr)
		}
		if res.Event != "ok.event" {
			t.Fatalf("expected early hint within 64 KiB to resolve, got %q", res.Event)
		}
		if res.Subject != "" || res.Delivery != "" {
			t.Fatalf("hints beyond 64 KiB MUST NOT resolve on overflow body, got %+v", res)
		}
	})
}
