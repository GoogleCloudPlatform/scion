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
	"log/slog"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

const aeKey = api.EnvAutoExposePorts

// createAutoExposeTemplate creates a global template whose config env carries
// the given entries.
func createAutoExposeTemplate(t *testing.T, s store.Store, slug string, env map[string]string) {
	t.Helper()
	require.NoError(t, s.CreateTemplate(context.Background(), &store.Template{
		ID:          tid("template-" + slug + "-" + t.Name()),
		Name:        slug,
		Slug:        slug,
		Harness:     "claude",
		ContentHash: "d00dfeed",
		Scope:       store.TemplateScopeGlobal,
		Status:      "active",
		Config:      &store.TemplateConfig{Env: env},
	}))
}

func inlineEnv(ag *store.Agent) map[string]string {
	if ag.AppliedConfig.InlineConfig == nil {
		return nil
	}
	return ag.AppliedConfig.InlineConfig.Env
}

func createInputsEnv(ag *store.Agent) map[string]string {
	ci := ag.AppliedConfig.CreateInputs
	if ci == nil || ci.InlineConfig == nil {
		return nil
	}
	return ci.InlineConfig.Env
}

func ptrBool(b bool) *bool { return &b }

// autoExposeDispatchFixture builds a dispatcher with a store holding one
// project/broker/provider, so start and restart dispatches can run.
func autoExposeDispatchFixture(t *testing.T) (*HTTPAgentDispatcher, *mockRuntimeBrokerClient, *store.Agent) {
	t.Helper()
	ctx := context.Background()
	memStore := createTestStore(t)
	require.NoError(t, memStore.CreateProject(ctx, &store.Project{
		ID: tid("project-1"), Name: "test-project", Slug: "test-project",
		GitRemote: "https://github.com/example/repo.git",
	}))
	require.NoError(t, memStore.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID: tid("broker-1"), Name: "test-broker", Slug: "test-broker",
		Endpoint: "http://localhost:9800", Status: store.BrokerStatusOnline,
	}))
	require.NoError(t, memStore.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: tid("project-1"), BrokerID: tid("broker-1"), BrokerName: "test-broker",
		LocalPath: "/home/user/projects/myproject/.scion", Status: store.BrokerStatusOnline,
	}))
	client := &mockRuntimeBrokerClient{}
	d := NewHTTPAgentDispatcherWithClient(memStore, client, false, slog.Default())
	ag := &store.Agent{
		ID: "agent-uuid-123", Name: "test-agent", Slug: "test-agent-slug",
		ProjectID: tid("project-1"), OwnerID: "owner-uuid-789", RuntimeBrokerID: tid("broker-1"),
	}
	return d, client, ag
}
