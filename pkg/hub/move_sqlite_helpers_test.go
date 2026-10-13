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
	"github.com/stretchr/testify/require"
)

func newMoveDispatchFixture(t *testing.T, caps *store.BrokerCapabilities) (*HTTPAgentDispatcher, *mockRuntimeBrokerClient, *store.Agent) {
	t.Helper()
	ctx := context.Background()
	s := createTestStore(t)
	broker := &store.RuntimeBroker{
		ID: tid("move-dispatch-broker"), Name: "move-dispatch", Slug: "move-dispatch",
		Endpoint: "http://localhost:9800", Status: store.BrokerStatusOnline, Capabilities: caps,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	client := &mockRuntimeBrokerClient{}
	d := NewHTTPAgentDispatcherWithClient(s, client, false, slog.Default())
	agent := &store.Agent{
		ID: tid("move-dispatch-agent"), Name: "mover", Slug: "mover", ProjectID: tid("project-1"),
		RuntimeBrokerID: broker.ID, RunID: "run-1",
		AppliedConfig: &store.AgentAppliedConfig{HarnessConfig: "claude"},
	}
	return d, client, agent
}
