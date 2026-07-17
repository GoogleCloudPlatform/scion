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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// TestHTTPAgentDispatcher_DispatchAgentStart_InjectsTransportMode verifies that
// DispatchAgentStart injects SCION_TRANSPORT_MODE alongside the existing
// transport token env vars when a transport minter is configured.
func TestHTTPAgentDispatcher_DispatchAgentStart_InjectsTransportMode(t *testing.T) {
	ctx := context.Background()
	memStore := createTestStore(t)

	project := &store.Project{
		ID:        tid("project-tm"),
		Name:      "transport-test",
		Slug:      "transport-test",
		GitRemote: "https://github.com/example/repo.git",
	}
	if err := memStore.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	broker := &store.RuntimeBroker{
		ID:       tid("broker-tm"),
		Name:     "test-broker-tm",
		Slug:     "test-broker-tm",
		Endpoint: "http://localhost:9800",
		Status:   store.BrokerStatusOnline,
	}
	if err := memStore.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create runtime broker: %v", err)
	}

	provider := &store.ProjectProvider{
		ProjectID:  tid("project-tm"),
		BrokerID:   tid("broker-tm"),
		BrokerName: "test-broker-tm",
		LocalPath:  "/home/user/projects/myproject/.scion",
		Status:     store.BrokerStatusOnline,
	}
	if err := memStore.AddProjectProvider(ctx, provider); err != nil {
		t.Fatalf("failed to add project provider: %v", err)
	}

	mockClient := &mockRuntimeBrokerClient{}
	dispatcher := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false, slog.Default())

	minter := &FakeTransportMinter{
		Token:  "fake-oidc-token",
		Expiry: time.Now().Add(1 * time.Hour),
	}
	dispatcher.SetTransportMinter(minter, "https://iap-client-id.apps.googleusercontent.com", "iap")

	agent := &store.Agent{
		ID:              "agent-tm-123",
		Name:            "transport-test-agent",
		Slug:            "transport-test-agent",
		ProjectID:       tid("project-tm"),
		RuntimeBrokerID: tid("broker-tm"),
	}

	err := dispatcher.DispatchAgentStart(ctx, agent, "", false)
	if err != nil {
		t.Fatalf("DispatchAgentStart failed: %v", err)
	}

	if !mockClient.startCalled {
		t.Fatal("expected StartAgent to be called")
	}

	// Verify SCION_TRANSPORT_TOKEN is injected
	if v, ok := mockClient.lastResolvedEnv["SCION_TRANSPORT_TOKEN"]; !ok {
		t.Error("expected SCION_TRANSPORT_TOKEN in resolvedEnv")
	} else if v != "fake-oidc-token" {
		t.Errorf("expected SCION_TRANSPORT_TOKEN='fake-oidc-token', got %q", v)
	}

	// Verify SCION_TRANSPORT_AUDIENCE is injected
	if v, ok := mockClient.lastResolvedEnv["SCION_TRANSPORT_AUDIENCE"]; !ok {
		t.Error("expected SCION_TRANSPORT_AUDIENCE in resolvedEnv")
	} else if v != "https://iap-client-id.apps.googleusercontent.com" {
		t.Errorf("unexpected SCION_TRANSPORT_AUDIENCE value: %q", v)
	}

	// Verify SCION_TRANSPORT_MODE is injected (the new behavior)
	if v, ok := mockClient.lastResolvedEnv["SCION_TRANSPORT_MODE"]; !ok {
		t.Error("expected SCION_TRANSPORT_MODE in resolvedEnv")
	} else if v != "iap" {
		t.Errorf("expected SCION_TRANSPORT_MODE='iap', got %q", v)
	}
}

// TestHTTPAgentDispatcher_DispatchAgentStart_NoTransportMode verifies that
// SCION_TRANSPORT_MODE is NOT injected when transport mode is empty.
func TestHTTPAgentDispatcher_DispatchAgentStart_NoTransportMode(t *testing.T) {
	ctx := context.Background()
	memStore := createTestStore(t)

	project := &store.Project{
		ID:        tid("project-nt"),
		Name:      "no-transport",
		Slug:      "no-transport",
		GitRemote: "https://github.com/example/repo.git",
	}
	if err := memStore.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	broker := &store.RuntimeBroker{
		ID:       tid("broker-nt"),
		Name:     "test-broker-nt",
		Slug:     "test-broker-nt",
		Endpoint: "http://localhost:9800",
		Status:   store.BrokerStatusOnline,
	}
	if err := memStore.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create runtime broker: %v", err)
	}

	provider := &store.ProjectProvider{
		ProjectID:  tid("project-nt"),
		BrokerID:   tid("broker-nt"),
		BrokerName: "test-broker-nt",
		LocalPath:  "/home/user/projects/.scion",
		Status:     store.BrokerStatusOnline,
	}
	if err := memStore.AddProjectProvider(ctx, provider); err != nil {
		t.Fatalf("failed to add project provider: %v", err)
	}

	mockClient := &mockRuntimeBrokerClient{}
	dispatcher := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false, slog.Default())

	// No transport minter configured → no transport env vars
	agent := &store.Agent{
		ID:              "agent-nt-123",
		Name:            "no-transport-agent",
		Slug:            "no-transport-agent",
		ProjectID:       tid("project-nt"),
		RuntimeBrokerID: tid("broker-nt"),
	}

	err := dispatcher.DispatchAgentStart(ctx, agent, "", false)
	if err != nil {
		t.Fatalf("DispatchAgentStart failed: %v", err)
	}

	// Verify no transport env vars are injected
	if _, ok := mockClient.lastResolvedEnv["SCION_TRANSPORT_MODE"]; ok {
		t.Error("SCION_TRANSPORT_MODE should not be in resolvedEnv when no transport minter")
	}
	if _, ok := mockClient.lastResolvedEnv["SCION_TRANSPORT_TOKEN"]; ok {
		t.Error("SCION_TRANSPORT_TOKEN should not be in resolvedEnv when no transport minter")
	}
}
