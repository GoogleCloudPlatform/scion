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

//go:build !no_sqlite

package trigger_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/durablequeue"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/trigger"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
)

func signGitHubTestBody(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestIngress_GitHubV1_StripeV1_And_RoutingHints(t *testing.T) {
	ctx := context.Background()
	cs := entadapter.NewCompositeStore(enttest.NewClient(t))
	stor, err := storage.NewLocal(storage.Config{
		Provider:  storage.ProviderLocal,
		Bucket:    "test-bucket",
		LocalPath: t.TempDir(),
	})
	require.NoError(t, err)

	now := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	projID := uuid.NewString()
	require.NoError(t, cs.CreateProject(ctx, &store.Project{
		ID:   projID,
		Name: "p4b-proj",
		Slug: "p4b-" + projID[:8],
	}))

	const (
		secV1 = "provider-secret-v1"
		secV2 = "provider-secret-v2"
	)
	rotatedAt := now.Add(-1 * time.Hour)

	ghEP := &store.WebhookEndpoint{
		ID:                  uuid.NewString(),
		ProjectID:           projID,
		Name:                "gh-ep",
		OwnerPrincipalKind:  store.DelegationPrincipalUser,
		OwnerPrincipalID:    uuid.NewString(),
		OwnerCredentialKind: store.InitiatorCredentialKindSession,
		SignatureMode:       store.WebhookSignatureModeGitHubV1,
		RoutingHints:        &store.WebhookRoutingHints{SubjectPath: "/repository/full_name"},
		SecretVersion:       2,
		SecretRotatedAt:     &rotatedAt,
		Status:              store.WebhookEndpointStatusActive,
	}
	require.NoError(t, cs.CreateWebhookEndpoint(ctx, ghEP))

	stripeEP := &store.WebhookEndpoint{
		ID:                  uuid.NewString(),
		ProjectID:           projID,
		Name:                "stripe-ep",
		OwnerPrincipalKind:  store.DelegationPrincipalUser,
		OwnerPrincipalID:    uuid.NewString(),
		OwnerCredentialKind: store.InitiatorCredentialKindSession,
		SignatureMode:       store.WebhookSignatureModeStripeV1,
		MaxBodyBytes:        trigger.MaxEndpointBodyBytes,
		ReplayWindowSeconds: 120,
		SecretVersion:       2,
		SecretRotatedAt:     &rotatedAt,
		Status:              store.WebhookEndpointStatusActive,
	}
	require.NoError(t, cs.CreateWebhookEndpoint(ctx, stripeEP))

	baseQ := durablequeue.NewQueue(cs, durablequeue.WithQueueClock(func() time.Time { return now }))
	var prevErr error
	ing := trigger.NewIngress(trigger.IngressConfig{
		Store:      cs,
		QueueStore: cs,
		Queue:      baseQ,
		Storage:    func() storage.Storage { return stor },
		Now:        func() time.Time { return now },
		LoadSecret: func(_ context.Context, _, _ string, version int) (string, error) {
			switch version {
			case 2:
				return secV2, nil
			case 1:
				if prevErr != nil {
					return "", prevErr
				}
				return secV1, nil
			default:
				return "", store.ErrNotFound
			}
		},
	})

	// 1. GitHub fixture verified, typed, subject lifted via hint, deduped on X-GitHub-Delivery.
	ghBody := []byte(`{"action":"opened","repository":{"full_name":"acme/app"}}`)
	sendGH := func(secret, evName, delivery string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/hooks/"+ghEP.ID, bytes.NewReader(ghBody))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(trigger.HeaderGitHubSignature256, signGitHubTestBody(secret, ghBody))
		req.Header.Set(trigger.HeaderGitHubEvent, evName)
		req.Header.Set(trigger.HeaderGitHubDelivery, delivery)
		rec := httptest.NewRecorder()
		ing.ServeHTTP(rec, req)
		return rec
	}

	rec := sendGH(secV2, "pull_request", "gh-deliv-1")
	require.Equal(t, http.StatusAccepted, rec.Code)
	var acc trigger.IngressAcceptedResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &acc))
	assert.Equal(t, trigger.IngressStatusQueued, acc.Status)

	ev, err := cs.GetTriggerEvent(ctx, acc.EventID)
	require.NoError(t, err)
	assert.Equal(t, "webhook.pull_request", ev.Type)
	assert.Equal(t, "webhook:"+ghEP.ID+"/acme/app", ev.Subject)
	assert.Equal(t, "gh-deliv-1", ev.DedupKey)
	assert.Equal(t, store.WebhookSignatureModeGitHubV1, ev.Attributes["signature_mode"])

	// Duplicate X-GitHub-Delivery returns 202 duplicate.
	dupRec := sendGH(secV1, "pull_request", "gh-deliv-1") // also exercises v1 rotation grace!
	require.Equal(t, http.StatusAccepted, dupRec.Code)
	var dupAcc trigger.IngressAcceptedResponse
	require.NoError(t, json.Unmarshal(dupRec.Body.Bytes(), &dupAcc))
	assert.Equal(t, trigger.IngressStatusDuplicate, dupAcc.Status)
	assert.Equal(t, acc.EventID, dupAcc.EventID)

	// 2. Stripe fixture verified with default hints; stale t refused; rotation grace works.
	stripeBody := []byte(`{"id":"evt_stripe_1","type":"invoice.payment_succeeded","data":{"object":{"id":"in_999"}}}`)
	sendStripe := func(secret string, ts int64, body []byte) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/hooks/"+stripeEP.ID, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(trigger.HeaderStripeSignature, trigger.SignStripeV1(secret, ts, body))
		rec := httptest.NewRecorder()
		ing.ServeHTTP(rec, req)
		return rec
	}

	// Stale timestamp (> 120s replay_window_seconds) refused with 401.
	assert.Equal(t, http.StatusUnauthorized, sendStripe(secV2, now.Unix()-121, stripeBody).Code)

	// Valid delivery accepted with previous secret in rotation grace.
	sRec := sendStripe(secV1, now.Unix()-30, stripeBody)
	require.Equal(t, http.StatusAccepted, sRec.Code)
	var sAcc trigger.IngressAcceptedResponse
	require.NoError(t, json.Unmarshal(sRec.Body.Bytes(), &sAcc))
	assert.Equal(t, trigger.IngressStatusQueued, sAcc.Status)

	sev, err := cs.GetTriggerEvent(ctx, sAcc.EventID)
	require.NoError(t, err)
	assert.Equal(t, "webhook.invoice.payment_succeeded", sev.Type)
	assert.Equal(t, "webhook:"+stripeEP.ID+"/in_999", sev.Subject)
	assert.Equal(t, "evt_stripe_1", sev.DedupKey)
	assert.Equal(t, store.WebhookSignatureModeStripeV1, sev.Attributes["signature_mode"])

	// Transient prev-secret load error returns 503 during grace.
	ing.InvalidateEndpoint(stripeEP.ID)
	prevErr = errors.New("kms timeout")
	assert.Equal(t, http.StatusServiceUnavailable, sendStripe(secV1, now.Unix(), stripeBody).Code)
	prevErr = nil
	ing.InvalidateEndpoint(stripeEP.ID)

	// 3. Non-string hint recorded in routing_hint_error and ignored.
	badHintBody := []byte(`{"id":"evt_bad_hint","type":{"bad":true},"data":{"object":{"id":"in_ok"}}}`)
	bhRec := sendStripe(secV2, now.Unix(), badHintBody)
	require.Equal(t, http.StatusAccepted, bhRec.Code)
	var bhAcc trigger.IngressAcceptedResponse
	require.NoError(t, json.Unmarshal(bhRec.Body.Bytes(), &bhAcc))
	bhev, err := cs.GetTriggerEvent(ctx, bhAcc.EventID)
	require.NoError(t, err)
	assert.Equal(t, "webhook.received", bhev.Type)
	assert.NotEmpty(t, bhev.Attributes["routing_hint_error"])
	assert.Equal(t, "webhook:"+stripeEP.ID+"/in_ok", bhev.Subject)

	// 4. Overflow body (> 64 KiB): hint beyond 64 KiB is NOT resolved.
	pad := strings.Repeat("a", trigger.MaxPayloadBytes+2048)
	overflowBody := []byte(`{"id":"evt_overflow_1","pad":"` + pad + `","type":"invoice.paid","data":{"object":{"id":"in_late"}}}`)
	ovRec := sendStripe(secV2, now.Unix(), overflowBody)
	require.Equal(t, http.StatusAccepted, ovRec.Code)
	var ovAcc trigger.IngressAcceptedResponse
	require.NoError(t, json.Unmarshal(ovRec.Body.Bytes(), &ovAcc))
	ovev, err := cs.GetTriggerEvent(ctx, ovAcc.EventID)
	require.NoError(t, err)
	assert.NotEmpty(t, ovev.PayloadRef)
	assert.Equal(t, "evt_overflow_1", ovev.DedupKey)       // within first 64 KiB -> resolved
	assert.Equal(t, "webhook.received", ovev.Type)         // beyond 64 KiB -> NOT resolved
	assert.Equal(t, "webhook:"+stripeEP.ID, ovev.Subject)  // beyond 64 KiB -> NOT resolved
	assert.Empty(t, ovev.Attributes["routing_hint_error"]) // beyond 64 KiB is silently ignored
}
