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

package hubclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWebhookEndpointService(t *testing.T) {
	now := time.Date(2026, 11, 16, 5, 0, 0, 0, time.UTC)

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/projects/proj-1/webhook-endpoints", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			var req CreateWebhookEndpointRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
			assert.Equal(t, "ci-hook", req.Name)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(WebhookEndpointCreated{
				WebhookEndpoint: WebhookEndpoint{
					ID:            "ep-123",
					ProjectID:     "proj-1",
					Name:          req.Name,
					Description:   req.Description,
					URL:           "https://hub.example.com/api/v1/hooks/ep-123",
					SignatureMode: "scion_v1",
					SecretVersion: 1,
					Status:        "active",
					CreatedAt:     now,
					UpdatedAt:     now,
				},
				Secret: "one-time-secret",
			})
		case http.MethodGet:
			assert.Equal(t, "active", r.URL.Query().Get("status"))
			assert.Equal(t, "10", r.URL.Query().Get("limit"))
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(ListWebhookEndpointsResponse{
				Scope: TriggerListScopeCaller,
				Endpoints: []WebhookEndpoint{{
					ID:            "ep-123",
					ProjectID:     "proj-1",
					Name:          "ci-hook",
					URL:           "https://hub.example.com/api/v1/hooks/ep-123",
					SignatureMode: "scion_v1",
					SecretVersion: 1,
					Status:        "active",
					CreatedAt:     now,
					UpdatedAt:     now,
				}},
				TotalCount: 1,
				ServerTime: now,
			})
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
	mux.HandleFunc("/api/v1/projects/proj-1/webhook-endpoints/ep-123", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(WebhookEndpoint{
				ID:            "ep-123",
				ProjectID:     "proj-1",
				Name:          "ci-hook",
				URL:           "https://hub.example.com/api/v1/hooks/ep-123",
				SignatureMode: "scion_v1",
				SecretVersion: 1,
				Status:        "active",
				CreatedAt:     now,
				UpdatedAt:     now,
			})
		case http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	c, err := New(server.URL)
	require.NoError(t, err)
	ctx := context.Background()
	svc := c.WebhookEndpoints("proj-1")

	created, err := svc.Create(ctx, &CreateWebhookEndpointRequest{Name: "ci-hook", Description: "CI deploys"})
	require.NoError(t, err)
	assert.Equal(t, "ep-123", created.ID)
	assert.Equal(t, "one-time-secret", created.Secret)

	got, err := svc.Get(ctx, "ep-123")
	require.NoError(t, err)
	assert.Equal(t, "ci-hook", got.Name)

	listed, err := svc.List(ctx, &ListWebhookEndpointsOptions{
		Status: "active",
		Page:   apiclient.PageOptions{Limit: 10},
	})
	require.NoError(t, err)
	assert.Equal(t, TriggerListScopeCaller, listed.Scope)
	require.Len(t, listed.Endpoints, 1)

	require.NoError(t, svc.Delete(ctx, "ep-123"))
}

func TestTriggerService(t *testing.T) {
	now := time.Date(2026, 11, 16, 5, 0, 0, 0, time.UTC)

	sampleTrigger := func(status string, rev int) Trigger {
		return Trigger{
			ID:        "tr-456",
			ProjectID: "proj-1",
			Name:      "deploy-notify",
			Source:    TriggerSourceSpec{Kind: "webhook", ID: "ep-123"},
			Match:     TriggerMatchSpec{Types: []string{"webhook.received"}, Dialect: "cel"},
			Action:    TriggerActionSpec{Type: "message", AgentName: "lead", Message: "deployed", Wake: true},
			Subject:   TriggerSubjectSpec{Dialect: "cel", Mode: "new"},
			Status:    status,

			AuthorizationRevision: rev,
			CreatedAt:             now,
			UpdatedAt:             now,
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/projects/proj-1/triggers", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			var req CreateTriggerRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
			assert.Equal(t, "deploy-notify", req.Name)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(sampleTrigger("active", 1))
		case http.MethodGet:
			assert.Equal(t, "webhook", r.URL.Query().Get("source"))
			assert.Equal(t, "ep-123", r.URL.Query().Get("sourceId"))
			assert.Equal(t, "active", r.URL.Query().Get("status"))
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(ListTriggersResponse{
				Triggers:   []Trigger{sampleTrigger("active", 1)},
				TotalCount: 1,
				ServerTime: now,
			})
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
	mux.HandleFunc("/api/v1/projects/proj-1/triggers/tr-456", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(sampleTrigger("active", 1))
		case http.MethodPatch:
			assert.Equal(t, `"1"`, r.Header.Get("If-Match"))
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(sampleTrigger("active", 2))
		case http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
	mux.HandleFunc("/api/v1/projects/proj-1/triggers/tr-456/pause", func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(sampleTrigger("paused", 2))
	})
	mux.HandleFunc("/api/v1/projects/proj-1/triggers/tr-456/resume", func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(sampleTrigger("active", 3))
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	c, err := New(server.URL)
	require.NoError(t, err)
	ctx := context.Background()
	svc := c.Triggers("proj-1")

	created, err := svc.Create(ctx, &CreateTriggerRequest{
		Name:   "deploy-notify",
		Source: TriggerSourceSpec{Kind: "webhook", ID: "ep-123"},
		Action: TriggerActionSpec{Type: "message", AgentName: "lead", Message: "deployed", Wake: true},
	})
	require.NoError(t, err)
	assert.Equal(t, "tr-456", created.ID)

	got, err := svc.Get(ctx, "tr-456")
	require.NoError(t, err)
	assert.Equal(t, 1, got.AuthorizationRevision)

	listed, err := svc.List(ctx, &ListTriggersOptions{
		Source:   "webhook",
		SourceID: "ep-123",
		Status:   "active",
	})
	require.NoError(t, err)
	require.Len(t, listed.Triggers, 1)

	desc := "updated"
	updated, err := svc.Update(ctx, "tr-456", &UpdateTriggerRequest{
		Description: &desc,
		IfMatch:     `"1"`,
	})
	require.NoError(t, err)
	assert.Equal(t, 2, updated.AuthorizationRevision)

	paused, err := svc.Pause(ctx, "tr-456")
	require.NoError(t, err)
	assert.Equal(t, "paused", paused.Status)

	resumed, err := svc.Resume(ctx, "tr-456")
	require.NoError(t, err)
	assert.Equal(t, "active", resumed.Status)
	assert.Equal(t, 3, resumed.AuthorizationRevision)

	require.NoError(t, svc.Delete(ctx, "tr-456"))
}
