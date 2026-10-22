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

//go:build !no_sqlite

package cmd

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/clock"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hub"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	relayModeTestPeerSecret    = "peer-secret-for-startup-tests-0123456789"
	relayModeTestSessionSecret = "session-secret-for-startup-tests-01234"
)

// TestValidateConduitPeerSecret (design §3.9, criterion 30): peer_secret
// is required in external relay mode and on --role relay, and a hub refuses
// one equal to its session secret. Errors name the setting, never a value.
func TestValidateConduitPeerSecret(t *testing.T) {
	external := config.HubConduitConfig{RelayMode: config.ConduitRelayModeExternal}
	withSecret := func(c config.HubConduitConfig, s string) config.HubConduitConfig { c.PeerSecret = s; return c }
	for _, tc := range []struct {
		name      string
		conduit   config.HubConduitConfig
		relayRole bool
		session   string
		wantErr   string
	}{
		{name: "in-process without peer secret", conduit: config.HubConduitConfig{}},
		{name: "external without peer secret", conduit: external, wantErr: "server.hub.conduit.peer_secret is required"},
		{name: "relay role without peer secret", relayRole: true, wantErr: "server.hub.conduit.peer_secret is required on --role relay"},
		{name: "external with peer secret", conduit: withSecret(external, relayModeTestPeerSecret), session: relayModeTestSessionSecret},
		{name: "relay role with peer secret", conduit: withSecret(config.HubConduitConfig{}, relayModeTestPeerSecret), relayRole: true},
		{name: "equal to the session secret", conduit: withSecret(external, relayModeTestSessionSecret), session: relayModeTestSessionSecret, wantErr: "must differ from the session secret"},
		{name: "equal to the session secret, in-process", conduit: withSecret(config.HubConduitConfig{}, relayModeTestSessionSecret), session: relayModeTestSessionSecret, wantErr: "must differ from the session secret"},
		{name: "equal to the session secret after the trailing newline is trimmed", conduit: withSecret(external, relayModeTestSessionSecret+"\n"), session: relayModeTestSessionSecret, wantErr: "must differ from the session secret"},
		{name: "a second trailing newline is part of the secret", conduit: withSecret(external, relayModeTestSessionSecret+"\n\n"), session: relayModeTestSessionSecret},
		{name: "only a newline is unset (external)", conduit: withSecret(external, "\n"), wantErr: "server.hub.conduit.peer_secret is required"},
		{name: "only a newline is unset (relay role)", conduit: withSecret(config.HubConduitConfig{}, "\n"), relayRole: true, wantErr: "server.hub.conduit.peer_secret is required on --role relay"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateConduitPeerSecret(tc.conduit, tc.relayRole, tc.session)
			if tc.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
			assert.NotContains(t, err.Error(), relayModeTestPeerSecret)
			assert.NotContains(t, err.Error(), relayModeTestSessionSecret)
		})
	}
}

// TestValidateServerPreflight_RelayMode: the hub's startup preflight
// applies the relay-mode rules with the configured session secret, and
// rejects a weak peer secret.
func TestValidateServerPreflight_RelayMode(t *testing.T) {
	t.Cleanup(resetServerFlags)
	for _, tc := range []struct {
		name    string
		conduit config.HubConduitConfig
		wantErr string
	}{
		{name: "external without peer secret", conduit: config.HubConduitConfig{RelayMode: "external"}, wantErr: "server.hub.conduit.peer_secret is required"},
		{name: "peer secret equal to the session secret", conduit: config.HubConduitConfig{RelayMode: "external", PeerSecret: relayModeTestSessionSecret}, wantErr: "must differ from the session secret"},
		{name: "peer secret too short", conduit: config.HubConduitConfig{RelayMode: "external", PeerSecret: "short"}, wantErr: "server.hub.conduit.peer_secret"},
		{name: "peer secret only a newline", conduit: config.HubConduitConfig{RelayMode: "external", PeerSecret: "\n"}, wantErr: "server.hub.conduit.peer_secret is required"},
		{name: "unknown relay mode", conduit: config.HubConduitConfig{RelayMode: "sidecar"}, wantErr: "server.hub.conduit.relay_mode"},
		{name: "external with a peer secret", conduit: config.HubConduitConfig{RelayMode: "external", PeerSecret: relayModeTestPeerSecret}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetServerFlags()
			enableHub = true
			t.Setenv("SCION_SERVER_SESSION_SECRET", relayModeTestSessionSecret)
			cfg := &config.GlobalConfig{}
			cfg.Hub.Conduit = tc.conduit
			err := validateServerPreflight(cfg)
			if tc.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
			assert.NotContains(t, err.Error(), relayModeTestSessionSecret)
		})
	}
}

// TestValidateServerPreflight_SessionSecretOnlyInTheEnv: with the session
// secret given only in the env, the preflight still refuses an equal
// peer_secret in external relay mode after scrubServerSecretEnv has removed
// it from the env.
func TestValidateServerPreflight_SessionSecretOnlyInTheEnv(t *testing.T) {
	t.Cleanup(resetServerFlags)
	resetServerFlags()
	isolateServerSecretEnv(t)
	enableHub = true
	t.Setenv(sessionSecretEnv, relayModeTestSessionSecret)
	scrubServerSecretEnv()
	_, inEnv := os.LookupEnv(sessionSecretEnv)
	require.False(t, inEnv, "the scrub left the session secret in the env")

	cfg := &config.GlobalConfig{}
	cfg.Hub.Conduit = config.HubConduitConfig{RelayMode: "external", PeerSecret: relayModeTestSessionSecret}
	err := validateServerPreflight(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must differ from the session secret")
	assert.NotContains(t, err.Error(), relayModeTestSessionSecret)
}

// TestExternalPeerAuthOptions_SessionSecretOnlyInTheEnv: in the same case
// the external relay mode peer auth options carry the session secret as
// the shared signing secret, so peer auth can refuse an equal peer secret.
func TestExternalPeerAuthOptions_SessionSecretOnlyInTheEnv(t *testing.T) {
	t.Cleanup(resetServerFlags)
	resetServerFlags()
	isolateServerSecretEnv(t)
	t.Setenv(sessionSecretEnv, relayModeTestSessionSecret)
	scrubServerSecretEnv()

	cfg := &config.GlobalConfig{}
	cfg.Hub.Conduit = config.HubConduitConfig{RelayMode: "external", PeerSecret: relayModeTestPeerSecret}
	opts := externalPeerAuthOptions(context.Background(), cfg, "hub-0", false)
	assert.NotEmpty(t, opts.SharedSecret, "no shared signing secret after the scrub")
	assert.True(t, opts.SharedSecret == relayModeTestSessionSecret, "the shared signing secret is not the session secret")
	assert.Equal(t, relayModeTestPeerSecret, opts.PeerSecret)
}

// TestSessionSecretValue_AfterTheScrub: sessionSecretValue returns the flag,
// else the value scrubServerSecretEnv captured, else the env, without a
// deprecation warning. An env-only reader fails the captured case, whose
// env is empty after the scrub.
func TestSessionSecretValue_AfterTheScrub(t *testing.T) {
	t.Cleanup(resetServerFlags)
	resetServerFlags()
	isolateServerSecretEnv(t)
	t.Setenv(sessionSecretEnv, "captured-value")
	scrubServerSecretEnv()
	assert.True(t, sessionSecretValue() == "captured-value", "the captured value is not used after the scrub")

	webSessionSecret = "flag-value"
	assert.True(t, sessionSecretValue() == "flag-value", "the flag does not win")
	webSessionSecret = ""

	t.Setenv(sessionSecretEnv, "later-env-value")
	assert.True(t, sessionSecretValue() == "captured-value", "the env wins over the captured value")

	capturedSessionSecret = ""
	require.NoError(t, os.Unsetenv(sessionSecretEnv))
	logs := captureSecretEnvSlog(t)
	t.Setenv(legacySessionSecretEnv, "legacy-value")
	assert.True(t, sessionSecretValue() == "legacy-value", "the legacy env name is not read")
	assert.NotContains(t, logs.String(), "deprecated", "sessionSecretValue logged a warning")
}

// TestStartConduit_ExternalRelayNeedsExperiment: external relay mode with
// hub.conduit off is a startup error.
func TestStartConduit_ExternalRelayNeedsExperiment(t *testing.T) {
	t.Cleanup(resetServerFlags)
	srv := conduitHubServer(t, false)
	cfg := &config.GlobalConfig{}
	cfg.Hub.Conduit = config.HubConduitConfig{RelayMode: "external", PeerSecret: relayModeTestPeerSecret}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	t.Cleanup(func() { cancel(); wg.Wait() })
	q := newTestShutdown(cancel)
	err := startConduit(ctx, q, cfg, srv, "https://hub.example.com", &wg, make(chan error, 1))
	require.ErrorIs(t, err, errConduitExternalNeedsExperiment)

	// In-process mode with the experiment off still starts nothing.
	cfg.Hub.Conduit = config.HubConduitConfig{}
	assert.NoError(t, startConduit(ctx, q, cfg, srv, "https://hub.example.com", &wg, make(chan error, 1)))
	q.mu.Lock()
	defer q.mu.Unlock()
	assert.Empty(t, q.steps, "no shutdown step without a relay")
}

// TestStartConduit_ExternalRelayMode: with hub.conduit on, external relay
// mode starts the external runtime instead of a relay and records no
// shutdown grace. With the ingress proxy serving it registers two shutdown
// steps: the ingress stop, which runs before the hub's mark step although
// that step was registered first (as in runServerStart), and the ingress
// drain, which runs after the marks and before cancel with the signal time
// and the hold deadline H (with G = 10s, T0+5s: 1s before the HTTP-drain
// deadline T0+6s, earlier than the step deadline T0+8s), so cancel starts
// at H. With the proxy off, or after a failed start, it registers none.
// Any failure is fatal, outside hosted HA too (design §3.9 "Drain through
// the proxy", v2.21.58).
func TestStartConduit_ExternalRelayMode(t *testing.T) {
	t.Cleanup(resetServerFlags)
	captureStdLog(t)
	srv := conduitHubServer(t, true)
	cfg := &config.GlobalConfig{}
	cfg.Hub.Conduit = config.HubConduitConfig{RelayMode: "external", PeerSecret: relayModeTestPeerSecret}
	cfg.Hub.Conduit.ShutdownGrace = "10s"

	prevExt, prevRelay, prevSetGrace := startConduitExternalFn, startConduitRelayFn, conduitSetShutdownGrace
	prevDrain, prevActive, prevStop := conduitIngressShutdownDrain, conduitIngressProxyActive, conduitIngressStop
	t.Cleanup(func() {
		startConduitExternalFn, startConduitRelayFn, conduitSetShutdownGrace = prevExt, prevRelay, prevSetGrace
		conduitIngressShutdownDrain, conduitIngressProxyActive, conduitIngressStop = prevDrain, prevActive, prevStop
	})
	relayStarted, graceRecorded := false, false
	startConduitRelayFn = func(context.Context, *config.GlobalConfig, *hub.Server, string, *sync.WaitGroup, chan<- error, bool) error {
		relayStarted = true
		return nil
	}
	conduitSetShutdownGrace = func(*hub.Server, time.Duration) { graceRecorded = true }
	proxyActive := true
	conduitIngressProxyActive = func(*hub.Server) bool { return proxyActive }

	var extErr error
	externalCalls := 0
	startConduitExternalFn = func(context.Context, *config.GlobalConfig, *hub.Server, *sync.WaitGroup, chan<- error) error {
		externalCalls++
		return extErr
	}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	t.Cleanup(func() { cancel(); wg.Wait() })

	type drainCall struct {
		srv    *hub.Server
		opts   hub.ConduitIngressDrainOptions
		ctxErr error
	}
	var orderMu sync.Mutex
	var order []string
	record := func(step string) {
		orderMu.Lock()
		defer orderMu.Unlock()
		order = append(order, step)
	}
	stopped := make(chan *hub.Server, 1)
	conduitIngressStop = func(s *hub.Server) {
		record("ingress stop")
		stopped <- s
	}
	drained := make(chan drainCall, 1)
	conduitIngressShutdownDrain = func(s *hub.Server, opts hub.ConduitIngressDrainOptions) {
		record("ingress drain")
		drained <- drainCall{srv: s, opts: opts, ctxErr: ctx.Err()}
		// Like the hub drain with a leg that never closes: return at the
		// hold deadline.
		fire, stop := clock.After(opts.Clock, opts.HoldDeadline.Sub(opts.Clock.Now()))
		defer stop()
		<-fire
	}
	steps := func(q *serverShutdownSequence) int {
		q.mu.Lock()
		defer q.mu.Unlock()
		return len(q.steps)
	}

	clk := clock.NewFake(shutdownTestT0)
	cancelledAt := make(chan time.Time, 1)
	q := newServerShutdownSequence(func() {
		record("cancel")
		cancelledAt <- clk.Now()
		cancel()
	}, clk)
	q.addStep(shutdownPhaseMark, func() { record("hub mark") }) // registered first, as in runServerStart
	require.NoError(t, startConduit(ctx, q, cfg, srv, "https://hub.example.com", &wg, make(chan error, 1)))
	assert.Equal(t, 1, externalCalls)
	assert.False(t, relayStarted, "no in-process relay in external mode")
	assert.Equal(t, 3, steps(q), "the hub mark plus the ingress stop and drain steps, with the proxy serving")
	assert.False(t, graceRecorded, "no shutdown grace recorded without a relay")

	proxyActive = false
	qOff := newServerShutdownSequence(func() {}, clk)
	require.NoError(t, startConduit(ctx, qOff, cfg, srv, "https://hub.example.com", &wg, make(chan error, 1)))
	assert.Zero(t, steps(qOff), "no shutdown step with the ingress proxy off")

	proxyActive = true
	extErr = errors.New("boom")
	qFail := newServerShutdownSequence(func() {}, clk)
	err := startConduit(ctx, qFail, cfg, srv, "https://hub.example.com", &wg, make(chan error, 1))
	require.Error(t, err, "a failure is fatal outside hosted HA too")
	assert.Contains(t, err.Error(), "conduit external relay mode")
	assert.Zero(t, steps(qFail), "no shutdown step after a failed start")

	go q.begin(syscall.SIGTERM)
	select {
	case s := <-stopped:
		assert.Same(t, srv, s, "the ingress stop runs on this hub")
	case <-time.After(shutdownTestWait):
		t.Fatal("the shutdown did not stop the ingress")
	}
	select {
	case call := <-drained:
		assert.Same(t, srv, call.srv, "the ingress drain runs on this hub")
		assert.NoError(t, call.ctxErr, "the ingress drain runs before cancel")
		assert.Equal(t, shutdownTestT0, call.opts.SignalAt, "the signal time")
		assert.Equal(t, shutdownTestT0.Add(5*time.Second), call.opts.HoldDeadline,
			"G = 10s: H = min(step deadline T0+8s, HTTP-drain deadline T0+6s - 1s)")
		assert.Equal(t, 10*time.Second, call.opts.Grace)
		assert.True(t, call.opts.GraceSet)
		assert.NotNil(t, call.opts.Clock, "the drain runs on the shutdown clock")
	case <-time.After(shutdownTestWait):
		t.Fatal("the shutdown did not run the ingress drain")
	}
	require.True(t, clk.WaitForTimer(shutdownTestWait, shutdownTestT0.Add(5*time.Second)), "the hold deadline is armed")
	clk.Advance(5 * time.Second)
	select {
	case at := <-cancelledAt:
		httpDrainEnd := serverShutdownBudget{grace: 10 * time.Second, set: true}.httpDrainEnd(shutdownTestT0)
		assert.Equal(t, shutdownTestT0.Add(6*time.Second), httpDrainEnd)
		assert.Equal(t, shutdownTestT0.Add(5*time.Second), at, "cancel starts at H")
		assert.False(t, at.After(httpDrainEnd.Add(-time.Second)), "cancel starts with at least 1s of the HTTP drain left")
	case <-time.After(shutdownTestWait):
		t.Fatal("the shutdown was not cancelled after the ingress drain")
	}
	assert.Error(t, ctx.Err(), "the server context is cancelled after the drain")
	orderMu.Lock()
	assert.Equal(t, []string{"ingress stop", "hub mark", "ingress drain", "cancel"}, order,
		"proxying stops before the hub's mark; the drain runs after the marks and before cancel")
	orderMu.Unlock()
}

// TestShutdownPhaseIngressStopFirst (review a3ii2x-r2-F1): the ingress
// stop phase sorts before every named shutdown phase, so the stop runs
// before the hub's mark step whatever the registration order. Every
// shutdownPhase constant declared in the package's source must be in the
// table, so a phase added later (for example one right before the mark
// phase) fails this test until it is checked too.
func TestShutdownPhaseIngressStopFirst(t *testing.T) {
	named := map[string]int{
		"shutdownPhaseMark":  shutdownPhaseMark,
		"shutdownPhaseDrain": shutdownPhaseDrain,
	}
	for name, phase := range named {
		assert.Less(t, shutdownPhaseIngressStop, phase, "shutdownPhaseIngressStop must sort before %s", name)
	}

	files, err := filepath.Glob("*.go")
	require.NoError(t, err)
	fset := token.NewFileSet()
	seen := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(fset, f, nil, parser.SkipObjectResolution)
		require.NoError(t, err, f)
		for _, d := range af.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, sp := range gd.Specs {
				vs, ok := sp.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for _, n := range vs.Names {
					if !strings.HasPrefix(n.Name, "shutdownPhase") || n.Name == "shutdownPhaseIngressStop" {
						continue
					}
					seen++
					_, checked := named[n.Name]
					assert.True(t, checked, "%s (%s) is a shutdown phase this test does not check; add it to the table", n.Name, f)
				}
			}
		}
	}
	assert.Equal(t, len(named), seen, "every named shutdown phase is declared once")
}

// TestConduitIngressHoldDeadline (design v2.21.58): H = min(step deadline,
// HTTP-drain deadline - 1s); without a budget, the step deadline.
func TestConduitIngressHoldDeadline(t *testing.T) {
	t0 := shutdownTestT0
	for _, tc := range []struct {
		name string
		b    serverShutdownBudget
		want time.Duration // H - T0
	}{
		{name: "G = 10s: the HTTP-drain deadline binds", b: serverShutdownBudget{grace: 10 * time.Second, set: true}, want: 5 * time.Second},
		{name: "G = 30s: the step deadline", b: serverShutdownBudget{grace: 30 * time.Second, set: true}, want: 12 * time.Second},
		{name: "G = 19s: the HTTP-drain deadline binds", b: serverShutdownBudget{grace: 19 * time.Second, set: true}, want: 11750 * time.Millisecond},
		{name: "no budget: no HTTP-drain deadline, the step deadline", b: serverShutdownBudget{}, want: 12 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			step := t0.Add(conduitDrainStepBound(tc.b))
			h := conduitIngressHoldDeadline(tc.b, t0, step)
			assert.Equal(t, t0.Add(tc.want), h)
			assert.False(t, h.After(step), "never after the step deadline")
			if tc.b.set {
				assert.False(t, h.After(tc.b.httpDrainEnd(t0).Add(-conduitIngressHTTPDrainMargin)), "at least 1s before the HTTP-drain deadline")
			}
		})
	}
}

// TestRegisterConduitIngressShutdownDrain_BoundedWait: an ingress drain
// that blocks (a slow relay) holds the shutdown only until its bound on
// the shutdown clock, and the deadline it was given is that same instant;
// cancel then runs.
func TestRegisterConduitIngressShutdownDrain_BoundedWait(t *testing.T) {
	captureStdLog(t)
	clk := clock.NewFake(shutdownTestT0)
	cancelled := make(chan time.Time, 1)
	q := newServerShutdownSequence(func() { cancelled <- clk.Now() }, clk)
	release := make(chan struct{})
	defer close(release)
	gotDeadline := make(chan time.Time, 1)
	require.True(t, registerConduitIngressShutdownDrain(q, 2*time.Second, func(_, stepDeadline time.Time) {
		gotDeadline <- stepDeadline
		<-release
	}))
	go q.begin(syscall.SIGTERM)
	require.True(t, clk.WaitForTimer(shutdownTestWait, shutdownTestT0.Add(2*time.Second)), "drain bound not armed")
	select {
	case d := <-gotDeadline:
		assert.Equal(t, shutdownTestT0.Add(2*time.Second), d, "the drain's deadline is the step bound")
	case <-time.After(shutdownTestWait):
		t.Fatal("the ingress drain did not start")
	}
	clk.Advance(2 * time.Second)
	select {
	case at := <-cancelled:
		assert.Equal(t, shutdownTestT0.Add(2*time.Second), at, "cancel at the drain bound")
	case <-time.After(shutdownTestWait):
		t.Fatal("a blocked ingress drain held the shutdown past its bound")
	}
}

// TestRegisterConduitIngressShutdownDrain_RefusedAfterBegin: an ingress
// drain registered once the shutdown has begun is refused and never runs.
func TestRegisterConduitIngressShutdownDrain_RefusedAfterBegin(t *testing.T) {
	captureStdLog(t)
	q := newTestShutdown(func() {})
	q.begin(syscall.SIGTERM)
	ran := false
	assert.False(t, registerConduitIngressShutdownDrain(q, time.Minute, func(_, _ time.Time) { ran = true }))
	q.begin(syscall.SIGTERM)
	assert.False(t, ran)
}

// TestStartConduitExternal_RequiresInternalListen: relays call the hub
// internal API on the internal listener, so external relay mode refuses to
// start without one.
func TestStartConduitExternal_RequiresInternalListen(t *testing.T) {
	cfg := &config.GlobalConfig{}
	cfg.Hub.Conduit = config.HubConduitConfig{RelayMode: "external", PeerSecret: relayModeTestPeerSecret}
	var wg sync.WaitGroup
	err := startConduitExternal(context.Background(), cfg, nil, &wg, make(chan error, 1))
	require.ErrorIs(t, err, errConduitExternalNoInternalListen)
}

// TestValidateConduitIngress (design §3.9 "Ingress"): proxy ingress needs
// HMAC relay-peer auth, and startup refuses it under an OIDC peer-auth
// mode; redirect is refused for now. Errors name the setting.
func TestValidateConduitIngress(t *testing.T) {
	autoOnGCP, err := hub.ResolveConduitPeerAuthMode("", true)
	require.NoError(t, err)
	require.Equal(t, config.ConduitPeerAuthOIDC, autoOnGCP, "auto resolves to oidc on GCP")
	for _, tc := range []struct {
		name    string
		ingress string
		mode    string
		wantOff string
		wantErr string
	}{
		{name: "proxy (default) with hmac", mode: config.ConduitPeerAuthHMAC},
		{name: "proxy with hmac", ingress: config.ConduitIngressProxy, mode: config.ConduitPeerAuthHMAC},
		{name: "proxy with oidc starts with the proxy off", ingress: config.ConduitIngressProxy, mode: config.ConduitPeerAuthOIDC, wantOff: hub.ConduitIngressCauseRequiresHMAC},
		{name: "default auto on GCP starts with the proxy off", mode: autoOnGCP, wantOff: hub.ConduitIngressCauseRequiresHMAC},
		{name: "redirect", ingress: config.ConduitIngressRedirect, mode: config.ConduitPeerAuthHMAC, wantErr: "server.hub.conduit.ingress is redirect"},
		{name: "redirect with oidc", ingress: config.ConduitIngressRedirect, mode: config.ConduitPeerAuthOIDC, wantErr: "server.hub.conduit.ingress is redirect"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			off, err := validateConduitIngress(config.HubConduitConfig{RelayMode: config.ConduitRelayModeExternal, Ingress: tc.ingress}, tc.mode)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				return
			}
			require.NoError(t, err, "no startup refusal")
			assert.Equal(t, tc.wantOff, off)
		})
	}
}
