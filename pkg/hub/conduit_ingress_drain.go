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
	"log/slog"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
)

// Drain through the proxy, hub side (design §3.9 "Drain through the
// proxy", v2.21.53; contracts "Addition (6.2 (a3))"). On SIGTERM a hub in
// external relay mode first stops proxying conduit dials (503
// conduit_not_serving, cause ingress_terminating, before any byte to a
// relay), then asks every live relay to send GoAway to the sessions it
// proxied. The fan-out is bounded and best effort: a failed call is logged
// and counted and never blocks shutdown.

// conduitIngressCauseTerminating is the cause logged for conduit dials
// refused while the hub shuts down.
const conduitIngressCauseTerminating = "ingress_terminating"

// conduitIngressDrainUnsetBound bounds the drain fan-out when
// server.hub.conduit.shutdown_grace is unset.
const conduitIngressDrainUnsetBound = 10 * time.Second

// conduitIngressDrainBound is the time the drain fan-out may take: with a
// set shutdown_grace, max(grace - 1s, grace/2), so a grace of 1s or less
// still leaves time to send; unset, conduitIngressDrainUnsetBound.
func conduitIngressDrainBound(grace time.Duration, graceSet bool) time.Duration {
	if !graceSet {
		return conduitIngressDrainUnsetBound
	}
	return max(grace-time.Second, grace/2)
}

// conduitIngressDrainDeadline is the drain deadline the hub asks the
// relays for: with a set shutdown_grace, max(shutdown_grace - 2s, 1ms), as
// the in-process SIGTERM drain sends (a grace of 2s or less gives 1ms,
// never the relay default); unset, 0 (field omitted: the relay's own).
func conduitIngressDrainDeadline(grace time.Duration, graceSet bool) time.Duration {
	if !graceSet {
		return 0
	}
	return max(relay.ShutdownDrainCap(grace), time.Millisecond)
}

// DrainConduitIngressForShutdown is the SIGTERM step of external relay
// mode: it stops proxying new conduit dials, then fans out drain to every
// live relay (draining ones included) and returns when every call has
// answered or failed, within conduitIngressDrainBound. With the ingress
// proxy off (IngressOffCause) no session was proxied, so there is no
// fan-out. It runs once; later calls, and calls outside external mode,
// return at once.
func (s *Server) DrainConduitIngressForShutdown(grace time.Duration, graceSet bool) {
	rt := s.conduitExternal.Load()
	if rt == nil || !rt.terminating.CompareAndSwap(false, true) {
		return
	}
	if rt.ingressOffCause != "" {
		// The ingress proxy was never on, so this hub proxied no session:
		// there is nothing to drain.
		slog.Info("Conduit ingress: shutting down; the ingress proxy was off, so no relay is asked to drain",
			"cause", rt.ingressOffCause)
		return
	}
	bound := conduitIngressDrainBound(grace, graceSet)
	deadline := conduitIngressDrainDeadline(grace, graceSet)
	slog.Warn("Conduit ingress: shutting down; new conduit dials are refused, draining the sessions this hub proxied",
		"cause", conduitIngressCauseTerminating, "ingress_instance", rt.instanceID, "bound", bound, "drain_deadline", deadline)
	ctx, cancel := context.WithTimeout(context.Background(), bound)
	defer cancel()
	relays, err := rt.registry.LiveRelays(ctx, rt.now())
	if err != nil {
		slog.Warn("Conduit ingress: listing live relays for the drain failed; sessions end when this hub exits", "error", err)
		return
	}
	var wg sync.WaitGroup
	for _, ri := range relays {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n, err := rt.peers.Drain(ctx, ri.InternalEndpoint, ri.InstanceID, ri.Generation, rt.instanceID, deadline)
			if err != nil {
				cause, status := relay.DrainFailedUnreachable, 0
				var de *relay.DrainError
				if errors.As(err, &de) {
					cause, status = de.Cause, de.Status
				}
				s.recordConduitIngressDrainFailed(cause)
				slog.Warn("Conduit ingress: drain call failed", "relay_instance_id", ri.InstanceID, "cause", cause, "status", status, "error", err)
				return
			}
			slog.Info("Conduit ingress: relay drained this hub's sessions", "relay_instance_id", ri.InstanceID, "goaway_sent", n)
		}()
	}
	wg.Wait()
}

// OTelConduitIngressDrainMetrics counts failed drain calls of a
// terminating hub ingress as scion.hub.conduit.ingress_drain_failed
// (attribute cause: unreachable, timeout, refused or bad_response).
type OTelConduitIngressDrainMetrics struct {
	failed metric.Int64Counter
}

// NewOTelConduitIngressDrainMetrics creates the counter.
func NewOTelConduitIngressDrainMetrics(mp metric.MeterProvider) (*OTelConduitIngressDrainMetrics, error) {
	if mp == nil {
		return nil, errors.New("otel conduit ingress drain metrics: nil MeterProvider")
	}
	c, err := mp.Meter(instrumentationScope).Int64Counter("scion.hub.conduit.ingress_drain_failed",
		metric.WithUnit("{call}"))
	if err != nil {
		return nil, err
	}
	return &OTelConduitIngressDrainMetrics{failed: c}, nil
}

// SetConduitIngressDrainMetrics wires the
// scion.hub.conduit.ingress_drain_failed counter. Until it is set,
// failures are only logged.
func (s *Server) SetConduitIngressDrainMetrics(m *OTelConduitIngressDrainMetrics) {
	s.conduitIngressDrainMetrics.Store(m)
}

func (s *Server) recordConduitIngressDrainFailed(cause string) {
	if m := s.conduitIngressDrainMetrics.Load(); m != nil {
		m.failed.Add(context.Background(), 1, metric.WithAttributes(attribute.String("cause", cause)))
	}
}
