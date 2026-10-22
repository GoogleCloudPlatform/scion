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

package cmd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/compute/metadata"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/clock"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/util/cloudrunenv"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// Conduit flags of `server start`. Each overrides its server.hub.conduit
// setting when set explicitly.
var (
	conduitInternalListen     string
	conduitInternalAdvertise  string
	conduitGrantKeyActivation string
	conduitReconnectWindow    string
	conduitAuthzRecheck       string
	conduitLifetimeCap        string
	conduitUserStreamAuthzMax string
	conduitTCPAllowedPorts    []int
)

// registerConduitServerFlags registers the conduit flags of `server start`.
func registerConduitServerFlags(f *pflag.FlagSet) {
	f.StringVar(&conduitInternalListen, "internal-listen", "", "host:port of the internal conduit relay API listener (hub.conduit; multi-node hubs). Must be reachable only inside the cluster/VPC. TLS on this hop is recommended (e.g. a service mesh or TLS-terminating proxy, advertised as https://); plain http:// is accepted")
	f.StringVar(&conduitInternalAdvertise, "internal-advertise", "", "Base URL other hub nodes use to reach the internal listener (default: POD_IP or the listen host). Prefer https:// (TLS on the internal hop); http:// is accepted")
	f.StringVar(&conduitGrantKeyActivation, "conduit-grant-key-activation", "", "Publish-before-sign delay of a new conduit grant key (default 15m, minimum 1m)")
	f.StringVar(&conduitReconnectWindow, "conduit-reconnect-window", "", "Jitter window targets redial in after a planned conduit close (default 5s, 0s-5m)")
	f.StringVar(&conduitAuthzRecheck, "conduit-authz-recheck-interval", "", "Period of the authorization re-check sweep of open conduit user streams (default 60s, 1s-10m)")
	f.StringVar(&conduitLifetimeCap, "conduit-lifetime-cap", "", "Platform lifetime cap of a conduit session; the relay sends GoAway 60s before it (default 3500s, 90s-24h)")
	f.StringVar(&conduitUserStreamAuthzMax, "conduit-stream-authz-max-user", "", "Authorization interval of user-originated conduit streams: at its end the hub re-checks the user and renews or closes the stream (default 8h, 1m-168h)")
	f.StringVar(&serverRole, "role", serverRoleHub, "Server role: hub (default) or relay, a separate conduit relay process (requires the hub.conduit experiment, server.hub.conduit.peer_secret, server.relay.hub_internal_url, server.relay.public_url and server.hub.conduit.internal_listen)")
	f.IntSliceVar(&conduitTCPAllowedPorts, "conduit-tcp-allowed-ports", nil, "Additional agent-local ports a conduit TCP stream may target besides the agent's exposed ports (comma-separated; reserved ports are always refused; default: exposed ports only)")
}

// applyConduitFlagOverrides copies the explicitly set conduit flags into
// cfg. validateServerPreflight checks the result.
func applyConduitFlagOverrides(cmd *cobra.Command, cfg *config.GlobalConfig) {
	f := cmd.Flags()
	if f.Changed("internal-listen") {
		cfg.Hub.Conduit.InternalListen = conduitInternalListen
	}
	if f.Changed("internal-advertise") {
		cfg.Hub.Conduit.InternalAdvertise = conduitInternalAdvertise
	}
	if f.Changed("conduit-grant-key-activation") {
		cfg.Hub.Conduit.GrantKeyActivation = conduitGrantKeyActivation
	}
	if f.Changed("conduit-reconnect-window") {
		cfg.Hub.Conduit.ReconnectWindow = conduitReconnectWindow
	}
	if f.Changed("conduit-authz-recheck-interval") {
		cfg.Hub.Conduit.AuthzRecheckInterval = conduitAuthzRecheck
	}
	if f.Changed("conduit-lifetime-cap") {
		cfg.Hub.Conduit.LifetimeCap = conduitLifetimeCap
	}
	if f.Changed("conduit-stream-authz-max-user") {
		cfg.Hub.Conduit.StreamAuthzMax.User = conduitUserStreamAuthzMax
	}
	if f.Changed("conduit-tcp-allowed-ports") {
		cfg.Hub.Conduit.TCPAllowedPorts = append([]int(nil), conduitTCPAllowedPorts...)
	}
}

// appendConduitDaemonArgs forwards the explicitly set conduit flags to the
// --foreground daemon child.
func appendConduitDaemonArgs(cmd *cobra.Command, args []string) []string {
	f := cmd.Flags()
	for _, name := range []string{"role", "internal-listen", "internal-advertise", "conduit-grant-key-activation", "conduit-reconnect-window", "conduit-authz-recheck-interval", "conduit-lifetime-cap", "conduit-stream-authz-max-user"} {
		if f.Changed(name) {
			args = append(args, fmt.Sprintf("--%s=%s", name, f.Lookup(name).Value.String()))
		}
	}
	if f.Changed("conduit-tcp-allowed-ports") {
		ports := make([]string, len(conduitTCPAllowedPorts))
		for i, p := range conduitTCPAllowedPorts {
			ports[i] = strconv.Itoa(p)
		}
		args = append(args, "--conduit-tcp-allowed-ports="+strings.Join(ports, ","))
	}
	return args
}

// conduitGrantKeyActivationSetting returns the configured grant key activation
// (0 = hub default). validateServerPreflight has already rejected a
// malformed value.
func conduitGrantKeyActivationSetting(cfg *config.GlobalConfig) time.Duration {
	d, err := cfg.Hub.Conduit.GrantKeyActivationDuration()
	if err != nil {
		return 0
	}
	return d
}

// conduitReconnectWindowSetting returns the configured GoAway jitter window (0 =
// relay default). validateServerPreflight has already rejected a malformed
// value.
func conduitReconnectWindowSetting(cfg *config.GlobalConfig) time.Duration {
	d, err := cfg.Hub.Conduit.ReconnectWindowDuration()
	if err != nil {
		return 0
	}
	return d
}

// conduitAuthzRecheckIntervalSetting returns the configured user-stream
// re-check sweep period (0 = hub default). validateServerPreflight has
// already rejected a malformed value.
func conduitAuthzRecheckIntervalSetting(cfg *config.GlobalConfig) time.Duration {
	d, err := cfg.Hub.Conduit.AuthzRecheckIntervalDuration()
	if err != nil {
		return 0
	}
	return d
}

// warnConduitTargetStreamDeadlines logs a startup warning when
// server.hub.conduit.target_stream_deadlines is enforce in external relay
// mode: renewals for streams held by or bridged through a separate relay
// land later, so until then user streams to renewal-capable agents are
// refused (503 stream_renewal_unavailable) on those routes.
func warnConduitTargetStreamDeadlines(c config.HubConduitConfig) {
	if c.TargetStreamDeadlinesValue() != config.ConduitTargetStreamDeadlinesEnforce || !c.ExternalRelay() {
		return
	}
	slog.Warn("server.hub.conduit.target_stream_deadlines is enforce with relay_mode external: " +
		"user streams to renewal-capable agents are refused (503 stream_renewal_unavailable) until relay renewals land")
}

// conduitLifetimeCapSetting returns the configured conduit session lifetime
// cap (default 3500s). validateServerPreflight has already rejected a
// malformed or out-of-range value.
func conduitLifetimeCapSetting(cfg *config.GlobalConfig) time.Duration {
	d, err := cfg.Hub.Conduit.LifetimeCapDuration()
	if err != nil {
		return config.ConduitDefaultLifetimeCap
	}
	return d
}

// conduitUserStreamAuthzMaxSetting returns the configured authorization
// interval of user-originated streams (0 = hub default).
// validateServerPreflight has already rejected a malformed value.
func conduitUserStreamAuthzMaxSetting(cfg *config.GlobalConfig) time.Duration {
	m, err := cfg.Hub.Conduit.StreamAuthzMaxDurations()
	if err != nil {
		return 0
	}
	return m.User
}

// conduitProxySessionMaxAgeSetting returns the configured cap on new
// tunnels for proxy-authenticated user sessions (default 1h).
// validateServerPreflight has already rejected a malformed or out-of-range
// value.
func conduitProxySessionMaxAgeSetting(cfg *config.GlobalConfig) time.Duration {
	d, err := cfg.Hub.Conduit.ProxySessionMaxAgeDuration()
	if err != nil {
		return config.ConduitDefaultProxySessionMaxAge
	}
	return d
}

// conduitShutdownDrainSetting returns the platform shutdown grace
// (server.hub.conduit.shutdown_grace) and whether it is set. It has no
// default: only a set grace caps the SIGTERM drain (design §3.3); unset,
// the drain deadline stays 30s. validateServerPreflight has already
// rejected a malformed value.
func conduitShutdownDrainSetting(cfg *config.GlobalConfig) (time.Duration, bool) {
	d, set, err := cfg.Hub.Conduit.ShutdownGraceDuration()
	if err != nil {
		return 0, false
	}
	return d, set
}

// Test seams for startConduit and its shutdown step.
var (
	// startConduitRelayFn starts the relay (startConduitRelay).
	startConduitRelayFn = startConduitRelay
	// startConduitExternalFn starts external relay mode
	// (startConduitExternal).
	startConduitExternalFn = startConduitExternal
	// conduitShutdownDrain is the shutdown step's drain: the hub's
	// SIGTERM drain with the configured grace.
	conduitShutdownDrain = func(hubSrv *hub.Server, grace time.Duration, capDrain bool) {
		hubSrv.DrainConduitForShutdown(grace, capDrain)
	}
	// conduitSetShutdownGrace records a set shutdown_grace on the hub, so
	// the teardown's relay shutdown caps the drain too (ptone/scion#5105).
	conduitSetShutdownGrace = func(hubSrv *hub.Server, grace time.Duration) {
		hubSrv.SetConduitShutdownGrace(grace)
	}
	// conduitIngressShutdownDrain is the ingress drain step's drain in
	// external relay mode: ask the relays to drain this hub's proxied
	// sessions, hold the proxied legs and close the ones left at the step
	// deadline.
	conduitIngressShutdownDrain = func(hubSrv *hub.Server, opts hub.ConduitIngressDrainOptions) {
		hubSrv.DrainConduitIngressForShutdown(opts)
	}
	// conduitIngressProxyActive reports whether the hub proxies conduit
	// dials to relays (external mode with proxy ingress serving).
	conduitIngressProxyActive = func(hubSrv *hub.Server) bool {
		return hubSrv.ConduitIngressProxyActive()
	}
	// conduitIngressStop stops the hub proxying conduit dials (the
	// ingress stop step).
	conduitIngressStop = func(hubSrv *hub.Server) {
		hubSrv.BeginConduitIngressShutdown()
	}
	// serveConduitInternalListenerFn opens the internal listener that
	// serves the internal relay API (serveConduitInternalListener).
	serveConduitInternalListenerFn = serveConduitInternalListener
	// conduitRecordStartFailure records a failed relay start on the hub,
	// with the failing setting's key or "".
	conduitRecordStartFailure = func(hubSrv *hub.Server, setting string) {
		hubSrv.SetConduitRelayStartFailure(setting)
	}
	// conduitOnGCE reports whether this node runs on GCE, where relay-peer
	// auth may use the node's own service account (metadata.OnGCE).
	conduitOnGCE = metadata.OnGCE
	// conduitStartHubRelay starts the hub's in-process relay
	// (hub.Server.StartConduitRelay).
	conduitStartHubRelay = func(ctx context.Context, hubSrv *hub.Server, opts hub.ConduitRelayOptions) error {
		return hubSrv.StartConduitRelay(ctx, opts)
	}
)

// relayDrainWriteTimeout and relayDrainSendMargin mirror the relay's
// bounds on a SIGTERM drain (pkg/conduit/relay: deleteTimeout and
// drainSendMargin): before it sends any GoAway, the relay waits for its two
// draining registry writes for at most deleteTimeout, and, with a capped
// drain, at most the drain cap less drainSendMargin.
const (
	relayDrainWriteTimeout = 10 * time.Second
	relayDrainSendMargin   = 500 * time.Millisecond
)

// conduitDrainSendSlack is the time the drain step allows, after the
// relay's registry-write bound, for the GoAways to be sent.
const conduitDrainSendSlack = 2 * time.Second

// conduitDrainStepBound is how long the pre-cancel conduit drain step may
// hold the shutdown before cancel: the relay's own bound on its draining
// registry writes plus conduitDrainSendSlack, so cancel (which closes the
// listeners) never runs before the GoAways unless the relay itself is past
// its bounds. Unset: 10s + 2s. With shutdown_grace set: min(10s, cap -
// 0.5s) + 2s, clamped to [2s, S], where cap = shutdown_grace - 2s. A slow
// registry therefore uses part of the later phases' time (the phase
// deadlines are measured from the signal); the process still exits within
// the grace (exit wait at S, watchdog at G - 250ms).
func conduitDrainStepBound(b serverShutdownBudget) time.Duration {
	if !b.set {
		return relayDrainWriteTimeout + conduitDrainSendSlack
	}
	writes := max(min(relayDrainWriteTimeout, relay.ShutdownDrainCap(b.grace)-relayDrainSendMargin), 0)
	return min(writes+conduitDrainSendSlack, b.span())
}

// registerConduitShutdownDrain makes the relay drain a pre-cancel step of
// the process's single graceful shutdown (design §3.3, ptone/scion#5054):
// on the first SIGINT or SIGTERM, after the servers are marked shutting
// down, drain sends GoAway to every relay session, and only then is the
// server context cancelled, so every session has its GoAway before any
// listener closes. The step waits for drain at most bound on q's clock.
// The conduit no longer subscribes to signals itself; a second subscriber
// would cancel before the drain (ptone/scion#5105). It returns false, and
// the drain never runs from here, when the shutdown has already begun.
func registerConduitShutdownDrain(q *serverShutdownSequence, bound time.Duration, drain func()) bool {
	return q.addStep(shutdownPhaseDrain, boundedShutdownStep(q.clk, bound, "conduit drain", drain))
}

// shutdownPhaseIngressStop is the phase of the step that stops the hub
// proxying conduit dials. It sorts before shutdownPhaseMark (the shutdown
// runs steps in phase order), so the stop runs before the hub's mark
// step: after the mark a broker-signed dial still passes the hub's
// shutdown refusal, and it must already find proxying stopped (cause
// ingress_terminating), whatever the registration order of the steps.
const shutdownPhaseIngressStop = shutdownPhaseMark - 1

// conduitIngressHTTPDrainMargin is how long before the shutdown's HTTP-drain
// deadline the ingress hold ends, so the step returns and cancel starts
// with time left for the HTTP drain (design v2.21.58).
const conduitIngressHTTPDrainMargin = time.Second

// conduitIngressHoldDeadline is the ingress drain's hold deadline H, the
// absolute deadline handed to the hub drain (design v2.21.58): min(step
// deadline, HTTP-drain deadline - conduitIngressHTTPDrainMargin), where the
// HTTP-drain deadline is the shutdown's own (T0 + 3S/4, httpDrainEnd).
// Without a budget there is no HTTP-drain deadline, and H is the step
// deadline. With G = 10s: S = 8s, the HTTP-drain deadline is T0 + 6s and H
// = T0 + 5s (the step deadline is T0 + 8s); with G = 30s, H is the step
// deadline T0 + 12s.
func conduitIngressHoldDeadline(b serverShutdownBudget, t0, stepDeadline time.Time) time.Time {
	if !b.set {
		return stepDeadline
	}
	if h := b.httpDrainEnd(t0).Add(-conduitIngressHTTPDrainMargin); h.Before(stepDeadline) {
		return h
	}
	return stepDeadline
}

// registerConduitIngressShutdownDrain makes the ingress drain a pre-cancel
// step of the process's single graceful shutdown, in the same phase and
// with the same bound as the relay drain (registerConduitShutdownDrain);
// a process registers one of the two. drain gets when the shutdown began
// and the step's absolute deadline (step start plus bound on q's clock);
// the caller derives the hold deadline from it (conduitIngressHoldDeadline). The step waits for drain at most bound.
// It returns false, and the drain never runs from here, when the shutdown
// has already begun.
func registerConduitIngressShutdownDrain(q *serverShutdownSequence, bound time.Duration, drain func(signalAt, stepDeadline time.Time)) bool {
	return q.addStep(shutdownPhaseDrain, func() {
		start := q.clk.Now()
		signalAt, _ := q.startedAt()
		boundedShutdownStep(q.clk, bound, "conduit ingress drain", func() { drain(signalAt, start.Add(bound)) })()
	})
}

// boundedShutdownStep returns step bounded to bound on clk: it runs step
// in its own goroutine and returns when step does or when bound has
// passed, whichever is first (the step then goes on in the background).
func boundedShutdownStep(clk clock.Clock, bound time.Duration, name string, step func()) func() {
	return func() {
		done := make(chan struct{})
		go func() {
			defer close(done)
			step()
		}()
		fire, stop := clock.After(clk, bound)
		defer stop()
		select {
		case <-done:
		case <-fire:
			slog.Warn("Shutdown step still running at its bound; continuing the shutdown", "step", name, "bound", bound)
		}
	}
}

// conduitAdvertiseEndpoint derives the internal endpoint other hub nodes
// use to reach this node: the configured internal_advertise, else
// http://<host>:<port> where host is the listen host when it is a specific
// address, else POD_IP. It returns "" (unaddressable) when no address is
// known.
func conduitAdvertiseEndpoint(advertise string, bound net.Addr, podIP string) string {
	if advertise != "" {
		return advertise
	}
	if bound == nil {
		return ""
	}
	host, port, err := net.SplitHostPort(bound.String())
	if err != nil {
		return ""
	}
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		host = podIP
	}
	if host == "" {
		return ""
	}
	return "http://" + net.JoinHostPort(host, port)
}

// conduitPeerAuthOptions builds the relay-peer auth options from config
// and the environment.
func conduitPeerAuthOptions(ctx context.Context, cfg *config.GlobalConfig, selfID string, onGCP bool) hub.ConduitPeerAuthOptions {
	o := hub.ConduitPeerAuthOptions{
		Mode:            cfg.Hub.Conduit.PeerAuthMode(),
		OnGCP:           onGCP,
		SelfID:          selfID,
		SharedSecret:    resolveSessionSecret(),
		Audience:        cfg.Hub.Conduit.PeerAudience,
		ServiceAccounts: cfg.Hub.Conduit.PeerServiceAccounts,
	}
	if onGCP {
		if email, err := metadata.EmailWithContext(ctx, "default"); err == nil {
			o.OwnServiceAccount = email
		}
	}
	return o
}

// validateConduitPeerSecret applies the startup rules of the relay-peer
// secret (design §3.9): it is required in external relay mode and, with
// relayRole, on --role relay; when set, a hub refuses one equal to its
// session secret (a relay never resolves the session secret and passes
// ""). Strength is checked by HubConduitConfig.Validate. Errors name the
// setting, never the value.
func validateConduitPeerSecret(c config.HubConduitConfig, relayRole bool, sessionSecret string) error {
	secret := c.PeerSecretValue() // a value that is only a newline is unset
	switch {
	case secret == "" && relayRole:
		return errors.New("server.hub.conduit.peer_secret is required on --role relay")
	case secret == "" && c.ExternalRelay():
		return errors.New("server.hub.conduit.peer_secret is required when server.hub.conduit.relay_mode is external")
	case secret != "" && sessionSecret != "" && secret == sessionSecret:
		return errors.New("server.hub.conduit.peer_secret must differ from the session secret")
	}
	return nil
}

// conduitInternalRelayAPIServed reports whether this node serves the
// internal relay API, the only thing relay-peer auth guards inbound: true
// when an internal listener is configured, and always in hosted HA, where
// a node without one cannot start a relay anyway. When false the relay
// starts with relay.NoPeerAuth instead of building relay-peer auth.
func conduitInternalRelayAPIServed(cfg *config.GlobalConfig, requireHA bool) bool {
	return requireHA || cfg.Hub.Conduit.InternalListen != ""
}

// errConduitExternalNeedsExperiment refuses external relay mode on a hub
// where hub.conduit is off (design §3.9 startup check).
var errConduitExternalNeedsExperiment = errors.New("server.hub.conduit.relay_mode is external, which requires the hub.conduit experiment; turn it on or remove relay_mode")

// startConduit opens the internal listener (if configured) and starts the
// hub's conduit relay when hub.conduit is on; once the relay runs, the
// graceful shutdown q drains it before the server context is cancelled
// (registerConduitShutdownDrain), and a set shutdown_grace also caps the
// teardown's relay shutdown (hub.Server.SetConduitShutdownGrace). With the
// flag off it does nothing (no shutdown step). In hosted-HA mode every failure is
// returned and the server exits non-zero (C8); elsewhere a failure is logged and the hub serves
// without a relay. A relay that stops on its own (superseded) reports on
// errCh so the process exits and restarts.
func startConduit(ctx context.Context, q *serverShutdownSequence, cfg *config.GlobalConfig, hubSrv *hub.Server, hubEndpoint string, wg *sync.WaitGroup, errCh chan<- error) error {
	if hubSrv == nil {
		return nil
	}
	if cfg.Hub.Conduit.ExternalRelay() && !hubSrv.ConduitEnabled() {
		return errConduitExternalNeedsExperiment
	}
	if !hubSrv.ConduitEnabled() {
		return nil
	}
	if cfg.Hub.Conduit.ExternalRelay() {
		// External relay mode: no relay here, so no relay drain and no
		// grace recorded. Its configuration was chosen explicitly, so
		// every failure is fatal, in and outside hosted HA. With the
		// ingress proxy serving, the shutdown stops proxying before it
		// marks any server (shutdownPhaseIngressStop), and the ingress
		// drain step asks the relays to drain the sessions this hub
		// proxied, holds the proxied legs and closes the ones left at its
		// deadline (design §3.9 "Drain through the proxy"). With the
		// proxy off there are no steps.
		if err := startConduitExternalFn(ctx, cfg, hubSrv, wg, errCh); err != nil {
			return fmt.Errorf("conduit external relay mode: %w", err)
		}
		if conduitIngressProxyActive(hubSrv) {
			grace, graceSet := conduitShutdownDrainSetting(cfg)
			budget := serverShutdownBudgetFor(cfg)
			bound := conduitDrainStepBound(budget)
			q.addStep(shutdownPhaseIngressStop, func() { conduitIngressStop(hubSrv) })
			registerConduitIngressShutdownDrain(q, bound, func(signalAt, stepDeadline time.Time) {
				conduitIngressShutdownDrain(hubSrv, hub.ConduitIngressDrainOptions{
					Clock:        q.clk,
					SignalAt:     signalAt,
					HoldDeadline: conduitIngressHoldDeadline(budget, signalAt, stepDeadline),
					Grace:        grace,
					GraceSet:     graceSet,
				})
			})
		}
		return nil
	}
	requireHA := hostedHAGuardsRequired(cfg)
	err := startConduitRelayFn(ctx, cfg, hubSrv, hubEndpoint, wg, errCh, requireHA)
	if err == nil {
		grace, capDrain := conduitShutdownDrainSetting(cfg)
		if capDrain {
			conduitSetShutdownGrace(hubSrv, grace)
		}
		bound := conduitDrainStepBound(serverShutdownBudgetFor(cfg))
		registerConduitShutdownDrain(q, bound, func() { conduitShutdownDrain(hubSrv, grace, capDrain) })
		return nil
	}
	if requireHA {
		return fmt.Errorf("conduit relay startup failed (hosted HA): %w", err)
	}
	// The setting key goes to the log and to the relay-not-running response;
	// a SettingError message holds only fixed text and setting names.
	var se *relay.SettingError
	if errors.As(err, &se) {
		slog.Error("Conduit relay not started; agent conduit sessions are unavailable on this node", "setting", se.Key, "error", err)
		conduitRecordStartFailure(hubSrv, se.Key)
		return nil
	}
	slog.Error("Conduit relay not started; agent conduit sessions are unavailable on this node", "error", err)
	conduitRecordStartFailure(hubSrv, "")
	return nil
}

func startConduitRelay(ctx context.Context, cfg *config.GlobalConfig, hubSrv *hub.Server, hubEndpoint string, wg *sync.WaitGroup, errCh chan<- error, requireHA bool) (err error) {
	// C8: checked first, before any listener opens or peer auth is built,
	// so the operator sees this cause rather than a later symptom.
	if requireHA && !hubSrv.ConduitGrantRingShared() {
		return hub.ErrConduitNoAtRestKey
	}
	if requireHA && cloudrunenv.IsService() {
		return errConduitRelayOnCloudRun
	}
	// Hosted HA only: a single-node hub may advertise the same host as its
	// public endpoint (e.g. localhost on another port), and its self-check
	// still guards addressability.
	if requireHA {
		if err := checkConduitAdvertiseHost(cfg.Hub.Conduit.InternalAdvertise, hubEndpoint); err != nil {
			return err
		}
	}
	if cfg.Hub.Conduit.InternalListen == "" && cfg.Hub.Conduit.InternalAdvertise != "" {
		return &relay.SettingError{Key: "server.hub.conduit.internal_advertise", Err: errors.New("server.hub.conduit.internal_advertise is set but internal_listen is not")}
	}
	id := hub.ConduitInstanceID(cfg.Hub.Conduit.InstanceID)
	var auth relay.PeerAuth
	var mode string
	if conduitInternalRelayAPIServed(cfg, requireHA) {
		auth, mode, err = hub.NewConduitPeerAuth(conduitPeerAuthOptions(ctx, cfg, id, conduitOnGCE()))
		if err != nil {
			return err
		}
	} else {
		// No internal listener: no relay peer can reach this node, so it
		// needs no relay-peer credential. NoPeerAuth refuses both ways.
		auth, mode = relay.NoPeerAuth{}, "none"
		slog.Info("Conduit relay: internal relay API not served on this node (server.hub.conduit.internal_listen is unset); requests to other relays are refused")
	}

	var endpoint string
	if listen := cfg.Hub.Conduit.InternalListen; listen != "" {
		addr, closeListener, lerr := serveConduitInternalListenerFn(ctx, listen, hubSrv.ConduitInternalHandler(), wg, errCh)
		if lerr != nil {
			return lerr
		}
		// A relay that does not start must not keep the port.
		defer func() {
			if err != nil {
				closeListener()
			}
		}()
		endpoint = conduitAdvertiseEndpoint(cfg.Hub.Conduit.InternalAdvertise, addr, os.Getenv("POD_IP"))
		slog.Info("Conduit internal listener started", "addr", addr.String(), "advertise", endpoint)
	}

	if err := conduitStartHubRelay(ctx, hubSrv, hub.ConduitRelayOptions{
		InstanceID:       id,
		InternalEndpoint: endpoint,
		RequireHA:        requireHA,
		PeerAuth:         auth,
		ReconnectWindow:  conduitReconnectWindowSetting(cfg),
		LifetimeCap:      conduitLifetimeCapSetting(cfg),
	}); err != nil {
		return err
	}
	slog.Info("Conduit relay started", "instance_id", id, "peer_auth", mode, "internal_endpoint", endpoint, "hosted_ha", strconv.FormatBool(requireHA))

	if fatal := hubSrv.ConduitRelayFatal(); fatal != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case err, ok := <-fatal:
				if !ok || err == nil {
					err = errors.New("relay stopped serving")
				}
				sendConduitErr(errCh, fmt.Errorf("conduit relay: %w", err))
			case <-ctx.Done():
			}
		}()
	}
	return nil
}

// errConduitExternalNoInternalListen refuses external relay mode without
// the internal listener the relays call the hub internal API on.
var errConduitExternalNoInternalListen = errors.New("server.hub.conduit.internal_listen is required when server.hub.conduit.relay_mode is external: relays call the hub internal API on it")

// startConduitExternal starts the hub's side of external relay mode
// (design §3.9): relay-peer auth keyed from the dedicated peer secret, the
// routing runtime over the shared registry, and the internal listener
// serving the hub internal API. validateServerPreflight has already
// checked peer_secret; the hub's session secret is passed only so peer
// auth refuses an equal peer secret again.
func startConduitExternal(ctx context.Context, cfg *config.GlobalConfig, hubSrv *hub.Server, wg *sync.WaitGroup, errCh chan<- error) (err error) {
	if cfg.Hub.Conduit.InternalListen == "" {
		return errConduitExternalNoInternalListen
	}
	id := hub.ConduitInstanceID(cfg.Hub.Conduit.InstanceID)
	opts := externalPeerAuthOptions(ctx, cfg, id, metadata.OnGCE())
	if opts.PeerSecret == "" {
		return errors.New("server.hub.conduit.peer_secret is required when server.hub.conduit.relay_mode is external")
	}
	auth, mode, err := hub.NewConduitPeerAuth(opts)
	if err != nil {
		return err
	}
	ingressOff, err := validateConduitIngress(cfg.Hub.Conduit, mode)
	if err != nil {
		return err
	}
	if ingressOff != "" {
		slog.Warn("Conduit ingress proxy is off: it requires HMAC relay-peer authentication; conduit dials get 503 conduit_not_serving",
			"cause", ingressOff, "peer_auth", mode, "fix", "set server.hub.conduit.peer_auth to hmac, or upgrade to the release with the relay-peer ingress header")
	}
	if err := hubSrv.StartConduitExternal(ctx, hub.ConduitExternalOptions{
		InstanceID:      id,
		PeerAuth:        auth,
		Ingress:         cfg.Hub.Conduit.IngressValue(),
		IngressOffCause: ingressOff,
		LifetimeCap:     conduitLifetimeCapSetting(cfg),
	}); err != nil {
		return err
	}
	addr, _, err := serveConduitInternalListenerFn(ctx, cfg.Hub.Conduit.InternalListen, hubSrv.ConduitInternalHandler(), wg, errCh)
	if err != nil {
		return err
	}
	slog.Info("Conduit external relay mode started: this hub runs no relay", "instance_id", id, "peer_auth", mode, "internal_listen", addr.String())
	return nil
}

// validateConduitIngress applies the startup rules of the conduit ingress
// in external relay mode (design §3.9 "Ingress", v2.21.52): redirect is not
// served yet (an error naming the setting). Proxy needs HMAC relay-peer
// authentication, because the OIDC modes carry the peer token in
// Authorization, which on the proxy leg is the client's own credential;
// under any other resolved mode (explicit, or auto resolving to oidc) the
// hub still starts with its ingress proxy off, and ingressOff is the cause
// (hub.ConduitIngressCauseRequiresHMAC). peerAuthMode is the resolved
// peer-auth mode.
func validateConduitIngress(c config.HubConduitConfig, peerAuthMode string) (ingressOff string, err error) {
	switch c.IngressValue() {
	case config.ConduitIngressRedirect:
		return "", errors.New("server.hub.conduit.ingress is redirect, which is not supported yet; use proxy")
	case config.ConduitIngressProxy:
		if peerAuthMode != config.ConduitPeerAuthHMAC {
			return hub.ConduitIngressCauseRequiresHMAC, nil
		}
	}
	return "", nil
}

// externalPeerAuthOptions is the hub's relay-peer auth configuration in
// external relay mode: keyed from the dedicated peer secret, with the hub's
// shared signing secret passed so peer auth refuses an equal peer secret.
func externalPeerAuthOptions(ctx context.Context, cfg *config.GlobalConfig, id string, onGCP bool) hub.ConduitPeerAuthOptions {
	opts := conduitPeerAuthOptions(ctx, cfg, id, onGCP)
	opts.PeerSecret = cfg.Hub.Conduit.PeerSecretValue()
	opts.SharedSecret = sessionSecretValue()
	return opts
}

// serveConduitInternalListener opens the internal listener on listen and
// serves h on it until ctx ends (graceful shutdown, 10s). A serve error
// after startup is reported on errCh. closeListener releases the port at
// once, for a startup that fails after the listener opened.
func serveConduitInternalListener(ctx context.Context, listen string, h http.Handler, wg *sync.WaitGroup, errCh chan<- error) (addr net.Addr, closeListener func(), err error) {
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return nil, nil, fmt.Errorf("conduit internal listener %s: %w", listen, err)
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
	wg.Add(2)
	go func() {
		defer wg.Done()
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			sendConduitErr(errCh, fmt.Errorf("conduit internal listener: %w", err))
		}
	}()
	go func() {
		defer wg.Done()
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	return ln.Addr(), func() {
		_ = srv.Close()
		_ = ln.Close() // Serve may not have taken it over yet
	}, nil
}

// errConduitRelayOnCloudRun refuses the in-process relay on Cloud Run in
// hosted HA: Cloud Run instances are not individually addressable, so other
// hub nodes cannot route a session to a specific instance. A separate relay
// role will cover this deployment later.
var errConduitRelayOnCloudRun = errors.New("the in-process conduit relay is not supported on Cloud Run in hosted HA " +
	"(K_SERVICE is set): Cloud Run instances are not individually addressable; run the hub where each node has " +
	"its own internal address, or turn hub.conduit off (a separate relay role will support this deployment later)")

// checkConduitAdvertiseHost refuses an internal_advertise whose host is the
// public hub endpoint's host or a Cloud Run (*.run.app) host: in hosted HA
// those names reach an arbitrary instance behind a load balancer, not this
// node. It runs only in hosted HA.
func checkConduitAdvertiseHost(advertise, hubEndpoint string) error {
	if advertise == "" {
		return nil
	}
	au, err := url.Parse(advertise)
	if err != nil {
		return fmt.Errorf("server.hub.conduit.internal_advertise %q: %w", advertise, err)
	}
	host := strings.ToLower(strings.TrimSuffix(au.Hostname(), "."))
	if host == "run.app" || strings.HasSuffix(host, ".run.app") {
		return fmt.Errorf("server.hub.conduit.internal_advertise %q is a Cloud Run host; it must address this node directly, not a load-balanced service", advertise)
	}
	if hubEndpoint == "" {
		return nil
	}
	hu, err := url.Parse(hubEndpoint)
	if err != nil {
		return nil
	}
	if hubHost := strings.ToLower(strings.TrimSuffix(hu.Hostname(), ".")); hubHost != "" && hubHost == host {
		return fmt.Errorf("server.hub.conduit.internal_advertise %q uses the public hub host %q; it must address this node directly, not the load-balanced hub endpoint", advertise, hubHost)
	}
	return nil
}

// sendConduitErr reports a fatal conduit error without blocking: any error
// already queued ends the server just the same.
func sendConduitErr(errCh chan<- error, err error) {
	select {
	case errCh <- err:
	default:
		slog.Error("Conduit fatal error (server already stopping)", "error", err)
	}
}
