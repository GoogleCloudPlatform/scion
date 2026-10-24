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

package relayserver

import (
	"context"
	"errors"
	"log/slog"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relayrole"
)

// Relay process metrics (design §3.9 "Cloud Run singleton", v2.21.55):
// exported through the relay MeterProvider that the --role relay process
// builds from the observability settings, like the hub's. No listener or
// endpoint is added.

const relayInstrumentationScope = "github.com/GoogleCloudPlatform/scion/pkg/conduit/relayrole/relayserver"

// Metric names.
const (
	// metricSingletonEndpointPeers: the other live, non-draining relays
	// sharing a singleton's internal endpoint (singletons only).
	metricSingletonEndpointPeers = "scion.relay.singleton_endpoint_peers"
	// metricIngressHeaderRefused: dials whose ingress headers the relay did
	// not use, by cause (ingress_header_untrusted, ingress_cap_invalid).
	metricIngressHeaderRefused = "scion.relay.ingress_headers_refused"
)

// registerRelayMetrics registers the relay's observable instruments on mp
// (nil: none): the singleton shared-endpoint gauge, on a singleton only,
// and the ingress header counter of the role.
func registerRelayMetrics(mp metric.MeterProvider, r *relay.Relay, role *relayrole.Role, singleton bool) error {
	if mp == nil {
		return nil
	}
	m := mp.Meter(relayInstrumentationScope)
	var errs []error
	if singleton {
		_, err := m.Int64ObservableGauge(metricSingletonEndpointPeers,
			metric.WithUnit("{relay}"),
			metric.WithDescription("Other live, non-draining relays sharing this singleton's internal endpoint; above 0 for more than 75s means two instances are running"),
			metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
				o.Observe(r.SingletonEndpointPeers())
				return nil
			}))
		errs = append(errs, err)
	}
	_, err := m.Int64ObservableCounter(metricIngressHeaderRefused,
		metric.WithUnit("{dial}"),
		metric.WithDescription("Conduit dials whose ingress headers the relay did not use, by cause"),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			o.Observe(int64(role.IngressHeaderUntrustedCount()), metric.WithAttributes(attribute.String("cause", "ingress_header_untrusted")))
			o.Observe(int64(role.IngressCapInvalidCount()), metric.WithAttributes(attribute.String("cause", "ingress_cap_invalid")))
			return nil
		}))
	errs = append(errs, err)
	return errors.Join(errs...)
}

// logRegisterRelayMetrics registers the relay metrics and logs a failure;
// metrics never stop the relay.
func logRegisterRelayMetrics(log *slog.Logger, mp metric.MeterProvider, r *relay.Relay, role *relayrole.Role, singleton bool) {
	if err := registerRelayMetrics(mp, r, role, singleton); err != nil {
		log.Warn("Conduit relay: registering metrics failed", "error", err)
	}
}
