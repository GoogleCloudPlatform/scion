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

package hub

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay/relaytest"
)

// agentSessionsAdmittedPoints collects scion.hub.conduit.agent_sessions_admitted
// by incarnation_source, and whether the instrument is a monotonic sum.
func agentSessionsAdmittedPoints(t *testing.T, reader *sdkmetric.ManualReader) (map[string]int64, bool) {
	t.Helper()
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	out := map[string]int64{}
	monotonic := false
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "scion.hub.conduit.agent_sessions_admitted" {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			require.True(t, ok, "instrument type %T", m.Data)
			monotonic = sum.IsMonotonic
			for _, dp := range sum.DataPoints {
				src, _ := dp.Attributes.Value("incarnation_source")
				out[src.AsString()] = dp.Value
			}
		}
	}
	return out, monotonic
}

// TestConduitAgentSessionsAdmitted: the hub counter reports nothing without
// an in-process relay, and the in-process relay's per-source admission
// counts once one runs, as a monotonic counter.
func TestConduitAgentSessionsAdmitted(t *testing.T) {
	require.Error(t, (&Server{}).RegisterConduitAgentSessionsAdmitted(nil))

	s := &Server{}
	reader := sdkmetric.NewManualReader()
	require.NoError(t, s.RegisterConduitAgentSessionsAdmitted(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))))
	got, _ := agentSessionsAdmittedPoints(t, reader)
	assert.Empty(t, got, "no in-process relay: nothing reported")

	w := relaytest.NewWorld(t)
	node := w.StartNode("relay-a", nil)
	w.SetPrincipal("a", relay.Principal{Kind: registry.PrincipalAgent, ID: "agent-1", ProjectID: "proj-1",
		Agent: relay.AgentIncarnationFacts{LaunchID: "L1", Generation: 2}})
	s.conduit.Store(&conduitRuntime{relay: node.Relay})
	_, _ = node.MustDial("a", relaytest.AgentHello("agent-1", "", "", "pty"), conduit.Config{})

	got, monotonic := agentSessionsAdmittedPoints(t, reader)
	assert.True(t, monotonic, "a monotonic counter")
	assert.Equal(t, map[string]int64{relay.IncarnationSourceLaunchID: 0, relay.IncarnationSourceGeneration: 1}, got)
}
