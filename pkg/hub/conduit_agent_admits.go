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
	"errors"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
)

// RegisterConduitAgentSessionsAdmitted registers
// scion.hub.conduit.agent_sessions_admitted: the agent sessions this hub's
// in-process conduit relay admitted, by incarnation_source (launch_id, or
// generation for the interim fallback of a Hello without a launch id). It
// reports nothing while no in-process relay runs (hub.conduit off, or
// external relay mode, where the separate relays report
// scion.relay.agent_sessions_admitted). The counts start at zero with each
// relay.
func (s *Server) RegisterConduitAgentSessionsAdmitted(mp metric.MeterProvider) error {
	if mp == nil {
		return errors.New("conduit agent sessions admitted counter: nil MeterProvider")
	}
	_, err := mp.Meter(instrumentationScope).Int64ObservableCounter("scion.hub.conduit.agent_sessions_admitted",
		metric.WithUnit("{session}"),
		metric.WithDescription("Agent conduit sessions admitted by the in-process relay, by incarnation source; generation counts Hellos without a launch id"),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			rt := s.conduit.Load()
			if rt == nil || rt.relay == nil {
				return nil
			}
			counts := rt.relay.AgentSessionsAdmitted()
			for _, src := range []string{relay.IncarnationSourceLaunchID, relay.IncarnationSourceGeneration} {
				o.Observe(int64(counts[src]), metric.WithAttributes(attribute.String("incarnation_source", src)))
			}
			return nil
		}))
	return err
}
