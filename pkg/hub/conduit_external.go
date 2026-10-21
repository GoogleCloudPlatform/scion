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
	"fmt"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/clock"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/router"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
)

// External relay mode (design §3.9 "Separate relay").
//
// The hub runs no relay and inserts no relay_instances row. Separate relay
// processes (scion server --role relay) hold the conduit sessions; the hub
// reaches them through the router, which resolves every session to its
// owner relay's internal_endpoint. The hub still owns everything that is
// not a relay's job: grants, its own user-stream re-check for the streams
// whose leaf it holds (port proxy, PTY), the registry maintenance and the
// revocation events relays listen to.
//
// The hub's GET /api/v1/conduit holds no sessions in this mode: with
// server.hub.conduit.ingress = proxy it proxies each dial to a relay
// (conduit_ingress.go). When no relay takes the dial, or the proxy is off
// (IngressOffCause), it gets 503 unavailable with reason
// conduit_not_serving, and the readiness gauge reports how many relays are
// eligible, with the cause attribute while the proxy is off. The hub does not advertise
// hub.conduit to agents (dispatchExperiments) in this mode.

// conduitRelayRecheckPeriod is how often the eligible-relay count of the
// readiness gauge is refreshed (the relay heartbeat period).
const conduitRelayRecheckPeriod = 15 * time.Second

// errConduitExternalDisabled refuses external mode while hub.conduit is off.
var errConduitExternalDisabled = errors.New("conduit external relay mode requires the hub.conduit experiment")

// ConduitExternalOptions configures StartConduitExternal.
type ConduitExternalOptions struct {
	// InstanceID names this hub process in its broker control channel
	// resolutions ("" = ConduitInstanceID("")). No relay row carries it.
	InstanceID string
	// PeerAuth authenticates this hub to the relays' internal API (keyed
	// from server.hub.conduit.peer_secret). Required.
	PeerAuth relay.PeerAuth
	// HTTPClient performs the internal calls to relays (nil: default).
	HTTPClient *http.Client
	// Ingress is the conduit ingress mode (config.ConduitIngressProxy or
	// config.ConduitIngressRedirect; "" = proxy). Startup refuses redirect
	// for now, so only proxy serves dials; any other value refuses them.
	Ingress string
	// IngressOffCause, when set, keeps the ingress proxy off: every dial
	// gets 503 conduit_not_serving and no request reaches a relay. It is
	// ConduitIngressCauseRequiresHMAC when proxy ingress runs under a
	// relay-peer auth mode other than hmac (design §3.9, v2.21.52). The
	// readiness gauge carries it as the cause attribute.
	IngressOffCause string
	// LifetimeCap is the hub's conduit session lifetime cap, sent to relays
	// on the ingress leg (0 = config.ConduitDefaultLifetimeCap).
	LifetimeCap time.Duration
	// AuthzRecheckInterval overrides ServerConfig.ConduitAuthzRecheckInterval
	// (0 = use it; negative disables the sweep, for tests).
	AuthzRecheckInterval time.Duration

	// replayStore replaces the shared relay-peer replay store (tests).
	replayStore relayPeerReplayStore

	// withoutStreamRenewer starts external relay mode without the renewal
	// path of relay-held streams (tests of the startup check only).
	withoutStreamRenewer bool

	// Test seams. RegistryNow is the registry clock (nil = time.Now).
	Registry    *registry.Registry
	Store       registry.Store
	Clock       clock.Clock
	RegistryNow func() time.Time
}

// conduitExternalRuntime is the hub's conduit runtime in external mode.
type conduitExternalRuntime struct {
	router   *router.Router
	registry *registry.Registry
	store    registry.Store
	now      func() time.Time
	// peerAuth authenticates relays calling the hub internal API.
	peerAuth relay.PeerAuth
	// replay overrides the shared relay-peer replay store (tests).
	replay relayPeerReplayStore
	// instanceID is this hub's instance id (the ingress instance header).
	instanceID string
	// ingress is the conduit ingress mode, and lifetimeCap the hub's
	// session lifetime cap sent on the ingress leg.
	ingress     string
	lifetimeCap time.Duration
	// ingressOffCause keeps the ingress proxy off ("" when it serves).
	ingressOffCause string
	// ingressTransport carries proxied conduit dials to the relays
	// (newConduitIngressTransport: bounded dial and response headers).
	ingressTransport http.RoundTripper
	// ingressCountFailures counts relay session counts that failed during
	// relay selection.
	ingressCountFailures atomic.Int64
	// peers calls the relays' internal API (the drain fan-out).
	peers *relay.PeerClient
	// terminating is set once, before the shutdown marks any server
	// (BeginConduitIngressShutdown): from then on no conduit dial is
	// proxied (cause ingress_terminating).
	terminating atomic.Bool
	// drainStarted makes the ingress drain step run once.
	drainStarted atomic.Bool
	// legs counts the hub's open proxied legs; the drain step holds them
	// and closes the ones left at its deadline.
	legs conduitIngressLegs
}

// StartConduitExternal starts the hub's side of external relay mode: the
// router over the shared registry, the user-stream re-check of the leaves
// this hub holds, and the eligible-relay readiness count. It must be called
// at most once, instead of StartConduitRelay, before the background
// services start.
func (s *Server) StartConduitExternal(ctx context.Context, opts ConduitExternalOptions) error {
	if !s.experimentEnabled(conduitExperiment) {
		return errConduitExternalDisabled
	}
	if s.conduit.Load() != nil || s.conduitExternal.Load() != nil {
		return errors.New("conduit already started")
	}
	// Admission tokens minted on one hub replica must open on every other.
	if _, err := s.conduitAdmissionAEAD(); err != nil {
		return fmt.Errorf("conduit external relay mode: %w", err)
	}
	if opts.PeerAuth == nil {
		return errors.New("conduit external relay mode: no relay-peer authenticator configured")
	}
	// Every relay-to-hub call records its nonce in the shared replay store;
	// without one the hub internal API would refuse every call.
	if opts.replayStore == nil && !s.hasRelayPeerReplayStore() {
		return errors.New("conduit external relay mode: no shared relay-peer replay store (no database client)")
	}
	// Relay-held streams renew only through the renewal re-check (design
	// v2.21.40 §3.9, Startup). Under target_stream_deadlines enforce,
	// targets end streams at their deadlines, so a relay mode without the
	// renewal path would end every relay-held stream at its first one.
	if !opts.withoutStreamRenewer {
		s.setConduitStreamRenewer(s.renewConduitStream)
	}
	if s.streamRenewer() == nil {
		if s.conduitTargetStreamDeadlines() == config.ConduitTargetStreamDeadlinesEnforce {
			return errors.New("conduit external relay mode: server.hub.conduit.target_stream_deadlines is enforce, but relay-held streams have no renewal path")
		}
		slog.Warn("Conduit external relay mode: relay-held streams have no renewal path; their re-checks are unavailable",
			"target_stream_deadlines", s.conduitTargetStreamDeadlines())
	}
	st := opts.Store
	if st == nil {
		s.mu.RLock()
		client := s.entClient
		s.mu.RUnlock()
		if client == nil {
			return errors.New("conduit external relay mode: no database client for the conduit registry")
		}
		st = entadapter.NewConduitRegistryStore(client)
	}
	reg := opts.Registry
	if reg == nil {
		reg = registry.New(st, registry.Config{})
	}
	now := opts.RegistryNow
	if now == nil {
		now = time.Now
	}
	id := opts.InstanceID
	if id == "" {
		id = ConduitInstanceID("")
	}
	peers := &relay.PeerClient{HTTP: opts.HTTPClient, Auth: opts.PeerAuth}
	rtr, err := router.New(router.Config{
		Registry: reg,
		Store:    st,
		Peers:    peers,
		Now:      now,
		Brokers:  newLegacyBrokerResolver(s.controlChannel, id),
	})
	if err != nil {
		return fmt.Errorf("conduit router: %w", err)
	}
	interval := opts.AuthzRecheckInterval
	if interval == 0 {
		interval = s.config.ConduitAuthzRecheckInterval
	}
	// The hub holds the leaf of its port proxy and PTY streams, so it keeps
	// re-checking them as in the in-process profile.
	_, stopAuthz, err := s.startConduitStreamAuthz(ctx, opts.Clock, interval)
	if err != nil {
		return err
	}
	rt := &conduitExternalRuntime{router: rtr, registry: reg, store: st, now: now, peerAuth: opts.PeerAuth, replay: opts.replayStore, peers: peers,
		instanceID: id, ingress: opts.Ingress, ingressOffCause: opts.IngressOffCause, lifetimeCap: opts.LifetimeCap, ingressTransport: newConduitIngressTransport(nil, conduitIngressResponseHeaderTimeout)}
	if rt.ingress == "" {
		rt.ingress = config.ConduitIngressProxy
	}
	if rt.lifetimeCap <= 0 {
		rt.lifetimeCap = config.ConduitDefaultLifetimeCap
	}
	if opts.HTTPClient != nil && opts.HTTPClient.Transport != nil {
		rt.ingressTransport = newConduitIngressTransport(opts.HTTPClient.Transport, conduitIngressResponseHeaderTimeout)
	}
	s.conduitEligibleRelays.Store(-1)
	if !s.conduitExternal.CompareAndSwap(nil, rt) {
		stopAuthz()
		return errors.New("conduit already started")
	}
	s.refreshConduitEligibleRelays(ctx, rt)
	go func() {
		t := time.NewTicker(conduitRelayRecheckPeriod)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.refreshConduitEligibleRelays(ctx, rt)
			}
		}
	}()
	return nil
}

// ConduitExternal reports whether the hub runs in external relay mode.
func (s *Server) ConduitExternal() bool {
	return s.conduitExternal.Load() != nil
}

// refreshConduitEligibleRelays counts the eligible relays for the readiness
// gauge and returns the count (-1 when the registry could not be read).
func (s *Server) refreshConduitEligibleRelays(ctx context.Context, rt *conduitExternalRuntime) int {
	relays, err := rt.registry.EligibleRelays(ctx, registry.TransportWS, rt.now())
	if err != nil {
		slog.Warn("Conduit: counting eligible relays failed", "error", err)
		s.conduitEligibleRelays.Store(-1)
		return -1
	}
	s.conduitEligibleRelays.Store(int64(len(relays)))
	return len(relays)
}

// conduitRouter returns the router of whichever conduit runtime runs on
// this node, or nil when none does.
func (s *Server) conduitRouter() *router.Router {
	if rt := s.conduit.Load(); rt != nil {
		return rt.router
	}
	if rt := s.conduitExternal.Load(); rt != nil {
		return rt.router
	}
	return nil
}

// conduitRegistry returns the registry and store of whichever conduit
// runtime runs on this node (nil, nil when none does).
func (s *Server) conduitRegistry() (*registry.Registry, registry.Store, func() time.Time) {
	if rt := s.conduit.Load(); rt != nil {
		return rt.registry, rt.store, rt.now
	}
	if rt := s.conduitExternal.Load(); rt != nil {
		return rt.registry, rt.store, rt.now
	}
	return nil, nil, nil
}

// conduitRoutable reports whether this node can route hub-originated
// conduit traffic (port proxy, agent control, PTY) right now: hub.conduit
// is on and a runtime, in-process or external, runs. conduitServing, which
// also decides what agents are told to dial, stays in-process only.
func (s *Server) conduitRoutable() bool {
	return s.experimentEnabled(conduitExperiment) && s.conduitRouter() != nil
}

// Messages of the external-mode refusal of GET /api/v1/conduit. Both carry
// reason conduit_not_serving; the message tells the two cases apart.
const (
	conduitExternalNoRelayMsg   = "No conduit relay is available (external relay mode with no eligible relay)"
	conduitExternalNoIngressMsg = "No conduit relay took the session (external relay mode)"
	conduitExternalIngressOff   = "This hub does not proxy conduit dials (external relay mode)"
	conduitExternalTerminating  = "This hub is shutting down; reconnect (external relay mode)"
)

// refuseConduitExternal answers a conduit dial on a hub in external relay
// mode: 503 unavailable with reason conduit_not_serving. The message tells
// whether any relay was eligible at the last count (refreshed every
// conduitRelayRecheckPeriod); a dial never reads the registry. The hub
// stays up.
func (s *Server) refuseConduitExternal(w http.ResponseWriter, r *http.Request, rt *conduitExternalRuntime) {
	msg := conduitExternalNoIngressMsg
	switch {
	case rt != nil && rt.terminating.Load():
		msg = conduitExternalTerminating
		slog.Debug("Conduit ingress: dial refused", "cause", conduitIngressCauseTerminating)
	case rt != nil && rt.ingressOffCause != "":
		msg = conduitExternalIngressOff
	case s.conduitEligibleRelays.Load() <= 0:
		msg = conduitExternalNoRelayMsg
	}
	writeError(w, http.StatusServiceUnavailable, ErrCodeUnavailable, msg,
		map[string]interface{}{"reason": ptyReasonConduitNotServing})
}

// RegisterConduitRelayReadinessGauge registers
// scion.hub.conduit.eligible_relays, the number of eligible separate relays
// at the last check. It reports only in external relay mode, and only once
// a check has succeeded; 0 means conduit dials get 503 conduit_not_serving.
// While the ingress proxy is off, every observation carries the attribute
// cause (for example ingress_requires_hmac), and dials get 503
// conduit_not_serving whatever the count.
func (s *Server) RegisterConduitRelayReadinessGauge(mp metric.MeterProvider) error {
	if mp == nil {
		return errors.New("conduit relay readiness gauge: nil MeterProvider")
	}
	_, err := mp.Meter(instrumentationScope).Int64ObservableGauge("scion.hub.conduit.eligible_relays",
		metric.WithUnit("{relay}"),
		metric.WithDescription("Eligible separate conduit relays (external relay mode); 0 means conduit dials are refused with conduit_not_serving"),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			if s.conduitExternal.Load() == nil {
				return nil
			}
			if n := s.conduitEligibleRelays.Load(); n >= 0 {
				if rt := s.conduitExternal.Load(); rt != nil && rt.ingressOffCause != "" {
					o.Observe(n, metric.WithAttributes(attribute.String("cause", rt.ingressOffCause)))
				} else {
					o.Observe(n)
				}
			}
			return nil
		}))
	return err
}
