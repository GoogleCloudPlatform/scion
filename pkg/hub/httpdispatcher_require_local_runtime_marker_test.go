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

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// TestDispatchAgentStartRestart_UnflaggedGrantClearsStaleRequireLocalRuntimeMarker
// covers a marker-persistence gap distinct from the passthrough gate itself:
// SCION_METADATA_REQUIRE_LOCAL_RUNTIME is not supposed to persist to storage
// (it is a SCION_-prefixed key, rejected by shouldPersistResolvedEnvKey), but
// a project-scoped env var or an environment-type secret can still supply the
// same key by name, and resolveEnvFromStorage/resolveSecrets fill it in for
// any key not already set by a higher-precedence source. Both DispatchAgentStart
// and DispatchAgentRestart must remove that value from the request sent to the
// broker whenever the agent's current GCPIdentity grant is not itself
// flagged (RequireLocalRuntime false) -- otherwise a stale or
// lower-precedence value for this key would remain in the request sent to the
// broker even though the current grant does not set it.
func TestDispatchAgentStartRestart_UnflaggedGrantClearsStaleRequireLocalRuntimeMarker(t *testing.T) {
	ctx := context.Background()
	memStore := createTestStore(t)

	broker := &store.RuntimeBroker{
		ID:       tid("broker-marker-clear"),
		Name:     "test-broker",
		Slug:     "test-broker",
		Endpoint: "http://localhost:9800",
		Status:   store.BrokerStatusOnline,
	}
	if err := memStore.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create runtime broker: %v", err)
	}

	projectID := tid("project-marker-clear")

	// A project-scoped, non-secret env var named exactly like the marker,
	// standing in for either a storage-sourced or an environment-type-secret
	// source (resolveEnvFromStorage and resolveSecrets share the same
	// fill-absent behavior, so this one seed exercises both call sites
	// without needing a separate secret backend fixture).
	if err := memStore.CreateEnvVar(ctx, &store.EnvVar{
		ID:            tid("envvar-marker-clear"),
		Key:           "SCION_METADATA_REQUIRE_LOCAL_RUNTIME",
		Value:         "true",
		Scope:         store.ScopeProject,
		ScopeID:       projectID,
		InjectionMode: store.InjectionModeAlways,
	}); err != nil {
		t.Fatalf("failed to create env var: %v", err)
	}

	newAgent := func(id string) *store.Agent {
		return &store.Agent{
			ID:              tid(id),
			Name:            id,
			Slug:            id,
			ProjectID:       projectID,
			RuntimeBrokerID: tid("broker-marker-clear"),
			AppliedConfig: &store.AgentAppliedConfig{
				GCPIdentity: &store.GCPIdentityConfig{
					MetadataMode:        store.GCPMetadataModeAssign,
					RequireLocalRuntime: false,
				},
			},
		}
	}

	t.Run("start", func(t *testing.T) {
		mockClient := &mockRuntimeBrokerClient{}
		dispatcher := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false, slog.Default())
		dispatcher.SetProfileTimezoneProvider(func(name string) string { return "" })

		agent := newAgent("agent-marker-clear-start")
		if err := dispatcher.DispatchAgentStart(ctx, agent, "", false); err != nil {
			t.Fatalf("DispatchAgentStart failed: %v", err)
		}
		if !mockClient.startCalled {
			t.Fatal("expected StartAgent to be called")
		}
		if v, ok := mockClient.lastResolvedEnv["SCION_METADATA_REQUIRE_LOCAL_RUNTIME"]; ok {
			t.Errorf("expected SCION_METADATA_REQUIRE_LOCAL_RUNTIME to be absent from an unflagged start dispatch, got %q", v)
		}
	})

	t.Run("restart", func(t *testing.T) {
		mockClient := &mockRuntimeBrokerClient{}
		dispatcher := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false, slog.Default())

		agent := newAgent("agent-marker-clear-restart")
		if err := dispatcher.DispatchAgentRestart(ctx, agent); err != nil {
			t.Fatalf("DispatchAgentRestart failed: %v", err)
		}
		if !mockClient.restartCalled {
			t.Fatal("expected RestartAgent to be called")
		}
		if v, ok := mockClient.lastRestartResolvedEnv["SCION_METADATA_REQUIRE_LOCAL_RUNTIME"]; ok {
			t.Errorf("expected SCION_METADATA_REQUIRE_LOCAL_RUNTIME to be absent from an unflagged restart dispatch, got %q", v)
		}
	})
}
