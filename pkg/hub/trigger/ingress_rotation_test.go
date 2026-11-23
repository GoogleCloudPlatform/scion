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
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/durablequeue"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/trigger"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
)

func TestIngress_SecretRotationGraceWindow(t *testing.T) {
	ctx := context.Background()
	cs := entadapter.NewCompositeStore(enttest.NewClient(t))

	const (
		secV1 = "old-secret-v1-value-1234567890"
		secV2 = "new-secret-v2-value-abcdefghij"
	)

	rotatedAt := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	now := rotatedAt.Add(1 * time.Hour)

	projID := uuid.NewString()
	require.NoError(t, cs.CreateProject(ctx, &store.Project{
		ID:   projID,
		Name: "rot-proj",
		Slug: "rot-" + projID[:8],
	}))
	ep := &store.WebhookEndpoint{
		ID:                  uuid.NewString(),
		ProjectID:           projID,
		Name:                "rot-ep",
		OwnerPrincipalKind:  store.DelegationPrincipalUser,
		OwnerPrincipalID:    uuid.NewString(),
		OwnerCredentialKind: store.InitiatorCredentialKindSession,
		SecretVersion:       2,
		SecretRotatedAt:     &rotatedAt,
		SignatureMode:       store.WebhookSignatureModeScionV1,
		Status:              store.WebhookEndpointStatusActive,
	}
	require.NoError(t, cs.CreateWebhookEndpoint(ctx, ep))

	baseQ := durablequeue.NewQueue(cs, durablequeue.WithQueueClock(func() time.Time { return now }))
	failCurrentSecret := false
	ing := trigger.NewIngress(trigger.IngressConfig{
		Store:      cs,
		QueueStore: cs,
		Queue:      baseQ,
		Now:        func() time.Time { return now },
		LoadSecret: func(_ context.Context, _, _ string, version int) (string, error) {
			switch version {
			case 2:
				if failCurrentSecret {
					return "", errors.New("cipher unavailable")
				}
				return secV2, nil
			case 1:
				return secV1, nil
			default:
				return "", store.ErrNotFound
			}
		},
	})

	sendSigned := func(secret, delivery string) *httptest.ResponseRecorder {
		body := []byte(`{"ok":true}`)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/hooks/"+ep.ID, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(trigger.HeaderDelivery, delivery)
		req.Header.Set(trigger.HeaderSignature, trigger.SignScionV1(secret, now.Unix(), delivery, "", "", body))
		rec := httptest.NewRecorder()
		ing.ServeHTTP(rec, req)
		return rec
	}

	// 1. Within 24h grace window: both v2 (new) and v1 (old) are accepted.
	assert.Equal(t, http.StatusAccepted, sendSigned(secV2, "del-v2-in-grace").Code)
	assert.Equal(t, http.StatusAccepted, sendSigned(secV1, "del-v1-in-grace").Code)

	// 2. Wrong secret is still rejected with 401.
	assert.Equal(t, http.StatusUnauthorized, sendSigned("wrong-secret", "del-wrong").Code)

	// 3. If current secret (v2) fails to load, ingress fails closed (503) rather
	//    than silently falling back to v1.
	ing.InvalidateEndpoint(ep.ID)
	failCurrentSecret = true
	assert.Equal(t, http.StatusServiceUnavailable, sendSigned(secV1, "del-fail-closed").Code)
	failCurrentSecret = false

	// 4. After 24h grace window: v2 is accepted, v1 is refused with 401.
	now = rotatedAt.Add(store.WebhookSecretRotationGraceWindow + time.Second)
	assert.Equal(t, http.StatusAccepted, sendSigned(secV2, "del-v2-after-grace").Code)
	assert.Equal(t, http.StatusUnauthorized, sendSigned(secV1, "del-v1-after-grace").Code)
}
