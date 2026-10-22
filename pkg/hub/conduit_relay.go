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

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/clock"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/grant"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/router"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/target"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/transport/ws"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
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
	// sessionWriteWait, when positive, is the per-write deadline of every
	// relay session (conduit.Config.WriteWait; 0 = conduit.DefaultWriteWait).
	// It runs on Clock, so tests that advance a fake Clock set it above
	// their largest advance. It also stretches the backstop a session arms
	// when the peer sends GoAway (drain deadline + WriteWait). Tests only.
	sessionWriteWait time.Duration
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
	// LifetimeCap is the lifetime cap of a conduit session: the relay
	// sends GoAway{4503 relay_restart} 60s before it and every drain
	// deadline ends by it (server.hub.conduit.lifetime_cap; 0 = the
	// default, 3500s).
	LifetimeCap time.Duration
	// AuthzRecheckInterval overrides ServerConfig.ConduitAuthzRecheckInterval
	// (0 = use it; negative disables the sweep, for tests).
	AuthzRecheckInterval time.Duration

	// Test seams. RegistryNow is the clock of the registry maintenance
	// singleton (nil = time.Now).
	Registry    *registry.Registry
	Store       registry.Store
	Clock       clock.Clock
	RegistryNow func() time.Time
	// AuthzClock, when set, drives the user-stream re-check and its renewal
	// driver instead of Clock, so a test can move stream deadlines without
	// moving the relay's session timers.
	AuthzClock clock.Clock
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

// SetConduitRelayStartFailure records that the in-process relay failed to
// start. setting is the key of the setting that caused it, or "" when the
// cause was not one setting; it is shown in the relay-not-running response
// (writeConduitRelayNotRunning), so it must be a key, never a value.
func (s *Server) SetConduitRelayStartFailure(setting string) {
	s.conduitStartFailure.Store(&setting)
}

// writeConduitRelayNotRunning answers a conduit handshake on a node with no
// relay running (503). It names the failing setting when startup recorded
// one, and otherwise the only other cause: hub.conduit was turned on after
// startup.
func (s *Server) writeConduitRelayNotRunning(w http.ResponseWriter) {
	msg := "Conduit relay is not running on this hub node (hub.conduit was enabled after startup; restart required)"
	if p := s.conduitStartFailure.Load(); p != nil {
		if *p != "" {
			msg = "Conduit relay is not running on this hub node: startup failed on " + *p + "; see the startup log"
		} else {
			msg = "Conduit relay is not running on this hub node: startup failed; see the startup log"
		}
	}
	writeError(w, http.StatusServiceUnavailable, ErrCodeUnavailable, msg, nil)
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
	if s.conduit.Load() != nil || s.conduitExternal.Load() != nil {
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
	lifetimeCap := opts.LifetimeCap
	if lifetimeCap == 0 {
		lifetimeCap = config.ConduitDefaultLifetimeCap
	}
	r, err := relay.New(relay.Config{
		InstanceID:       id,
		InternalEndpoint: opts.InternalEndpoint,
		RequireSelfCheck: opts.RequireHA,
		Registry:         reg,
		Store:            st,
		GrantKeys:        grantKeys,
		Revalidate:       s.conduitRevalidatePrincipal,
		Refresh:          s.conduitAgentRefresh,
		PeerAuth:         opts.PeerAuth,
		HTTPClient:       opts.HTTPClient,
		Clock:            opts.Clock,
		Session:          conduit.Config{WriteWait: opts.sessionWriteWait},
		ReconnectWindow:  opts.ReconnectWindow,
		LifetimeCap:      lifetimeCap,
		OnBrokerSessionsSuperseded: func(brokerID string, closed int) {
			s.recordBrokerSessionsSuperseded(brokerID, brokerSupersededAtAdmission, closed)
		},
		OnBrokerIncarnationFlap: s.recordBrokerIncarnationFlap,
		Logger:                  slog.Default().With("subsystem", "hub.conduit"),
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
	// The stream re-check starts before the relay is published, so every
	// user stream opened through it is tracked.
	interval := opts.AuthzRecheckInterval
	if interval == 0 {
		interval = s.config.ConduitAuthzRecheckInterval
	}
	authzClock := opts.Clock
	if opts.AuthzClock != nil {
		authzClock = opts.AuthzClock
	}
	_, stopAuthz, err := s.startConduitStreamAuthz(ctx, authzClock, interval)
	if err != nil {
		return err
	}
	stopAuthz = s.startConduitBrokerRevoke(ctx, opts.Clock, interval, stopAuthz)
	// Published before Start: the internal listener is already serving and
	// the self-check probe must reach this relay's internal handler.
	if !s.conduit.CompareAndSwap(nil, rt) {
		stopAuthz()
		return errors.New("conduit relay already started")
	}
	if err := r.Start(ctx); err != nil {
		s.conduit.Store(nil)
		stopAuthz()
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
	return relay.InstanceID(configured)
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

// ConduitInternalHandler serves the internal relay API, or in external
// relay mode the hub internal API. It is mounted only on the internal
// listener, never on the public mux, and answers 503 while no runtime is
// published (not yet started, or its start failed).
func (s *Server) ConduitInternalHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ext := s.conduitExternal.Load(); ext != nil {
			// External relay mode: the hub internal API relays call.
			s.serveHubInternal(w, r, ext)
			return
		}
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
	keys, err := s.conduitHubDirectGrantKeys(ctx)
	if err != nil {
		if !errors.Is(err, errConduitDisabled) {
			slog.Warn("Conduit grant keys unavailable for the token-refresh response", "error", err)
		}
		return nil
	}
	return grant.ToWire(keys)
}

// SetConduitShutdownGrace records server.hub.conduit.shutdown_grace (the
// time the platform allows between SIGTERM and process kill) when it is
// set, so every relay shutdown path caps the GoAway drain deadline at
// grace - 2s: the SIGTERM drain (DrainConduitForShutdown) and the
// teardown's shutdownConduitRelay, which runs alone when the server
// context was cancelled without the SIGTERM drain (ptone/scion#5105
// timing). Unset (never called), the teardown's drain stays uncapped.
func (s *Server) SetConduitShutdownGrace(grace time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.conduitShutdownGrace, s.conduitShutdownGraceSet = grace, true
}

// conduitShutdownGraceSetting returns the grace SetConduitShutdownGrace
// recorded and whether it was set.
func (s *Server) conduitShutdownGraceSetting() (time.Duration, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.conduitShutdownGrace, s.conduitShutdownGraceSet
}

// shutdownConduitRelay drains the relay (relay row draining, GoAway to every
// session, wait until ctx is done). It runs first in CleanupResources. With
// a shutdown grace recorded (SetConduitShutdownGrace), the GoAway drain
// deadline is capped as in the SIGTERM drain; when that drain already ran,
// this only waits for it. The relay marks session rows draining with a
// bounded number of writes in flight; one batched per-relay UPDATE is
// tracked in ptone/scion#2897.
func (s *Server) shutdownConduitRelay(ctx context.Context) {
	rt := s.conduit.Load()
	if rt == nil {
		return
	}
	var opts relay.ShutdownOptions
	if grace, ok := s.conduitShutdownGraceSetting(); ok {
		opts = relay.ShutdownOptions{CapDrain: true, DrainCap: relay.ShutdownDrainCap(grace)}
	}
	if err := rt.relay.ShutdownWith(ctx, opts); err != nil {
		slog.Warn("Conduit relay shutdown", "error", err)
	}
}

// DrainConduitForShutdown starts the relay's SIGTERM drain (design §3.3)
// and returns once GoAway has been sent to every session the relay holds,
// so the caller can close the listeners after it. The drain itself goes on
// in the background; the later CleanupBackgroundResources waits for it.
// With capDrain (shutdown_grace set), the drain is bounded by grace, the
// time the platform allows between SIGTERM and process kill, and each
// GoAway's drain deadline is min(drain deadline, grace - 2s), or 1ms when
// that is 0 or less. Without it the drain is the same as a plain Shutdown.
// It returns at once when no relay runs.
func (s *Server) DrainConduitForShutdown(grace time.Duration, capDrain bool) {
	rt := s.conduit.Load()
	if rt == nil {
		return
	}
	ctx, cancel := context.Background(), context.CancelFunc(func() {})
	if capDrain {
		ctx, cancel = context.WithTimeout(ctx, grace)
	}
	sent := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer cancel()
		err := rt.relay.ShutdownWith(ctx, relay.ShutdownOptions{
			CapDrain:    capDrain,
			DrainCap:    relay.ShutdownDrainCap(grace),
			GoAwaysSent: func() { close(sent) }, // called exactly once
		})
		if err != nil {
			slog.Warn("Conduit relay SIGTERM drain", "error", err)
		}
	}()
	select {
	case <-sent:
	case <-done:
	}
}

// conduitRegistryReapHandler is the registry maintenance singleton: stale
// relays' sessions, stale sessions and idle relay rows, with the registry's
// default horizons.
func (s *Server) conduitRegistryReapHandler() func(ctx context.Context) {
	return func(ctx context.Context) {
		reg, _, clk := s.conduitRegistry()
		if reg == nil {
			return
		}
		now := clk()
		if n, err := reg.ReapStaleRelays(ctx, 0, now); err != nil {
			slog.Warn("Conduit registry: reaping sessions of stale relays failed", "error", err)
		} else if n > 0 {
			slog.Info("Conduit registry: reaped sessions of stale relays", "count", n)
		}
		if n, err := reg.ReapStaleSessions(ctx, 0, now); err != nil {
			slog.Warn("Conduit registry: reaping stale sessions failed", "error", err)
		} else if n > 0 {
			slog.Info("Conduit registry: reaped stale sessions", "count", n)
		}
		if n, err := reg.PruneRelayInstances(ctx, 0, now); err != nil {
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
	reg, st, _ := s.conduitRegistry()
	if reg == nil || agentID == "" {
		return
	}
	// The local close needs a local relay. In external relay mode the
	// owner relay closes the sessions when its session re-check sees the
	// agent gone (the agent deletion event, or its next sweep).
	if rt := s.conduit.Load(); rt != nil {
		rt.relay.CloseSessionsOf(registry.PrincipalAgent, agentID, conduit.CloseUnauthenticated, relay.ReasonUnauthenticated+": agent deleted")
	}
	ps, err := st.ListPrincipalSessions(ctx, registry.PrincipalAgent, agentID)
	if err != nil {
		slog.Warn("Conduit: listing sessions of a deleted agent failed", "agent_id", agentID, "error", err)
	} else {
		for _, v := range ps.Sessions {
			if _, err := reg.DeleteSessionCAS(ctx, v.Session.SessionID, v.Session.RelayInstanceID, v.Session.RelayGeneration); err != nil {
				slog.Warn("Conduit: deleting a session of a deleted agent failed", "agent_id", agentID, "session_id", v.Session.SessionID, "error", err)
			}
		}
	}
	if err := reg.ForgetPrincipalEpoch(ctx, registry.PrincipalAgent, agentID); err != nil {
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
	switch p.Kind {
	case registry.PrincipalBroker:
		return s.conduitRevalidateBroker(ctx, p)
	case registry.PrincipalAgent:
	case registry.PrincipalUser:
		// A user session reaches nothing by itself: each tunnel on it is
		// authorized when opened and re-checked by the stream re-check.
		return nil
	default:
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

// conduitAdmission is the pre-upgrade admission decision of a conduit
// handshake (decideConduit): the principal the session is admitted as and
// what serving it needs. The decision only reads; the session is created
// by serveConduitAdmitted.
type conduitAdmission struct {
	rt        *conduitRuntime
	principal relay.Principal
	// brokerSecret is the fingerprint of the broker secret the upgrade
	// was signed with (brokers only).
	brokerSecret conduitBrokerSecretFingerprint
	// user and credentialExpiry are the user principal and its
	// credential's expiry (users only).
	user             UserIdentity
	credentialExpiry time.Time
}

// handleConduit serves GET /api/v1/conduit: a conduit session for an
// agent, a Runtime Broker (HMAC headers, decideConduitBroker) or a user
// (decideConduitUser; the session asks for tunnels by RPC). Agent
// principals need an agent token with agent:port:forward; anonymous
// callers and other principals are refused at the HTTP layer before any
// upgrade. An agent's project, launch id and generation come from the
// agent row read here, never from the Hello; admission then applies the
// 1d-i incarnation policy (launch-id match, the legacy-fallback fence,
// gen-N fallback).
func (s *Server) handleConduit(w http.ResponseWriter, r *http.Request) {
	if rt := s.conduitExternal.Load(); rt != nil {
		// External relay mode: this hub holds no conduit sessions.
		switch {
		case !s.experimentEnabled(conduitExperiment):
			NotFound(w, "route")
		case r.Method != http.MethodGet:
			MethodNotAllowed(w, http.MethodGet)
		case rt.ingress == config.ConduitIngressProxy && rt.ingressOffCause == "" && !rt.terminating.Load():
			s.serveConduitIngress(w, r, rt)
		default:
			s.refuseConduitExternal(w, r, rt)
		}
		return
	}
	a, ok := s.decideConduit(w, r)
	if !ok {
		return
	}
	s.serveConduitAdmitted(w, r, a)
}

// decideConduit runs every pre-upgrade check of a conduit handshake and
// returns the admission. On a refusal it writes the response and returns
// false.
func (s *Server) decideConduit(w http.ResponseWriter, r *http.Request) (conduitAdmission, bool) {
	return s.decideConduitFor(w, r, false)
}

// decideConduitFor is decideConduit; forRelay decides for a separate relay
// (Admit), which holds the session itself, so no in-process relay is
// required and the admission carries no runtime.
func (s *Server) decideConduitFor(w http.ResponseWriter, r *http.Request, forRelay bool) (conduitAdmission, bool) {
	if !s.experimentEnabled(conduitExperiment) {
		NotFound(w, "route")
		return conduitAdmission{}, false
	}
	if r.Method != http.MethodGet {
		MethodNotAllowed(w, http.MethodGet)
		return conduitAdmission{}, false
	}
	// One arm per principal kind; anything else is refused before any
	// upgrade. A broker authenticates with HMAC headers in its own handler,
	// so it is recognized by its broker-id header rather than by a context
	// identity.
	identity := GetIdentityFromContext(r.Context())
	agentIdent, isAgent := identity.(AgentIdentity)
	userIdent, isUser := identity.(UserIdentity)
	switch {
	case r.Header.Get(HeaderBrokerID) != "":
		return s.decideConduitBroker(w, r, forRelay)
	case isAgent:
		return s.decideConduitAgent(w, r, agentIdent, forRelay)
	case isUser:
		return s.decideConduitUser(w, r, userIdent, forRelay)
	default:
		writeError(w, http.StatusForbidden, ErrCodeForbidden, "Conduit sessions are open to agent, broker and user principals only", nil)
		return conduitAdmission{}, false
	}
}

// decideConduitAgent is decideConduit for an agent principal.
func (s *Server) decideConduitAgent(w http.ResponseWriter, r *http.Request, ident AgentIdentity, forRelay bool) (conduitAdmission, bool) {
	if err := conduitAgentScopeCheck(ident); err != nil {
		writeError(w, http.StatusForbidden, ErrCodeForbidden, "Missing required scope: agent:port:forward", nil)
		return conduitAdmission{}, false
	}
	rt := s.conduit.Load()
	if rt == nil && !forRelay {
		s.writeConduitRelayNotRunning(w)
		return conduitAdmission{}, false
	}
	agent, err := s.conduitAgentRowCheck(r.Context(), ident)
	switch {
	case errors.Is(err, errConduitAgentGone):
		writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized, "Agent no longer exists", nil)
		return conduitAdmission{}, false
	case errors.Is(err, errConduitAgentMismatch):
		writeError(w, http.StatusForbidden, ErrCodeForbidden, "Agent token does not match the agent", nil)
		return conduitAdmission{}, false
	case err != nil:
		writeErrorFromErr(w, errors.Unwrap(err), "") // the store error, as read
		return conduitAdmission{}, false
	}
	if !isWebSocketUpgrade(r) {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "WebSocket upgrade required", nil)
		return conduitAdmission{}, false
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
	return conduitAdmission{rt: rt, principal: p}, true
}

// serveConduitAdmitted upgrades the connection and serves the admitted
// session until it ends. An admission of any other principal kind is
// refused before any upgrade.
func (s *Server) serveConduitAdmitted(w http.ResponseWriter, r *http.Request, a conduitAdmission) {
	switch a.principal.Kind {
	case registry.PrincipalAgent:
		s.serveConduitAgent(w, r, a)
	case registry.PrincipalBroker:
		s.serveConduitBroker(w, r, a)
	case registry.PrincipalUser:
		s.serveConduitUser(w, r, a)
	default:
		writeError(w, http.StatusForbidden, ErrCodeForbidden, "Conduit sessions are open to agent, broker and user principals only", nil)
	}
}

// serveConduitAgent serves an admitted agent session.
func (s *Server) serveConduitAgent(w http.ResponseWriter, r *http.Request, a conduitAdmission) {
	conn, err := ws.Upgrade(w, r, nil, ws.Options{})
	if err != nil {
		return
	}
	rt, p := a.rt, a.principal
	// The session outlives no request deadline: it ends when the
	// connection closes or the relay drains.
	ctx := context.WithoutCancel(r.Context())
	if err := rt.relay.Serve(ctx, conn, p); err != nil && !errors.Is(err, relay.ErrNotServing) {
		slog.Debug("Conduit session ended", "agent_id", p.ID, "error", err)
	}
	// An agent deleted during the handshake: the session row is gone
	// (Serve deleted it); drop the epoch row its admission recreated.
	if errors.Is(s.conduitRevalidatePrincipal(ctx, p), errConduitAgentGone) {
		if err := rt.registry.ForgetPrincipalEpoch(ctx, registry.PrincipalAgent, p.ID); err != nil {
			slog.Warn("Conduit: deleting the epoch row of a deleted agent failed", "agent_id", p.ID, "error", err)
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
