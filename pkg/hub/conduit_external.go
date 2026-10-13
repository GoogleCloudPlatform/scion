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
	"time"

	"go.opentelemetry.io/otel/metric"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/clock"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/router"
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
// The hub's GET /api/v1/conduit serves no sessions in this mode. Until the
// ingress (proxy or redirect) lands, a dial gets 503 unavailable with
// reason conduit_not_serving, and the readiness gauge reports how many
// relays are eligible. The hub does not advertise hub.conduit to agents
// (dispatchExperiments) in this mode.

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
	// AuthzRecheckInterval overrides ServerConfig.ConduitAuthzRecheckInterval
	// (0 = use it; negative disables the sweep, for tests).
	AuthzRecheckInterval time.Duration

	// replayStore replaces the shared relay-peer replay store (tests).
	replayStore relayPeerReplayStore

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
	if opts.PeerAuth == nil {
		return errors.New("conduit external relay mode: no relay-peer authenticator configured")
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
	rtr, err := router.New(router.Config{
		Registry: reg,
		Store:    st,
		Peers:    &relay.PeerClient{HTTP: opts.HTTPClient, Auth: opts.PeerAuth},
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
	rt := &conduitExternalRuntime{router: rtr, registry: reg, store: st, now: now, peerAuth: opts.PeerAuth, replay: opts.replayStore}
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
	conduitExternalNoIngressMsg = "This hub does not serve conduit sessions in external relay mode"
)

// refuseConduitExternal answers a conduit dial on a hub in external relay
// mode: 503 unavailable with reason conduit_not_serving. The message tells
// whether any relay was eligible at the last count (refreshed every
// conduitRelayRecheckPeriod); a dial never reads the registry. The hub
// stays up.
func (s *Server) refuseConduitExternal(w http.ResponseWriter, r *http.Request, _ *conduitExternalRuntime) {
	msg := conduitExternalNoIngressMsg
	if s.conduitEligibleRelays.Load() <= 0 {
		msg = conduitExternalNoRelayMsg
	}
	writeError(w, http.StatusServiceUnavailable, ErrCodeUnavailable, msg,
		map[string]interface{}{"reason": ptyReasonConduitNotServing})
}

// RegisterConduitRelayReadinessGauge registers
// scion.hub.conduit.eligible_relays, the number of eligible separate relays
// at the last check. It reports only in external relay mode, and only once
// a check has succeeded; 0 means conduit dials get 503 conduit_not_serving.
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
				o.Observe(n)
			}
			return nil
		}))
	return err
}
