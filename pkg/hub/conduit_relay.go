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
	"os"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/clock"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/grant"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/router"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/target"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/transport/ws"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
	"github.com/google/uuid"
)

// The hub's in-process conduit relay (design v2.4 §3.9: every hub process
// runs one when hub.conduit is on).
//
// Lifecycle. StartConduitRelay is called once at startup, after the
// internal listener (if any) is serving. Whether a relay runs is decided
// there, from hub.conduit as it is at startup: turning the experiment on or
// off takes effect for the relay only after a restart. The per-request
// checks (GET /api/v1/conduit answers 404 when the flag is off) follow the
// live flag. A hub started with the flag off runs no relay, internal
// listener, registry jobs or LISTEN of any kind.
//
// Startup checks (C8). In hosted-HA mode the relay must be addressable and
// pass registry.SelfCheck (relay.Config.RequireSelfCheck), and the grant
// key ring must be persisted with the shared at-rest key; any failure is
// returned and the server exits non-zero. Outside hosted-HA an
// unaddressable relay only logs a warning.

// ErrConduitNoAtRestKey is the C8 startup error for a hosted-HA hub whose
// grant key ring cannot be shared between nodes.
var ErrConduitNoAtRestKey = errors.New("conduit: hosted-HA requires the grant key ring to be persisted with the shared at-rest encryption key, but none is configured (each node would hold its own ring and refuse the others' grants); set the shared session/signing secret (--session-secret or SCION_SERVER_SESSION_SECRET) on every node")

// conduitRegistryReapInterval is the period (minutes) of the registry
// maintenance singleton.
const conduitRegistryReapInterval = 1

// ConduitRelayOptions configures StartConduitRelay.
type ConduitRelayOptions struct {
	// InstanceID is this process's relay instance id ("" =
	// ConduitInstanceID("")). It must be unique among live processes.
	InstanceID string
	// testHookAdmission, when set, runs during admission (before the
	// session is registered). Tests only.
	testHookAdmission func()
	// InternalEndpoint is the advertised base URL of this process's
	// internal listener ("" = unaddressable).
	InternalEndpoint string
	// RequireHA applies the hosted-HA startup checks (C8).
	RequireHA bool
	// PeerAuth authenticates the internal relay API. Required.
	PeerAuth relay.PeerAuth
	// HTTPClient performs the self-check probe (nil: relay default).
	HTTPClient *http.Client
	// ReconnectWindow is GoAway.reconnect_after_ms, the jitter window
	// targets draw their redial delay from (0 = the relay default, 5s).
	ReconnectWindow time.Duration

	// Test seams. RegistryNow is the clock of the registry maintenance
	// singleton (nil = time.Now).
	Registry    *registry.Registry
	Store       registry.Store
	Clock       clock.Clock
	RegistryNow func() time.Time
}

// conduitRuntime is the running relay and what it was built from.
type conduitRuntime struct {
	relay    *relay.Relay
	registry *registry.Registry
	router   *router.Router
	store    registry.Store
	now      func() time.Time
}

// ConduitEnabled reports whether hub.conduit is on right now. The server
// command uses it at startup to decide whether to open the internal
// listener and start the relay.
func (s *Server) ConduitEnabled() bool {
	return s.experimentEnabled(conduitExperiment)
}

// conduitServing reports whether agents should dial the conduit endpoint:
// hub.conduit is on and this node runs the relay.
func (s *Server) conduitServing() bool {
	return s.conduitServingIn(s.experimentsSnapshot())
}

// conduitServingIn is conduitServing with hub.conduit read from snap.
func (s *Server) conduitServingIn(snap ExperimentsSnapshot) bool {
	return s.experimentEnabledIn(snap, conduitExperiment) && s.conduit.Load() != nil
}

// ConduitGrantRingShared reports whether the grant key ring is persisted
// with the shared at-rest encryption key, so every hub node signs and
// verifies with the same ring. Without it each node holds its own
// in-memory ring, which hosted HA refuses (C8).
func (s *Server) ConduitGrantRingShared() bool {
	return len(s.encryptionKey) > 0
}

// StartConduitRelay builds and starts the in-process relay when hub.conduit
// is on (a no-op otherwise). It must be called at most once, before the
// background services start (they register the registry singleton only
// when a relay runs).
func (s *Server) StartConduitRelay(ctx context.Context, opts ConduitRelayOptions) error {
	if !s.experimentEnabled(conduitExperiment) {
		slog.Info("Conduit relay not started: hub.conduit is off (turning it on takes effect after a restart)")
		return nil
	}
	if s.conduit.Load() != nil {
		return errors.New("conduit relay already started")
	}
	if opts.RequireHA && !s.ConduitGrantRingShared() {
		return ErrConduitNoAtRestKey
	}
	if opts.PeerAuth == nil {
		return errors.New("conduit relay: no relay-peer authenticator configured")
	}
	st := opts.Store
	if st == nil {
		s.mu.RLock()
		client := s.entClient
		s.mu.RUnlock()
		if client == nil {
			return errors.New("conduit relay: no database client for the conduit registry")
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
	grantKeys := relay.GrantKeySource(s.conduitWelcomeGrantKeys)
	if h := opts.testHookAdmission; h != nil {
		grantKeys = func(ctx context.Context) ([]*conduitv1.GrantKey, error) {
			h()
			return s.conduitWelcomeGrantKeys(ctx)
		}
	}
	r, err := relay.New(relay.Config{
		InstanceID:       id,
		InternalEndpoint: opts.InternalEndpoint,
		RequireSelfCheck: opts.RequireHA,
		Registry:         reg,
		Store:            st,
		GrantKeys:        grantKeys,
		Revalidate:       s.conduitRevalidatePrincipal,
		PeerAuth:         opts.PeerAuth,
		HTTPClient:       opts.HTTPClient,
		Clock:            opts.Clock,
		ReconnectWindow:  opts.ReconnectWindow,
		Logger:           slog.Default().With("subsystem", "hub.conduit"),
	})
	if err != nil {
		return fmt.Errorf("conduit relay: %w", err)
	}
	rtr, err := router.New(router.Config{
		Relay:    r,
		Registry: reg,
		Store:    st,
		Peers:    &relay.PeerClient{HTTP: opts.HTTPClient, Auth: opts.PeerAuth},
		Now:      now,
		Brokers:  newLegacyBrokerResolver(s.controlChannel, id),
	})
	if err != nil {
		return fmt.Errorf("conduit router: %w", err)
	}
	rt := &conduitRuntime{relay: r, registry: reg, router: rtr, store: st, now: now}
	// Published before Start: the internal listener is already serving and
	// the self-check probe must reach this relay's internal handler.
	if !s.conduit.CompareAndSwap(nil, rt) {
		return errors.New("conduit relay already started")
	}
	if err := r.Start(ctx); err != nil {
		s.conduit.Store(nil)
		return err
	}
	if r.InternalEndpoint() == "" && !opts.RequireHA {
		slog.Warn("Conduit relay is unaddressable (no internal endpoint); this is only correct for a single-node hub")
	}
	return nil
}

// ConduitInstanceID returns this process's relay instance id: the
// configured id, else POD_NAME, else the host name plus a random
// per-process suffix (a random id when there is no host name). Two live
// processes must never share an id: a relay start supersedes any relay
// registered under the same id.
func ConduitInstanceID(configured string) string {
	if configured != "" {
		return configured
	}
	if pod := os.Getenv("POD_NAME"); pod != "" {
		return pod
	}
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	if h, err := os.Hostname(); err == nil && h != "" {
		return h + "-" + suffix
	}
	return uuid.NewString()
}

// ConduitRelayFatal is closed or receives when the relay stops serving on
// its own (it was superseded); the process should exit and restart. It
// returns nil when no relay runs.
func (s *Server) ConduitRelayFatal() <-chan error {
	if rt := s.conduit.Load(); rt != nil {
		return rt.relay.Fatal()
	}
	return nil
}

// ConduitInternalHandler serves the internal relay API. It is mounted only
// on the internal listener, never on the public mux, and answers 503 while
// no relay is published (not yet started, or its start failed).
func (s *Server) ConduitInternalHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rt := s.conduit.Load()
		if rt == nil {
			http.Error(w, "conduit relay not running", http.StatusServiceUnavailable)
			return
		}
		rt.relay.InternalHandler().ServeHTTP(w, r)
	})
}

// conduitWelcomeGrantKeys supplies Welcome.grant_keys.
func (s *Server) conduitWelcomeGrantKeys(ctx context.Context) ([]*conduitv1.GrantKey, error) {
	keys, err := s.conduitGrantKeySet().publicKeys(ctx)
	if err != nil {
		return nil, err
	}
	return target.KeysToProto(keys), nil
}

// conduitRefreshGrantKeys returns the grant keys for the agent
// token-refresh response, or nil when the experiment is off or the keys are
// unavailable (the refresh itself must not fail over them).
func (s *Server) conduitRefreshGrantKeys(ctx context.Context) []grant.WireKey {
	keys, err := s.ConduitGrantPublicKeys(ctx)
	if err != nil {
		if !errors.Is(err, errConduitDisabled) {
			slog.Warn("Conduit grant keys unavailable for the token-refresh response", "error", err)
		}
		return nil
	}
	return grant.ToWire(keys)
}

// shutdownConduitRelay drains the relay (relay row draining, GoAway to every
// session, wait until ctx is done). It runs first in CleanupResources. The
// relay marks session rows draining with a bounded number of writes in
// flight; one batched per-relay UPDATE is tracked in ptone/scion#2897.
func (s *Server) shutdownConduitRelay(ctx context.Context) {
	rt := s.conduit.Load()
	if rt == nil {
		return
	}
	if err := rt.relay.Shutdown(ctx); err != nil {
		slog.Warn("Conduit relay shutdown", "error", err)
	}
}

// conduitRegistryReapHandler is the registry maintenance singleton: stale
// relays' sessions, stale sessions and idle relay rows, with the registry's
// default horizons.
func (s *Server) conduitRegistryReapHandler() func(ctx context.Context) {
	return func(ctx context.Context) {
		rt := s.conduit.Load()
		if rt == nil {
			return
		}
		now := rt.now()
		if n, err := rt.registry.ReapStaleRelays(ctx, 0, now); err != nil {
			slog.Warn("Conduit registry: reaping sessions of stale relays failed", "error", err)
		} else if n > 0 {
			slog.Info("Conduit registry: reaped sessions of stale relays", "count", n)
		}
		if n, err := rt.registry.ReapStaleSessions(ctx, 0, now); err != nil {
			slog.Warn("Conduit registry: reaping stale sessions failed", "error", err)
		} else if n > 0 {
			slog.Info("Conduit registry: reaped stale sessions", "count", n)
		}
		if n, err := rt.registry.PruneRelayInstances(ctx, 0, now); err != nil {
			slog.Warn("Conduit registry: pruning idle relay rows failed", "error", err)
		} else if n > 0 {
			slog.Info("Conduit registry: pruned idle relay rows", "count", n)
		}
	}
}

// conduitForgetAgent runs on agent deletion: it closes the agent's local
// sessions (4401), deletes every session row of the agent on any relay (a
// remote relay then finds its row gone on the next touch and closes the
// session) and deletes the agent's epoch counter. Errors are logged; rows
// left behind are reaped and are already unroutable (no agent row).
func (s *Server) conduitForgetAgent(ctx context.Context, agentID string) {
	rt := s.conduit.Load()
	if rt == nil || agentID == "" {
		return
	}
	rt.relay.CloseSessionsOf(registry.PrincipalAgent, agentID, conduit.CloseUnauthenticated, relay.ReasonUnauthenticated+": agent deleted")
	ps, err := rt.store.ListPrincipalSessions(ctx, registry.PrincipalAgent, agentID)
	if err != nil {
		slog.Warn("Conduit: listing sessions of a deleted agent failed", "agent_id", agentID, "error", err)
	} else {
		for _, v := range ps.Sessions {
			if _, err := rt.registry.DeleteSessionCAS(ctx, v.Session.SessionID, v.Session.RelayInstanceID, v.Session.RelayGeneration); err != nil {
				slog.Warn("Conduit: deleting a session of a deleted agent failed", "agent_id", agentID, "session_id", v.Session.SessionID, "error", err)
			}
		}
	}
	if err := rt.registry.ForgetPrincipalEpoch(ctx, registry.PrincipalAgent, agentID); err != nil {
		slog.Warn("Conduit: deleting the epoch row of a deleted agent failed", "agent_id", agentID, "error", err)
	}
}

// errConduitAgentGone is the revalidation failure of a deleted agent.
var errConduitAgentGone = errors.New("agent no longer exists")

// conduitRevalidatePrincipal re-reads the agent row once a session is
// admitted and registered: an agent deleted after handleConduit's read but
// before registration (so the delete's forget step did not see the
// session) is closed with 4401. A read error other than not-found keeps
// the session; the forget step and the reapers still cover it.
func (s *Server) conduitRevalidatePrincipal(ctx context.Context, p relay.Principal) error {
	if p.Kind != registry.PrincipalAgent {
		return nil
	}
	a, err := s.store.GetAgent(ctx, p.ID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return errConduitAgentGone
	case err != nil:
		slog.Warn("Conduit: re-reading the agent after admission failed; keeping the session", "agent_id", p.ID, "error", err)
		return nil
	case !a.DeletedAt.IsZero():
		return errConduitAgentGone
	}
	return nil
}

// handleConduit serves GET /api/v1/conduit: an agent's conduit session.
// Only agent principals (agent token with agent:port:forward) may connect;
// users, brokers and anonymous callers are refused at the HTTP layer
// before any upgrade. The principal's project, launch id and generation
// come from the agent row read here, never from the Hello; admission then
// applies the 1d-i incarnation policy (launch-id match, the
// legacy-fallback fence, gen-N fallback).
func (s *Server) handleConduit(w http.ResponseWriter, r *http.Request) {
	if !s.experimentEnabled(conduitExperiment) {
		NotFound(w, "route")
		return
	}
	if r.Method != http.MethodGet {
		MethodNotAllowed(w, http.MethodGet)
		return
	}
	ident := GetAgentIdentityFromContext(r.Context())
	if ident == nil {
		writeError(w, http.StatusForbidden, ErrCodeForbidden, "Conduit sessions are open to agent principals only", nil)
		return
	}
	if !ident.HasScope(ScopeAgentPortForward) {
		writeError(w, http.StatusForbidden, ErrCodeForbidden, "Missing required scope: agent:port:forward", nil)
		return
	}
	rt := s.conduit.Load()
	if rt == nil {
		writeError(w, http.StatusServiceUnavailable, ErrCodeUnavailable, "Conduit relay is not running on this hub node (hub.conduit was enabled after startup; restart required)", nil)
		return
	}
	agent, err := s.store.GetAgent(r.Context(), ident.ID())
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized, "Agent no longer exists", nil)
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}
	if agent.ProjectID != ident.ProjectID() || !agent.DeletedAt.IsZero() {
		writeError(w, http.StatusForbidden, ErrCodeForbidden, "Agent token does not match the agent", nil)
		return
	}
	if !isWebSocketUpgrade(r) {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "WebSocket upgrade required", nil)
		return
	}
	conn, err := ws.Upgrade(w, r, nil, ws.Options{})
	if err != nil {
		return
	}
	p := relay.Principal{
		Kind:      registry.PrincipalAgent,
		ID:        agent.ID,
		ProjectID: agent.ProjectID,
		Agent:     agentIncarnationFacts(agent),
	}
	if rs := s.authConfig.AgentRunScope; rs != nil {
		if claims := GetAgentFromContext(r.Context()); claims != nil {
			p.TokenRun = rs.conduitBinding(context.WithoutCancel(r.Context()), claims, runScopeRequestFrom(r))
		}
	}
	// The session outlives no request deadline: it ends when the
	// connection closes or the relay drains.
	ctx := context.WithoutCancel(r.Context())
	if err := rt.relay.Serve(ctx, conn, p); err != nil && !errors.Is(err, relay.ErrNotServing) {
		slog.Debug("Conduit session ended", "agent_id", agent.ID, "error", err)
	}
	// An agent deleted during the handshake: the session row is gone
	// (Serve deleted it); drop the epoch row its admission recreated.
	if errors.Is(s.conduitRevalidatePrincipal(ctx, p), errConduitAgentGone) {
		if err := rt.registry.ForgetPrincipalEpoch(ctx, registry.PrincipalAgent, agent.ID); err != nil {
			slog.Warn("Conduit: deleting the epoch row of a deleted agent failed", "agent_id", agent.ID, "error", err)
		}
	}
}

// agentIncarnationFacts returns the agent-row values that conduit admission
// and routing compare a session's endpoint incarnation with. The agent's
// launch id is its current run id (agents.run_id): the broker labels each
// container it starts with the run id and passes the same value in
// SCION_LAUNCH_ID, which sciontool presents in Hello.
func agentIncarnationFacts(agent *store.Agent) relay.AgentIncarnationFacts {
	return relay.AgentIncarnationFacts{LaunchID: agent.RunID, Generation: int64(agent.Generation)}
}
