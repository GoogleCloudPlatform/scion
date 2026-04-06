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

package hub

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHubNativeGrovePath(t *testing.T) {
	path, err := hubNativeGrovePath("my-test-grove")
	require.NoError(t, err)

	homeDir, err := os.UserHomeDir()
	require.NoError(t, err)

	expected := filepath.Join(homeDir, ".scion", "groves", "my-test-grove")
	assert.Equal(t, expected, path)
}

func TestCreateGrove_HubNative_NoGitRemote(t *testing.T) {
	srv, _ := testServer(t)

	body := CreateGroveRequest{
		Name: "Hub Native Grove",
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/groves", body)
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	var grove store.Grove
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&grove))

	assert.Equal(t, "Hub Native Grove", grove.Name)
	assert.Equal(t, "hub-native-grove", grove.Slug)
	assert.Empty(t, grove.GitRemote, "hub-native grove should have no git remote")

	// Verify the filesystem was initialized
	workspacePath, err := hubNativeGrovePath(grove.Slug)
	require.NoError(t, err)

	scionDir := filepath.Join(workspacePath, ".scion")
	settingsPath := filepath.Join(scionDir, "settings.yaml")

	_, err = os.Stat(settingsPath)
	assert.NoError(t, err, "settings.yaml should exist for hub-native grove")

	// Cleanup
	t.Cleanup(func() {
		os.RemoveAll(workspacePath)
	})
}

func TestCreateGrove_GitBacked_NoFilesystemInit(t *testing.T) {
	srv, _ := testServer(t)

	body := CreateGroveRequest{
		Name:      "Git Grove",
		GitRemote: "github.com/test/repo",
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/groves", body)
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	var grove store.Grove
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&grove))

	assert.Equal(t, "github.com/test/repo", grove.GitRemote)

	// Verify no filesystem was created for git-backed grove
	workspacePath, err := hubNativeGrovePath(grove.Slug)
	require.NoError(t, err)

	_, err = os.Stat(workspacePath)
	assert.True(t, os.IsNotExist(err), "no workspace directory should be created for git-backed groves")
}

func TestPopulateAgentConfig_HubNativeGrove_SetsWorkspace(t *testing.T) {
	srv, _ := testServer(t)

	grove := &store.Grove{
		ID:   "grove-hub-native",
		Name: "Hub Native",
		Slug: "hub-native",
		// No GitRemote — hub-native grove
	}

	agent := &store.Agent{
		ID:            "agent-test",
		AppliedConfig: &store.AgentAppliedConfig{},
	}

	srv.populateAgentConfig(agent, grove, nil)

	expectedPath, err := hubNativeGrovePath("hub-native")
	require.NoError(t, err)
	assert.Equal(t, expectedPath, agent.AppliedConfig.Workspace,
		"Workspace should be set for hub-native groves")
	assert.Nil(t, agent.AppliedConfig.GitClone,
		"GitClone should not be set for hub-native groves")
}

func TestPopulateAgentConfig_HubNativeGrove_RemoteBroker_WorkspaceSet(t *testing.T) {
	srv, _ := testServer(t)

	grove := &store.Grove{
		ID:   "grove-hub-native-remote",
		Name: "Hub Native Remote",
		Slug: "hub-native-remote",
		// No GitRemote — hub-native grove
	}

	agent := &store.Agent{
		ID:            "agent-remote",
		AppliedConfig: &store.AgentAppliedConfig{},
	}

	srv.populateAgentConfig(agent, grove, nil)

	// populateAgentConfig sets Workspace for hub-native groves.
	// For remote brokers, the createAgent handler later swaps this to
	// WorkspaceStoragePath. Here we verify the initial workspace is set.
	expectedPath, err := hubNativeGrovePath("hub-native-remote")
	require.NoError(t, err)
	assert.Equal(t, expectedPath, agent.AppliedConfig.Workspace)
}

func TestPopulateAgentConfig_GitGrove_NoWorkspace(t *testing.T) {
	srv, _ := testServer(t)

	grove := &store.Grove{
		ID:        "grove-git",
		Name:      "Git Grove",
		Slug:      "git-grove",
		GitRemote: "github.com/test/repo",
	}

	agent := &store.Agent{
		ID:            "agent-test",
		AppliedConfig: &store.AgentAppliedConfig{},
	}

	srv.populateAgentConfig(agent, grove, nil)

	assert.Empty(t, agent.AppliedConfig.Workspace,
		"Workspace should not be set for git-backed groves")
	assert.NotNil(t, agent.AppliedConfig.GitClone,
		"GitClone should be set for git-backed groves")
}

func TestPopulateAgentConfig_TemplateTelemetryMerged(t *testing.T) {
	srv, _ := testServer(t)

	grove := &store.Grove{
		ID:   "grove-telem",
		Name: "Telemetry Grove",
		Slug: "telemetry-grove",
	}

	enabled := true
	tmplTelemetry := &api.TelemetryConfig{
		Enabled: &enabled,
		Cloud: &api.TelemetryCloudConfig{
			Endpoint: "https://otel.example.com",
			Provider: "gcp",
		},
	}

	template := &store.Template{
		ID:   "tmpl-telem",
		Slug: "telem-template",
		Config: &store.TemplateConfig{
			Telemetry: tmplTelemetry,
		},
	}

	agent := &store.Agent{
		ID:            "agent-telem",
		AppliedConfig: &store.AgentAppliedConfig{},
	}

	srv.populateAgentConfig(agent, grove, template)

	require.NotNil(t, agent.AppliedConfig.InlineConfig,
		"InlineConfig should be created to hold template telemetry")
	require.NotNil(t, agent.AppliedConfig.InlineConfig.Telemetry,
		"Telemetry should be merged from template")
	assert.Equal(t, &enabled, agent.AppliedConfig.InlineConfig.Telemetry.Enabled)
	assert.Equal(t, "https://otel.example.com", agent.AppliedConfig.InlineConfig.Telemetry.Cloud.Endpoint)
	assert.Equal(t, "gcp", agent.AppliedConfig.InlineConfig.Telemetry.Cloud.Provider)
}

func TestPopulateAgentConfig_InlineTelemetryNotOverwritten(t *testing.T) {
	srv, _ := testServer(t)

	grove := &store.Grove{
		ID:   "grove-telem2",
		Name: "Telemetry Grove 2",
		Slug: "telemetry-grove-2",
	}

	enabled := true
	tmplTelemetry := &api.TelemetryConfig{
		Enabled: &enabled,
		Cloud: &api.TelemetryCloudConfig{
			Endpoint: "https://template-otel.example.com",
		},
	}

	inlineTelemetry := &api.TelemetryConfig{
		Cloud: &api.TelemetryCloudConfig{
			Endpoint: "https://inline-otel.example.com",
		},
	}

	template := &store.Template{
		ID:   "tmpl-telem2",
		Slug: "telem-template-2",
		Config: &store.TemplateConfig{
			Telemetry: tmplTelemetry,
		},
	}

	agent := &store.Agent{
		ID: "agent-telem2",
		AppliedConfig: &store.AgentAppliedConfig{
			InlineConfig: &api.ScionConfig{
				Telemetry: inlineTelemetry,
			},
		},
	}

	srv.populateAgentConfig(agent, grove, template)

	// Inline telemetry should NOT be overwritten by template telemetry
	assert.Equal(t, "https://inline-otel.example.com",
		agent.AppliedConfig.InlineConfig.Telemetry.Cloud.Endpoint,
		"Explicit inline telemetry should take precedence over template")
}

func TestPopulateAgentConfig_HubTelemetryDefault(t *testing.T) {
	srv, _ := testServer(t)

	// Set hub-level telemetry config
	hubEnabled := true
	srv.config.TelemetryConfig = &api.TelemetryConfig{
		Enabled: &hubEnabled,
		Cloud: &api.TelemetryCloudConfig{
			Endpoint: "https://hub-otel.example.com",
			Provider: "gcp",
		},
	}

	grove := &store.Grove{
		ID:   "grove-hub-tel",
		Name: "Hub Telemetry Grove",
		Slug: "hub-telemetry-grove",
	}

	agent := &store.Agent{
		ID:            "agent-hub-tel",
		AppliedConfig: &store.AgentAppliedConfig{},
	}

	srv.populateAgentConfig(agent, grove, nil)

	require.NotNil(t, agent.AppliedConfig.InlineConfig,
		"InlineConfig should be created to hold hub telemetry")
	require.NotNil(t, agent.AppliedConfig.InlineConfig.Telemetry,
		"Telemetry should be populated from hub config")
	assert.Equal(t, &hubEnabled, agent.AppliedConfig.InlineConfig.Telemetry.Enabled)
	assert.Equal(t, "https://hub-otel.example.com",
		agent.AppliedConfig.InlineConfig.Telemetry.Cloud.Endpoint)
}

func TestPopulateAgentConfig_HubTelemetryNotOverwrittenByTemplate(t *testing.T) {
	srv, _ := testServer(t)

	// Set hub-level telemetry config
	hubEnabled := true
	srv.config.TelemetryConfig = &api.TelemetryConfig{
		Enabled: &hubEnabled,
		Cloud: &api.TelemetryCloudConfig{
			Endpoint: "https://hub-otel.example.com",
		},
	}

	tmplEnabled := true
	template := &store.Template{
		ID:   "tmpl-hub-tel",
		Slug: "hub-tel-template",
		Config: &store.TemplateConfig{
			Telemetry: &api.TelemetryConfig{
				Enabled: &tmplEnabled,
				Cloud: &api.TelemetryCloudConfig{
					Endpoint: "https://template-otel.example.com",
				},
			},
		},
	}

	grove := &store.Grove{ID: "grove-hub-tel2", Slug: "hub-tel-grove-2"}

	agent := &store.Agent{
		ID:            "agent-hub-tel2",
		AppliedConfig: &store.AgentAppliedConfig{},
	}

	srv.populateAgentConfig(agent, grove, template)

	// Template telemetry should win over hub telemetry
	assert.Equal(t, "https://template-otel.example.com",
		agent.AppliedConfig.InlineConfig.Telemetry.Cloud.Endpoint,
		"Template telemetry should take precedence over hub default")
}

func TestPopulateAgentConfig_GroveTelemetryEnabledOverride(t *testing.T) {
	srv, _ := testServer(t)

	// Set hub-level telemetry config with enabled=true
	hubEnabled := true
	srv.config.TelemetryConfig = &api.TelemetryConfig{
		Enabled: &hubEnabled,
		Cloud: &api.TelemetryCloudConfig{
			Endpoint: "https://hub-otel.example.com",
		},
	}

	// Grove disables telemetry
	grove := &store.Grove{
		ID:   "grove-tel-override",
		Slug: "tel-override-grove",
		Annotations: map[string]string{
			groveSettingTelemetryEnabled: "false",
		},
	}

	agent := &store.Agent{
		ID:            "agent-tel-override",
		AppliedConfig: &store.AgentAppliedConfig{},
	}

	srv.populateAgentConfig(agent, grove, nil)

	require.NotNil(t, agent.AppliedConfig.InlineConfig.Telemetry)
	// Hub cloud config should still be present
	assert.Equal(t, "https://hub-otel.example.com",
		agent.AppliedConfig.InlineConfig.Telemetry.Cloud.Endpoint)
	// But enabled should be overridden by grove setting
	require.NotNil(t, agent.AppliedConfig.InlineConfig.Telemetry.Enabled)
	assert.False(t, *agent.AppliedConfig.InlineConfig.Telemetry.Enabled,
		"Grove TelemetryEnabled=false should override hub Enabled=true")
}

func TestPopulateAgentConfig_GroveTelemetryEnabledWithoutOtherConfig(t *testing.T) {
	srv, _ := testServer(t)

	// No hub telemetry config, no template — only grove sets enabled
	grove := &store.Grove{
		ID:   "grove-tel-only",
		Slug: "tel-only-grove",
		Annotations: map[string]string{
			groveSettingTelemetryEnabled: "true",
		},
	}

	agent := &store.Agent{
		ID:            "agent-tel-only",
		AppliedConfig: &store.AgentAppliedConfig{},
	}

	srv.populateAgentConfig(agent, grove, nil)

	require.NotNil(t, agent.AppliedConfig.InlineConfig)
	require.NotNil(t, agent.AppliedConfig.InlineConfig.Telemetry)
	require.NotNil(t, agent.AppliedConfig.InlineConfig.Telemetry.Enabled)
	assert.True(t, *agent.AppliedConfig.InlineConfig.Telemetry.Enabled,
		"Grove TelemetryEnabled=true should create telemetry config with Enabled=true")
}

// TestCreateAgent_HubNativeGrove_ExplicitBroker_AutoLinks tests that creating an agent
// in a hub-native grove with an explicitly selected broker auto-links the broker as a
// provider, even if it wasn't previously registered as one.
func TestCreateAgent_HubNativeGrove_ExplicitBroker_AutoLinks(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create a runtime broker
	broker := &store.RuntimeBroker{
		ID:     "broker-hub-autolink",
		Slug:   "hub-autolink-broker",
		Name:   "Hub Autolink Broker",
		Status: store.BrokerStatusOnline,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))

	// Create a hub-native grove (no git remote, no default broker, no providers)
	grove := &store.Grove{
		ID:   "grove-hub-autolink",
		Slug: "hub-autolink",
		Name: "Hub Autolink Grove",
		// No GitRemote — hub-native
		// No DefaultRuntimeBrokerID
	}
	require.NoError(t, s.CreateGrove(ctx, grove))

	// Create agent with explicit broker — this should auto-link the broker
	body := map[string]interface{}{
		"name":            "autolink-agent",
		"groveId":         grove.ID,
		"runtimeBrokerId": broker.ID,
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", body)
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	var resp CreateAgentResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))

	assert.NotNil(t, resp.Agent)
	assert.Equal(t, broker.ID, resp.Agent.RuntimeBrokerID,
		"Agent should be assigned to the explicitly selected broker")

	// Verify the broker was auto-linked as a provider
	provider, err := s.GetGroveProvider(ctx, grove.ID, broker.ID)
	require.NoError(t, err, "Broker should have been auto-linked as a provider")
	assert.Equal(t, broker.ID, provider.BrokerID)
	assert.Equal(t, "agent-create", provider.LinkedBy)

	// Verify the broker was set as the default
	updatedGrove, err := s.GetGrove(ctx, grove.ID)
	require.NoError(t, err)
	assert.Equal(t, broker.ID, updatedGrove.DefaultRuntimeBrokerID,
		"Broker should be set as the default for the grove")
}

// TestCreateGrove_HubNative_AutoProvide tests that creating a hub-native grove
// auto-links brokers with auto_provide enabled.
func TestCreateGrove_HubNative_AutoProvide(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create a broker with auto_provide enabled
	broker := &store.RuntimeBroker{
		ID:          "broker-autoprovide",
		Slug:        "autoprovide-broker",
		Name:        "Auto Provide Broker",
		Status:      store.BrokerStatusOnline,
		AutoProvide: true,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))

	// Create a hub-native grove via the API
	body := CreateGroveRequest{
		Name: "Auto Provide Grove",
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/groves", body)
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	var grove store.Grove
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&grove))
	assert.Empty(t, grove.GitRemote, "should be hub-native")

	// Verify the auto-provide broker was linked
	provider, err := s.GetGroveProvider(ctx, grove.ID, broker.ID)
	require.NoError(t, err, "Auto-provide broker should be linked as a provider")
	assert.Equal(t, "auto-provide", provider.LinkedBy)

	// Verify the broker was set as the default
	updatedGrove, err := s.GetGrove(ctx, grove.ID)
	require.NoError(t, err)
	assert.Equal(t, broker.ID, updatedGrove.DefaultRuntimeBrokerID,
		"Auto-provide broker should be set as the default")

	// Now create an agent — should work without explicit broker
	agentBody := map[string]interface{}{
		"name":    "autoprovide-agent",
		"groveId": grove.ID,
	}
	rec = doRequest(t, srv, http.MethodPost, "/api/v1/agents", agentBody)
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	var resp CreateAgentResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Equal(t, broker.ID, resp.Agent.RuntimeBrokerID,
		"Agent should use the auto-provided default broker")

	// Cleanup hub-native grove filesystem
	workspacePath, err := hubNativeGrovePath(grove.Slug)
	if err == nil {
		t.Cleanup(func() { os.RemoveAll(workspacePath) })
	}
}

// TestCreateAgent_HubNativeGrove_NoProviders_NoBroker tests that creating an agent
// in a hub-native grove with no providers and no explicit broker returns an appropriate error.
func TestDeleteGrove_HubNative_RemovesFilesystem(t *testing.T) {
	srv, s := testServer(t)

	// Create a hub-native grove via the API (initializes filesystem)
	grove, workspacePath := createTestHubNativeGrove(t, srv, "FS Delete Test")

	// Verify filesystem exists before deletion
	_, err := os.Stat(workspacePath)
	require.NoError(t, err, "workspace should exist before deletion")

	// Delete grove via API
	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/groves/"+grove.ID, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code)

	// Verify filesystem was removed
	_, err = os.Stat(workspacePath)
	assert.True(t, os.IsNotExist(err), "workspace should be deleted from filesystem")

	// Verify grove deleted from database
	ctx := context.Background()
	_, err = s.GetGrove(ctx, grove.ID)
	assert.ErrorIs(t, err, store.ErrNotFound, "grove should be deleted from database")
}

func TestDeleteGrove_GitBacked_NoFilesystemCleanup(t *testing.T) {
	srv, s := testServer(t)

	// Create a git-backed grove (no filesystem initialization)
	grove := createTestGitGrove(t, srv, "Git Delete Test", "github.com/test/git-delete-repo")

	// Delete grove via API
	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/groves/"+grove.ID, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code)

	// Verify grove deleted from database
	ctx := context.Background()
	_, err := s.GetGrove(ctx, grove.ID)
	assert.ErrorIs(t, err, store.ErrNotFound, "grove should be deleted from database")
}

func TestDeleteGrove_DeleteAgents_DispatchesToBroker(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Set up a mock dispatcher to track agent deletion
	disp := &deleteDispatcher{}
	srv.SetDispatcher(disp)

	grove, _, _ := setupOnlineBrokerAgent(t, s, "grove-del")

	// Create a second agent in the same grove
	agent2 := &store.Agent{
		ID:              "agent-online-grove-del-2",
		Slug:            "agent-online-grove-del-2-slug",
		Name:            "Agent Online grove-del 2",
		GroveID:         grove.ID,
		RuntimeBrokerID: "broker-online-grove-del",
		Phase:           string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, agent2))

	// Delete grove — agents are always cascade-deleted
	rec := doRequest(t, srv, http.MethodDelete,
		"/api/v1/groves/"+grove.ID, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code)

	// Verify dispatcher was called for both agents
	assert.Equal(t, 2, disp.deleteCalls,
		"DispatchAgentDelete should be called once per agent")

	// Verify grove deleted from database
	_, err := s.GetGrove(ctx, grove.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)

	// Verify agents cascade-deleted from database
	_, err = s.GetAgent(ctx, "agent-online-grove-del")
	assert.ErrorIs(t, err, store.ErrNotFound)
	_, err = s.GetAgent(ctx, agent2.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestDeleteGrove_AlwaysCascadeDeletesAgents(t *testing.T) {
	srv, s := testServer(t)

	disp := &deleteDispatcher{}
	srv.SetDispatcher(disp)

	grove, _, _ := setupOnlineBrokerAgent(t, s, "grove-nodelflag")

	// Delete grove without explicit deleteAgents param — agents should still be deleted
	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/groves/"+grove.ID, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code)

	// Dispatcher should have been called (cascade delete is always on)
	assert.Equal(t, 1, disp.deleteCalls,
		"DispatchAgentDelete should always be called when deleting a grove")

	// Grove should be deleted from database
	ctx := context.Background()
	_, err := s.GetGrove(ctx, grove.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestCreateAgent_HubNativeGrove_NoProviders_NoBroker(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create a hub-native grove with no providers
	grove := &store.Grove{
		ID:   "grove-hub-noproviders",
		Slug: "hub-noproviders",
		Name: "No Providers Grove",
	}
	require.NoError(t, s.CreateGrove(ctx, grove))

	body := map[string]interface{}{
		"name":    "orphan-agent",
		"groveId": grove.ID,
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", body)
	// Should fail because there are no providers and no broker specified
	assert.NotEqual(t, http.StatusCreated, rec.Code,
		"Should fail when no providers exist and no broker is specified")
}

// TestAutoLinkProviders_HubNativeGrove_NoLocalPath verifies that autoLinkProviders
// does NOT set LocalPath on the provider for hub-native groves. The hub's local
// path is not valid for remote brokers — instead, groveSlug is sent so each
// broker resolves the path on its own filesystem.
func TestAutoLinkProviders_HubNativeGrove_NoLocalPath(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create a broker with auto_provide enabled
	broker := &store.RuntimeBroker{
		ID:          "broker-localpath-auto",
		Slug:        "localpath-auto-broker",
		Name:        "LocalPath Auto Broker",
		Status:      store.BrokerStatusOnline,
		AutoProvide: true,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))

	// Create a hub-native grove via the API — this triggers autoLinkProviders
	body := CreateGroveRequest{
		Name: "LocalPath Auto Grove",
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/groves", body)
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	var grove store.Grove
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&grove))
	assert.Empty(t, grove.GitRemote, "should be hub-native")

	// Verify the auto-linked provider does NOT have LocalPath set
	provider, err := s.GetGroveProvider(ctx, grove.ID, broker.ID)
	require.NoError(t, err, "Auto-provide broker should be linked as a provider")
	assert.Equal(t, "auto-provide", provider.LinkedBy)
	assert.Empty(t, provider.LocalPath,
		"LocalPath should NOT be set for hub-native grove auto-linked provider")

	// Cleanup hub-native grove filesystem
	workspacePath, err := hubNativeGrovePath(grove.Slug)
	if err == nil {
		t.Cleanup(func() { os.RemoveAll(workspacePath) })
	}
}

// TestAutoLinkProviders_GitGrove_NoLocalPath verifies that autoLinkProviders
// does NOT set LocalPath on the provider for git-backed groves.
func TestAutoLinkProviders_GitGrove_NoLocalPath(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create a broker with auto_provide enabled
	broker := &store.RuntimeBroker{
		ID:          "broker-localpath-git",
		Slug:        "localpath-git-broker",
		Name:        "LocalPath Git Broker",
		Status:      store.BrokerStatusOnline,
		AutoProvide: true,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))

	// Create a git-backed grove via the API — this also triggers autoLinkProviders
	body := CreateGroveRequest{
		Name:      "LocalPath Git Grove",
		GitRemote: "github.com/test/localpath-git-repo",
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/groves", body)
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	var grove store.Grove
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&grove))

	// Verify the provider does NOT have LocalPath set
	provider, err := s.GetGroveProvider(ctx, grove.ID, broker.ID)
	require.NoError(t, err, "Auto-provide broker should be linked")
	assert.Empty(t, provider.LocalPath,
		"LocalPath should NOT be set for git-backed grove providers")
}

// TestDeleteGrove_HubNative_DispatchesCleanupToBrokers verifies that deleting a
// hub-native grove dispatches CleanupGrove to each provider broker (except the
// embedded/co-located broker).
func TestDeleteGrove_HubNative_DispatchesCleanupToBrokers(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create a hub-native grove
	grove := &store.Grove{
		ID:   "grove-cleanup-dispatch",
		Slug: "cleanup-dispatch",
		Name: "Cleanup Dispatch Grove",
		// No GitRemote — hub-native
	}
	require.NoError(t, s.CreateGrove(ctx, grove))

	// Create two brokers
	broker1 := &store.RuntimeBroker{
		ID:       "broker-cleanup-1",
		Slug:     "cleanup-broker-1",
		Name:     "Cleanup Broker 1",
		Status:   store.BrokerStatusOnline,
		Endpoint: "http://broker1:9800",
	}
	broker2 := &store.RuntimeBroker{
		ID:       "broker-cleanup-2",
		Slug:     "cleanup-broker-2",
		Name:     "Cleanup Broker 2",
		Status:   store.BrokerStatusOnline,
		Endpoint: "http://broker2:9800",
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker1))
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker2))

	// Link both as providers
	require.NoError(t, s.AddGroveProvider(ctx, &store.GroveProvider{
		GroveID:  grove.ID,
		BrokerID: broker1.ID,
		LinkedBy: "test",
	}))
	require.NoError(t, s.AddGroveProvider(ctx, &store.GroveProvider{
		GroveID:  grove.ID,
		BrokerID: broker2.ID,
		LinkedBy: "test",
	}))

	// Set up a mock client and dispatcher
	mockClient := &mockRuntimeBrokerClient{}
	disp := NewHTTPAgentDispatcherWithClient(s, mockClient, false, slog.Default())
	srv.SetDispatcher(disp)

	// Delete grove
	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/groves/"+grove.ID, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code)

	// Verify CleanupGrove was called for both brokers
	assert.Equal(t, 2, mockClient.cleanupCalls, "CleanupGrove should be called for each provider broker")
	assert.Contains(t, mockClient.cleanupSlugs, "cleanup-dispatch")

	// Verify grove deleted from database
	_, err := s.GetGrove(ctx, grove.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

// TestDeleteGrove_HubNative_SkipsEmbeddedBroker verifies that the embedded broker
// (co-located hub+broker) is not called for cleanup since the hub handles its own copy.
func TestDeleteGrove_HubNative_SkipsEmbeddedBroker(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create a hub-native grove
	grove := &store.Grove{
		ID:   "grove-cleanup-embedded",
		Slug: "cleanup-embedded",
		Name: "Cleanup Embedded Grove",
	}
	require.NoError(t, s.CreateGrove(ctx, grove))

	// Create embedded and remote brokers
	embeddedBroker := &store.RuntimeBroker{
		ID:       "broker-embedded",
		Slug:     "embedded-broker",
		Name:     "Embedded Broker",
		Status:   store.BrokerStatusOnline,
		Endpoint: "http://localhost:9800",
	}
	remoteBroker := &store.RuntimeBroker{
		ID:       "broker-remote",
		Slug:     "remote-broker",
		Name:     "Remote Broker",
		Status:   store.BrokerStatusOnline,
		Endpoint: "http://remote:9800",
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, embeddedBroker))
	require.NoError(t, s.CreateRuntimeBroker(ctx, remoteBroker))

	// Link both as providers
	require.NoError(t, s.AddGroveProvider(ctx, &store.GroveProvider{
		GroveID:  grove.ID,
		BrokerID: embeddedBroker.ID,
		LinkedBy: "test",
	}))
	require.NoError(t, s.AddGroveProvider(ctx, &store.GroveProvider{
		GroveID:  grove.ID,
		BrokerID: remoteBroker.ID,
		LinkedBy: "test",
	}))

	// Mark embedded broker
	srv.SetEmbeddedBrokerID(embeddedBroker.ID)

	// Set up mock client and dispatcher
	mockClient := &mockRuntimeBrokerClient{}
	disp := NewHTTPAgentDispatcherWithClient(s, mockClient, false, slog.Default())
	srv.SetDispatcher(disp)

	// Delete grove
	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/groves/"+grove.ID, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code)

	// Only the remote broker should receive CleanupGrove, not the embedded one
	assert.Equal(t, 1, mockClient.cleanupCalls, "CleanupGrove should only be called for non-embedded brokers")
	assert.Contains(t, mockClient.cleanupSlugs, "cleanup-embedded")
}

// TestDeleteGrove_GitBacked_NoCleanupDispatched verifies that deleting a git-backed
// grove does NOT trigger broker cleanup (those directories are externally managed).
func TestDeleteGrove_GitBacked_NoCleanupDispatched(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create a git-backed grove
	grove := &store.Grove{
		ID:        "grove-git-nocleanup",
		Slug:      "git-nocleanup",
		Name:      "Git No Cleanup Grove",
		GitRemote: "github.com/test/nocleanup",
	}
	require.NoError(t, s.CreateGrove(ctx, grove))

	// Create a broker and link as provider
	broker := &store.RuntimeBroker{
		ID:       "broker-git-nocleanup",
		Slug:     "git-nocleanup-broker",
		Name:     "Git NoCleanup Broker",
		Status:   store.BrokerStatusOnline,
		Endpoint: "http://broker:9800",
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	require.NoError(t, s.AddGroveProvider(ctx, &store.GroveProvider{
		GroveID:  grove.ID,
		BrokerID: broker.ID,
		LinkedBy: "test",
	}))

	// Set up mock client and dispatcher
	mockClient := &mockRuntimeBrokerClient{}
	disp := NewHTTPAgentDispatcherWithClient(s, mockClient, false, slog.Default())
	srv.SetDispatcher(disp)

	// Delete grove
	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/groves/"+grove.ID, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code)

	// CleanupGrove should NOT be called for git-backed groves
	assert.Equal(t, 0, mockClient.cleanupCalls, "CleanupGrove should not be called for git-backed groves")
}

// TestResolveRuntimeBroker_HubNativeGrove_NoLocalPath verifies that when a broker
// is auto-linked during agent creation for a hub-native grove, LocalPath is NOT
// set. Remote brokers resolve the path themselves via groveSlug.
func TestResolveRuntimeBroker_HubNativeGrove_NoLocalPath(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create a runtime broker (not auto-provide — will be explicitly selected)
	broker := &store.RuntimeBroker{
		ID:     "broker-resolve-localpath",
		Slug:   "resolve-localpath-broker",
		Name:   "Resolve LocalPath Broker",
		Status: store.BrokerStatusOnline,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))

	// Create a hub-native grove with no providers
	grove := &store.Grove{
		ID:   "grove-resolve-localpath",
		Slug: "resolve-localpath",
		Name: "Resolve LocalPath Grove",
	}
	require.NoError(t, s.CreateGrove(ctx, grove))

	// Create agent with explicit broker — triggers resolveRuntimeBroker auto-link
	agentBody := map[string]interface{}{
		"name":            "resolve-localpath-agent",
		"groveId":         grove.ID,
		"runtimeBrokerId": broker.ID,
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", agentBody)
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	// Verify the auto-linked provider does NOT have LocalPath set
	provider, err := s.GetGroveProvider(ctx, grove.ID, broker.ID)
	require.NoError(t, err, "Broker should have been auto-linked")
	assert.Equal(t, "agent-create", provider.LinkedBy)
	assert.Empty(t, provider.LocalPath,
		"LocalPath should NOT be set when auto-linking during agent creation for hub-native grove")
}

// TestGroveRegisterPreservesProviderLocalPath verifies that re-registering a
// grove from a local checkout does not overwrite an existing provider's empty
// localPath. This prevents a hub-native git grove (where agents clone from a
// URL) from being accidentally converted into a linked grove.
func TestGroveRegisterPreservesProviderLocalPath(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create a broker
	broker := &store.RuntimeBroker{
		ID:     "broker-preserve-path",
		Name:   "Preserve Path Broker",
		Slug:   "preserve-path-broker",
		Status: store.BrokerStatusOnline,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))

	// Step 1: Register grove (creates it) — this is the initial hub-native creation.
	// The broker is linked WITH a localPath (simulating CLI-initiated creation).
	body := map[string]interface{}{
		"name":      "preserve-path-grove",
		"gitRemote": "github.com/test/preserve-path",
		"brokerId":  broker.ID,
		"path":      "/original/path/.scion",
	}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/groves/register", body)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var resp RegisterGroveResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	require.True(t, resp.Created, "grove should be newly created")
	groveID := resp.Grove.ID

	// Verify provider has localPath from initial registration
	provider, err := s.GetGroveProvider(ctx, groveID, broker.ID)
	require.NoError(t, err)
	assert.Equal(t, "/original/path/.scion", provider.LocalPath,
		"newly created grove should have localPath from registration")

	// Now simulate converting to hub-native: clear localPath directly
	// (as autoLinkProviders would do, or via admin action)
	require.NoError(t, s.AddGroveProvider(ctx, &store.GroveProvider{
		GroveID:    groveID,
		BrokerID:   broker.ID,
		BrokerName: broker.Name,
		Status:     store.BrokerStatusOnline,
		LinkedBy:   "auto-provide",
		// LocalPath intentionally empty — hub-native provider
	}))

	// Verify localPath is now empty
	provider, err = s.GetGroveProvider(ctx, groveID, broker.ID)
	require.NoError(t, err)
	assert.Empty(t, provider.LocalPath, "provider should have no localPath after reset")

	// Step 2: Re-register from local checkout (CLI hubsync). This should NOT
	// overwrite the empty localPath with the new path.
	body2 := map[string]interface{}{
		"name":      "preserve-path-grove",
		"gitRemote": "github.com/test/preserve-path",
		"brokerId":  broker.ID,
		"path":      "/new/local/checkout/.scion",
	}
	rec2 := doRequest(t, srv, http.MethodPost, "/api/v1/groves/register", body2)
	require.Equal(t, http.StatusOK, rec2.Code, "body: %s", rec2.Body.String())

	var resp2 RegisterGroveResponse
	require.NoError(t, json.NewDecoder(rec2.Body).Decode(&resp2))
	assert.False(t, resp2.Created, "grove should already exist")

	// Verify the provider's localPath was preserved (still empty)
	provider, err = s.GetGroveProvider(ctx, groveID, broker.ID)
	require.NoError(t, err)
	assert.Empty(t, provider.LocalPath,
		"re-registration should not overwrite existing provider's empty localPath")
}

// TestCreateGrove_GitBacked_RandomID verifies that groves created with a git
// remote (but no explicit ID) get random UUIDs, and that creating two groves
// for the same repository produces different IDs with serial-numbered slugs.
func TestCreateGrove_GitBacked_RandomID(t *testing.T) {
	srv, _ := testServer(t)

	sshURL := "git@github.com:acme/widgets.git"
	httpsURL := "https://github.com/acme/widgets.git"

	// Create first grove via SSH URL
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/groves", CreateGroveRequest{
		Name:      "Widgets",
		GitRemote: sshURL,
	})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	var grove1 store.Grove
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&grove1))
	assert.NotEmpty(t, grove1.ID)
	assert.Equal(t, "widgets", grove1.Slug)

	// Create second grove via HTTPS URL (same repo) — should create a NEW grove
	rec = doRequest(t, srv, http.MethodPost, "/api/v1/groves", CreateGroveRequest{
		Name:      "Widgets",
		GitRemote: httpsURL,
	})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	var grove2 store.Grove
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&grove2))
	assert.NotEmpty(t, grove2.ID)
	assert.NotEqual(t, grove1.ID, grove2.ID, "two groves for same URL should have different IDs")
	assert.Equal(t, "widgets-1", grove2.Slug, "second grove should get serial-numbered slug")
	assert.Equal(t, "Widgets (1)", grove2.Name, "second grove should get serial display name")
}

// TestCreateGrove_NoGitRemote_RandomID verifies that groves without a git
// remote get a random UUID.
func TestCreateGrove_NoGitRemote_RandomID(t *testing.T) {
	srv, _ := testServer(t)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/groves", CreateGroveRequest{
		Name: "No Remote Grove",
	})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	var grove store.Grove
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&grove))
	assert.NotEmpty(t, grove.ID)
}

// TestRegisterGrove_GitBacked_RandomID verifies that the register endpoint
// assigns a random UUID (not deterministic) to groves created from a git remote.
func TestRegisterGrove_GitBacked_RandomID(t *testing.T) {
	srv, _ := testServer(t)

	gitRemote := "git@github.com:acme/gadgets.git"

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/groves/register", RegisterGroveRequest{
		Name:      "Gadgets",
		GitRemote: gitRemote,
	})
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var resp RegisterGroveResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.NotEmpty(t, resp.Grove.ID)
	assert.True(t, resp.Created)

	// ID should NOT be the deterministic hash — it should be a random UUID
	deterministicID := util.HashGroveID(util.NormalizeGitRemote(gitRemote))
	assert.NotEqual(t, deterministicID, resp.Grove.ID, "registered grove ID should be random, not deterministic")
}

// TestDeleteGrove_CascadesEnvVarsSecretsHarnessConfigs verifies that deleting a
// grove removes all grove-scoped env vars, secrets, and harness configs.
func TestDeleteGrove_CascadesEnvVarsSecretsHarnessConfigs(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	grove := createTestGitGrove(t, srv, "Cascade Resources Test", "github.com/test/cascade-resources")

	// Create grove-scoped env vars
	require.NoError(t, s.CreateEnvVar(ctx, &store.EnvVar{
		ID: api.NewUUID(), Key: "LOG_LEVEL", Value: "debug",
		Scope: store.ScopeGrove, ScopeID: grove.ID,
	}))
	require.NoError(t, s.CreateEnvVar(ctx, &store.EnvVar{
		ID: api.NewUUID(), Key: "REGION", Value: "us-east-1",
		Scope: store.ScopeGrove, ScopeID: grove.ID,
	}))

	// Create grove-scoped secrets
	require.NoError(t, s.CreateSecret(ctx, &store.Secret{
		ID: api.NewUUID(), Key: "API_KEY", EncryptedValue: "enc-val-1",
		Scope: store.ScopeGrove, ScopeID: grove.ID, Version: 1,
	}))

	// Create grove-scoped harness config
	require.NoError(t, s.CreateHarnessConfig(ctx, &store.HarnessConfig{
		ID: api.NewUUID(), Name: "grove-hc", Slug: "grove-hc",
		Harness: "claude", Scope: store.ScopeGrove, ScopeID: grove.ID,
		Status: store.HarnessConfigStatusActive, Visibility: store.VisibilityPrivate,
	}))

	// Create grove-scoped templates
	require.NoError(t, s.CreateTemplate(ctx, &store.Template{
		ID: api.NewUUID(), Name: "grove-tmpl", Slug: "grove-tmpl",
		Harness: "claude", Scope: store.ScopeGrove, ScopeID: grove.ID,
		Status: store.TemplateStatusActive, Visibility: store.VisibilityPrivate,
	}))

	// Also create a hub-scoped env var that should NOT be deleted
	hubEnvVarID := api.NewUUID()
	require.NoError(t, s.CreateEnvVar(ctx, &store.EnvVar{
		ID: hubEnvVarID, Key: "GLOBAL_VAR", Value: "keep-me",
		Scope: store.ScopeHub, ScopeID: "test-hub-id",
	}))

	// Verify resources exist before deletion
	envVars, err := s.ListEnvVars(ctx, store.EnvVarFilter{Scope: store.ScopeGrove, ScopeID: grove.ID})
	require.NoError(t, err)
	assert.Len(t, envVars, 2)

	secrets, err := s.ListSecrets(ctx, store.SecretFilter{Scope: store.ScopeGrove, ScopeID: grove.ID})
	require.NoError(t, err)
	assert.Len(t, secrets, 1)

	// Delete grove via API
	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/groves/"+grove.ID, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code)

	// Verify grove-scoped env vars were deleted
	envVars, err = s.ListEnvVars(ctx, store.EnvVarFilter{Scope: store.ScopeGrove, ScopeID: grove.ID})
	require.NoError(t, err)
	assert.Empty(t, envVars, "grove env vars should be cascade deleted")

	// Verify grove-scoped secrets were deleted
	secrets, err = s.ListSecrets(ctx, store.SecretFilter{Scope: store.ScopeGrove, ScopeID: grove.ID})
	require.NoError(t, err)
	assert.Empty(t, secrets, "grove secrets should be cascade deleted")

	// Verify grove-scoped harness configs were deleted
	hcResult, err := s.ListHarnessConfigs(ctx, store.HarnessConfigFilter{Scope: store.ScopeGrove, ScopeID: grove.ID}, store.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, hcResult.Items, "grove harness configs should be cascade deleted")

	// Verify grove-scoped templates were deleted
	tmplResult, err := s.ListTemplates(ctx, store.TemplateFilter{Scope: store.ScopeGrove, ScopeID: grove.ID}, store.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, tmplResult.Items, "grove templates should be cascade deleted")

	// Verify hub-scoped env var was NOT deleted
	hubVars, err := s.ListEnvVars(ctx, store.EnvVarFilter{Scope: store.ScopeHub, ScopeID: "test-hub-id"})
	require.NoError(t, err)
	assert.Len(t, hubVars, 1, "hub-scoped env var should not be affected")
}

// TestDeleteGrove_CleansUpGroveConfigsDir verifies that deleting a grove
// removes the ~/.scion/grove-configs/<slug>__<short-uuid>/ directory.
func TestDeleteGrove_CleansUpGroveConfigsDir(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	grove := createTestGitGrove(t, srv, "Config Cleanup Test", "github.com/test/config-cleanup-repo")

	// Create the grove-configs directory that would exist in workstation mode
	marker := &config.GroveMarker{
		GroveID:   grove.ID,
		GroveSlug: grove.Slug,
	}
	extPath, err := marker.ExternalGrovePath()
	require.NoError(t, err)

	// Create the directory structure: ~/.scion/grove-configs/<slug>__<uuid>/.scion/
	require.NoError(t, os.MkdirAll(extPath, 0755))
	// Also create an agents/ sibling directory
	agentsDir := filepath.Join(filepath.Dir(extPath), "agents", "test-agent", "home")
	require.NoError(t, os.MkdirAll(agentsDir, 0755))

	groveConfigDir := filepath.Dir(extPath)
	t.Cleanup(func() { os.RemoveAll(groveConfigDir) })

	// Verify directory exists before deletion
	_, err = os.Stat(groveConfigDir)
	require.NoError(t, err, "grove-configs dir should exist before deletion")

	// Delete grove via API
	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/groves/"+grove.ID, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code)

	// Verify grove-configs directory was removed
	_, err = os.Stat(groveConfigDir)
	assert.True(t, os.IsNotExist(err), "grove-configs dir should be removed after grove deletion")

	// Verify grove deleted from database
	_, err = s.GetGrove(ctx, grove.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

// TestGroveRegister_CreatesMembershipGroup verifies that registering a new grove
// automatically creates a membership group with the caller as owner.
func TestGroveRegister_CreatesMembershipGroup(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/groves/register", RegisterGroveRequest{
		Name:      "Membership Test",
		GitRemote: "https://github.com/test/membership-test.git",
	})
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var resp RegisterGroveResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	require.True(t, resp.Created)

	// Members group should exist
	membersSlug := "grove:" + resp.Grove.Slug + ":members"
	group, err := s.GetGroupBySlug(ctx, membersSlug)
	require.NoError(t, err, "members group should have been created")
	assert.Equal(t, resp.Grove.ID, group.GroveID)

	// The dev user should be an owner
	members, err := s.GetGroupMembers(ctx, group.ID)
	require.NoError(t, err)
	require.Len(t, members, 1, "should have exactly one member (the creator)")
	assert.Equal(t, DevUserID, members[0].MemberID)
	assert.Equal(t, store.GroupMemberRoleOwner, members[0].Role)
}

// TestGroveRegister_ExistingGrove_CreatesMembershipGroup verifies that
// registering against an existing grove (linking) still creates the membership
// group and adds the linking user as owner.
func TestGroveRegister_ExistingGrove_CreatesMembershipGroup(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create a grove directly in the store (simulating one created before
	// membership group support was added — no group exists yet).
	grove := &store.Grove{
		ID:        api.NewUUID(),
		Name:      "Pre-Existing Grove",
		Slug:      "pre-existing-grove",
		GitRemote: "github.com/test/pre-existing",
		CreatedBy: "original-creator-id",
	}
	require.NoError(t, s.CreateGrove(ctx, grove))

	// Verify no members group exists yet
	_, err := s.GetGroupBySlug(ctx, "grove:"+grove.Slug+":members")
	require.ErrorIs(t, err, store.ErrNotFound, "members group should not exist yet")

	// Register (link) via the API — this should backfill the group
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/groves/register", RegisterGroveRequest{
		ID:   grove.ID,
		Name: grove.Name,
	})
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var resp RegisterGroveResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.False(t, resp.Created, "should find existing grove")

	// Members group should now exist
	membersSlug := "grove:" + grove.Slug + ":members"
	group, err := s.GetGroupBySlug(ctx, membersSlug)
	require.NoError(t, err, "members group should have been created on link")
	assert.Equal(t, grove.ID, group.GroveID)

	// Both the original creator and the linking user should be owners
	members, err := s.GetGroupMembers(ctx, group.ID)
	require.NoError(t, err)

	ownerIDs := make(map[string]bool)
	for _, m := range members {
		if m.Role == store.GroupMemberRoleOwner {
			ownerIDs[m.MemberID] = true
		}
	}
	assert.True(t, ownerIDs["original-creator-id"], "original creator should be an owner")
	assert.True(t, ownerIDs[DevUserID], "linking user should be an owner")
}

// =============================================================================
// Git-Workspace Hybrid (Shared Workspace Mode) Tests
// =============================================================================

func TestCreateGrove_SharedWorkspace_SetsLabelAndInitFilesystem(t *testing.T) {
	srv, _ := testServer(t)

	// Create a local git repo to serve as the clone source
	sourceDir := t.TempDir()
	for _, args := range [][]string{
		{"init"},
		{"config", "user.email", "test@test.com"},
		{"config", "user.name", "Test"},
		{"commit", "--allow-empty", "-m", "init"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = sourceDir
		require.NoError(t, cmd.Run(), "git %v", args)
	}

	body := CreateGroveRequest{
		Name:          "Shared WS Grove",
		GitRemote:     "github.com/test/shared-ws",
		WorkspaceMode: "shared",
		Labels: map[string]string{
			"scion.dev/clone-url":      sourceDir,
			"scion.dev/default-branch": "master",
		},
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/groves", body)
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	var grove store.Grove
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&grove))

	assert.Equal(t, "github.com/test/shared-ws", grove.GitRemote)
	assert.Equal(t, store.WorkspaceModeShared, grove.Labels[store.LabelWorkspaceMode],
		"shared workspace label should be set")
	assert.True(t, grove.IsSharedWorkspace(), "grove should report as shared workspace")

	// Verify workspace was cloned (it's a git repo)
	workspacePath, err := hubNativeGrovePath(grove.Slug)
	require.NoError(t, err)
	t.Cleanup(func() { os.RemoveAll(workspacePath) })

	assert.True(t, util.IsGitRepoDir(workspacePath), "workspace should be a git repo")

	// Verify .scion directory was seeded
	scionDir := filepath.Join(workspacePath, ".scion")
	_, err = os.Stat(scionDir)
	assert.NoError(t, err, ".scion directory should exist for shared-workspace grove")
}

func TestCreateGrove_PerAgentGit_NoWorkspaceLabel(t *testing.T) {
	srv, _ := testServer(t)

	body := CreateGroveRequest{
		Name:      "Per-Agent Git Grove",
		GitRemote: "github.com/test/per-agent",
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/groves", body)
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	var grove store.Grove
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&grove))

	assert.Empty(t, grove.Labels[store.LabelWorkspaceMode],
		"per-agent git grove should not have workspace mode label")
	assert.False(t, grove.IsSharedWorkspace())
}

func TestPopulateAgentConfig_SharedWorkspace_SetsWorkspaceNotClone(t *testing.T) {
	srv, _ := testServer(t)

	grove := &store.Grove{
		ID:        "grove-shared-ws",
		Name:      "Shared WS",
		Slug:      "shared-ws",
		GitRemote: "github.com/test/shared",
		Labels: map[string]string{
			store.LabelWorkspaceMode: store.WorkspaceModeShared,
		},
	}

	agent := &store.Agent{
		ID:            "agent-shared",
		AppliedConfig: &store.AgentAppliedConfig{},
	}

	srv.populateAgentConfig(agent, grove, nil)

	expectedPath, err := hubNativeGrovePath("shared-ws")
	require.NoError(t, err)
	assert.Equal(t, expectedPath, agent.AppliedConfig.Workspace,
		"Workspace should be set for shared-workspace git groves")
	assert.Nil(t, agent.AppliedConfig.GitClone,
		"GitClone should NOT be set for shared-workspace git groves")
}

func TestPopulateAgentConfig_SharedWorkspace_DefaultsBranch(t *testing.T) {
	srv, _ := testServer(t)

	// Shared-workspace grove with explicit default branch label
	grove := &store.Grove{
		ID:        "grove-shared-branch",
		Name:      "Shared Branch",
		Slug:      "shared-branch",
		GitRemote: "github.com/test/shared-branch",
		Labels: map[string]string{
			store.LabelWorkspaceMode:   store.WorkspaceModeShared,
			"scion.dev/default-branch": "develop",
		},
	}

	agent := &store.Agent{
		ID:            "agent-branch-test",
		AppliedConfig: &store.AgentAppliedConfig{},
	}

	srv.populateAgentConfig(agent, grove, nil)

	assert.Equal(t, "develop", agent.AppliedConfig.Branch,
		"Branch should default to grove's default-branch label for shared workspace")

	// When branch is already set, it should not be overridden
	agent2 := &store.Agent{
		ID:            "agent-branch-test-2",
		AppliedConfig: &store.AgentAppliedConfig{Branch: "custom-branch"},
	}

	srv.populateAgentConfig(agent2, grove, nil)

	assert.Equal(t, "custom-branch", agent2.AppliedConfig.Branch,
		"Explicit branch should not be overridden by shared workspace default")

	// Without default-branch label, should default to "main"
	groveNoLabel := &store.Grove{
		ID:        "grove-shared-nolabel",
		Name:      "No Label",
		Slug:      "shared-nolabel",
		GitRemote: "github.com/test/nolabel",
		Labels: map[string]string{
			store.LabelWorkspaceMode: store.WorkspaceModeShared,
		},
	}

	agent3 := &store.Agent{
		ID:            "agent-branch-test-3",
		AppliedConfig: &store.AgentAppliedConfig{},
	}

	srv.populateAgentConfig(agent3, groveNoLabel, nil)

	assert.Equal(t, "main", agent3.AppliedConfig.Branch,
		"Branch should default to 'main' when no default-branch label is set")
}

func TestCloneSharedWorkspaceGrove_Success(t *testing.T) {
	srv, _ := testServer(t)

	// Create a local git repo to serve as the "remote"
	sourceDir := t.TempDir()
	for _, args := range [][]string{
		{"init"},
		{"config", "user.email", "test@test.com"},
		{"config", "user.name", "Test"},
		{"commit", "--allow-empty", "-m", "init"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = sourceDir
		require.NoError(t, cmd.Run(), "git %v", args)
	}

	grove := &store.Grove{
		ID:        "grove-clone-test",
		Name:      "Clone Test",
		Slug:      "clone-test-" + api.NewUUID()[:8],
		GitRemote: "local/test/repo",
		Labels: map[string]string{
			store.LabelWorkspaceMode:   store.WorkspaceModeShared,
			"scion.dev/clone-url":      sourceDir,
			"scion.dev/default-branch": "master",
		},
	}

	err := srv.cloneSharedWorkspaceGrove(context.Background(), grove)
	require.NoError(t, err)

	// Verify the workspace was created with a git repo
	workspacePath, err := hubNativeGrovePath(grove.Slug)
	require.NoError(t, err)
	t.Cleanup(func() { os.RemoveAll(workspacePath) })

	assert.True(t, util.IsGitRepoDir(workspacePath), "workspace should be a git repo")

	// Verify .scion directory was created
	scionDir := filepath.Join(workspacePath, ".scion")
	_, err = os.Stat(scionDir)
	assert.NoError(t, err, ".scion directory should exist in cloned workspace")

	// Verify git identity
	cmd := exec.Command("git", "-C", workspacePath, "config", "user.name")
	output, err := cmd.Output()
	require.NoError(t, err)
	assert.Equal(t, "Scion", strings.TrimSpace(string(output)))
}

func TestCloneSharedWorkspaceGrove_Failure_CleansUp(t *testing.T) {
	srv, _ := testServer(t)

	grove := &store.Grove{
		ID:        "grove-clone-fail",
		Name:      "Clone Fail",
		Slug:      "clone-fail-" + api.NewUUID()[:8],
		GitRemote: "github.com/nonexistent/repo",
		Labels: map[string]string{
			store.LabelWorkspaceMode: store.WorkspaceModeShared,
		},
	}

	err := srv.cloneSharedWorkspaceGrove(context.Background(), grove)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "shared workspace clone failed")

	// Verify the workspace directory was cleaned up
	workspacePath, pathErr := hubNativeGrovePath(grove.Slug)
	require.NoError(t, pathErr)
	_, statErr := os.Stat(workspacePath)
	assert.True(t, os.IsNotExist(statErr), "workspace directory should be cleaned up on clone failure")
}

func TestCreateGrove_SharedWorkspace_CloneFailure_RollsBackGrove(t *testing.T) {
	srv, st := testServer(t)

	body := CreateGroveRequest{
		Name:          "Clone Fail Grove",
		GitRemote:     "github.com/nonexistent/repo-that-does-not-exist",
		WorkspaceMode: "shared",
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/groves", body)
	// Clone failure returns 422 for classified errors (auth, not-found) or 500 for generic errors
	assert.True(t, rec.Code == http.StatusInternalServerError || rec.Code == http.StatusUnprocessableEntity,
		"shared workspace grove creation should fail when clone fails (got %d): %s", rec.Code, rec.Body.String())

	// Verify no grove record was left behind
	result, err := st.ListGroves(context.Background(), store.GroveFilter{
		Name: "Clone Fail Grove",
	}, store.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, result.Items, "grove record should be rolled back on clone failure")
}

func TestResolveCloneToken_NoCredentials(t *testing.T) {
	srv, _ := testServer(t)

	grove := &store.Grove{
		ID:        "grove-no-creds",
		GitRemote: "github.com/test/repo",
	}

	token := srv.resolveCloneToken(context.Background(), grove)
	assert.Empty(t, token, "should return empty when no credentials available")
}

func TestCreateGrove_AutoAssociatesGitHubInstallation(t *testing.T) {
	srv, st := testServer(t)
	ctx := context.Background()

	// Pre-register a GitHub App installation that covers "myorg/myrepo"
	inst := &store.GitHubInstallation{
		InstallationID: 77777,
		AccountLogin:   "myorg",
		AccountType:    "Organization",
		AppID:          1,
		Repositories:   []string{"myorg/myrepo"},
		Status:         store.GitHubInstallationStatusActive,
	}
	require.NoError(t, st.CreateGitHubInstallation(ctx, inst))

	// Create a grove whose git remote matches the installation's repo.
	// Use a local git repo as clone source so the clone actually succeeds.
	sourceDir := t.TempDir()
	for _, args := range [][]string{
		{"init"},
		{"config", "user.email", "test@test.com"},
		{"config", "user.name", "Test"},
		{"commit", "--allow-empty", "-m", "init"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = sourceDir
		require.NoError(t, cmd.Run(), "git %v", args)
	}

	body := CreateGroveRequest{
		Name:          "Auto Assoc Grove",
		GitRemote:     "github.com/myorg/myrepo",
		WorkspaceMode: "shared",
		Labels: map[string]string{
			"scion.dev/clone-url":      sourceDir,
			"scion.dev/default-branch": "master",
		},
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/groves", body)
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	var grove store.Grove
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&grove))

	// Clean up the cloned workspace
	workspacePath, err := hubNativeGrovePath(grove.Slug)
	require.NoError(t, err)
	t.Cleanup(func() { os.RemoveAll(workspacePath) })

	// Verify the grove was auto-associated with the installation
	updated, err := st.GetGrove(ctx, grove.ID)
	require.NoError(t, err)
	require.NotNil(t, updated.GitHubInstallationID,
		"grove should be auto-associated with GitHub App installation")
	assert.Equal(t, int64(77777), *updated.GitHubInstallationID)
}

func TestAutoAssociateGitHubInstallation_NoMatch(t *testing.T) {
	srv, st := testServer(t)
	ctx := context.Background()

	// Register an installation that covers a different repo
	inst := &store.GitHubInstallation{
		InstallationID: 88888,
		AccountLogin:   "otherorg",
		AccountType:    "Organization",
		AppID:          1,
		Repositories:   []string{"otherorg/otherrepo"},
		Status:         store.GitHubInstallationStatusActive,
	}
	require.NoError(t, st.CreateGitHubInstallation(ctx, inst))

	grove := &store.Grove{
		ID:        "grove-no-match",
		Name:      "No Match",
		Slug:      "no-match",
		GitRemote: "github.com/myorg/myrepo",
	}
	require.NoError(t, st.CreateGrove(ctx, grove))

	srv.autoAssociateGitHubInstallation(ctx, grove)

	assert.Nil(t, grove.GitHubInstallationID,
		"grove should not be associated when no installation matches")
}

func TestAutoAssociateGitHubInstallation_SkipsSuspended(t *testing.T) {
	srv, st := testServer(t)
	ctx := context.Background()

	// Register a suspended installation that covers the repo
	inst := &store.GitHubInstallation{
		InstallationID: 99999,
		AccountLogin:   "myorg",
		AccountType:    "Organization",
		AppID:          1,
		Repositories:   []string{"myorg/myrepo"},
		Status:         store.GitHubInstallationStatusSuspended,
	}
	require.NoError(t, st.CreateGitHubInstallation(ctx, inst))

	grove := &store.Grove{
		ID:        "grove-suspended",
		Name:      "Suspended",
		Slug:      "suspended",
		GitRemote: "github.com/myorg/myrepo",
	}
	require.NoError(t, st.CreateGrove(ctx, grove))

	srv.autoAssociateGitHubInstallation(ctx, grove)

	assert.Nil(t, grove.GitHubInstallationID,
		"grove should not be associated with a suspended installation")
}

func TestCreateGrove_DuplicateGitRemote_SerialSlug(t *testing.T) {
	srv, _ := testServer(t)

	// Create the first grove for a git remote.
	body1 := CreateGroveRequest{
		Name:      "widgets",
		GitRemote: "github.com/acme/widgets",
	}
	rec1 := doRequest(t, srv, http.MethodPost, "/api/v1/groves", body1)
	require.Equal(t, http.StatusCreated, rec1.Code, "body: %s", rec1.Body.String())

	var grove1 store.Grove
	require.NoError(t, json.NewDecoder(rec1.Body).Decode(&grove1))
	assert.Equal(t, "widgets", grove1.Slug)

	// Create a second grove for the same git remote.
	body2 := CreateGroveRequest{
		Name:      "widgets",
		GitRemote: "github.com/acme/widgets",
	}
	rec2 := doRequest(t, srv, http.MethodPost, "/api/v1/groves", body2)
	require.Equal(t, http.StatusCreated, rec2.Code, "body: %s", rec2.Body.String())

	var grove2 store.Grove
	require.NoError(t, json.NewDecoder(rec2.Body).Decode(&grove2))
	assert.Equal(t, "widgets-1", grove2.Slug, "second grove should get serial slug")
	assert.Equal(t, "widgets (1)", grove2.Name, "display name should have serial qualifier")
	assert.NotEqual(t, grove1.ID, grove2.ID, "groves should have different IDs")
	assert.Equal(t, grove1.GitRemote, grove2.GitRemote, "groves should share the same git remote")

	// Create a third grove.
	body3 := CreateGroveRequest{
		Name:      "widgets",
		GitRemote: "github.com/acme/widgets",
	}
	rec3 := doRequest(t, srv, http.MethodPost, "/api/v1/groves", body3)
	require.Equal(t, http.StatusCreated, rec3.Code, "body: %s", rec3.Body.String())

	var grove3 store.Grove
	require.NoError(t, json.NewDecoder(rec3.Body).Decode(&grove3))
	assert.Equal(t, "widgets-2", grove3.Slug, "third grove should get next serial slug")
	assert.Equal(t, "widgets (2)", grove3.Name)
}

func TestCreateGrove_ExplicitSlug_Unique(t *testing.T) {
	srv, _ := testServer(t)

	// Create first grove with an explicit slug.
	body1 := CreateGroveRequest{
		Name: "My Project",
		Slug: "my-project",
	}
	rec1 := doRequest(t, srv, http.MethodPost, "/api/v1/groves", body1)
	require.Equal(t, http.StatusCreated, rec1.Code, "body: %s", rec1.Body.String())

	var grove1 store.Grove
	require.NoError(t, json.NewDecoder(rec1.Body).Decode(&grove1))
	assert.Equal(t, "my-project", grove1.Slug)

	// Create second grove with the same explicit slug — should get serial suffix.
	body2 := CreateGroveRequest{
		Name: "My Project",
		Slug: "my-project",
	}
	rec2 := doRequest(t, srv, http.MethodPost, "/api/v1/groves", body2)
	require.Equal(t, http.StatusCreated, rec2.Code, "body: %s", rec2.Body.String())

	var grove2 store.Grove
	require.NoError(t, json.NewDecoder(rec2.Body).Decode(&grove2))
	assert.Equal(t, "my-project-1", grove2.Slug, "server should assign serial slug when explicit slug is taken")
}

func TestCreateGrove_ListByGitRemote_ReturnsMultiple(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Pre-create two groves for the same git remote.
	for _, g := range []*store.Grove{
		{ID: "g1", Name: "widgets", Slug: "widgets", GitRemote: "github.com/acme/widgets"},
		{ID: "g2", Name: "widgets (1)", Slug: "widgets-1", GitRemote: "github.com/acme/widgets"},
	} {
		require.NoError(t, s.CreateGrove(ctx, g))
	}

	// List groves by git remote should return both.
	rec := doRequest(t, srv, http.MethodGet, "/api/v1/groves?gitRemote=github.com/acme/widgets", nil)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var resp struct {
		Groves []store.Grove `json:"groves"`
	}
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Len(t, resp.Groves, 2, "listing by git remote should return all matching groves")
}
