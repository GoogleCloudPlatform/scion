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

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

// setupThinkingLevelDispatch wires the agent-create HTTP handler to a real
// HTTPAgentDispatcher backed by a mock RuntimeBrokerClient, so the assertion
// can be made on the RemoteCreateAgentRequest the hub actually builds — the
// stub AgentDispatcher used by most create tests short-circuits before
// buildCreateRequest and would never run the env injectors.
func setupThinkingLevelDispatch(t *testing.T) (*Server, store.Store, *store.Project, *mockRuntimeBrokerClient) {
	t.Helper()

	// This stub only satisfies setupCreateAgentServer's signature: it is
	// replaced by the SetDispatcher call below before any request is served
	// and is never invoked, so createPhase here is not load-bearing. The live
	// dispatcher is the real HTTPAgentDispatcher constructed below.
	srv, s, project := setupCreateAgentServer(t, &createAgentDispatcher{createPhase: string(state.PhaseRunning)})
	ctx := context.Background()

	// The real dispatcher refuses to dispatch to a broker with no endpoint.
	// The endpoint is deliberately un-dialable: .invalid is reserved by
	// RFC 2606 and can never resolve, so if a future change makes this path
	// dial for real it fails loudly here instead of quietly reaching whatever
	// happens to be listening on a plausible localhost port.
	broker, err := s.GetRuntimeBroker(ctx, project.DefaultRuntimeBrokerID)
	require.NoError(t, err)
	broker.Endpoint = "http://broker.invalid"
	require.NoError(t, s.UpdateRuntimeBroker(ctx, broker))

	mockClient := &mockRuntimeBrokerClient{}
	srv.SetDispatcher(NewHTTPAgentDispatcherWithClient(s, mockClient, false, slog.Default()))

	return srv, s, project, mockClient
}
