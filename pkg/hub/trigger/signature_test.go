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
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func TestVerifyScionV1SignatureWithWindow(t *testing.T) {
	secret := "test-secret-123"
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	body := []byte(`{"ok":true}`)

	// 60s window accepts +/-59s and rejects +/-61s.
	sigWithin := SignScionV1(secret, now.Unix()-59, "d1", "ev", "sub", body)
	if !VerifyScionV1SignatureWithWindow(secret, now, 60*time.Second, sigWithin, "d1", "ev", "sub", body) {
		t.Fatalf("expected 59s-old signature to pass 60s window")
	}
	sigOutside := SignScionV1(secret, now.Unix()-61, "d1", "ev", "sub", body)
	if VerifyScionV1SignatureWithWindow(secret, now, 60*time.Second, sigOutside, "d1", "ev", "sub", body) {
		t.Fatalf("expected 61s-old signature to fail 60s window")
	}

	// 900s window accepts 899s and rejects 901s; >900s is clamped to 900s.
	sig899 := SignScionV1(secret, now.Unix()-899, "d1", "ev", "sub", body)
	if !VerifyScionV1SignatureWithWindow(secret, now, 900*time.Second, sig899, "d1", "ev", "sub", body) {
		t.Fatalf("expected 899s-old signature to pass 900s window")
	}
	sig901 := SignScionV1(secret, now.Unix()-901, "d1", "ev", "sub", body)
	if VerifyScionV1SignatureWithWindow(secret, now, 3600*time.Second, sig901, "d1", "ev", "sub", body) {
		t.Fatalf("expected window > 900s to clamp to 900s and reject 901s-old signature")
	}

	// <30s is clamped to 30s.
	sig25 := SignScionV1(secret, now.Unix()-25, "d1", "ev", "sub", body)
	if !VerifyScionV1SignatureWithWindow(secret, now, 5*time.Second, sig25, "d1", "ev", "sub", body) {
		t.Fatalf("expected window < 30s to clamp to 30s and accept 25s-old signature")
	}
}

func TestSourceReplayProtected(t *testing.T) {
	cases := []struct {
		name       string
		sourceKind string
		ep         *store.WebhookEndpoint
		want       bool
	}{
		{
			name:       "webhook_hub_token_with_scion_v1",
			sourceKind: store.TriggerSourceKindWebhook,
			ep:         &store.WebhookEndpoint{AuthMode: store.WebhookAuthModeHubToken, SignatureMode: store.WebhookSignatureModeScionV1},
			want:       true,
		},
		{
			name:       "webhook_hub_token_with_body_only",
			sourceKind: store.TriggerSourceKindWebhook,
			ep:         &store.WebhookEndpoint{AuthMode: store.WebhookAuthModeHubToken, SignatureMode: store.WebhookSignatureModeBodyOnly},
			want:       true,
		},
		{
			name:       "webhook_scion_v1",
			sourceKind: store.TriggerSourceKindWebhook,
			ep:         &store.WebhookEndpoint{SignatureMode: store.WebhookSignatureModeScionV1},
			want:       true,
		},
		{
			name:       "webhook_stripe_v1",
			sourceKind: store.TriggerSourceKindWebhook,
			ep:         &store.WebhookEndpoint{SignatureMode: "stripe_v1"},
			want:       true,
		},
		{
			name:       "webhook_body_only",
			sourceKind: store.TriggerSourceKindWebhook,
			ep:         &store.WebhookEndpoint{SignatureMode: store.WebhookSignatureModeBodyOnly},
			want:       false,
		},
		{
			name:       "webhook_github_v1",
			sourceKind: store.TriggerSourceKindWebhook,
			ep:         &store.WebhookEndpoint{SignatureMode: "github_v1"},
			want:       false,
		},
		{
			name:       "webhook_empty_mode",
			sourceKind: store.TriggerSourceKindWebhook,
			ep:         &store.WebhookEndpoint{},
			want:       false,
		},
		{
			name:       "webhook_nil_endpoint",
			sourceKind: store.TriggerSourceKindWebhook,
			ep:         nil,
			want:       false,
		},
		{
			name:       "github_app_source",
			sourceKind: store.TriggerSourceKindGitHub,
			ep:         nil,
			want:       true,
		},
		{
			name:       "unknown_source_kind",
			sourceKind: "custom",
			ep:         &store.WebhookEndpoint{SignatureMode: store.WebhookSignatureModeScionV1},
			want:       false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := SourceReplayProtected(tc.sourceKind, tc.ep); got != tc.want {
				t.Errorf("SourceReplayProtected(%q, %+v) = %v, want %v", tc.sourceKind, tc.ep, got, tc.want)
			}
		})
	}
}
