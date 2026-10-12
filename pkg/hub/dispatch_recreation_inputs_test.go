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
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// recreationFixture is a project, broker and agent whose applied config
// carries every workspace-recreation input: a git clone, a branch, a Hub
// template identity and a Hub harness-config identity (ptone/scion#2157).
type recreationFixture struct {
	dispatcher *HTTPAgentDispatcher
	client     *mockRuntimeBrokerClient
	agent      *store.Agent
	gitClone   *api.GitCloneConfig
}

// newRecreationFixture builds the fixture. With localPath set the broker has
// a registered provider path for the project (a linked project); without it
// the project is resolved on the broker by slug (a hub-native project).
func newRecreationFixture(t *testing.T, localPath string) *recreationFixture {
	t.Helper()
	ctx := context.Background()
	memStore := createTestStore(t)

	project := &store.Project{
		ID:         tid("project-rec"),
		Name:       "recreation-project",
		Slug:       "recreation-project",
		GitRemote:  "https://github.com/example/repo.git",
		SharedDirs: []api.SharedDir{{Name: "cache"}},
		Labels: map[string]string{
			store.LabelWorkspaceMode: store.WorkspaceModeWorktreePerAgent,
		},
	}
	require.NoError(t, memStore.CreateProject(ctx, project))

	broker := &store.RuntimeBroker{
		ID:       tid("broker-rec"),
		Name:     "test-broker",
		Slug:     "test-broker",
		Endpoint: "http://localhost:9800",
		Status:   store.BrokerStatusOnline,
	}
	require.NoError(t, memStore.CreateRuntimeBroker(ctx, broker))

	if localPath != "" {
		require.NoError(t, memStore.AddProjectProvider(ctx, &store.ProjectProvider{
			ProjectID:  project.ID,
			BrokerID:   broker.ID,
			BrokerName: broker.Name,
			LocalPath:  localPath,
			Status:     store.BrokerStatusOnline,
		}))
	}

	client := &mockRuntimeBrokerClient{}
	dispatcher := NewHTTPAgentDispatcherWithClient(memStore, client, false, slog.Default())

	gitClone := &api.GitCloneConfig{URL: "https://github.com/example/repo.git"}
	agent := &store.Agent{
		ID:              tid("agent-rec"),
		Name:            "rec-agent",
		Slug:            "rec-agent",
		Template:        "web-dev",
		ProjectID:       project.ID,
		OwnerID:         "owner-rec",
		RuntimeBrokerID: broker.ID,
		AppliedConfig: &store.AgentAppliedConfig{
			GitClone:          gitClone,
			Branch:            "feature-branch",
			TemplateID:        "template-id-1",
			TemplateHash:      "sha256:template-hash-1",
			HarnessConfig:     "claude",
			HarnessConfigID:   "hc-id-1",
			HarnessConfigHash: "sha256:hc-hash-1",
		},
	}
	return &recreationFixture{dispatcher: dispatcher, client: client, agent: agent, gitClone: gitClone}
}

// TestDispatchAgentStart_CarriesTemplateIdentity proves a start sends the
// agent's Hub template identity (TemplateID and TemplateHash) next to the
// template name, so a broker that has to provision the agent again
// hydrates the agent's own template instead of the default one.
func TestDispatchAgentStart_CarriesTemplateIdentity(t *testing.T) {
	f := newRecreationFixture(t, "")
	require.NoError(t, f.dispatcher.DispatchAgentStart(context.Background(), f.agent, "", false))

	extras := f.client.lastStartExtras
	assert.Equal(t, "web-dev", extras.TemplateName)
	assert.Equal(t, "template-id-1", extras.TemplateID)
	assert.Equal(t, "sha256:template-hash-1", extras.TemplateHash)
	// The start request carries these as its own parameters, never as
	// restart-only extras.
	assert.Empty(t, extras.ProjectPath)
	assert.Empty(t, extras.ProjectSlug)
	assert.Empty(t, extras.HarnessConfig)
	assert.Empty(t, extras.SharedDirs)
}

// TestDispatchAgentRestart_CarriesStartInputs proves a restart sends the
// inputs a start sends (project path, harness config, shared dirs, the
// workspace spec and the template identity), so the broker can resolve the
// agent's project and recreate its workspace without a live container.
func TestDispatchAgentRestart_CarriesStartInputs(t *testing.T) {
	f := newRecreationFixture(t, "/home/user/projects/rec/.scion")
	require.NoError(t, f.dispatcher.DispatchAgentRestart(context.Background(), f.agent))
	require.True(t, f.client.restartCalled)

	extras := f.client.lastRestartExtras
	assert.Equal(t, "/home/user/projects/rec/.scion", extras.ProjectPath)
	assert.Empty(t, extras.ProjectSlug, "a provider path is sent instead of the slug")
	assert.Same(t, f.gitClone, extras.Workspace.GitClone)
	assert.Equal(t, "feature-branch", extras.Workspace.Branch)
	assert.Equal(t, store.WorkspaceModeWorktreePerAgent, extras.Workspace.WorkspaceMode)
	assert.Equal(t, "web-dev", extras.TemplateName)
	assert.Equal(t, "template-id-1", extras.TemplateID)
	assert.Equal(t, "sha256:template-hash-1", extras.TemplateHash)
	assert.Equal(t, "claude", extras.HarnessConfig)
	assert.Equal(t, "hc-id-1", extras.HarnessConfigID)
	assert.Equal(t, "sha256:hc-hash-1", extras.HarnessConfigHash)
	assert.Equal(t, []api.SharedDir{{Name: "cache"}}, extras.SharedDirs)
}

// TestDispatchAgentRestart_HubNativeProjectSendsSlug proves a restart of an
// agent in a project with no provider path sends the project slug, the same
// value DispatchAgentStart sends.
func TestDispatchAgentRestart_HubNativeProjectSendsSlug(t *testing.T) {
	f := newRecreationFixture(t, "")
	require.NoError(t, f.dispatcher.DispatchAgentRestart(context.Background(), f.agent))
	require.NoError(t, f.dispatcher.DispatchAgentStart(context.Background(), f.agent, "", false))

	assert.Empty(t, f.client.lastRestartExtras.ProjectPath)
	assert.Equal(t, "recreation-project", f.client.lastRestartExtras.ProjectSlug)
	assert.Equal(t, f.client.lastProjectSlug, f.client.lastRestartExtras.ProjectSlug,
		"restart and start must name the project the same way")
}

// TestApplyStartExtras_RecreationInputs proves the shared payload builder
// writes the template identity and the restart inputs under the wire keys
// the broker's start and restart handlers decode, and writes none of them
// when they are empty.
func TestApplyStartExtras_RecreationInputs(t *testing.T) {
	payload := map[string]interface{}{}
	applyStartExtras(payload, StartExtras{
		TemplateID:        "template-id-1",
		TemplateHash:      "sha256:template-hash-1",
		ProjectPath:       "/p/.scion",
		ProjectSlug:       "proj",
		HarnessConfig:     "claude",
		HarnessConfigID:   "hc-id-1",
		HarnessConfigHash: "sha256:hc-hash-1",
		SharedDirs:        []api.SharedDir{{Name: "cache"}},
	})
	assert.Equal(t, "template-id-1", payload["templateId"])
	assert.Equal(t, "sha256:template-hash-1", payload["templateHash"])
	assert.Equal(t, "/p/.scion", payload["projectPath"])
	assert.Equal(t, "proj", payload["projectSlug"])
	assert.Equal(t, "claude", payload["harnessConfig"])
	assert.Equal(t, "hc-id-1", payload["harnessConfigId"])
	assert.Equal(t, "sha256:hc-hash-1", payload["harnessConfigHash"])
	assert.Equal(t, []api.SharedDir{{Name: "cache"}}, payload["sharedDirs"])

	empty := map[string]interface{}{}
	applyStartExtras(empty, StartExtras{})
	for _, key := range []string{"templateId", "templateHash", "projectPath", "projectSlug", "harnessConfig", "harnessConfigId", "harnessConfigHash", "sharedDirs"} {
		_, ok := empty[key]
		assert.False(t, ok, "empty extras must not write %q", key)
	}
}

// restartWire is the subset of the restart request body these tests read.
type restartWire struct {
	ProjectPath   string              `json:"projectPath"`
	ProjectSlug   string              `json:"projectSlug"`
	TemplateID    string              `json:"templateId"`
	TemplateHash  string              `json:"templateHash"`
	HarnessConfig string              `json:"harnessConfig"`
	SharedDirs    []api.SharedDir     `json:"sharedDirs"`
	GitClone      *api.GitCloneConfig `json:"gitClone"`
	Branch        string              `json:"branch"`
	WorkspaceMode string              `json:"workspaceMode"`
}

func restartRecreationExtras() StartExtras {
	return StartExtras{
		Workspace: WorkspaceDispatchSpec{
			GitClone:      &api.GitCloneConfig{URL: "https://github.com/example/repo.git"},
			Branch:        "feature-branch",
			WorkspaceMode: store.WorkspaceModeWorktreePerAgent,
		},
		TemplateID:    "template-id-1",
		TemplateHash:  "sha256:template-hash-1",
		ProjectPath:   "/p/.scion",
		ProjectSlug:   "proj",
		HarnessConfig: "claude",
		SharedDirs:    []api.SharedDir{{Name: "cache"}},
	}
}

func assertRestartWire(t *testing.T, body []byte) {
	t.Helper()
	var wire restartWire
	require.NoError(t, json.Unmarshal(body, &wire), "body: %s", body)
	assert.Equal(t, "/p/.scion", wire.ProjectPath)
	assert.Equal(t, "proj", wire.ProjectSlug)
	assert.Equal(t, "template-id-1", wire.TemplateID)
	assert.Equal(t, "sha256:template-hash-1", wire.TemplateHash)
	assert.Equal(t, "claude", wire.HarnessConfig)
	assert.Equal(t, []api.SharedDir{{Name: "cache"}}, wire.SharedDirs)
	require.NotNil(t, wire.GitClone)
	assert.Equal(t, "https://github.com/example/repo.git", wire.GitClone.URL)
	assert.Equal(t, "feature-branch", wire.Branch)
	assert.Equal(t, store.WorkspaceModeWorktreePerAgent, wire.WorkspaceMode)
}

// TestBrokerHTTPTransport_RestartAgentSendsRecreationInputs proves the HTTP
// transport puts the restart's recreation inputs on the wire.
func TestBrokerHTTPTransport_RestartAgentSendsRecreationInputs(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		gotBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("failed to read request body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	transport := newBrokerHTTPTransport(false, nil)
	_, err := transport.RestartAgent(context.Background(), "broker-1", srv.URL, "agent-1", "project-id-1", nil, restartRecreationExtras())
	require.NoError(t, err)
	assertRestartWire(t, gotBody)
}

// TestControlChannelBrokerClient_RestartAgentSendsRecreationInputs is the
// control-channel counterpart of the HTTP transport test above.
func TestControlChannelBrokerClient_RestartAgentSendsRecreationInputs(t *testing.T) {
	runOverHubTunnels(t, func(t *testing.T, tr hubTunnelTransport) {
		tunnel := &mockControlChannelTunnel{connected: true}
		client := &ControlChannelBrokerClient{
			manager: tr.wrap(t, tunnel),
			signer:  &mockBrokerSigner{},
		}
		_, err := client.RestartAgent(context.Background(), "broker-1", "unused", "agent-1", "project-id-1", nil, restartRecreationExtras())
		require.NoError(t, err)
		require.NotNil(t, tunnel.lastRequest, "expected tunneled request to be captured")
		assertRestartWire(t, tunnel.lastRequest.Body)
	})
}
