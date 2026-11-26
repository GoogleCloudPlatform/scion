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

// TestIngress_O1_MissingOrUnparseableSignature_401BeforeSecretLoad verifies O1:
// for every HMAC mode (scion_v1, github_v1, stripe_v1), missing, duplicated, or
// syntactically unparseable signature headers return 401 BEFORE any secret load
// or rotation-grace fallback, so unsigned callers never receive 503.
func TestIngress_O1_MissingOrUnparseableSignature_401BeforeSecretLoad(t *testing.T) {
	ctx := context.Background()
	cs := entadapter.NewCompositeStore(enttest.NewClient(t))
	now := time.Date(2026, 11, 26, 11, 0, 0, 0, time.UTC)
	rotatedAt := now.Add(-30 * time.Minute)

	projID := uuid.NewString()
	require.NoError(t, cs.CreateProject(ctx, &store.Project{
		ID:   projID,
		Name: "o1-proj",
		Slug: "o1-" + projID[:8],
	}))

	modes := []struct {
		mode      string
		sigHeader string
		badValues [][]string
	}{
		{
			mode:      store.WebhookSignatureModeScionV1,
			sigHeader: trigger.HeaderSignature,
			badValues: [][]string{
				nil,                                      // missing
				{""},                                     // empty
				{"not-a-signature"},                      // unparseable
				{"t=123,v1=short"},                       // short v1
				{"t=0123,v1=" + strings.Repeat("a", 64)}, // non-canonical t
				{"t=1,v1=" + strings.Repeat("a", 64), "t=2,v1=" + strings.Repeat("b", 64)}, // duplicate header
			},
		},
		{
			mode:      store.WebhookSignatureModeGitHubV1,
			sigHeader: trigger.HeaderGitHubSignature256,
			badValues: [][]string{
				nil,
				{""},
				{"sha256="},
				{"sha1=" + strings.Repeat("a", 64)},
				{"sha256=" + strings.Repeat("A", 64)}, // uppercase hex
				{"sha256=" + strings.Repeat("a", 64), "sha256=" + strings.Repeat("b", 64)},
			},
		},
		{
			mode:      store.WebhookSignatureModeStripeV1,
			sigHeader: trigger.HeaderStripeSignature,
			badValues: [][]string{
				nil,
				{""},
				{"v1=" + strings.Repeat("a", 64)}, // missing t
				{"t=123"},                         // missing v1
				{"t=123,t=123,v1=" + strings.Repeat("a", 64)}, // duplicate t
				{"t=123,v1=" + strings.Repeat("a", 64), "t=123,v1=" + strings.Repeat("b", 64)},
			},
		},
	}

	baseQ := durablequeue.NewQueue(cs, durablequeue.WithQueueClock(func() time.Time { return now }))
	secretLoadCalls := 0
	ing := trigger.NewIngress(trigger.IngressConfig{
		Store:      cs,
		QueueStore: cs,
		Queue:      baseQ,
		Now:        func() time.Time { return now },
		LoadSecret: func(_ context.Context, _, _ string, _ int) (string, error) {
			secretLoadCalls++
			return "", errors.New("transient secret backend outage")
		},
	})

	for _, tc := range modes {
		t.Run(tc.mode, func(t *testing.T) {
			ep := &store.WebhookEndpoint{
				ID:                  uuid.NewString(),
				ProjectID:           projID,
				Name:                "o1-" + tc.mode,
				OwnerPrincipalKind:  store.DelegationPrincipalUser,
				OwnerPrincipalID:    uuid.NewString(),
				OwnerCredentialKind: store.InitiatorCredentialKindSession,
				SignatureMode:       tc.mode,
				SecretVersion:       2,
				SecretRotatedAt:     &rotatedAt,
				Status:              store.WebhookEndpointStatusActive,
			}
			require.NoError(t, cs.CreateWebhookEndpoint(ctx, ep))

			for _, vals := range tc.badValues {
				secretLoadCalls = 0
				req := httptest.NewRequest(http.MethodPost, "/api/v1/hooks/"+ep.ID, bytes.NewReader([]byte(`{"ok":true}`)))
				req.Header.Set("Content-Type", "application/json")
				for _, v := range vals {
					req.Header.Add(tc.sigHeader, v)
				}
				rec := httptest.NewRecorder()
				ing.ServeHTTP(rec, req)
				assert.Equal(t, http.StatusUnauthorized, rec.Code, "mode=%s hdr=%v body=%s", tc.mode, vals, rec.Body.String())
				assert.Equal(t, 0, secretLoadCalls, "mode=%s hdr=%v must not load secrets", tc.mode, vals)
			}
		})
	}
}

// TestIngress_O2_NonScionModesIgnoreXScionHeaders verifies O2: github_v1 and
// stripe_v1 ignore X-Scion-Event, X-Scion-Subject, and X-Scion-Delivery
// completely (neither routing nor storing them in trigger_events.headers).
func TestIngress_O2_NonScionModesIgnoreXScionHeaders(t *testing.T) {
	ctx := context.Background()
	cs := entadapter.NewCompositeStore(enttest.NewClient(t))
	now := time.Date(2026, 11, 26, 11, 30, 0, 0, time.UTC)

	projID := uuid.NewString()
	require.NoError(t, cs.CreateProject(ctx, &store.Project{
		ID:   projID,
		Name: "o2-proj",
		Slug: "o2-" + projID[:8],
	}))

	const secret = "o2-shared-secret"
	ghEP := &store.WebhookEndpoint{
		ID:                  uuid.NewString(),
		ProjectID:           projID,
		Name:                "o2-gh",
		OwnerPrincipalKind:  store.DelegationPrincipalUser,
		OwnerPrincipalID:    uuid.NewString(),
		OwnerCredentialKind: store.InitiatorCredentialKindSession,
		SignatureMode:       store.WebhookSignatureModeGitHubV1,
		Status:              store.WebhookEndpointStatusActive,
	}
	require.NoError(t, cs.CreateWebhookEndpoint(ctx, ghEP))

	stripeEP := &store.WebhookEndpoint{
		ID:                  uuid.NewString(),
		ProjectID:           projID,
		Name:                "o2-stripe",
		OwnerPrincipalKind:  store.DelegationPrincipalUser,
		OwnerPrincipalID:    uuid.NewString(),
		OwnerCredentialKind: store.InitiatorCredentialKindSession,
		SignatureMode:       store.WebhookSignatureModeStripeV1,
		Status:              store.WebhookEndpointStatusActive,
	}
	require.NoError(t, cs.CreateWebhookEndpoint(ctx, stripeEP))

	baseQ := durablequeue.NewQueue(cs, durablequeue.WithQueueClock(func() time.Time { return now }))
	ing := trigger.NewIngress(trigger.IngressConfig{
		Store:      cs,
		QueueStore: cs,
		Queue:      baseQ,
		Now:        func() time.Time { return now },
		LoadSecret: func(_ context.Context, _, _ string, _ int) (string, error) {
			return secret, nil
		},
	})

	// 1. github_v1 ignores X-Scion-* headers in both routing and stored headers.
	ghBody := []byte(`{"action":"opened"}`)
	ghReq := httptest.NewRequest(http.MethodPost, "/api/v1/hooks/"+ghEP.ID, bytes.NewReader(ghBody))
	ghReq.Header.Set("Content-Type", "application/json")
	ghReq.Header.Set(trigger.HeaderGitHubSignature256, signGitHubTestBody(secret, ghBody))
	ghReq.Header.Set(trigger.HeaderGitHubEvent, "issues")
	ghReq.Header.Set(trigger.HeaderGitHubDelivery, "gh-real-deliv")
	ghReq.Header.Set(trigger.HeaderEvent, "forged.event")
	ghReq.Header.Set(trigger.HeaderSubject, "forged/subject")
	ghReq.Header.Set(trigger.HeaderDelivery, "forged-deliv")
	ghRec := httptest.NewRecorder()
	ing.ServeHTTP(ghRec, ghReq)
	require.Equal(t, http.StatusAccepted, ghRec.Code, ghRec.Body.String())

	var ghAcc trigger.IngressAcceptedResponse
	require.NoError(t, json.Unmarshal(ghRec.Body.Bytes(), &ghAcc))
	ghEv, err := cs.GetTriggerEvent(ctx, ghAcc.EventID)
	require.NoError(t, err)
	assert.Equal(t, "webhook.issues", ghEv.Type)
	assert.Equal(t, "webhook:"+ghEP.ID, ghEv.Subject)
	assert.Equal(t, "gh-real-deliv", ghEv.DedupKey)
	assert.NotContains(t, ghEv.Headers, trigger.HeaderEvent)
	assert.NotContains(t, ghEv.Headers, trigger.HeaderSubject)
	assert.NotContains(t, ghEv.Headers, trigger.HeaderDelivery)
	assert.Equal(t, "issues", ghEv.Headers[trigger.HeaderGitHubEvent])
	assert.Equal(t, "gh-real-deliv", ghEv.Headers[trigger.HeaderGitHubDelivery])

	// 2. stripe_v1 ignores both X-Scion-* and X-GitHub-* headers.
	stBody := []byte(`{"id":"evt_real","type":"invoice.paid","data":{"object":{"id":"in_real"}}}`)
	stReq := httptest.NewRequest(http.MethodPost, "/api/v1/hooks/"+stripeEP.ID, bytes.NewReader(stBody))
	stReq.Header.Set("Content-Type", "application/json")
	stReq.Header.Set(trigger.HeaderStripeSignature, trigger.SignStripeV1(secret, now.Unix(), stBody))
	stReq.Header.Set(trigger.HeaderEvent, "forged.event")
	stReq.Header.Set(trigger.HeaderSubject, "forged/subject")
	stReq.Header.Set(trigger.HeaderDelivery, "forged-deliv")
	stReq.Header.Set(trigger.HeaderGitHubEvent, "forged_gh")
	stReq.Header.Set(trigger.HeaderGitHubDelivery, "forged-gh-deliv")
	stRec := httptest.NewRecorder()
	ing.ServeHTTP(stRec, stReq)
	require.Equal(t, http.StatusAccepted, stRec.Code, stRec.Body.String())

	var stAcc trigger.IngressAcceptedResponse
	require.NoError(t, json.Unmarshal(stRec.Body.Bytes(), &stAcc))
	stEv, err := cs.GetTriggerEvent(ctx, stAcc.EventID)
	require.NoError(t, err)
	assert.Equal(t, "webhook.invoice.paid", stEv.Type)
	assert.Equal(t, "webhook:"+stripeEP.ID+"/in_real", stEv.Subject)
	assert.Equal(t, "evt_real", stEv.DedupKey)
	assert.NotContains(t, stEv.Headers, trigger.HeaderEvent)
	assert.NotContains(t, stEv.Headers, trigger.HeaderSubject)
	assert.NotContains(t, stEv.Headers, trigger.HeaderDelivery)
	assert.NotContains(t, stEv.Headers, trigger.HeaderGitHubEvent)
	assert.NotContains(t, stEv.Headers, trigger.HeaderGitHubDelivery)
}
