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

package cmd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func TestResolveAgentIDForSubscription_Found(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/groves/grove-1/agents" && r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"agents": []map[string]interface{}{
					{"id": "uuid-1", "slug": "my-agent", "name": "my-agent", "status": "running"},
					{"id": "uuid-2", "slug": "other-agent", "name": "other-agent", "status": "running"},
				},
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	client, err := hubclient.New(server.URL)
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	agentID, err := resolveAgentIDForSubscription(context.Background(), client, "grove-1", "my-agent")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if agentID != "uuid-1" {
		t.Errorf("agent ID = %q, want %q", agentID, "uuid-1")
	}
}

func TestResolveAgentIDForSubscription_NotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/groves/grove-1/agents" && r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"agents": []map[string]interface{}{
					{"id": "uuid-1", "slug": "other-agent", "name": "other-agent", "status": "running"},
				},
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	client, err := hubclient.New(server.URL)
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	_, err = resolveAgentIDForSubscription(context.Background(), client, "grove-1", "missing-agent")
	if err == nil {
		t.Fatal("expected error for missing agent, got nil")
	}
}

func TestResolveAgentIDForSubscription_BySlugified(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/groves/grove-1/agents" && r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"agents": []map[string]interface{}{
					{"id": "uuid-1", "slug": "my-agent", "name": "My Agent", "status": "running"},
				},
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	client, err := hubclient.New(server.URL)
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	// Should find by name match
	agentID, err := resolveAgentIDForSubscription(context.Background(), client, "grove-1", "My Agent")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if agentID != "uuid-1" {
		t.Errorf("agent ID = %q, want %q", agentID, "uuid-1")
	}
}

func TestSubscriptionsListEndToEnd(t *testing.T) {
	subs := []hubclient.Subscription{
		{
			ID:                "sub-1",
			Scope:             store.SubscriptionScopeAgent,
			AgentID:           "agent-1",
			SubscriberType:    store.SubscriberTypeUser,
			SubscriberID:      "user-1",
			ProjectID:           "grove-1",
			TriggerActivities: []string{"COMPLETED", "WAITING_FOR_INPUT"},
			CreatedAt:         time.Date(2026, 3, 18, 0, 0, 0, 0, time.UTC),
			CreatedBy:         "user-1",
		},
		{
			ID:                "sub-2",
			Scope:             store.SubscriptionScopeProject,
			SubscriberType:    store.SubscriberTypeUser,
			SubscriberID:      "user-1",
			ProjectID:           "grove-1",
			TriggerActivities: []string{"COMPLETED"},
			CreatedAt:         time.Date(2026, 3, 17, 0, 0, 0, 0, time.UTC),
			CreatedBy:         "user-1",
		},
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/notifications/subscriptions" && r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(subs)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	client, err := hubclient.New(server.URL)
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	result, err := client.Subscriptions().List(context.Background(), &hubclient.ListSubscriptionsOptions{
		ProjectID: "grove-1",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(result) != 2 {
		t.Fatalf("expected 2 subscriptions, got %d", len(result))
	}

	if result[0].Scope != store.SubscriptionScopeAgent {
		t.Errorf("first subscription scope = %q, want %q", result[0].Scope, store.SubscriptionScopeAgent)
	}
	if result[1].Scope != store.SubscriptionScopeProject {
		t.Errorf("second subscription scope = %q, want %q", result[1].Scope, store.SubscriptionScopeProject)
	}
}

func TestSubscriptionCreateEndToEnd(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/notifications/subscriptions" && r.Method == http.MethodPost {
			var req hubclient.CreateSubscriptionRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}

			if req.Scope != store.SubscriptionScopeProject {
				t.Errorf("expected scope %q, got %q", store.SubscriptionScopeProject, req.Scope)
			}
			if req.ProjectID != "grove-1" {
				t.Errorf("expected groveId %q, got %q", "grove-1", req.ProjectID)
			}
			if req.AgentID != "" {
				t.Errorf("expected empty agentId for grove scope, got %q", req.AgentID)
			}

			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(hubclient.Subscription{
				ID:                "new-sub-id",
				Scope:             req.Scope,
				ProjectID:           req.ProjectID,
				SubscriberType:    store.SubscriberTypeUser,
				SubscriberID:      "user-1",
				TriggerActivities: req.TriggerActivities,
				CreatedAt:         time.Now(),
				CreatedBy:         "user-1",
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	client, err := hubclient.New(server.URL)
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	sub, err := client.Subscriptions().Create(context.Background(), &hubclient.CreateSubscriptionRequest{
		Scope:             store.SubscriptionScopeProject,
		ProjectID:           "grove-1",
		TriggerActivities: []string{"COMPLETED", "WAITING_FOR_INPUT"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if sub.ID != "new-sub-id" {
		t.Errorf("subscription ID = %q, want %q", sub.ID, "new-sub-id")
	}
	if sub.Scope != store.SubscriptionScopeProject {
		t.Errorf("subscription scope = %q, want %q", sub.Scope, store.SubscriptionScopeProject)
	}
}

func TestSubscriptionDeleteEndToEnd(t *testing.T) {
	var deletedID string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete && r.URL.Path == "/api/v1/notifications/subscriptions/sub-123" {
			deletedID = "sub-123"
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	client, err := hubclient.New(server.URL)
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	err = client.Subscriptions().Delete(context.Background(), "sub-123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if deletedID != "sub-123" {
		t.Errorf("deleted ID = %q, want %q", deletedID, "sub-123")
	}
}

func TestDefaultTriggers(t *testing.T) {
	expected := []string{"COMPLETED", "WAITING_FOR_INPUT", "LIMITS_EXCEEDED"}
	if len(defaultTriggers) != len(expected) {
		t.Fatalf("defaultTriggers length = %d, want %d", len(defaultTriggers), len(expected))
	}
	for i, v := range expected {
		if defaultTriggers[i] != v {
			t.Errorf("defaultTriggers[%d] = %q, want %q", i, defaultTriggers[i], v)
		}
	}
}
