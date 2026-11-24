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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveAgentIDForSubscription_Found(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/projects/project-1/agents" && r.Method == http.MethodGet {
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

	agentID, err := resolveAgentIDForSubscription(context.Background(), client, "project-1", "my-agent")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if agentID != "uuid-1" {
		t.Errorf("agent ID = %q, want %q", agentID, "uuid-1")
	}
}

func TestResolveAgentIDForSubscription_NotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/projects/project-1/agents" && r.Method == http.MethodGet {
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

	_, err = resolveAgentIDForSubscription(context.Background(), client, "project-1", "missing-agent")
	if err == nil {
		t.Fatal("expected error for missing agent, got nil")
	}
}

func TestResolveAgentIDForSubscription_BySlugified(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/projects/project-1/agents" && r.Method == http.MethodGet {
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
	agentID, err := resolveAgentIDForSubscription(context.Background(), client, "project-1", "My Agent")
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
			ProjectID:         "project-1",
			TriggerActivities: []string{"COMPLETED", "WAITING_FOR_INPUT"},
			CreatedAt:         time.Date(2026, 3, 18, 0, 0, 0, 0, time.UTC),
			CreatedBy:         "user-1",
		},
		{
			ID:                "sub-2",
			Scope:             store.SubscriptionScopeProject,
			SubscriberType:    store.SubscriberTypeUser,
			SubscriberID:      "user-1",
			ProjectID:         "project-1",
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
		ProjectID: "project-1",
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
			if req.ProjectID != "project-1" {
				t.Errorf("expected projectId %q, got %q", "project-1", req.ProjectID)
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
		ProjectID:         "project-1",
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
// the project path without leaking into other tests in this package. HOME
// and hub-context env vars are isolated via t.Setenv, which restores them
// automatically.
type requireHubClientTestState struct {
	projectPath string
}

func saveRequireHubClientTestState() requireHubClientTestState {
	return requireHubClientTestState{projectPath: projectPath}
}

func (s requireHubClientTestState) restore() {
	projectPath = s.projectPath
}

// clearHubContextEnv clears every env var config.IsHubContext and
// requireHubClient's auth resolution consult, so a test starts from a clean
// "definitely not in an agent container" baseline.
func clearHubContextEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"SCION_HUB_ENDPOINT", "SCION_HUB_URL", "SCION_PROJECT_ID",
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
	t.Setenv("HOME", tmpHome)
	projectDir := filepath.Join(tmpHome, "no-hub-project", ".scion")
	if err := os.MkdirAll(projectDir, 0755); err != nil {
		t.Fatalf("failed to create project dir: %v", err)
	}
	projectPath = projectDir

	_, _, err := requireHubClient()
	if err == nil {
		t.Fatal("expected an error when hub is neither enabled nor in an agent context")
	}
	if !errors.Is(err, errHubNotConfigured) {
		t.Errorf("error = %v, want errHubNotConfigured", err)
	}
}

// TestRequireHubClient_AgentHubContext_HubNotEnabledInSettings is the direct
// regression test for ptone/scion#1909: inside an agent container,
// hub.enabled is never persisted to settings (see config.IsHubContext's doc
// comment), but the hub context env vars and an agent-scoped SCION_AUTH_TOKEN
// are always present. requireHubClient must accept this the same way
// hubsync.EnsureHubReady's in-container fallback already does, without
// requiring hub.enabled to be set in settings.
func TestRequireHubClient_AgentHubContext_HubNotEnabledInSettings(t *testing.T) {
	orig := saveRequireHubClientTestState()
	defer orig.restore()
	clearHubContextEnv(t)

	t.Setenv("SCION_HUB_ENDPOINT", "https://hub.internal.example")
	t.Setenv("SCION_PROJECT_ID", "agent-project-id")
	t.Setenv("SCION_AUTH_TOKEN", "test-agent-token")

	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
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
	if settings.Hub != nil && settings.Hub.Enabled != nil {
		t.Fatal("test setup error: this case only proves the in-container fallback, hub.enabled must stay unset")
	}
}

// TestRunNotificationsList_AgentHubContext_HubNotEnabledInSettings covers the
// notifications half of the ptone/scion#1909 regression: "scion
// notifications" failed with "requires Hub mode" inside an agent container
// for the same reason conversation commands did — requireHubClient is the
// shared gate for both (cmd/notifications.go, cmd/conversation.go).
func TestRunNotificationsList_AgentHubContext_HubNotEnabledInSettings(t *testing.T) {
	origProjectPath := projectPath
	origJSON := notificationsJSON
	origShowAll := notificationsShowAll
	origOutputFormat := outputFormat
	defer func() {
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
	t.Setenv("SCION_PROJECT_ID", "agent-project-id")
	t.Setenv("SCION_AUTH_TOKEN", "test-agent-token")

	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
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
	expected := []string{"COMPLETED", "IDLE", "WAITING_FOR_INPUT", "LIMITS_EXCEEDED"}
	if len(defaultTriggers) != len(expected) {
		t.Fatalf("defaultTriggers length = %d, want %d", len(defaultTriggers), len(expected))
	}
	for i, v := range expected {
		if defaultTriggers[i] != v {
			t.Errorf("defaultTriggers[%d] = %q, want %q", i, defaultTriggers[i], v)
		}
	}
}

func TestNotificationsArgsValidators(t *testing.T) {
	noArgCmds := []*cobra.Command{
		notificationsCmd,
		notificationsSubscribeCmd,
		notificationsSubscriptionsCmd,
	}
	for _, cmd := range noArgCmds {
		t.Run(cmd.Name()+"_no_args", func(t *testing.T) {
			require.NotNil(t, cmd.Args, "%s must set Args to reject stray positional arguments", cmd.Name())
			assert.NoError(t, cmd.Args(cmd, []string{}))
			assert.Error(t, cmd.Args(cmd, []string{"bogus"}))
		})
	}

	maxOneArgCmds := []*cobra.Command{
		notificationsAckCmd,
		notificationsUnsubscribeCmd,
	}
	for _, cmd := range maxOneArgCmds {
		t.Run(cmd.Name()+"_max_one_arg", func(t *testing.T) {
			require.NotNil(t, cmd.Args, "%s must set Args to reject extra positional arguments", cmd.Name())
			assert.NoError(t, cmd.Args(cmd, []string{}))
			assert.NoError(t, cmd.Args(cmd, []string{"id-1"}))
			assert.Error(t, cmd.Args(cmd, []string{"id-1", "extra"}))
		})
	}
}

// TestNotificationsBareAndUnknownSubcommand covers ptone/scion#8831 through
// rootCmd.Execute(): bare "scion notifications" (and its aliases) still lists
// notifications, while unknown subcommands (e.g. "scion notifications bogus")
// and stray positional arguments on subcommands fail fast without hitting the hub.
func TestNotificationsBareAndUnknownSubcommand(t *testing.T) {
	restoreAllSilenceUsage(t)
	origProjectPath := projectPath
	origJSON := notificationsJSON
	origShowAll := notificationsShowAll
	origOutputFormat := outputFormat
	defer func() {
		projectPath = origProjectPath
		notificationsJSON = origJSON
		notificationsShowAll = origShowAll
		outputFormat = origOutputFormat
	}()
	clearHubContextEnv(t)

	var sawRequest bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawRequest = true
		if r.URL.Path == "/api/v1/notifications" && r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode([]hubclient.Notification{})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	t.Setenv("SCION_HUB_ENDPOINT", server.URL)
	t.Setenv("SCION_PROJECT_ID", "agent-project-id")
	t.Setenv("SCION_AUTH_TOKEN", "test-agent-token")

	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	projectDir := filepath.Join(tmpHome, "agent-project", ".scion")
	require.NoError(t, os.MkdirAll(projectDir, 0755))
	projectPath = projectDir
	notificationsJSON = false
	notificationsShowAll = false
	outputFormat = ""

	execArgs := func(t *testing.T, args []string) (string, error) {
		t.Helper()
		t.Cleanup(func() {
			resetCommandFlags(t, notificationsCmd)
			resetCommandFlags(t, notificationsAckCmd)
			resetCommandFlags(t, notificationsSubscribeCmd)
			resetCommandFlags(t, notificationsUnsubscribeCmd)
			resetCommandFlags(t, notificationsSubscriptionsCmd)
		})
		var buf bytes.Buffer
		rootCmd.SetArgs(args)
		rootCmd.SetOut(&buf)
		rootCmd.SetErr(&buf)
		defer func() {
			rootCmd.SetArgs(nil)
			rootCmd.SetOut(nil)
			rootCmd.SetErr(nil)
		}()
		var execErr error
		stdout := captureStdout(t, func() {
			execErr = rootCmd.Execute()
		})
		return stdout + buf.String(), execErr
	}

	for _, bare := range [][]string{{"notifications"}, {"notification"}, {"notif"}} {
		t.Run("bare_"+bare[0], func(t *testing.T) {
			sawRequest = false
			out, err := execArgs(t, bare)
			require.NoError(t, err)
			assert.True(t, sawRequest, "bare %v must call the hub list endpoint", bare)
			assert.Contains(t, out, "No notifications")
		})
	}

	for _, tc := range []struct {
		name    string
		args    []string
		wantErr string
	}{
		{
			name:    "unknown_subcommand",
			args:    []string{"notifications", "bogus"},
			wantErr: `unknown command "bogus" for "scion notifications"`,
		},
		{
			name:    "notification_alias_unknown_subcommand",
			args:    []string{"notification", "bogus"},
			wantErr: `unknown command "bogus" for "scion notifications"`,
		},
		{
			name:    "notif_alias_unknown_subcommand",
			args:    []string{"notif", "bogus"},
			wantErr: `unknown command "bogus" for "scion notifications"`,
		},
		{
			name:    "subscribe_stray_arg",
			args:    []string{"notifications", "subscribe", "extra"},
			wantErr: `unknown command "extra" for "scion notifications subscribe"`,
		},
		{
			name:    "subscriptions_stray_arg",
			args:    []string{"notifications", "subscriptions", "extra"},
			wantErr: `unknown command "extra" for "scion notifications subscriptions"`,
		},
		{
			name:    "subs_alias_stray_arg",
			args:    []string{"notifications", "subs", "extra"},
			wantErr: `unknown command "extra" for "scion notifications subscriptions"`,
		},
		{
			name:    "ack_extra_arg",
			args:    []string{"notifications", "ack", "notif-1", "extra"},
			wantErr: `accepts at most 1 arg(s), received 2`,
		},
		{
			name:    "unsubscribe_extra_arg",
			args:    []string{"notifications", "unsubscribe", "sub-1", "extra"},
			wantErr: `accepts at most 1 arg(s), received 2`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sawRequest = false
			_, err := execArgs(t, tc.args)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
			assert.False(t, sawRequest, "unknown subcommand/arg must fail fast before calling the hub")
		})
	}
}

func TestResolveSubscriptionID(t *testing.T) {
	const (
		subA = "a1b2c3d4-1111-4000-8000-000000000001"
		subB = "a1b2c3d4-2222-4000-8000-000000000002"
		subC = "deadbeef-3333-4000-8000-000000000003"
	)

	var listCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/notifications/subscriptions" && r.Method == http.MethodGet {
			listCalls++
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode([]hubclient.Subscription{
				{ID: subA},
				{ID: subB},
				{ID: subC},
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)

	t.Run("full UUID skips list", func(t *testing.T) {
		listCalls = 0
		got, err := resolveSubscriptionID(context.Background(), client, subA)
		require.NoError(t, err)
		assert.Equal(t, subA, got)
		assert.Equal(t, 0, listCalls)
	})

	t.Run("unique 8-char prefix", func(t *testing.T) {
		listCalls = 0
		got, err := resolveSubscriptionID(context.Background(), client, "deadbeef")
		require.NoError(t, err)
		assert.Equal(t, subC, got)
		assert.Equal(t, 1, listCalls)
	})

	t.Run("unique 12-char table prefix", func(t *testing.T) {
		listCalls = 0
		got, err := resolveSubscriptionID(context.Background(), client, "a1b2c3d4-111")
		require.NoError(t, err)
		assert.Equal(t, subA, got)
		assert.Equal(t, 1, listCalls)
	})

	t.Run("ambiguous prefix returns clear error", func(t *testing.T) {
		_, err := resolveSubscriptionID(context.Background(), client, "a1b2c3d4")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "ambiguous subscription ID prefix")
		assert.Contains(t, err.Error(), subA)
		assert.Contains(t, err.Error(), subB)
	})

	t.Run("not found prefix returns clear error", func(t *testing.T) {
		_, err := resolveSubscriptionID(context.Background(), client, "ffffffff")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no subscription found matching")
	})

	t.Run("empty ref returns usage error", func(t *testing.T) {
		_, err := resolveSubscriptionID(context.Background(), client, "   ")
		require.Error(t, err)
		assert.True(t, isUsageError(err))
	})
}

func TestResolveNotificationID(t *testing.T) {
	const (
		unackedA = "a1b2c3d4-1111-4000-8000-000000000001"
		unackedB = "a1b2c3d4-2222-4000-8000-000000000002"
		ackedC   = "cafebabe-3333-4000-8000-000000000003"
	)

	var listQueries []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/notifications" && r.Method == http.MethodGet {
			ackParam := r.URL.Query().Get("acknowledged")
			listQueries = append(listQueries, ackParam)
			w.Header().Set("Content-Type", "application/json")
			if ackParam == "true" {
				_ = json.NewEncoder(w).Encode([]hubclient.Notification{
					{ID: unackedA},
					{ID: unackedB},
					{ID: ackedC, Acknowledged: true},
				})
				return
			}
			_ = json.NewEncoder(w).Encode([]hubclient.Notification{
				{ID: unackedA},
				{ID: unackedB},
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)

	t.Run("full UUID skips list", func(t *testing.T) {
		listQueries = nil
		got, err := resolveNotificationID(context.Background(), client, unackedA)
		require.NoError(t, err)
		assert.Equal(t, unackedA, got)
		assert.Empty(t, listQueries)
	})

	t.Run("unique 12-char prefix in unacknowledged", func(t *testing.T) {
		listQueries = nil
		got, err := resolveNotificationID(context.Background(), client, "a1b2c3d4-111")
		require.NoError(t, err)
		assert.Equal(t, unackedA, got)
		assert.Equal(t, []string{""}, listQueries)
	})

	t.Run("ambiguous prefix in unacknowledged fails immediately", func(t *testing.T) {
		listQueries = nil
		_, err := resolveNotificationID(context.Background(), client, "a1b2c3d4")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "ambiguous notification ID prefix")
		assert.Equal(t, []string{""}, listQueries)
	})

	t.Run("falls back to acknowledged notifications", func(t *testing.T) {
		listQueries = nil
		got, err := resolveNotificationID(context.Background(), client, "cafebabe")
		require.NoError(t, err)
		assert.Equal(t, ackedC, got)
		assert.Equal(t, []string{"", "true"}, listQueries)
	})

	t.Run("not found returns clear error", func(t *testing.T) {
		listQueries = nil
		_, err := resolveNotificationID(context.Background(), client, "ffffffff")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no notification found matching")
	})
}

func TestNotificationsPrefixCommandsEndToEnd(t *testing.T) {
	const (
		subA   = "1111aaaa-1111-4000-8000-000000000001"
		subB   = "1111bbbb-2222-4000-8000-000000000002"
		notifA = "2222aaaa-1111-4000-8000-000000000001"
	)

	origProjectPath := projectPath
	origUpdateTriggers := updateTriggers
	origUnsubAll := unsubscribeAll
	origAckAll := notificationsAckAll
	origOutputFormat := outputFormat
	defer func() {
		projectPath = origProjectPath
		updateTriggers = origUpdateTriggers
		unsubscribeAll = origUnsubAll
		notificationsAckAll = origAckAll
		outputFormat = origOutputFormat
	}()
	clearHubContextEnv(t)

	var patchedID, deletedID, ackedID string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/notifications/subscriptions":
			_ = json.NewEncoder(w).Encode([]hubclient.Subscription{
				{ID: subA, TriggerActivities: []string{"COMPLETED"}},
				{ID: subB, TriggerActivities: []string{"COMPLETED"}},
			})
		case r.Method == http.MethodPatch && r.URL.Path == "/api/v1/notifications/subscriptions/"+subA:
			patchedID = subA
			_ = json.NewEncoder(w).Encode(hubclient.Subscription{
				ID:                subA,
				TriggerActivities: []string{"COMPLETED", "ERROR"},
			})
		case r.Method == http.MethodDelete && r.URL.Path == "/api/v1/notifications/subscriptions/"+subB:
			deletedID = subB
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/notifications":
			_ = json.NewEncoder(w).Encode([]hubclient.Notification{
				{ID: notifA},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/notifications/"+notifA+"/ack":
			ackedID = notifA
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	t.Setenv("SCION_HUB_ENDPOINT", server.URL)
	t.Setenv("SCION_PROJECT_ID", "proj-1")
	t.Setenv("SCION_AUTH_TOKEN", "test-token")
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	projectDir := filepath.Join(tmpHome, "proj", ".scion")
	require.NoError(t, os.MkdirAll(projectDir, 0o755))
	projectPath = projectDir
	outputFormat = ""

	// Update by unique 8-char prefix
	updateTriggers = []string{"COMPLETED,ERROR"}
	out := captureStdout(t, func() {
		require.NoError(t, runNotificationsUpdate(nil, []string{"1111aaaa"}))
	})
	assert.Equal(t, subA, patchedID)
	assert.Contains(t, out, subA)

	// Update by ambiguous 4-char prefix fails before PATCH
	patchedID = ""
	err := runNotificationsUpdate(nil, []string{"1111"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ambiguous subscription ID prefix")
	assert.Empty(t, patchedID)

	// Unsubscribe by unique 12-char prefix
	unsubscribeAll = false
	out = captureStdout(t, func() {
		require.NoError(t, runNotificationsUnsubscribe(nil, []string{"1111bbbb-222"}))
	})
	assert.Equal(t, subB, deletedID)
	assert.Contains(t, out, subB)

	// Ack by unique 8-char prefix
	notificationsAckAll = false
	out = captureStdout(t, func() {
		require.NoError(t, runNotificationsAck(nil, []string{"2222aaaa"}))
	})
	assert.Equal(t, notifA, ackedID)
	assert.Contains(t, out, notifA)
}
