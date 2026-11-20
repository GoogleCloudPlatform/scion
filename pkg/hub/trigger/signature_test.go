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

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func TestSourceReplayProtected(t *testing.T) {
	cases := []struct {
		name       string
		sourceKind string
		ep         *store.WebhookEndpoint
		want       bool
	}{
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
