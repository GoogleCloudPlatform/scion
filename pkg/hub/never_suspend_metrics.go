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
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
)

// RegisterNeverSuspendMetrics registers scion.hub.never_suspend_agents, the
// number of running agents with suspend_policy=never by project and runtime
// broker, read from the store at each collection. It observes nothing while
// the lifecycle.never_suspend experiment is off.
func (s *Server) RegisterNeverSuspendMetrics(mp metric.MeterProvider) error {
	if mp == nil {
		return errors.New("never-suspend metrics: nil MeterProvider")
	}
	_, err := mp.Meter(instrumentationScope).Int64ObservableGauge("scion.hub.never_suspend_agents",
		metric.WithUnit("{agent}"),
		metric.WithDescription("Running agents with suspend_policy=never by project and runtime broker"),
		metric.WithInt64Callback(func(ctx context.Context, o metric.Int64Observer) error {
			if !s.experimentEnabled(experiments.NeverSuspend) {
				return nil
			}
			ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			counts, err := s.store.CountRunningNeverSuspendAgents(ctx)
			if err != nil {
				return err
			}
			for _, c := range counts {
				o.Observe(c.Count, metric.WithAttributes(
					attribute.String("project", c.ProjectID),
					attribute.String("runtime_broker", c.RuntimeBrokerID),
				))
			}
			return nil
		}))
	return err
}
