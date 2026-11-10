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
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Broker presence (design v2.21.50 §3.4): whether a runtime broker is
// connected, for every presence reader (brokerReachable,
// brokerRecordReachable and the tunnel's broker check). It never decides a
// route: conduit routing reads only the registry, and the control
// channel's in-process map stays a local fast path.

// brokerPresenceSource names what found a broker connected.
type brokerPresenceSource string

const (
	// brokerPresenceNone: nothing found the broker connected.
	brokerPresenceNone brokerPresenceSource = "none"
	// brokerPresenceRegistry: a live, current conduit registry session.
	brokerPresenceRegistry brokerPresenceSource = "registry"
	// brokerPresenceLegacy: the legacy signals (brokerLegacyConnected).
	brokerPresenceLegacy brokerPresenceSource = "legacy"
)

// brokerPresenceResult is a broker's presence and what decided it.
type brokerPresenceResult struct {
	Connected bool
	Source    brokerPresenceSource
}

// brokerPresence reports whether broker b is connected. With hub.conduit on,
// a live, current registry session counts (brokerRegistryLive), and so do
// the legacy signals; with it off, only the legacy signals do. A registry
// read that fails falls back to the legacy signals: it is logged and
// counted (brokerPresenceRegistryFallbacks, cause=registry_read), never
// treated as offline on its own. A read cut short by the caller's own
// context is not a registry failure and is neither logged nor counted.
func (s *Server) brokerPresence(ctx context.Context, b *store.RuntimeBroker) brokerPresenceResult {
	if b == nil {
		return brokerPresenceResult{Source: brokerPresenceNone}
	}
	if s.experimentEnabled(conduitExperiment) {
		live, err := s.brokerRegistryLive(ctx, b.ID)
		switch {
		case err != nil && ctx.Err() != nil:
			// The caller's context ended: the registry did not fail, so
			// nothing is logged or counted; the legacy signals decide.
		case err != nil:
			s.brokerPresenceRegistryFallbacks.Add(1)
			slog.WarnContext(ctx, "Broker presence: the conduit registry could not be read; using the legacy signals",
				"broker_id", b.ID, "error", err)
		case live:
			return brokerPresenceResult{Connected: true, Source: brokerPresenceRegistry}
		}
	}
	if s.brokerLegacyConnected(b) {
		return brokerPresenceResult{Connected: true, Source: brokerPresenceLegacy}
	}
	return brokerPresenceResult{Source: brokerPresenceNone}
}

// BrokerPresent reports whether the broker with ID brokerID is present
// (brokerPresence) for callers outside this package: the co-located
// broker's startup repair of orphaned agents and project defaults. A broker
// with no record is not present. Any other read error of the broker record
// is returned, and the caller counts the broker as present (the startup
// repair does not move its agents): this is the one log line for the
// failure, at Warn, and it is counted in
// brokerPresenceRecordReadFallbacks (cause=broker_record_read). A read cut
// short by the caller's own context is returned without a log or a count.
// At startup the conduit registry may not be open yet; the registry read is
// then not live and the legacy signals decide, as they did before presence.
func (s *Server) BrokerPresent(ctx context.Context, brokerID string) (bool, error) {
	b, err := s.store.GetRuntimeBroker(ctx, brokerID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return false, nil
	case err != nil && ctx.Err() != nil:
		return false, err
	case err != nil:
		s.brokerPresenceRecordReadFallbacks.Add(1)
		slog.WarnContext(ctx, "Broker presence: the broker record could not be read; the error is returned and the caller counts the broker as present",
			"broker_id", brokerID, "error", err)
		return false, err
	}
	return s.brokerPresence(ctx, b).Connected, nil
}

// brokerRegistryLive reports whether broker id has a live, current session
// in this node's conduit registry: Registry.BrokerReference finds one (a
// session whose relay row is present, current and fresh, itself fresh,
// and the newest of the broker's sessions with an incarnation). With no
// conduit runtime on this node there is no registry to read: not live, and
// not an error.
func (s *Server) brokerRegistryLive(ctx context.Context, id string) (bool, error) {
	reg, _, now := s.conduitRegistry()
	if reg == nil || now == nil {
		return false, nil
	}
	_, ok, err := reg.BrokerReference(ctx, id, now())
	if err != nil {
		return false, err
	}
	return ok, nil
}

// brokerLegacyConnected reports the legacy presence signals of broker b: its
// control channel is held by this node (the local fast path), or the
// affinity columns name a holder and its heartbeat is fresh (the rule of
// StoreAffinityLookup, defaultAffinityFreshness), or the stored status is
// online (written by heartbeats and by co-located brokers, which never claim
// the affinity columns).
func (s *Server) brokerLegacyConnected(b *store.RuntimeBroker) bool {
	if s.controlChannel != nil && s.controlChannel.IsConnected(b.ID) {
		return true
	}
	if b.ConnectedHubID != nil && *b.ConnectedHubID != "" &&
		!b.LastHeartbeat.IsZero() && time.Since(b.LastHeartbeat) < defaultAffinityFreshness {
		return true
	}
	return b.Status == store.BrokerStatusOnline
}

// brokerPresenceFallbacksMetric is the exported count of broker presence
// reads that failed and took a fallback answer, with a cause attribute
// telling the two kinds apart (ptone/scion#5479):
//   - cause=registry_read (brokerPresenceRegistryFallbacks): a conduit
//     registry read that fell back to the legacy signals;
//   - cause=broker_record_read (brokerPresenceRecordReadFallbacks): a
//     broker record read in BrokerPresent (the co-located broker's startup
//     repair) whose broker counts as present.
const brokerPresenceFallbacksMetric = "scion.hub.broker.presence_registry_fallbacks"

// The values of the cause attribute on brokerPresenceFallbacksMetric.
const (
	brokerPresenceFallbackCauseRegistry = "registry_read"
	brokerPresenceFallbackCauseRecord   = "broker_record_read"
)

// RegisterBrokerPresenceMetrics registers
// scion.hub.broker.presence_registry_fallbacks, a cumulative count of broker
// presence reads that failed and took a fallback answer, observed once per
// cause (brokerPresenceFallbacksMetric): cause=registry_read for a conduit
// registry read that fell back to the legacy signals, and
// cause=broker_record_read for a broker record read in BrokerPresent whose
// broker counts as present. Reads cut short by the caller's own context are
// not counted.
func (s *Server) RegisterBrokerPresenceMetrics(mp metric.MeterProvider) error {
	if mp == nil {
		return errors.New("broker presence metrics: nil MeterProvider")
	}
	registryCause := metric.WithAttributes(attribute.String("cause", brokerPresenceFallbackCauseRegistry))
	recordCause := metric.WithAttributes(attribute.String("cause", brokerPresenceFallbackCauseRecord))
	_, err := mp.Meter(instrumentationScope).Int64ObservableCounter(brokerPresenceFallbacksMetric,
		metric.WithUnit("{read}"),
		metric.WithDescription("Broker presence reads that failed and took a fallback answer, by cause: registry_read (a conduit registry read that fell back to the legacy presence signals) or broker_record_read (a broker record read whose broker counts as present)"),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			o.Observe(s.brokerPresenceRegistryFallbacks.Load(), registryCause)
			o.Observe(s.brokerPresenceRecordReadFallbacks.Load(), recordCause)
			return nil
		}))
	return err
}
