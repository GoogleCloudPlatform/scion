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
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConversationCommandRegistered(t *testing.T) {
	// Verify conversation command is registered in rootCmd.
	found := false
	for _, cmd := range rootCmd.Commands() {
		if cmd.Name() == "conversation" {
			found = true
			break
		}
	}
	assert.True(t, found, "conversation command should be registered in rootCmd")
}

func TestConversationAliases(t *testing.T) {
	assert.Contains(t, conversationCmd.Aliases, "conv", "conversation should have 'conv' alias")
}

func TestConversationSubcommands(t *testing.T) {
	subcommands := make(map[string]bool)
	for _, cmd := range conversationCmd.Commands() {
		subcommands[cmd.Name()] = true
	}

	assert.True(t, subcommands["list"], "should have 'list' subcommand")
	assert.True(t, subcommands["messages"], "should have 'messages' subcommand")
	assert.True(t, subcommands["create"], "should have 'create' subcommand")
	assert.True(t, subcommands["get"], "should have 'get' subcommand")
	assert.True(t, subcommands["get-message"], "should have 'get-message' subcommand")
	assert.True(t, subcommands["set-default"], "should have 'set-default' subcommand")
	assert.True(t, subcommands["participants"], "should have 'participants' subcommand")
	assert.True(t, subcommands["join"], "should have 'join' subcommand")
	assert.True(t, subcommands["leave"], "should have 'leave' subcommand")
	assert.True(t, subcommands["catch-up"], "should have 'catch-up' subcommand")
}

func TestConversationListFlags(t *testing.T) {
	flags := conversationListCmd.Flags()

	f := flags.Lookup("kind")
	require.NotNil(t, f, "--kind flag should exist")
	assert.Equal(t, "", f.DefValue)

	f = flags.Lookup("surface")
	require.NotNil(t, f, "--surface flag should exist")
	assert.Equal(t, "", f.DefValue)

	f = flags.Lookup("project")
	require.NotNil(t, f, "--project flag should exist")
	assert.Equal(t, "", f.DefValue)

	f = flags.Lookup("json")
	require.NotNil(t, f, "--json flag should exist")
	assert.Equal(t, "false", f.DefValue)

	f = flags.Lookup("limit")
	require.NotNil(t, f, "--limit flag should exist")
	assert.Equal(t, "50", f.DefValue)
}

func TestConversationMessagesFlags(t *testing.T) {
	flags := conversationMessagesCmd.Flags()

	f := flags.Lookup("limit")
	require.NotNil(t, f, "--limit flag should exist")
	assert.Equal(t, "25", f.DefValue)

	f = flags.Lookup("before")
	require.NotNil(t, f, "--before flag should exist")

	f = flags.Lookup("after")
	require.NotNil(t, f, "--after flag should exist")

	f = flags.Lookup("json")
	require.NotNil(t, f, "--json flag should exist")
}

func TestConversationCreateFlags(t *testing.T) {
	flags := conversationCreateCmd.Flags()

	f := flags.Lookup("project")
	require.NotNil(t, f, "--project flag should exist")

	f = flags.Lookup("json")
	require.NotNil(t, f, "--json flag should exist")
}

func TestConversationGetFlags(t *testing.T) {
	flags := conversationGetCmd.Flags()

	f := flags.Lookup("json")
	require.NotNil(t, f, "--json flag should exist")
}

func TestConversationGetMessageFlags(t *testing.T) {
	flags := conversationGetMessageCmd.Flags()

	f := flags.Lookup("json")
	require.NotNil(t, f, "--json flag should exist")
	assert.Equal(t, "false", f.DefValue)
}

func TestConversationMessagesRequiresArgs(t *testing.T) {
	err := conversationMessagesCmd.Args(conversationMessagesCmd, []string{})
	assert.Error(t, err, "messages should require a conversation ref argument")
}

func TestConversationCreateRequiresArgs(t *testing.T) {
	err := conversationCreateCmd.Args(conversationCreateCmd, []string{})
	assert.Error(t, err, "create should require a name argument")
}

func TestConversationSetDefaultRequiresArgs(t *testing.T) {
	err := conversationSetDefaultCmd.Args(conversationSetDefaultCmd, []string{})
	assert.Error(t, err, "set-default should require two arguments")

	err = conversationSetDefaultCmd.Args(conversationSetDefaultCmd, []string{"ref"})
	assert.Error(t, err, "set-default should require two arguments")
}

func TestConversationGetRequiresArgs(t *testing.T) {
	err := conversationGetCmd.Args(conversationGetCmd, []string{})
	assert.Error(t, err, "get should require a conversation ref argument")
}

func TestConversationGetMessageRequiresTwoArgs(t *testing.T) {
	err := conversationGetMessageCmd.Args(conversationGetMessageCmd, []string{})
	assert.Error(t, err, "get-message should require two arguments")

	err = conversationGetMessageCmd.Args(conversationGetMessageCmd, []string{"conv:conversation-id"})
	assert.Error(t, err, "get-message should require two arguments")

	err = conversationGetMessageCmd.Args(conversationGetMessageCmd, []string{"conv:conversation-id", "message-id"})
	assert.NoError(t, err, "get-message should accept exactly two arguments")
}

func TestConversationParticipantsRequiresArgs(t *testing.T) {
	err := conversationParticipantsCmd.Args(conversationParticipantsCmd, []string{})
	assert.Error(t, err, "participants should require a conversation ref argument")
}

func TestConversationParticipantsFlags(t *testing.T) {
	flags := conversationParticipantsCmd.Flags()

	f := flags.Lookup("json")
	require.NotNil(t, f, "--json flag should exist")
	assert.Equal(t, "false", f.DefValue)
}

func TestConversationJoinRequiresArgs(t *testing.T) {
	err := conversationJoinCmd.Args(conversationJoinCmd, []string{})
	assert.Error(t, err, "join should require three arguments")

	err = conversationJoinCmd.Args(conversationJoinCmd, []string{"ref"})
	assert.Error(t, err, "join should require three arguments")

	err = conversationJoinCmd.Args(conversationJoinCmd, []string{"ref", "agent"})
	assert.Error(t, err, "join should require three arguments")
}

func TestConversationLeaveRequiresArgs(t *testing.T) {
	err := conversationLeaveCmd.Args(conversationLeaveCmd, []string{})
	assert.Error(t, err, "leave should require a conversation ref argument")
}

func TestConversationCatchUpRequiresArgs(t *testing.T) {
	err := conversationCatchUpCmd.Args(conversationCatchUpCmd, []string{})
	assert.Error(t, err, "catch-up should require a conversation ref argument")
}

func TestConversationCatchUpFlags(t *testing.T) {
	flags := conversationCatchUpCmd.Flags()

	f := flags.Lookup("since")
	require.NotNil(t, f, "--since flag should exist")
	assert.Equal(t, "1h", f.DefValue)

	f = flags.Lookup("json")
	require.NotNil(t, f, "--json flag should exist")
	assert.Equal(t, "false", f.DefValue)
}

func TestFormatTimeAgo(t *testing.T) {
	tests := []struct {
		name     string
		duration time.Duration
		expected string
	}{
		{"just now", 30 * time.Second, "just now"},
		{"1 minute", 1 * time.Minute, "1m ago"},
		{"5 minutes", 5 * time.Minute, "5m ago"},
		{"1 hour", 1 * time.Hour, "1h ago"},
		{"3 hours", 3 * time.Hour, "3h ago"},
		{"1 day", 24 * time.Hour, "1d ago"},
		{"7 days", 7 * 24 * time.Hour, "7d ago"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := formatTimeAgo(time.Now().Add(-tt.duration))
			assert.Equal(t, tt.expected, result)
		})
	}
}

// ---------------------------------------------------------------------------
// runConversationCreate end-to-end tests (review round 1, R2 / AC-11)
//
// TestResolveProjectID (notifications_test.go) only proves resolveProjectID's
// resolution order — it would still pass if the `if convProject == ""` block
// in runConversationCreate were deleted. These tests exercise the actual
// wire request the command sends, mirroring the httptest-hub pattern already
// used by TestSubscriptionCreateEndToEnd / setupSecretProject.
// ---------------------------------------------------------------------------

// conversationCreateTestState snapshots the package-level flag/global state
// runConversationCreate depends on, so tests can restore it afterward.
type conversationCreateTestState struct {
	home           string
	projectPath    string
	convProject    string
	convCreateJSON bool
	outputFormat   string
}

func saveConversationCreateTestState() conversationCreateTestState {
	return conversationCreateTestState{
		home:           os.Getenv("HOME"),
		projectPath:    projectPath,
		convProject:    convProject,
		convCreateJSON: convCreateJSON,
		outputFormat:   outputFormat,
	}
}

func (s conversationCreateTestState) restore() {
	_ = os.Setenv("HOME", s.home)
	projectPath = s.projectPath
	convProject = s.convProject
	convCreateJSON = s.convCreateJSON
	outputFormat = s.outputFormat
}

// isolateHubEnvForTest overrides every SCION_ env var that
// config.LoadSettingsKoanf's env layer (pkg/config/koanf.go) merges over
// project settings — SCION_HUB_ENDPOINT (-> hub.endpoint), SCION_HUB_URL
// (cmd.GetHubEndpoint's secondary fallback), and SCION_PROJECT_ID /
// SCION_GROVE_ID (-> the top-level, last-resort project ID resolveProjectID
// falls back to). Without this, these tests are not hermetic when run
// inside a live Scion agent container, which sets all four to point at the
// real orchestration hub and project — silently defeating the "nothing
// resolves" case and masking which value actually won. t.Setenv restores
// the originals automatically.
func isolateHubEnvForTest(t *testing.T, hubEndpoint, projectID string) {
	t.Helper()
	t.Setenv("SCION_HUB_ENDPOINT", hubEndpoint)
	t.Setenv("SCION_HUB_URL", hubEndpoint)
	t.Setenv("SCION_PROJECT_ID", projectID)
	t.Setenv("SCION_GROVE_ID", projectID)
}

// setupConversationCreateProject creates a project directory with hub
// settings pointing at endpoint, optionally with a hub-linked project ID.
// Mirrors setupSecretProject in hub_secret_test.go.
func setupConversationCreateProject(t *testing.T, home, endpoint, hubProjectID string) string {
	t.Helper()
	projectDir := filepath.Join(home, "project", ".scion")
	require.NoError(t, os.MkdirAll(projectDir, 0755))

	hub := map[string]interface{}{
		"enabled":  true,
		"endpoint": endpoint,
	}
	if hubProjectID != "" {
		hub["projectId"] = hubProjectID
	}
	settings := map[string]interface{}{
		"hub": hub,
	}
	data, err := json.Marshal(settings)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(projectDir, "settings.json"), data, 0644))

	return projectDir
}

// TestRunConversationCreate_DefaultsProjectFromHubContext is the R2 fix:
// AC-11 says "scion conversation create X with no --project sends the
// hub-context project ID." This exercises the actual request
// runConversationCreate sends when convProject is empty and the project
// resolves via the hub-linked project (§3.6).
func TestRunConversationCreate_DefaultsProjectFromHubContext(t *testing.T) {
	orig := saveConversationCreateTestState()
	defer orig.restore()

	var gotProjectID string
	var sawRequest bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/conversations" && r.Method == http.MethodPost {
			var req struct {
				ProjectID string `json:"projectId"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			gotProjectID = req.ProjectID
			sawRequest = true

			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(store.Conversation{
				ID:         "new-conv-id",
				Kind:       "group",
				Surface:    "native",
				DriftState: "active",
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	isolateHubEnvForTest(t, server.URL, "")
	tmpHome := t.TempDir()
	_ = os.Setenv("HOME", tmpHome)
	projectDir := setupConversationCreateProject(t, tmpHome, server.URL, "hub-linked-project-id")
	projectPath = projectDir
	convProject = ""
	convCreateJSON = true // routes output through outputJSON instead of Printf

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	err := runConversationCreate(cmd, []string{"hub-context-test"})
	require.NoError(t, err)
	require.True(t, sawRequest, "expected the create request to reach the mock hub")
	assert.Equal(t, "hub-linked-project-id", gotProjectID,
		"AC-11: with no --project, the CLI must send the hub-linked project ID")
}

// TestRunConversationCreate_NoProjectResolvable_SendsEmpty covers the other
// half: when nothing resolves (no --project, no hub-linked project, no local
// project), the CLI must not fabricate a project ID — it sends empty and
// lets the server-side agent-token fallback (or the Phase 2 "projectId is
// required" 400) apply.
func TestRunConversationCreate_NoProjectResolvable_SendsEmpty(t *testing.T) {
	orig := saveConversationCreateTestState()
	defer orig.restore()

	var gotProjectID string
	var sawRequest bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/conversations" && r.Method == http.MethodPost {
			var req struct {
				ProjectID string `json:"projectId"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			gotProjectID = req.ProjectID
			sawRequest = true

			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(store.Conversation{
				ID:         "new-conv-id-2",
				Kind:       "group",
				Surface:    "native",
				DriftState: "active",
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	isolateHubEnvForTest(t, server.URL, "") // no local/hub project ID anywhere
	tmpHome := t.TempDir()
	_ = os.Setenv("HOME", tmpHome)
	projectDir := setupConversationCreateProject(t, tmpHome, server.URL, "") // no hub.projectId, no project_id
	projectPath = projectDir
	convProject = ""
	convCreateJSON = true

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	err := runConversationCreate(cmd, []string{"no-project-test"})
	require.NoError(t, err)
	require.True(t, sawRequest, "expected the create request to reach the mock hub")
	assert.Empty(t, gotProjectID, "projectId must be empty, not fabricated, when nothing resolves")
}
