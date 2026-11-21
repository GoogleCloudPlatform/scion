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
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// TestBuildCreateRequest_WarnsWhenGCPTelemetryBlockedWithoutCredentials pins
// the dispatch wiring in buildCreateRequest so removing the warning call site
// fails this test.
func TestBuildCreateRequest_WarnsWhenGCPTelemetryBlockedWithoutCredentials(t *testing.T) {
	boolPtr := func(v bool) *bool { return &v }
	srv := &Server{
		store:       createTestStore(t),
		maintenance: NewMaintenanceState(false, ""),
		config: ServerConfig{
			HubEndpoint: "https://hub.example.com",
			TelemetryConfig: &api.TelemetryConfig{
				Enabled: boolPtr(true),
				Cloud: &api.TelemetryCloudConfig{
					Enabled:  boolPtr(true),
					Provider: "gcp",
				},
			},
		},
	}
	d := srv.CreateAuthenticatedDispatcher()

	// Default agent has nil GCPIdentity, which resolves to block on non-Kubernetes runtimes.
	ctx, warns := withDispatchWarnings(context.Background())
	if _, err := d.buildCreateRequest(ctx, hubDefaultsDispatchAgent(), "test"); err != nil {
		t.Fatalf("buildCreateRequest: %v", err)
	}
	got := warns.Warnings()
	found := false
	for _, w := range got {
		if strings.Contains(w, telemetryGCPCredentialsSecretName) {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected dispatch warning mentioning %q, got %v", telemetryGCPCredentialsSecretName, got)
	}
}

// TestDispatchStartAndRestart_WarnsWhenGCPTelemetryBlockedWithoutCredentials
// pins the warning call sites in DispatchAgentStart and DispatchAgentRestart.
func TestDispatchStartAndRestart_WarnsWhenGCPTelemetryBlockedWithoutCredentials(t *testing.T) {
	boolPtr := func(v bool) *bool { return &v }
	memStore := createTestStore(t)
	ctx := context.Background()

	project := &store.Project{
		ID:   tid("project-warn"),
		Name: "warn-project",
		Slug: "warn-project",
	}
	if err := memStore.CreateProject(ctx, project); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	broker := &store.RuntimeBroker{
		ID:       tid("broker-warn"),
		Name:     "warn-broker",
		Slug:     "warn-broker",
		Endpoint: "http://localhost:9800",
		Status:   store.BrokerStatusOnline,
	}
	if err := memStore.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("CreateRuntimeBroker: %v", err)
	}
	if err := memStore.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID:  project.ID,
		BrokerID:   broker.ID,
		BrokerName: broker.Name,
		Status:     store.BrokerStatusOnline,
	}); err != nil {
		t.Fatalf("AddProjectProvider: %v", err)
	}

	srv := &Server{
		store:       memStore,
		maintenance: NewMaintenanceState(false, ""),
		config: ServerConfig{
			HubEndpoint: "https://hub.example.com",
			TelemetryConfig: &api.TelemetryConfig{
				Enabled: boolPtr(true),
				Cloud: &api.TelemetryCloudConfig{
					Enabled:  boolPtr(true),
					Provider: "gcp",
				},
			},
		},
	}
	d := NewHTTPAgentDispatcherWithClient(memStore, &mockRuntimeBrokerClient{}, false, slog.Default())
	d.hubTelemetryProvider = srv.hubTelemetryDefault

	agent := &store.Agent{
		ID:              tid("agent-warn"),
		Name:            "warn-agent",
		Slug:            "warn-agent",
		ProjectID:       project.ID,
		RuntimeBrokerID: broker.ID,
	}

	assertWarning := func(t *testing.T, warns *dispatchWarnings) {
		t.Helper()
		for _, w := range warns.Warnings() {
			if strings.Contains(w, telemetryGCPCredentialsSecretName) {
				return
			}
		}
		t.Fatalf("expected dispatch warning mentioning %q, got %v", telemetryGCPCredentialsSecretName, warns.Warnings())
	}

	t.Run("start", func(t *testing.T) {
		wCtx, warns := withDispatchWarnings(ctx)
		if err := d.DispatchAgentStart(wCtx, agent, "task", false); err != nil {
			t.Fatalf("DispatchAgentStart: %v", err)
		}
		assertWarning(t, warns)
	})

	t.Run("restart", func(t *testing.T) {
		wCtx, warns := withDispatchWarnings(ctx)
		if err := d.DispatchAgentRestart(wCtx, agent); err != nil {
			t.Fatalf("DispatchAgentRestart: %v", err)
		}
		assertWarning(t, warns)
	})
}
