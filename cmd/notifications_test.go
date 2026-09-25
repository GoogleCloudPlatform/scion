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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/spf13/cobra"
)

func TestResolveAgentIDForSubscription_Found(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/projects/grove-1/agents" && r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
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
		if r.URL.Path == "/api/v1/projects/grove-1/agents" && r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
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
		if r.URL.Path == "/api/v1/projects/grove-1/agents" && r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
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
			ProjectID:         "grove-1",
			TriggerActivities: []string{"COMPLETED", "WAITING_FOR_INPUT"},
			CreatedAt:         time.Date(2026, 3, 18, 0, 0, 0, 0, time.UTC),
			CreatedBy:         "user-1",
		},
		{
			ID:                "sub-2",
			Scope:             store.SubscriptionScopeProject,
			SubscriberType:    store.SubscriberTypeUser,
			SubscriberID:      "user-1",
			ProjectID:         "grove-1",
			TriggerActivities: []string{"COMPLETED"},
			CreatedAt:         time.Date(2026, 3, 17, 0, 0, 0, 0, time.UTC),
			CreatedBy:         "user-1",
		},
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/notifications/subscriptions" && r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(subs)
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
				t.Errorf("expected empty agentId for project scope, got %q", req.AgentID)
			}

			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(hubclient.Subscription{
				ID:                "new-sub-id",
				Scope:             req.Scope,
				ProjectID:         req.ProjectID,
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
		ProjectID:         "grove-1",
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

// TestResolveProjectID covers the resolution order runConversationCreate
// (cmd/conversation.go, chat-thread-bridge §3.6) now relies on to fill
// --project when it is empty: flag > hub-linked project > local project,
// erroring only when none is available.
func TestResolveProjectID(t *testing.T) {
	t.Run("flag wins over everything", func(t *testing.T) {
		settings := &config.Settings{
			ProjectID: "local-proj",
			Hub:       &config.HubClientConfig{ProjectID: "hub-proj"},
		}
		got, err := resolveProjectID(settings, "flag-proj")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "flag-proj" {
			t.Errorf("projectID = %q, want %q", got, "flag-proj")
		}
	})

	t.Run("hub project used when flag empty", func(t *testing.T) {
		settings := &config.Settings{
			ProjectID: "local-proj",
			Hub:       &config.HubClientConfig{ProjectID: "hub-proj"},
		}
		got, err := resolveProjectID(settings, "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "hub-proj" {
			t.Errorf("projectID = %q, want %q", got, "hub-proj")
		}
	})

	t.Run("local project used when no flag and no hub project", func(t *testing.T) {
		settings := &config.Settings{ProjectID: "local-proj"}
		got, err := resolveProjectID(settings, "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "local-proj" {
			t.Errorf("projectID = %q, want %q", got, "local-proj")
		}
	})

	t.Run("error when nothing resolves", func(t *testing.T) {
		settings := &config.Settings{}
		_, err := resolveProjectID(settings, "")
		if err == nil {
			t.Fatal("expected an error when no project can be determined, got nil")
		}
	})
}

// requireHubClientTestState saves/restores the package-level state
// requireHubClient depends on (cmd/notifications.go), so tests can isolate
// hub-context env vars and the project path without leaking into other
// tests in this package.
type requireHubClientTestState struct {
	home        string
	projectPath string
}

func saveRequireHubClientTestState() requireHubClientTestState {
	return requireHubClientTestState{home: os.Getenv("HOME"), projectPath: projectPath}
}

func (s requireHubClientTestState) restore() {
	_ = os.Setenv("HOME", s.home)
	projectPath = s.projectPath
}

// clearHubContextEnv clears every env var config.IsHubContext and
// requireHubClient's auth resolution consult, so a test starts from a clean
// "definitely not in an agent container" baseline.
func clearHubContextEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"SCION_HUB_ENDPOINT", "SCION_HUB_URL", "SCION_GROVE_ID", "SCION_PROJECT_ID",
		"SCION_AUTH_TOKEN", "SCION_HUB_TOKEN", "SCION_DEV_TOKEN",
	} {
		t.Setenv(key, "")
	}
}

// TestRequireHubClient_NoHubEnabled_NoAgentContext_Errors is the control
// case for the ptone/scion#1909 fix: outside of a hub-connected agent
// container (no hub context env vars) and with hub not enabled in settings,
// requireHubClient must still refuse. The fix must not widen access beyond
// the in-container fallback.
func TestRequireHubClient_NoHubEnabled_NoAgentContext_Errors(t *testing.T) {
	orig := saveRequireHubClientTestState()
	defer orig.restore()
	clearHubContextEnv(t)

	tmpHome := t.TempDir()
	_ = os.Setenv("HOME", tmpHome)
	projectDir := filepath.Join(tmpHome, "no-hub-project", ".scion")
	if err := os.MkdirAll(projectDir, 0755); err != nil {
		t.Fatalf("failed to create project dir: %v", err)
	}
	projectPath = projectDir

	_, _, err := requireHubClient()
	if err == nil {
		t.Fatal("expected an error when hub is neither enabled nor in an agent context")
	}
	if got := err.Error(); !strings.Contains(got, "require Hub mode") {
		t.Errorf("error = %q, want it to mention %q", got, "require Hub mode")
	}
}

// TestRequireHubClient_AgentHubContext_HubNotEnabledInSettings is the direct
// regression test for ptone/scion#1909: inside an agent container,
// hub.enabled is never persisted to settings (see config.IsHubContext's doc
// comment), but the hub context env vars and an agent-scoped SCION_AUTH_TOKEN
// are always present. requireHubClient must accept this the same way
// hubsync.EnsureHubReady's in-container fallback already does, without
// requiring settings.IsHubEnabled() to be true.
func TestRequireHubClient_AgentHubContext_HubNotEnabledInSettings(t *testing.T) {
	orig := saveRequireHubClientTestState()
	defer orig.restore()
	clearHubContextEnv(t)

	t.Setenv("SCION_HUB_ENDPOINT", "http://hub.internal.example")
	t.Setenv("SCION_GROVE_ID", "agent-project-id")
	t.Setenv("SCION_AUTH_TOKEN", "test-agent-token")

	tmpHome := t.TempDir()
	_ = os.Setenv("HOME", tmpHome)
	projectDir := filepath.Join(tmpHome, "agent-project", ".scion")
	if err := os.MkdirAll(projectDir, 0755); err != nil {
		t.Fatalf("failed to create project dir: %v", err)
	}
	projectPath = projectDir

	settings, client, err := requireHubClient()
	if err != nil {
		t.Fatalf("agent-context callers must not be rejected for missing hub.enabled in settings: %v", err)
	}
	if client == nil {
		t.Fatal("expected a non-nil hub client")
	}
	if settings.IsHubEnabled() {
		t.Fatal("test setup error: this case only proves the in-container fallback, settings.IsHubEnabled() must stay false")
	}
}

// TestRunNotificationsList_AgentHubContext_HubNotEnabledInSettings covers the
// notifications half of the ptone/scion#1909 regression: "scion
// notifications" failed with "requires Hub mode" inside an agent container
// for the same reason conversation commands did — requireHubClient is the
// shared gate for both (cmd/notifications.go, cmd/conversation.go).
func TestRunNotificationsList_AgentHubContext_HubNotEnabledInSettings(t *testing.T) {
	origHome := os.Getenv("HOME")
	origProjectPath := projectPath
	origJSON := notificationsJSON
	origShowAll := notificationsShowAll
	origOutputFormat := outputFormat
	defer func() {
		_ = os.Setenv("HOME", origHome)
		projectPath = origProjectPath
		notificationsJSON = origJSON
		notificationsShowAll = origShowAll
		outputFormat = origOutputFormat
	}()
	clearHubContextEnv(t)

	var sawRequest bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/notifications" && r.Method == http.MethodGet {
			sawRequest = true
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode([]hubclient.Notification{})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	t.Setenv("SCION_HUB_ENDPOINT", server.URL)
	t.Setenv("SCION_GROVE_ID", "agent-project-id")
	t.Setenv("SCION_AUTH_TOKEN", "test-agent-token")

	tmpHome := t.TempDir()
	_ = os.Setenv("HOME", tmpHome)
	projectDir := filepath.Join(tmpHome, "agent-project", ".scion")
	if err := os.MkdirAll(projectDir, 0755); err != nil {
		t.Fatalf("failed to create project dir: %v", err)
	}
	projectPath = projectDir
	notificationsJSON = true
	notificationsShowAll = false

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := runNotificationsList(cmd, nil); err != nil {
		t.Fatalf("agent-context notifications list must not require hub.enabled in settings: %v", err)
	}
	if !sawRequest {
		t.Fatal("expected the notifications list request to reach the mock hub")
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
