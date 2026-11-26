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
	"io"
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
			out := WebhookEndpointCreated{
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
			}
			if req.DefaultTrigger != nil {
				out.TriggerID = "tr-default-123"
				out.Trigger = &Trigger{
					ID:                    "tr-default-123",
					ProjectID:             "proj-1",
					Name:                  req.Name,
					Source:                TriggerSourceSpec{Kind: "webhook", ID: "ep-123"},
					Match:                 TriggerMatchSpec{Types: []string{"webhook.received"}, Rules: []TriggerMatchRuleSpec{}},
					Action:                TriggerActionSpec{Type: "message", AgentName: req.DefaultTrigger.AgentName, Message: req.DefaultTrigger.Message, Interrupt: req.DefaultTrigger.Interrupt, Wake: req.DefaultTrigger.Wake},
					Subject:               TriggerSubjectSpec{Dialect: "cel", Mode: "new"},
					Status:                "active",
					AuthorizationRevision: 1,
					CreatedAt:             now,
					UpdatedAt:             now,
				}
			}
			_ = json.NewEncoder(w).Encode(out)
		case http.MethodGet:
			assert.Equal(t, "ci-hook", r.URL.Query().Get("name"))
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
	assert.Empty(t, created.TriggerID)
	assert.Nil(t, created.Trigger)

	createdWithDefault, err := svc.Create(ctx, &CreateWebhookEndpointRequest{
		Name:        "ci-hook",
		Description: "CI deploys with default trigger",
		DefaultTrigger: &WebhookEndpointDefaultTriggerSpec{
			AgentName: "lead",
			Message:   "Handle webhook",
			Interrupt: true,
			Wake:      true,
		},
	})
	require.NoError(t, err)
	assert.Equal(t, "ep-123", createdWithDefault.ID)
	assert.Equal(t, "tr-default-123", createdWithDefault.TriggerID)
	require.NotNil(t, createdWithDefault.Trigger)
	assert.Equal(t, "tr-default-123", createdWithDefault.Trigger.ID)
	assert.Equal(t, "Handle webhook", createdWithDefault.Trigger.Action.Message)

	got, err := svc.Get(ctx, "ep-123")
	require.NoError(t, err)
	assert.Equal(t, "ci-hook", got.Name)

	listed, err := svc.List(ctx, &ListWebhookEndpointsOptions{
		Name:   "ci-hook",
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
			assert.Equal(t, "deploy-notify", r.URL.Query().Get("name"))
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
	mux.HandleFunc("/api/v1/projects/proj-1/triggers/tr-456/history", func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "5", r.URL.Query().Get("limit"))
		assert.Equal(t, "cur-1", r.URL.Query().Get("cursor"))
		w.Header().Set("Content-Type", "application/json")
		finished := now.Add(time.Second)
		_ = json.NewEncoder(w).Encode(ListTriggerFiresResponse{
			Fires: []TriggerFire{
				{
					ID:         "fire-1",
					TriggerID:  "tr-456",
					EventID:    "evt-1",
					ProjectID:  "proj-1",
					Subject:    "webhook:ep-123",
					Status:     "fired",
					StartedAt:  now,
					FinishedAt: &finished,
				},
			},
			NextCursor: "cur-2",
			TotalCount: 2,
			ServerTime: now,
		})
	})
	mux.HandleFunc("/api/v1/projects/proj-1/triggers/tr-456/render", func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "evt-1", r.URL.Query().Get("eventId"))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(TriggerRenderResponse{
			Slots: TriggerRenderSlots{
				Task: &TriggerRenderSlotResult{
					Rendered: "Fix PR #42",
					Used: []TriggerRenderUsedVar{
						{Path: "event.attributes.number", Class: "untrusted"},
					},
				},
				Name: &TriggerRenderSlotResult{
					Rendered: "pr-42",
					Used:     []TriggerRenderUsedVar{{Path: "event.attributes.number", Class: "untrusted"}},
				},
			},
			Errors: []TriggerRenderError{},
		})
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
		Name:     "deploy-notify",
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

	hist, err := svc.History(ctx, "tr-456", &ListTriggerFiresOptions{
		Page: apiclient.PageOptions{Limit: 5, Cursor: "cur-1"},
	})
	require.NoError(t, err)
	assert.Equal(t, 2, hist.TotalCount)
	assert.Equal(t, "cur-2", hist.NextCursor)
	require.Len(t, hist.Fires, 1)
	assert.Equal(t, "fired", hist.Fires[0].Status)

	rendered, err := svc.Render(ctx, "tr-456", "evt-1")
	require.NoError(t, err)
	require.NotNil(t, rendered.Slots.Task)
	assert.Equal(t, "Fix PR #42", rendered.Slots.Task.Rendered)
	require.Len(t, rendered.Slots.Task.Used, 1)
	assert.Equal(t, "event.attributes.number", rendered.Slots.Task.Used[0].Path)
	assert.Equal(t, "untrusted", rendered.Slots.Task.Used[0].Class)
	require.NotNil(t, rendered.Slots.Name)
	assert.Equal(t, "pr-42", rendered.Slots.Name.Rendered)
	assert.Empty(t, rendered.Errors)

	require.NoError(t, svc.Delete(ctx, "tr-456"))
}

func TestTriggerEventService(t *testing.T) {
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	since := now.Add(-time.Hour)
	processed := now.Add(2 * time.Second)

	summary := TriggerEventSummary{
		ID:         "evt-789",
		ProjectID:  "proj-1",
		Source:     TriggerSourceSpec{Kind: "github", ID: "777001"},
		Type:       "github.issue_comment.created",
		Subject:    "github:acme/widgets#42",
		DedupKey:   "github:550e8400-e29b-41d4-a716-446655440000",
		OccurredAt: now,
		ReceivedAt: now,
		Attributes: map[string]string{
			"command": "ping",
			"repo":    "acme/widgets",
		},
		Lists: map[string][]string{
			"labels":    {"bug", "triggers"},
			"assignees": {},
		},
		PayloadBytes: 128,
		Status:       "processed",
		MatchCount:   1,
		FireCount:    1,
		ProcessedAt:  &processed,
		CreatedAt:    now,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/projects/proj-1/trigger-events", func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "github", r.URL.Query().Get("source"))
		assert.Equal(t, "777001", r.URL.Query().Get("sourceId"))
		assert.Equal(t, "processed", r.URL.Query().Get("status"))
		assert.Equal(t, since.UTC().Format(time.RFC3339Nano), r.URL.Query().Get("since"))
		assert.Equal(t, "10", r.URL.Query().Get("limit"))
		assert.Equal(t, "cur-0", r.URL.Query().Get("cursor"))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ListTriggerEventsResponse{
			Scope:      TriggerListScopeCaller,
			Events:     []TriggerEventSummary{summary},
			NextCursor: "cur-1",
			TotalCount: 1,
			ServerTime: now,
		})
	})
	mux.HandleFunc("/api/v1/projects/proj-1/trigger-events/evt-789", func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodGet, r.Method)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("payload") == "1" {
			_, _ = w.Write([]byte(`{"action":"created","overflow":true}`))
			return
		}
		_ = json.NewEncoder(w).Encode(TriggerEventDetail{
			TriggerEventSummary: summary,
			Headers:             map[string]string{"x-github-event": "issue_comment"},
			Payload:             json.RawMessage(`{"action":"created","comment":{"body":"@scion /ping"}}`),
		})
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	c, err := New(server.URL)
	require.NoError(t, err)
	ctx := context.Background()
	svc := c.TriggerEvents("proj-1")

	listed, err := svc.List(ctx, &ListTriggerEventsOptions{
		Source:   "github",
		SourceID: "777001",
		Status:   "processed",
		Since:    &since,
		Page:     apiclient.PageOptions{Limit: 10, Cursor: "cur-0"},
	})
	require.NoError(t, err)
	assert.Equal(t, TriggerListScopeCaller, listed.Scope)
	assert.Equal(t, "cur-1", listed.NextCursor)
	require.Len(t, listed.Events, 1)
	assert.Equal(t, "ping", listed.Events[0].Attributes["command"])
	assert.Equal(t, []string{"bug", "triggers"}, listed.Events[0].Lists["labels"])

	detail, err := svc.Get(ctx, "evt-789")
	require.NoError(t, err)
	assert.Equal(t, "github.issue_comment.created", detail.Type)
	assert.Equal(t, "issue_comment", detail.Headers["x-github-event"])
	assert.Equal(t, []string{"bug", "triggers"}, detail.Lists["labels"])
	assert.JSONEq(t, `{"action":"created","comment":{"body":"@scion /ping"}}`, string(detail.Payload))

	rc, err := svc.GetPayload(ctx, "evt-789")
	require.NoError(t, err)
	streamed, err := io.ReadAll(rc)
	require.NoError(t, rc.Close())
	require.NoError(t, err)
	assert.JSONEq(t, `{"action":"created","overflow":true}`, string(streamed))
}

func TestWebhookEndpointService_UpdateAndRotateSecret(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/projects/proj-1/webhook-endpoints", func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "ag-1", r.URL.Query().Get("boundAgentId"))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ListWebhookEndpointsResponse{
			Endpoints: []WebhookEndpoint{{
				ID:            "ep-123",
				ProjectID:     "proj-1",
				Name:          "bound-ep",
				BoundAgent:    &WebhookEndpointBoundAgent{ID: "ag-1", Name: "worker"},
				BoundLifetime: "agent",
				Status:        "active",
				CreatedAt:     now,
				UpdatedAt:     now,
			}},
			TotalCount: 1,
			ServerTime: now,
		})
	})
	mux.HandleFunc("/api/v1/projects/proj-1/webhook-endpoints/ep-123", func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPatch, r.Method)
		var req UpdateWebhookEndpointRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		require.NotNil(t, req.Status)
		assert.Equal(t, "disabled", *req.Status)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(WebhookEndpoint{
			ID:            "ep-123",
			ProjectID:     "proj-1",
			Name:          "bound-ep",
			Status:        "disabled",
			ConfigVersion: 2,
			CreatedAt:     now,
			UpdatedAt:     now,
		})
	})
	mux.HandleFunc("/api/v1/projects/proj-1/webhook-endpoints/ep-123/rotate-secret", func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(WebhookEndpointRotated{
			WebhookEndpoint: WebhookEndpoint{
				ID:              "ep-123",
				ProjectID:       "proj-1",
				Name:            "bound-ep",
				SecretVersion:   2,
				SecretRotatedAt: &now,
				Status:          "active",
				CreatedAt:       now,
				UpdatedAt:       now,
			},
			Secret:             "rotated-v2-secret",
			GraceWindowExpires: now.Add(24 * time.Hour),
		})
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	c, err := New(srv.URL)
	require.NoError(t, err)
	svc := c.WebhookEndpoints("proj-1")
	ctx := context.Background()

	listed, err := svc.List(ctx, &ListWebhookEndpointsOptions{BoundAgentID: "ag-1"})
	require.NoError(t, err)
	require.Len(t, listed.Endpoints, 1)
	require.NotNil(t, listed.Endpoints[0].BoundAgent)
	assert.Equal(t, "ag-1", listed.Endpoints[0].BoundAgent.ID)
	assert.Equal(t, "worker", listed.Endpoints[0].BoundAgent.Name)

	disabled := "disabled"
	updated, err := svc.Update(ctx, "ep-123", &UpdateWebhookEndpointRequest{Status: &disabled})
	require.NoError(t, err)
	assert.Equal(t, "disabled", updated.Status)
	assert.Equal(t, 2, updated.ConfigVersion)

	rotated, err := svc.RotateSecret(ctx, "ep-123")
	require.NoError(t, err)
	assert.Equal(t, 2, rotated.SecretVersion)
	assert.Equal(t, "rotated-v2-secret", rotated.Secret)
	assert.True(t, rotated.GraceWindowExpires.After(now))
}
