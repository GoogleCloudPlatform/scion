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
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHubNativeProjectPath(t *testing.T) {
	path, err := hubNativeProjectPath("my-test-project")
	require.NoError(t, err)

	homeDir, err := os.UserHomeDir()
	require.NoError(t, err)

	expected := filepath.Join(homeDir, ".scion", "projects", "my-test-project")
	assert.Equal(t, expected, path)
}

func TestCreateProject_HubNative_NoGitRemote(t *testing.T) {
	srv, _ := testServer(t)

	body := CreateProjectRequest{
		Name: "Hub Native Project",
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects", body)
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	var project store.Project
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&project))

	assert.Equal(t, "Hub Native Project", project.Name)
	assert.Equal(t, "hub-native-project", project.Slug)
	assert.Empty(t, project.GitRemote, "hub-native project should have no git remote")

	// Verify the filesystem was initialized
	workspacePath, err := hubNativeProjectPath(project.Slug)
	require.NoError(t, err)

	scionDir := filepath.Join(workspacePath, ".scion")
	settingsPath := filepath.Join(scionDir, "settings.yaml")

	_, err = os.Stat(settingsPath)
	assert.NoError(t, err, "settings.yaml should exist for hub-native project")

	// Cleanup
	t.Cleanup(func() {
		os.RemoveAll(workspacePath)
	})
}

func TestCreateProject_GitBacked_NoFilesystemInit(t *testing.T) {
	srv, _ := testServer(t)

	body := CreateProjectRequest{
		Name:      "Git Project",
		GitRemote: "github.com/test/repo",
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects", body)
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	var project store.Project
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&project))

	assert.Equal(t, "github.com/test/repo", project.GitRemote)

	// Verify no filesystem was created for git-backed project
	workspacePath, err := hubNativeProjectPath(project.Slug)
	require.NoError(t, err)

	_, err = os.Stat(workspacePath)
	assert.True(t, os.IsNotExist(err), "no workspace directory should be created for git-backed projects")
}

func TestPopulateAgentConfig_HubNativeProject_SetsWorkspace(t *testing.T) {
	srv, _ := testServer(t)

	project := &store.Project{
		ID:   "project-hub-native",
		Name: "Hub Native",
		Slug: "hub-native",
		// No GitRemote — hub-native project
	}

	agent := &store.Agent{
		ID:            "agent-test",
		AppliedConfig: &store.AgentAppliedConfig{},
	}

	srv.populateAgentConfig(agent, project, nil)

	expectedPath, err := hubNativeProjectPath("hub-native")
	require.NoError(t, err)
	assert.Equal(t, expectedPath, agent.AppliedConfig.Workspace,
		"Workspace should be set for hub-native projects")
	assert.Nil(t, agent.AppliedConfig.GitClone,
		"GitClone should not be set for hub-native projects")
}

func TestPopulateAgentConfig_HubNativeProject_RemoteBroker_WorkspaceSet(t *testing.T) {
	srv, _ := testServer(t)

	project := &store.Project{
		ID:   "project-hub-native-remote",
		Name: "Hub Native Remote",
		Slug: "hub-native-remote",
		// No GitRemote — hub-native project
	}

	agent := &store.Agent{
		ID:            "agent-remote",
		AppliedConfig: &store.AgentAppliedConfig{},
	}

	srv.populateAgentConfig(agent, project, nil)

	// populateAgentConfig sets Workspace for hub-native projects.
	// For remote brokers, the createAgent handler later swaps this to
	// WorkspaceStoragePath. Here we verify the initial workspace is set.
	expectedPath, err := hubNativeProjectPath("hub-native-remote")
	require.NoError(t, err)
	assert.Equal(t, expectedPath, agent.AppliedConfig.Workspace)
}

func TestPopulateAgentConfig_GitProject_NoWorkspace(t *testing.T) {
	srv, _ := testServer(t)

	project := &store.Project{
		ID:        "project-git",
		Name:      "Git Project",
		Slug:      "git-project",
		GitRemote: "github.com/test/repo",
	}

	agent := &store.Agent{
		ID:            "agent-test",
		AppliedConfig: &store.AgentAppliedConfig{},
	}

	srv.populateAgentConfig(agent, project, nil)

	assert.Empty(t, agent.AppliedConfig.Workspace,
		"Workspace should not be set for git-backed projects")
	assert.NotNil(t, agent.AppliedConfig.GitClone,
		"GitClone should be set for git-backed projects")
}

func TestPopulateAgentConfig_TemplateTelemetryMerged(t *testing.T) {
	srv, _ := testServer(t)

	project := &store.Project{
		ID:   "project-telem",
		Name: "Telemetry Project",
		Slug: "telemetry-project",
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

	srv.populateAgentConfig(agent, project, template)

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

	project := &store.Project{
		ID:   "project-telem2",
		Name: "Telemetry Project 2",
		Slug: "telemetry-project-2",
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

	srv.populateAgentConfig(agent, project, template)

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

	project := &store.Project{
		ID:   "project-hub-tel",
		Name: "Hub Telemetry Project",
		Slug: "hub-telemetry-project",
	}

	agent := &store.Agent{
		ID:            "agent-hub-tel",
		AppliedConfig: &store.AgentAppliedConfig{},
	}

	srv.populateAgentConfig(agent, project, nil)

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

	project := &store.Project{ID: "project-hub-tel2", Slug: "hub-tel-project-2"}

	agent := &store.Agent{
		ID:            "agent-hub-tel2",
		AppliedConfig: &store.AgentAppliedConfig{},
	}

	srv.populateAgentConfig(agent, project, template)

	// Template telemetry should win over hub telemetry
	assert.Equal(t, "https://template-otel.example.com",
		agent.AppliedConfig.InlineConfig.Telemetry.Cloud.Endpoint,
		"Template telemetry should take precedence over hub default")
}

func TestPopulateAgentConfig_ProjectTelemetryEnabledOverride(t *testing.T) {
	srv, _ := testServer(t)

	// Set hub-level telemetry config with enabled=true
	hubEnabled := true
	srv.config.TelemetryConfig = &api.TelemetryConfig{
		Enabled: &hubEnabled,
		Cloud: &api.TelemetryCloudConfig{
			Endpoint: "https://hub-otel.example.com",
		},
	}

	// Project disables telemetry
	project := &store.Project{
		ID:   "project-tel-override",
		Slug: "tel-override-project",
		Annotations: map[string]string{
			projectSettingTelemetryEnabled: "false",
		},
	}

	agent := &store.Agent{
		ID:            "agent-tel-override",
		AppliedConfig: &store.AgentAppliedConfig{},
	}

	srv.populateAgentConfig(agent, project, nil)

	require.NotNil(t, agent.AppliedConfig.InlineConfig.Telemetry)
	// Hub cloud config should still be present
	assert.Equal(t, "https://hub-otel.example.com",
		agent.AppliedConfig.InlineConfig.Telemetry.Cloud.Endpoint)
	// But enabled should be overridden by project setting
	require.NotNil(t, agent.AppliedConfig.InlineConfig.Telemetry.Enabled)
	assert.False(t, *agent.AppliedConfig.InlineConfig.Telemetry.Enabled,
		"Project TelemetryEnabled=false should override hub Enabled=true")
}

func TestPopulateAgentConfig_ProjectTelemetryEnabledWithoutOtherConfig(t *testing.T) {
	srv, _ := testServer(t)

	// No hub telemetry config, no template — only project sets enabled
	project := &store.Project{
		ID:   "project-tel-only",
		Slug: "tel-only-project",
		Annotations: map[string]string{
			projectSettingTelemetryEnabled: "true",
		},
	}

	agent := &store.Agent{
		ID:            "agent-tel-only",
		AppliedConfig: &store.AgentAppliedConfig{},
	}

	srv.populateAgentConfig(agent, project, nil)

	require.NotNil(t, agent.AppliedConfig.InlineConfig)
	require.NotNil(t, agent.AppliedConfig.InlineConfig.Telemetry)
	require.NotNil(t, agent.AppliedConfig.InlineConfig.Telemetry.Enabled)
	assert.True(t, *agent.AppliedConfig.InlineConfig.Telemetry.Enabled,
		"Project TelemetryEnabled=true should create telemetry config with Enabled=true")
}

// TestCreateAgent_HubNativeProject_ExplicitBroker_AutoLinks tests that creating an agent
// in a hub-native project with an explicitly selected broker auto-links the broker as a
// provider, even if it wasn't previously registered as one.
func TestCreateAgent_HubNativeProject_ExplicitBroker_AutoLinks(t *testing.T) {
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

	// Create a hub-native project (no git remote, no default broker, no providers)
	project := &store.Project{
		ID:   "project-hub-autolink",
		Slug: "hub-autolink",
		Name: "Hub Autolink Project",
		// No GitRemote — hub-native
		// No DefaultRuntimeBrokerID
	}
	require.NoError(t, s.CreateProject(ctx, project))

	// Create agent with explicit broker — this should auto-link the broker
	body := map[string]interface{}{
		"name":            "autolink-agent",
		"projectId":         project.ID,
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
	provider, err := s.GetProjectProvider(ctx, project.ID, broker.ID)
	require.NoError(t, err, "Broker should have been auto-linked as a provider")
	assert.Equal(t, broker.ID, provider.BrokerID)
	assert.Equal(t, "agent-create", provider.LinkedBy)

	// Verify the broker was set as the default
	updatedProject, err := s.GetProject(ctx, project.ID)
	require.NoError(t, err)
	assert.Equal(t, broker.ID, updatedProject.DefaultRuntimeBrokerID,
		"Broker should be set as the default for the project")
}

// TestCreateProject_HubNative_AutoProvide tests that creating a hub-native project
// auto-links brokers with auto_provide enabled.
func TestCreateProject_HubNative_AutoProvide(t *testing.T) {
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

	// Create a hub-native project via the API
	body := CreateProjectRequest{
		Name: "Auto Provide Project",
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects", body)
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	var project store.Project
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&project))
	assert.Empty(t, project.GitRemote, "should be hub-native")

	// Verify the auto-provide broker was linked
	provider, err := s.GetProjectProvider(ctx, project.ID, broker.ID)
	require.NoError(t, err, "Auto-provide broker should be linked as a provider")
	assert.Equal(t, "auto-provide", provider.LinkedBy)

	// Verify the broker was set as the default
	updatedProject, err := s.GetProject(ctx, project.ID)
	require.NoError(t, err)
	assert.Equal(t, broker.ID, updatedProject.DefaultRuntimeBrokerID,
		"Auto-provide broker should be set as the default")

	// Now create an agent — should work without explicit broker
	agentBody := map[string]interface{}{
		"name":    "autoprovide-agent",
		"projectId": project.ID,
	}
	rec = doRequest(t, srv, http.MethodPost, "/api/v1/agents", agentBody)
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	var resp CreateAgentResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Equal(t, broker.ID, resp.Agent.RuntimeBrokerID,
		"Agent should use the auto-provided default broker")

	// Cleanup hub-native project filesystem
	workspacePath, err := hubNativeProjectPath(project.Slug)
	if err == nil {
		t.Cleanup(func() { os.RemoveAll(workspacePath) })
	}
}

// TestCreateAgent_HubNativeProject_NoProviders_NoBroker tests that creating an agent
// in a hub-native project with no providers and no explicit broker returns an appropriate error.
func TestDeleteProject_HubNative_RemovesFilesystem(t *testing.T) {
	srv, s := testServer(t)

	// Create a hub-native project via the API (initializes filesystem)
	project, workspacePath := createTestHubNativeProject(t, srv, "FS Delete Test")

	// Verify filesystem exists before deletion
	_, err := os.Stat(workspacePath)
	require.NoError(t, err, "workspace should exist before deletion")

	// Delete project via API
	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/projects/"+project.ID, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code)

	// Verify filesystem was removed
	_, err = os.Stat(workspacePath)
	assert.True(t, os.IsNotExist(err), "workspace should be deleted from filesystem")

	// Verify project deleted from database
	ctx := context.Background()
	_, err = s.GetProject(ctx, project.ID)
	assert.ErrorIs(t, err, store.ErrNotFound, "project should be deleted from database")
}

func TestDeleteProject_GitBacked_NoFilesystemCleanup(t *testing.T) {
	srv, s := testServer(t)

	// Create a git-backed project (no filesystem initialization)
	project := createTestGitProject(t, srv, "Git Delete Test", "github.com/test/git-delete-repo")

	// Delete project via API
	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/projects/"+project.ID, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code)

	// Verify project deleted from database
	ctx := context.Background()
	_, err := s.GetProject(ctx, project.ID)
	assert.ErrorIs(t, err, store.ErrNotFound, "project should be deleted from database")
}

func TestDeleteProject_DeleteAgents_DispatchesToBroker(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Set up a mock dispatcher to track agent deletion
	disp := &deleteDispatcher{}
	srv.SetDispatcher(disp)

	project, _, _ := setupOnlineBrokerAgent(t, s, "project-del")

	// Create a second agent in the same project
	agent2 := &store.Agent{
		ID:              "agent-online-project-del-2",
		Slug:            "agent-online-project-del-2-slug",
		Name:            "Agent Online project-del 2",
		ProjectID:         project.ID,
		RuntimeBrokerID: "broker-online-project-del",
		Phase:           string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, agent2))

	// Delete project — agents are always cascade-deleted
	rec := doRequest(t, srv, http.MethodDelete,
		"/api/v1/projects/"+project.ID, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code)

	// Verify dispatcher was called for both agents
	assert.Equal(t, 2, disp.deleteCalls,
		"DispatchAgentDelete should be called once per agent")

	// Verify project deleted from database
	_, err := s.GetProject(ctx, project.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)

	// Verify agents cascade-deleted from database
	_, err = s.GetAgent(ctx, "agent-online-project-del")
	assert.ErrorIs(t, err, store.ErrNotFound)
	_, err = s.GetAgent(ctx, agent2.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestDeleteProject_AlwaysCascadeDeletesAgents(t *testing.T) {
	srv, s := testServer(t)

	disp := &deleteDispatcher{}
	srv.SetDispatcher(disp)

	project, _, _ := setupOnlineBrokerAgent(t, s, "project-nodelflag")

	// Delete project without explicit deleteAgents param — agents should still be deleted
	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/projects/"+project.ID, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code)

	// Dispatcher should have been called (cascade delete is always on)
	assert.Equal(t, 1, disp.deleteCalls,
		"DispatchAgentDelete should always be called when deleting a project")

	// Project should be deleted from database
	ctx := context.Background()
	_, err := s.GetProject(ctx, project.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestCreateAgent_HubNativeProject_NoProviders_NoBroker(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create a hub-native project with no providers
	project := &store.Project{
		ID:   "project-hub-noproviders",
		Slug: "hub-noproviders",
		Name: "No Providers Project",
	}
	require.NoError(t, s.CreateProject(ctx, project))

	body := map[string]interface{}{
		"name":    "orphan-agent",
		"projectId": project.ID,
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", body)
	// Should fail because there are no providers and no broker specified
	assert.NotEqual(t, http.StatusCreated, rec.Code,
		"Should fail when no providers exist and no broker is specified")
}

// TestAutoLinkProviders_HubNativeProject_NoLocalPath verifies that autoLinkProviders
// does NOT set LocalPath on the provider for hub-native projects. The hub's local
// path is not valid for remote brokers — instead, projectSlug is sent so each
// broker resolves the path on its own filesystem.
func TestAutoLinkProviders_HubNativeProject_NoLocalPath(t *testing.T) {
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

	// Create a hub-native project via the API — this triggers autoLinkProviders
	body := CreateProjectRequest{
		Name: "LocalPath Auto Project",
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects", body)
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	var project store.Project
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&project))
	assert.Empty(t, project.GitRemote, "should be hub-native")

	// Verify the auto-linked provider does NOT have LocalPath set
	provider, err := s.GetProjectProvider(ctx, project.ID, broker.ID)
	require.NoError(t, err, "Auto-provide broker should be linked as a provider")
	assert.Equal(t, "auto-provide", provider.LinkedBy)
	assert.Empty(t, provider.LocalPath,
		"LocalPath should NOT be set for hub-native project auto-linked provider")

	// Cleanup hub-native project filesystem
	workspacePath, err := hubNativeProjectPath(project.Slug)
	if err == nil {
		t.Cleanup(func() { os.RemoveAll(workspacePath) })
	}
}

// TestAutoLinkProviders_GitProject_NoLocalPath verifies that autoLinkProviders
// does NOT set LocalPath on the provider for git-backed projects.
func TestAutoLinkProviders_GitProject_NoLocalPath(t *testing.T) {
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

	// Create a git-backed project via the API — this also triggers autoLinkProviders
	body := CreateProjectRequest{
		Name:      "LocalPath Git Project",
		GitRemote: "github.com/test/localpath-git-repo",
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects", body)
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	var project store.Project
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&project))

	// Verify the provider does NOT have LocalPath set
	provider, err := s.GetProjectProvider(ctx, project.ID, broker.ID)
	require.NoError(t, err, "Auto-provide broker should be linked")
	assert.Empty(t, provider.LocalPath,
		"LocalPath should NOT be set for git-backed project providers")
}

// TestDeleteProject_HubNative_DispatchesCleanupToBrokers verifies that deleting a
// hub-native project dispatches CleanupProject to each provider broker (except the
// embedded/co-located broker).
func TestDeleteProject_HubNative_DispatchesCleanupToBrokers(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create a hub-native project
	project := &store.Project{
		ID:   "project-cleanup-dispatch",
		Slug: "cleanup-dispatch",
		Name: "Cleanup Dispatch Project",
		// No GitRemote — hub-native
	}
	require.NoError(t, s.CreateProject(ctx, project))

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
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID:  project.ID,
		BrokerID: broker1.ID,
		LinkedBy: "test",
	}))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID:  project.ID,
		BrokerID: broker2.ID,
		LinkedBy: "test",
	}))

	// Set up a mock client and dispatcher
	mockClient := &mockRuntimeBrokerClient{}
	disp := NewHTTPAgentDispatcherWithClient(s, mockClient, false, slog.Default())
	srv.SetDispatcher(disp)

	// Delete project
	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/projects/"+project.ID, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code)

	// Verify CleanupProject was called for both brokers
	assert.Equal(t, 2, mockClient.cleanupCalls, "CleanupProject should be called for each provider broker")
	assert.Contains(t, mockClient.cleanupSlugs, "cleanup-dispatch")

	// Verify project deleted from database
	_, err := s.GetProject(ctx, project.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

// TestDeleteProject_HubNative_SkipsEmbeddedBroker verifies that the embedded broker
// (co-located hub+broker) is not called for cleanup since the hub handles its own copy.
func TestDeleteProject_HubNative_SkipsEmbeddedBroker(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create a hub-native project
	project := &store.Project{
		ID:   "project-cleanup-embedded",
		Slug: "cleanup-embedded",
		Name: "Cleanup Embedded Project",
	}
	require.NoError(t, s.CreateProject(ctx, project))

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
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID:  project.ID,
		BrokerID: embeddedBroker.ID,
		LinkedBy: "test",
	}))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID:  project.ID,
		BrokerID: remoteBroker.ID,
		LinkedBy: "test",
	}))

	// Mark embedded broker
	srv.SetEmbeddedBrokerID(embeddedBroker.ID)

	// Set up mock client and dispatcher
	mockClient := &mockRuntimeBrokerClient{}
	disp := NewHTTPAgentDispatcherWithClient(s, mockClient, false, slog.Default())
	srv.SetDispatcher(disp)

	// Delete project
	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/projects/"+project.ID, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code)

	// Only the remote broker should receive CleanupProject, not the embedded one
	assert.Equal(t, 1, mockClient.cleanupCalls, "CleanupProject should only be called for non-embedded brokers")
	assert.Contains(t, mockClient.cleanupSlugs, "cleanup-embedded")
}

// TestDeleteProject_GitBacked_NoCleanupDispatched verifies that deleting a git-backed
// project does NOT trigger broker cleanup (those directories are externally managed).
func TestDeleteProject_GitBacked_NoCleanupDispatched(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create a git-backed project
	project := &store.Project{
		ID:        "project-git-nocleanup",
		Slug:      "git-nocleanup",
		Name:      "Git No Cleanup Project",
		GitRemote: "github.com/test/nocleanup",
	}
	require.NoError(t, s.CreateProject(ctx, project))

	// Create a broker and link as provider
	broker := &store.RuntimeBroker{
		ID:       "broker-git-nocleanup",
		Slug:     "git-nocleanup-broker",
		Name:     "Git NoCleanup Broker",
		Status:   store.BrokerStatusOnline,
		Endpoint: "http://broker:9800",
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID:  project.ID,
		BrokerID: broker.ID,
		LinkedBy: "test",
	}))

	// Set up mock client and dispatcher
	mockClient := &mockRuntimeBrokerClient{}
	disp := NewHTTPAgentDispatcherWithClient(s, mockClient, false, slog.Default())
	srv.SetDispatcher(disp)

	// Delete project
	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/projects/"+project.ID, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code)

	// CleanupProject should NOT be called for git-backed projects
	assert.Equal(t, 0, mockClient.cleanupCalls, "CleanupProject should not be called for git-backed projects")
}

// TestResolveRuntimeBroker_HubNativeProject_NoLocalPath verifies that when a broker
// is auto-linked during agent creation for a hub-native project, LocalPath is NOT
// set. Remote brokers resolve the path themselves via projectSlug.
func TestResolveRuntimeBroker_HubNativeProject_NoLocalPath(t *testing.T) {
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

	// Create a hub-native project with no providers
	project := &store.Project{
		ID:   "project-resolve-localpath",
		Slug: "resolve-localpath",
		Name: "Resolve LocalPath Project",
	}
	require.NoError(t, s.CreateProject(ctx, project))

	// Create agent with explicit broker — triggers resolveRuntimeBroker auto-link
	agentBody := map[string]interface{}{
		"name":            "resolve-localpath-agent",
		"projectId":         project.ID,
		"runtimeBrokerId": broker.ID,
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", agentBody)
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	// Verify the auto-linked provider does NOT have LocalPath set
	provider, err := s.GetProjectProvider(ctx, project.ID, broker.ID)
	require.NoError(t, err, "Broker should have been auto-linked")
	assert.Equal(t, "agent-create", provider.LinkedBy)
	assert.Empty(t, provider.LocalPath,
		"LocalPath should NOT be set when auto-linking during agent creation for hub-native project")
}

// TestProjectRegisterPreservesProviderLocalPath verifies that re-registering a
// project from a local checkout does not overwrite an existing provider's empty
// localPath. This prevents a hub-native git project (where agents clone from a
// URL) from being accidentally converted into a linked project.
func TestProjectRegisterPreservesProviderLocalPath(t *testing.T) {
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

	// Step 1: Register project (creates it) — this is the initial hub-native creation.
	// The broker is linked WITH a localPath (simulating CLI-initiated creation).
	body := map[string]interface{}{
		"name":      "preserve-path-project",
		"gitRemote": "github.com/test/preserve-path",
		"brokerId":  broker.ID,
		"path":      "/original/path/.scion",
	}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/register", body)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var resp RegisterProjectResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	require.True(t, resp.Created, "project should be newly created")
	projectID := resp.Project.ID

	// Verify provider has localPath from initial registration
	provider, err := s.GetProjectProvider(ctx, projectID, broker.ID)
	require.NoError(t, err)
	assert.Equal(t, "/original/path/.scion", provider.LocalPath,
		"newly created project should have localPath from registration")

	// Now simulate converting to hub-native: clear localPath directly
	// (as autoLinkProviders would do, or via admin action)
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID:    projectID,
		BrokerID:   broker.ID,
		BrokerName: broker.Name,
		Status:     store.BrokerStatusOnline,
		LinkedBy:   "auto-provide",
		// LocalPath intentionally empty — hub-native provider
	}))

	// Verify localPath is now empty
	provider, err = s.GetProjectProvider(ctx, projectID, broker.ID)
	require.NoError(t, err)
	assert.Empty(t, provider.LocalPath, "provider should have no localPath after reset")

	// Step 2: Re-register from local checkout (CLI hubsync). This should NOT
	// overwrite the empty localPath with the new path.
	body2 := map[string]interface{}{
		"name":      "preserve-path-project",
		"gitRemote": "github.com/test/preserve-path",
		"brokerId":  broker.ID,
		"path":      "/new/local/checkout/.scion",
	}
	rec2 := doRequest(t, srv, http.MethodPost, "/api/v1/projects/register", body2)
	require.Equal(t, http.StatusOK, rec2.Code, "body: %s", rec2.Body.String())

	var resp2 RegisterProjectResponse
	require.NoError(t, json.NewDecoder(rec2.Body).Decode(&resp2))
	assert.False(t, resp2.Created, "project should already exist")

	// Verify the provider's localPath was preserved (still empty)
	provider, err = s.GetProjectProvider(ctx, projectID, broker.ID)
	require.NoError(t, err)
	assert.Empty(t, provider.LocalPath,
		"re-registration should not overwrite existing provider's empty localPath")
}

// TestCreateProject_GitBacked_RandomID verifies that projects created with a git
// remote (but no explicit ID) get random UUIDs, and that creating two projects
// for the same repository produces different IDs with serial-numbered slugs.
func TestCreateProject_GitBacked_RandomID(t *testing.T) {
	srv, _ := testServer(t)

	sshURL := "git@github.com:acme/widgets.git"
	httpsURL := "https://github.com/acme/widgets.git"

	// Create first project via SSH URL
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects", CreateProjectRequest{
		Name:      "Widgets",
		GitRemote: sshURL,
	})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	var project1 store.Project
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&project1))
	assert.NotEmpty(t, project1.ID)
	assert.Equal(t, "widgets", project1.Slug)

	// Create second project via HTTPS URL (same repo) — should create a NEW project
	rec = doRequest(t, srv, http.MethodPost, "/api/v1/projects", CreateProjectRequest{
		Name:      "Widgets",
		GitRemote: httpsURL,
	})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	var project2 store.Project
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&project2))
	assert.NotEmpty(t, project2.ID)
	assert.NotEqual(t, project1.ID, project2.ID, "two projects for same URL should have different IDs")
	assert.Equal(t, "widgets-1", project2.Slug, "second project should get serial-numbered slug")
	assert.Equal(t, "Widgets (1)", project2.Name, "second project should get serial display name")
}

// TestCreateProject_NoGitRemote_RandomID verifies that projects without a git
// remote get a random UUID.
func TestCreateProject_NoGitRemote_RandomID(t *testing.T) {
	srv, _ := testServer(t)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects", CreateProjectRequest{
		Name: "No Remote Project",
	})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	var project store.Project
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&project))
	assert.NotEmpty(t, project.ID)
}

// TestRegisterProject_GitBacked_RandomID verifies that the register endpoint
// assigns a random UUID (not deterministic) to projects created from a git remote.
func TestRegisterProject_GitBacked_RandomID(t *testing.T) {
	srv, _ := testServer(t)

	gitRemote := "git@github.com:acme/gadgets.git"

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/register", RegisterProjectRequest{
		Name:      "Gadgets",
		GitRemote: gitRemote,
	})
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var resp RegisterProjectResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.NotEmpty(t, resp.Project.ID)
	assert.True(t, resp.Created)

	// ID should NOT be the deterministic hash — it should be a random UUID
	deterministicID := util.HashProjectID(util.NormalizeGitRemote(gitRemote))
	assert.NotEqual(t, deterministicID, resp.Project.ID, "registered project ID should be random, not deterministic")
}

// TestDeleteProject_CascadesEnvVarsSecretsHarnessConfigs verifies that deleting a
// project removes all project-scoped env vars, secrets, and harness configs.
func TestDeleteProject_CascadesEnvVarsSecretsHarnessConfigs(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := createTestGitProject(t, srv, "Cascade Resources Test", "github.com/test/cascade-resources")

	// Create project-scoped env vars
	require.NoError(t, s.CreateEnvVar(ctx, &store.EnvVar{
		ID: api.NewUUID(), Key: "LOG_LEVEL", Value: "debug",
		Scope: store.ScopeProject, ScopeID: project.ID,
	}))
	require.NoError(t, s.CreateEnvVar(ctx, &store.EnvVar{
		ID: api.NewUUID(), Key: "REGION", Value: "us-east-1",
		Scope: store.ScopeProject, ScopeID: project.ID,
	}))

	// Create project-scoped secrets
	require.NoError(t, s.CreateSecret(ctx, &store.Secret{
		ID: api.NewUUID(), Key: "API_KEY", EncryptedValue: "enc-val-1",
		Scope: store.ScopeProject, ScopeID: project.ID, Version: 1,
	}))

	// Create project-scoped harness config
	require.NoError(t, s.CreateHarnessConfig(ctx, &store.HarnessConfig{
		ID: api.NewUUID(), Name: "project-hc", Slug: "project-hc",
		Harness: "claude", Scope: store.ScopeProject, ScopeID: project.ID,
		Status: store.HarnessConfigStatusActive, Visibility: store.VisibilityPrivate,
	}))

	// Create project-scoped templates
	require.NoError(t, s.CreateTemplate(ctx, &store.Template{
		ID: api.NewUUID(), Name: "project-tmpl", Slug: "project-tmpl",
		Harness: "claude", Scope: store.ScopeProject, ScopeID: project.ID,
		Status: store.TemplateStatusActive, Visibility: store.VisibilityPrivate,
	}))

	// Also create a hub-scoped env var that should NOT be deleted
	hubEnvVarID := api.NewUUID()
	require.NoError(t, s.CreateEnvVar(ctx, &store.EnvVar{
		ID: hubEnvVarID, Key: "GLOBAL_VAR", Value: "keep-me",
		Scope: store.ScopeHub, ScopeID: "test-hub-id",
	}))

	// Verify resources exist before deletion
	envVars, err := s.ListEnvVars(ctx, store.EnvVarFilter{Scope: store.ScopeProject, ScopeID: project.ID})
	require.NoError(t, err)
	assert.Len(t, envVars, 2)

	secrets, err := s.ListSecrets(ctx, store.SecretFilter{Scope: store.ScopeProject, ScopeID: project.ID})
	require.NoError(t, err)
	assert.Len(t, secrets, 1)

	// Delete project via API
	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/projects/"+project.ID, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code)

	// Verify project-scoped env vars were deleted
	envVars, err = s.ListEnvVars(ctx, store.EnvVarFilter{Scope: store.ScopeProject, ScopeID: project.ID})
	require.NoError(t, err)
	assert.Empty(t, envVars, "project env vars should be cascade deleted")

	// Verify project-scoped secrets were deleted
	secrets, err = s.ListSecrets(ctx, store.SecretFilter{Scope: store.ScopeProject, ScopeID: project.ID})
	require.NoError(t, err)
	assert.Empty(t, secrets, "project secrets should be cascade deleted")

	// Verify project-scoped harness configs were deleted
	hcResult, err := s.ListHarnessConfigs(ctx, store.HarnessConfigFilter{Scope: store.ScopeProject, ScopeID: project.ID}, store.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, hcResult.Items, "project harness configs should be cascade deleted")

	// Verify project-scoped templates were deleted
	tmplResult, err := s.ListTemplates(ctx, store.TemplateFilter{Scope: store.ScopeProject, ScopeID: project.ID}, store.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, tmplResult.Items, "project templates should be cascade deleted")

	// Verify hub-scoped env var was NOT deleted
	hubVars, err := s.ListEnvVars(ctx, store.EnvVarFilter{Scope: store.ScopeHub, ScopeID: "test-hub-id"})
	require.NoError(t, err)
	assert.Len(t, hubVars, 1, "hub-scoped env var should not be affected")
}

// TestDeleteProject_CleansUpProjectConfigsDir verifies that deleting a project
// removes the ~/.scion/project-configs/<slug>__<short-uuid>/ directory.
func TestDeleteProject_CleansUpProjectConfigsDir(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := createTestGitProject(t, srv, "Config Cleanup Test", "github.com/test/config-cleanup-repo")

	// Create the project-configs directory that would exist in workstation mode
	marker := &config.ProjectMarker{
		ProjectID:   project.ID,
		ProjectSlug: project.Slug,
	}
	extPath, err := marker.ExternalProjectPath()
	require.NoError(t, err)

	// Create the directory structure: ~/.scion/project-configs/<slug>__<uuid>/.scion/
	require.NoError(t, os.MkdirAll(extPath, 0755))
	// Also create an agents/ sibling directory
	agentsDir := filepath.Join(filepath.Dir(extPath), "agents", "test-agent", "home")
	require.NoError(t, os.MkdirAll(agentsDir, 0755))

	projectConfigDir := filepath.Dir(extPath)
	t.Cleanup(func() { os.RemoveAll(projectConfigDir) })

	// Verify directory exists before deletion
	_, err = os.Stat(projectConfigDir)
	require.NoError(t, err, "project-configs dir should exist before deletion")

	// Delete project via API
	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/projects/"+project.ID, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code)

	// Verify project-configs directory was removed
	_, err = os.Stat(projectConfigDir)
	assert.True(t, os.IsNotExist(err), "project-configs dir should be removed after project deletion")

	// Verify project deleted from database
	_, err = s.GetProject(ctx, project.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

// TestProjectRegister_CreatesMembershipGroup verifies that registering a new project
// automatically creates a membership group with the caller as owner.
func TestProjectRegister_CreatesMembershipGroup(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/register", RegisterProjectRequest{
		Name:      "Membership Test",
		GitRemote: "https://github.com/test/membership-test.git",
	})
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var resp RegisterProjectResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	require.True(t, resp.Created)

	// Members group should exist
	membersSlug := "project:" + resp.Project.Slug + ":members"
	group, err := s.GetGroupBySlug(ctx, membersSlug)
	require.NoError(t, err, "members group should have been created")
	assert.Equal(t, resp.Project.ID, group.ProjectID)

	// The dev user should be an owner
	members, err := s.GetGroupMembers(ctx, group.ID)
	require.NoError(t, err)
	require.Len(t, members, 1, "should have exactly one member (the creator)")
	assert.Equal(t, DevUserID, members[0].MemberID)
	assert.Equal(t, store.GroupMemberRoleOwner, members[0].Role)
}

// TestProjectRegister_ExistingProject_CreatesMembershipGroup verifies that
// registering against an existing project (linking) still creates the membership
// group and adds the linking user as owner.
func TestProjectRegister_ExistingProject_CreatesMembershipGroup(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create a project directly in the store (simulating one created before
	// membership group support was added — no group exists yet).
	project := &store.Project{
		ID:        api.NewUUID(),
		Name:      "Pre-Existing Project",
		Slug:      "pre-existing-project",
		GitRemote: "github.com/test/pre-existing",
		CreatedBy: "original-creator-id",
	}
	require.NoError(t, s.CreateProject(ctx, project))

	// Verify no members group exists yet
	_, err := s.GetGroupBySlug(ctx, "project:"+project.Slug+":members")
	require.ErrorIs(t, err, store.ErrNotFound, "members group should not exist yet")

	// Register (link) via the API — this should backfill the group
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/register", RegisterProjectRequest{
		ID:   project.ID,
		Name: project.Name,
	})
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var resp RegisterProjectResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.False(t, resp.Created, "should find existing project")

	// Members group should now exist
	membersSlug := "project:" + project.Slug + ":members"
	group, err := s.GetGroupBySlug(ctx, membersSlug)
	require.NoError(t, err, "members group should have been created on link")
	assert.Equal(t, project.ID, group.ProjectID)

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

func TestCreateProject_SharedWorkspace_SetsLabelAndInitFilesystem(t *testing.T) {
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

	body := CreateProjectRequest{
		Name:          "Shared WS Project",
		GitRemote:     "github.com/test/shared-ws",
		WorkspaceMode: "shared",
		Labels: map[string]string{
			"scion.dev/clone-url":      sourceDir,
			"scion.dev/default-branch": "master",
		},
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects", body)
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	var project store.Project
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&project))

	assert.Equal(t, "github.com/test/shared-ws", project.GitRemote)
	assert.Equal(t, store.WorkspaceModeShared, project.Labels[store.LabelWorkspaceMode],
		"shared workspace label should be set")
	assert.True(t, project.IsSharedWorkspace(), "project should report as shared workspace")

	// Verify workspace was cloned (it's a git repo)
	workspacePath, err := hubNativeProjectPath(project.Slug)
	require.NoError(t, err)
	t.Cleanup(func() { os.RemoveAll(workspacePath) })

	assert.True(t, util.IsGitRepoDir(workspacePath), "workspace should be a git repo")

	// Verify .scion directory was seeded
	scionDir := filepath.Join(workspacePath, ".scion")
	_, err = os.Stat(scionDir)
	assert.NoError(t, err, ".scion directory should exist for shared-workspace project")
}

func TestCreateProject_PerAgentGit_NoWorkspaceLabel(t *testing.T) {
	srv, _ := testServer(t)

	body := CreateProjectRequest{
		Name:      "Per-Agent Git Project",
		GitRemote: "github.com/test/per-agent",
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects", body)
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	var project store.Project
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&project))

	assert.Empty(t, project.Labels[store.LabelWorkspaceMode],
		"per-agent git project should not have workspace mode label")
	assert.False(t, project.IsSharedWorkspace())
}

func TestPopulateAgentConfig_SharedWorkspace_SetsWorkspaceNotClone(t *testing.T) {
	srv, _ := testServer(t)

	project := &store.Project{
		ID:        "project-shared-ws",
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

	srv.populateAgentConfig(agent, project, nil)

	expectedPath, err := hubNativeProjectPath("shared-ws")
	require.NoError(t, err)
	assert.Equal(t, expectedPath, agent.AppliedConfig.Workspace,
		"Workspace should be set for shared-workspace git projects")
	assert.Nil(t, agent.AppliedConfig.GitClone,
		"GitClone should NOT be set for shared-workspace git projects")
}

func TestPopulateAgentConfig_SharedWorkspace_DefaultsBranch(t *testing.T) {
	srv, _ := testServer(t)

	// Shared-workspace project with explicit default branch label
	project := &store.Project{
		ID:        "project-shared-branch",
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

	srv.populateAgentConfig(agent, project, nil)

	assert.Equal(t, "develop", agent.AppliedConfig.Branch,
		"Branch should default to project's default-branch label for shared workspace")

	// When branch is already set, it should not be overridden
	agent2 := &store.Agent{
		ID:            "agent-branch-test-2",
		AppliedConfig: &store.AgentAppliedConfig{Branch: "custom-branch"},
	}

	srv.populateAgentConfig(agent2, project, nil)

	assert.Equal(t, "custom-branch", agent2.AppliedConfig.Branch,
		"Explicit branch should not be overridden by shared workspace default")

	// Without default-branch label, should default to "main"
	projectNoLabel := &store.Project{
		ID:        "project-shared-nolabel",
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

	srv.populateAgentConfig(agent3, projectNoLabel, nil)

	assert.Equal(t, "main", agent3.AppliedConfig.Branch,
		"Branch should default to 'main' when no default-branch label is set")
}

func TestCloneSharedWorkspaceProject_Success(t *testing.T) {
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

	project := &store.Project{
		ID:        "project-clone-test",
		Name:      "Clone Test",
		Slug:      "clone-test-" + api.NewUUID()[:8],
		GitRemote: "local/test/repo",
		Labels: map[string]string{
			store.LabelWorkspaceMode:   store.WorkspaceModeShared,
			"scion.dev/clone-url":      sourceDir,
			"scion.dev/default-branch": "master",
		},
	}

	err := srv.cloneSharedWorkspaceProject(context.Background(), project)
	require.NoError(t, err)

	// Verify the workspace was created with a git repo
	workspacePath, err := hubNativeProjectPath(project.Slug)
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

func TestCloneSharedWorkspaceProject_Failure_CleansUp(t *testing.T) {
	srv, _ := testServer(t)

	project := &store.Project{
		ID:        "project-clone-fail",
		Name:      "Clone Fail",
		Slug:      "clone-fail-" + api.NewUUID()[:8],
		GitRemote: "github.com/nonexistent/repo",
		Labels: map[string]string{
			store.LabelWorkspaceMode: store.WorkspaceModeShared,
		},
	}

	err := srv.cloneSharedWorkspaceProject(context.Background(), project)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "shared workspace clone failed")

	// Verify the workspace directory was cleaned up
	workspacePath, pathErr := hubNativeProjectPath(project.Slug)
	require.NoError(t, pathErr)
	_, statErr := os.Stat(workspacePath)
	assert.True(t, os.IsNotExist(statErr), "workspace directory should be cleaned up on clone failure")
}

func TestCreateProject_SharedWorkspace_CloneFailure_RollsBackProject(t *testing.T) {
	srv, st := testServer(t)

	body := CreateProjectRequest{
		Name:          "Clone Fail Project",
		GitRemote:     "github.com/nonexistent/repo-that-does-not-exist",
		WorkspaceMode: "shared",
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects", body)
	// Clone failure returns 422 for classified errors (auth, not-found) or 500 for generic errors
	assert.True(t, rec.Code == http.StatusInternalServerError || rec.Code == http.StatusUnprocessableEntity,
		"shared workspace project creation should fail when clone fails (got %d): %s", rec.Code, rec.Body.String())

	// Verify no project record was left behind
	result, err := st.ListProjects(context.Background(), store.ProjectFilter{
		Name: "Clone Fail Project",
	}, store.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, result.Items, "project record should be rolled back on clone failure")
}

func TestResolveCloneToken_NoCredentials(t *testing.T) {
	srv, _ := testServer(t)

	project := &store.Project{
		ID:        "project-no-creds",
		GitRemote: "github.com/test/repo",
	}

	token := srv.resolveCloneToken(context.Background(), project)
	assert.Empty(t, token, "should return empty when no credentials available")
}

func TestResolveCloneToken_FallsBackToCreatorUserToken(t *testing.T) {
	srv, st := testServer(t)
	ctx := context.Background()

	require.NoError(t, st.CreateSecret(ctx, &store.Secret{
		ID:             "sec-user-gh",
		Key:            "GITHUB_TOKEN",
		EncryptedValue: "ghp_user_token_123",
		SecretType:     store.SecretTypeEnvironment,
		Target:         "GITHUB_TOKEN",
		Scope:          store.ScopeUser,
		ScopeID:        "creator-user-1",
	}))

	backend := secret.NewLocalBackend(st, "test-hub-id")
	srv.SetSecretBackend(backend)

	project := &store.Project{
		ID:        "project-bootstrap",
		GitRemote: "github.com/test/private-repo",
		CreatedBy: "creator-user-1",
	}

	token := srv.resolveCloneToken(ctx, project)
	assert.Equal(t, "ghp_user_token_123", token, "should fall back to creator's user-scoped GITHUB_TOKEN")
}

func TestResolveCloneToken_PrefersProjectTokenOverUserToken(t *testing.T) {
	srv, st := testServer(t)
	ctx := context.Background()

	require.NoError(t, st.CreateSecret(ctx, &store.Secret{
		ID:             "sec-project-gh",
		Key:            "GITHUB_TOKEN",
		EncryptedValue: "ghp_project_token",
		SecretType:     store.SecretTypeEnvironment,
		Target:         "GITHUB_TOKEN",
		Scope:          store.ScopeProject,
		ScopeID:        "project-with-both",
	}))
	require.NoError(t, st.CreateSecret(ctx, &store.Secret{
		ID:             "sec-user-gh-2",
		Key:            "GITHUB_TOKEN",
		EncryptedValue: "ghp_user_token",
		SecretType:     store.SecretTypeEnvironment,
		Target:         "GITHUB_TOKEN",
		Scope:          store.ScopeUser,
		ScopeID:        "creator-user-2",
	}))

	backend := secret.NewLocalBackend(st, "test-hub-id")
	srv.SetSecretBackend(backend)

	project := &store.Project{
		ID:        "project-with-both",
		GitRemote: "github.com/test/repo",
		CreatedBy: "creator-user-2",
	}

	token := srv.resolveCloneToken(ctx, project)
	assert.Equal(t, "ghp_project_token", token, "should prefer project-scoped token over user-scoped token")
}

func TestCreateProject_AutoAssociatesGitHubInstallation(t *testing.T) {
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

	// Create a project whose git remote matches the installation's repo.
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

	body := CreateProjectRequest{
		Name:          "Auto Assoc Project",
		GitRemote:     "github.com/myorg/myrepo",
		WorkspaceMode: "shared",
		Labels: map[string]string{
			"scion.dev/clone-url":      sourceDir,
			"scion.dev/default-branch": "master",
		},
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects", body)
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	var project store.Project
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&project))

	// Clean up the cloned workspace
	workspacePath, err := hubNativeProjectPath(project.Slug)
	require.NoError(t, err)
	t.Cleanup(func() { os.RemoveAll(workspacePath) })

	// Verify the project was auto-associated with the installation
	updated, err := st.GetProject(ctx, project.ID)
	require.NoError(t, err)
	require.NotNil(t, updated.GitHubInstallationID,
		"project should be auto-associated with GitHub App installation")
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

	project := &store.Project{
		ID:        "project-no-match",
		Name:      "No Match",
		Slug:      "no-match",
		GitRemote: "github.com/myorg/myrepo",
	}
	require.NoError(t, st.CreateProject(ctx, project))

	srv.autoAssociateGitHubInstallation(ctx, project)

	assert.Nil(t, project.GitHubInstallationID,
		"project should not be associated when no installation matches")
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

	project := &store.Project{
		ID:        "project-suspended",
		Name:      "Suspended",
		Slug:      "suspended",
		GitRemote: "github.com/myorg/myrepo",
	}
	require.NoError(t, st.CreateProject(ctx, project))

	srv.autoAssociateGitHubInstallation(ctx, project)

	assert.Nil(t, project.GitHubInstallationID,
		"project should not be associated with a suspended installation")
}

func TestCreateProject_DuplicateGitRemote_SerialSlug(t *testing.T) {
	srv, _ := testServer(t)

	// Create the first project for a git remote.
	body1 := CreateProjectRequest{
		Name:      "widgets",
		GitRemote: "github.com/acme/widgets",
	}
	rec1 := doRequest(t, srv, http.MethodPost, "/api/v1/projects", body1)
	require.Equal(t, http.StatusCreated, rec1.Code, "body: %s", rec1.Body.String())

	var project1 store.Project
	require.NoError(t, json.NewDecoder(rec1.Body).Decode(&project1))
	assert.Equal(t, "widgets", project1.Slug)

	// Create a second project for the same git remote.
	body2 := CreateProjectRequest{
		Name:      "widgets",
		GitRemote: "github.com/acme/widgets",
	}
	rec2 := doRequest(t, srv, http.MethodPost, "/api/v1/projects", body2)
	require.Equal(t, http.StatusCreated, rec2.Code, "body: %s", rec2.Body.String())

	var project2 store.Project
	require.NoError(t, json.NewDecoder(rec2.Body).Decode(&project2))
	assert.Equal(t, "widgets-1", project2.Slug, "second project should get serial slug")
	assert.Equal(t, "widgets (1)", project2.Name, "display name should have serial qualifier")
	assert.NotEqual(t, project1.ID, project2.ID, "projects should have different IDs")
	assert.Equal(t, project1.GitRemote, project2.GitRemote, "projects should share the same git remote")

	// Create a third project.
	body3 := CreateProjectRequest{
		Name:      "widgets",
		GitRemote: "github.com/acme/widgets",
	}
	rec3 := doRequest(t, srv, http.MethodPost, "/api/v1/projects", body3)
	require.Equal(t, http.StatusCreated, rec3.Code, "body: %s", rec3.Body.String())

	var project3 store.Project
	require.NoError(t, json.NewDecoder(rec3.Body).Decode(&project3))
	assert.Equal(t, "widgets-2", project3.Slug, "third project should get next serial slug")
	assert.Equal(t, "widgets (2)", project3.Name)
}

func TestCreateProject_ExplicitSlug_Unique(t *testing.T) {
	srv, _ := testServer(t)

	// Create first project with an explicit slug.
	body1 := CreateProjectRequest{
		Name: "My Project",
		Slug: "my-project",
	}
	rec1 := doRequest(t, srv, http.MethodPost, "/api/v1/projects", body1)
	require.Equal(t, http.StatusCreated, rec1.Code, "body: %s", rec1.Body.String())

	var project1 store.Project
	require.NoError(t, json.NewDecoder(rec1.Body).Decode(&project1))
	assert.Equal(t, "my-project", project1.Slug)

	// Create second project with the same explicit slug — should get serial suffix.
	body2 := CreateProjectRequest{
		Name: "My Project",
		Slug: "my-project",
	}
	rec2 := doRequest(t, srv, http.MethodPost, "/api/v1/projects", body2)
	require.Equal(t, http.StatusCreated, rec2.Code, "body: %s", rec2.Body.String())

	var project2 store.Project
	require.NoError(t, json.NewDecoder(rec2.Body).Decode(&project2))
	assert.Equal(t, "my-project-1", project2.Slug, "server should assign serial slug when explicit slug is taken")
}

func TestCreateProject_ListByGitRemote_ReturnsMultiple(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Pre-create two projects for the same git remote.
	for _, g := range []*store.Project{
		{ID: "g1", Name: "widgets", Slug: "widgets", GitRemote: "github.com/acme/widgets"},
		{ID: "g2", Name: "widgets (1)", Slug: "widgets-1", GitRemote: "github.com/acme/widgets"},
	} {
		require.NoError(t, s.CreateProject(ctx, g))
	}

	// List projects by git remote should return both.
	rec := doRequest(t, srv, http.MethodGet, "/api/v1/projects?gitRemote=github.com/acme/widgets", nil)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var resp struct {
		Projects []store.Project `json:"projects"`
	}
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Len(t, resp.Projects, 2, "listing by git remote should return all matching projects")
}
