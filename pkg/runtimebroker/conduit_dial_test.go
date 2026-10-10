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

package runtimebroker

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/brokeridentity"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/clock"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/transport/ws"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// agentsOnlyBody is the 403 body of a hub whose conduit endpoint admits
// agent principals only.
const agentsOnlyBody = `{"error":{"code":"forbidden","message":"Conduit sessions are open to agent principals only"}}`

// fakeConduitHub answers /api/v1/conduit with a scripted step per request.
// A step either answers with an HTTP status and body (no upgrade) or
// admits the session and hands it to after.
type fakeConduitHub struct {
	t     *testing.T
	srv   *httptest.Server
	mu    sync.Mutex
	steps []hubStep
	// hellos receives each admitted Hello; headers each upgrade's headers.
	hellos  chan *conduitv1.Hello
	headers chan http.Header
	// ended receives once per admitted session when it has ended.
	ended chan struct{}
}

type hubStep struct {
	status int
	body   string
	// after runs on an admitted session (nil: keep it open).
	after func(conduit.LocalSession)
}

type welcomeAdmitter struct{ hellos chan *conduitv1.Hello }

func (a welcomeAdmitter) Admit(_ context.Context, h *conduitv1.Hello) (*conduitv1.Welcome, error) {
	a.hellos <- h
	return &conduitv1.Welcome{SessionId: "s-" + h.GetCapabilities().GetEndpointIncarnation(), EndpointIncarnation: h.GetCapabilities().GetEndpointIncarnation()}, nil
}

func (welcomeAdmitter) Refresh(context.Context, *conduitv1.AuthRefresh) error { return nil }

func newFakeConduitHub(t *testing.T, steps ...hubStep) *fakeConduitHub {
	t.Helper()
	h := &fakeConduitHub{t: t, steps: steps, hellos: make(chan *conduitv1.Hello, 16), headers: make(chan http.Header, 16), ended: make(chan struct{}, 16)}
	h.srv = httptest.NewServer(http.HandlerFunc(h.serve))
	t.Cleanup(h.srv.Close)
	return h
}

func (h *fakeConduitHub) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != conduitEndpointPath {
		http.NotFound(w, r)
		return
	}
	h.mu.Lock()
	var st hubStep
	if len(h.steps) > 0 {
		st = h.steps[0]
		if len(h.steps) > 1 {
			h.steps = h.steps[1:]
		}
	}
	h.mu.Unlock()
	h.headers <- r.Header.Clone()
	if st.status != 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(st.status)
		_, _ = w.Write([]byte(st.body))
		return
	}
	conn, err := ws.Upgrade(w, r, nil, ws.Options{})
	if err != nil {
		return
	}
	s, err := conduit.Accept(r.Context(), conn, conduit.Config{Clock: clock.Real()}, welcomeAdmitter{hellos: h.hellos})
	if err != nil {
		return
	}
	ls := s.(conduit.LocalSession)
	if st.after != nil {
		st.after(ls)
	}
	<-ls.Done()
	h.ended <- struct{}{}
}

func waitHello(t *testing.T, hub *fakeConduitHub) *conduitv1.Hello {
	t.Helper()
	select {
	case h := <-hub.hellos:
		return h
	case <-time.After(10 * time.Second):
		t.Fatal("no conduit hello")
		return nil
	}
}

// dialerEnd is one Reconnector.Decide outcome.
type dialerEnd struct {
	end   conduit.End
	delay time.Duration
	err   error
}

// startTestDialer runs a dialer against url on clk and returns its ends,
// its log and a stop function that waits for Run to return.
func startTestDialer(t *testing.T, url string, clk clock.Clock, rnd func(int64) int64, mod func(*ConduitDialConfig)) (<-chan dialerEnd, *syncBuffer, func() error) {
	t.Helper()
	logs := &syncBuffer{}
	ends := make(chan dialerEnd, 64)
	cfg := ConduitDialConfig{
		HubEndpoint: url,
		BrokerID:    "broker-1",
		SecretKey:   []byte("0123456789abcdef0123456789abcdef"),
		Version:     "test",
		Clock:       clk,
		Rand:        rnd,
		Log:         slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		OnEnd: func(end conduit.End, delay time.Duration, err error) {
			ends <- dialerEnd{end: end, delay: delay, err: err}
		},
	}
	if mod != nil {
		mod(&cfg)
	}
	d, err := NewConduitDialer(cfg)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	var once sync.Once
	var runErr error
	// stop cancels Run and returns its error; the cancellation itself
	// (context.Canceled) is the expected end and reads as nil.
	stop := func() error {
		once.Do(func() {
			cancel()
			runErr = <-done
			if errors.Is(runErr, context.Canceled) {
				runErr = nil
			}
		})
		return runErr
	}
	t.Cleanup(func() { _ = stop() })
	return ends, logs, stop
}

func nextEnd(t *testing.T, ends <-chan dialerEnd) dialerEnd {
	t.Helper()
	select {
	case e := <-ends:
		return e
	case <-time.After(10 * time.Second):
		t.Fatal("no dial outcome")
		return dialerEnd{}
	}
}

// maxRand makes every jittered delay its ceiling.
func maxRand(n int64) int64 { return n - 1 }

// TestConduitDialer_PreUpgradeRefusalFallsBack: every refusal before the
// upgrade keeps the broker on the control channel and re-probes; 404 and
// the agents-only 403 log at INFO and wait the full cap, auth refusals
// log at ERROR, other failures at WARN with the probe backoff.
func TestConduitDialer_PreUpgradeRefusalFallsBack(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		body      string
		wantDelay time.Duration
		wantLevel string
	}{
		{name: "404 conduit off", status: http.StatusNotFound, body: `{"error":{"code":"not_found"}}`, wantDelay: conduitProbeMax, wantLevel: "level=INFO"},
		{name: "403 agents only", status: http.StatusForbidden, body: agentsOnlyBody, wantDelay: conduitProbeMax, wantLevel: "level=INFO"},
		{name: "403 other", status: http.StatusForbidden, body: `{"error":{"code":"forbidden","message":"no"}}`, wantDelay: time.Second, wantLevel: "level=ERROR"},
		{name: "401", status: http.StatusUnauthorized, body: `{"error":{"code":"broker_auth_failed"}}`, wantDelay: time.Second, wantLevel: "level=ERROR"},
		{name: "503", status: http.StatusServiceUnavailable, body: `{}`, wantDelay: time.Second, wantLevel: "level=WARN"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hub := newFakeConduitHub(t, hubStep{status: tc.status, body: tc.body})
			ends, logs, stop := startTestDialer(t, hub.srv.URL, clock.NewFake(time.Now()), maxRand, nil)
			e := nextEnd(t, ends)
			require.NoError(t, stop())
			require.Error(t, e.end.DialErr)
			assert.NoError(t, e.err, "a refusal never stops the dialer")
			assert.Equal(t, tc.wantDelay, e.delay)
			assert.Contains(t, logs.String(), tc.wantLevel)
			assert.Contains(t, logs.String(), "control channel only")
		})
	}
	t.Run("dial failure", func(t *testing.T) {
		srv := httptest.NewServer(http.NotFoundHandler())
		url := srv.URL
		srv.Close()
		ends, logs, stop := startTestDialer(t, url, clock.NewFake(time.Now()), maxRand, nil)
		e := nextEnd(t, ends)
		require.NoError(t, stop())
		assert.Equal(t, time.Second, e.delay)
		assert.Contains(t, logs.String(), "level=WARN")
	})
}

// TestConduitDialer_ExperimentOffProbesAtMostEveryFiveMinutes: against a
// hub that answers 404, the dialer probes once per conduitProbeMax and
// nothing else: over 30 minutes of fake time exactly 7 probes, each logged
// once at INFO.
func TestConduitDialer_ExperimentOffProbesAtMostEveryFiveMinutes(t *testing.T) {
	hub := newFakeConduitHub(t, hubStep{status: http.StatusNotFound})
	clk := clock.NewFake(time.Now())
	ends, logs, stop := startTestDialer(t, hub.srv.URL, clk, maxRand, nil)

	probes := 1
	assert.Equal(t, conduitProbeMax, nextEnd(t, ends).delay)
	for i := 0; i < 6; i++ {
		require.True(t, clk.WaitForTimer(10*time.Second, clk.Now().Add(conduitProbeMax)), "re-probe timer armed")
		clk.Advance(conduitProbeMax - time.Second)
		select {
		case e := <-ends:
			t.Fatalf("probe before the cap: %+v", e)
		default:
		}
		assert.Equal(t, 1, clk.Pending(), "the re-probe timer has not fired")
		clk.Advance(time.Second)
		assert.Equal(t, conduitProbeMax, nextEnd(t, ends).delay)
		probes++
	}
	require.NoError(t, stop())
	assert.Equal(t, 7, probes)
	assert.Len(t, hub.headers, 7, "one request per probe")
	out := logs.String()
	assert.Equal(t, 7, strings.Count(out, "level=INFO"))
	assert.NotContains(t, out, "level=WARN")
	assert.NotContains(t, out, "level=ERROR")
}

// TestConduitDialer_HelloAndHeaders: the dialer signs the upgrade with the
// broker's HMAC headers and presents the broker principal, its process
// incarnation and exec scope, and no stream kinds or RPCs.
func TestConduitDialer_HelloAndHeaders(t *testing.T) {
	hub := newFakeConduitHub(t, hubStep{})
	_, _, stop := startTestDialer(t, hub.srv.URL, clock.Real(), maxRand, func(c *ConduitDialConfig) { c.ExecScope = "target-1" })

	var hello *conduitv1.Hello
	select {
	case hello = <-hub.hellos:
	case <-time.After(10 * time.Second):
		t.Fatal("no hello")
	}
	h := <-hub.headers
	require.NoError(t, stop())
	assert.Equal(t, "broker-1", h.Get("X-Scion-Broker-ID"))
	assert.NotEmpty(t, h.Get("X-Scion-Signature"))
	assert.Equal(t, conduitv1.PrincipalKind_PRINCIPAL_KIND_BROKER, hello.GetPrincipalKind())
	assert.Equal(t, "broker-1", hello.GetPrincipalId())
	assert.Equal(t, "test", hello.GetClientVersion())
	caps := hello.GetCapabilities()
	assert.Equal(t, conduitProcessIncarnation, caps.GetEndpointIncarnation())
	assert.NotEmpty(t, caps.GetEndpointIncarnation())
	assert.Equal(t, "target-1", caps.GetExecScope())
	assert.Empty(t, caps.GetStreamKinds())
	assert.Empty(t, caps.GetRpc())
}

// TestConduitDialer_PostAdmissionCloses: closes after the upgrade follow
// the close-code rules, never the fallback: 4503 and 4401 redial with the
// session backoff, 4400 waits the maximum backoff, 4403 stops the dialer.
func TestConduitDialer_PostAdmissionCloses(t *testing.T) {
	closeWith := func(code uint32) func(conduit.LocalSession) {
		return func(ls conduit.LocalSession) { _ = ls.CloseWithCode(code, "test") }
	}
	for _, tc := range []struct {
		name      string
		code      uint32
		wantDelay time.Duration
		wantErr   error
	}{
		{name: "4503 relay restart", code: conduit.CloseRelayRestart, wantDelay: 0},
		{name: "4401 unauthenticated", code: conduit.CloseUnauthenticated, wantDelay: 0},
		{name: "4400 protocol error", code: conduit.CloseProtocolError, wantDelay: conduit.BackoffMax},
		{name: "4403 forbidden", code: conduit.CloseForbidden, wantErr: errConduitForbidden},
		{name: "4409 superseded incarnation", code: closeSupersededIncarnation, wantDelay: conduit.BackoffMax},
		{name: "4504 relay timeout", code: conduit.CloseRelayTimeout, wantDelay: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hub := newFakeConduitHub(t, hubStep{after: closeWith(tc.code)}, hubStep{})
			ends, logs, stop := startTestDialer(t, hub.srv.URL, clock.Real(), func(int64) int64 { return 0 }, nil)
			e := nextEnd(t, ends)
			assert.NoError(t, e.end.DialErr)
			assert.Equal(t, tc.code, e.end.Code())
			assert.NotContains(t, logs.String(), "control channel only", "not a fallback")
			if tc.wantErr != nil {
				assert.ErrorIs(t, e.err, tc.wantErr)
				assert.ErrorIs(t, stop(), tc.wantErr)
				return
			}
			require.NoError(t, e.err)
			assert.Equal(t, tc.wantDelay, e.delay)
			if tc.wantDelay == 0 {
				select {
				case <-hub.hellos:
				case <-time.After(10 * time.Second):
					t.Fatal("no first hello")
				}
				select {
				case <-hub.hellos:
				case <-time.After(10 * time.Second):
					t.Fatal("no redial")
				}
			}
			require.NoError(t, stop())
		})
	}
}

// TestConduitDialer_SessionResetsProbeBackoff: the probe backoff grows
// across refusals and starts over after a session was established.
func TestConduitDialer_SessionResetsProbeBackoff(t *testing.T) {
	hub := newFakeConduitHub(t,
		hubStep{status: http.StatusServiceUnavailable},
		hubStep{status: http.StatusServiceUnavailable},
		hubStep{after: func(ls conduit.LocalSession) { _ = ls.CloseWithCode(conduit.CloseRelayTimeout, "test") }},
		hubStep{status: http.StatusServiceUnavailable},
	)
	clk := clock.NewFake(time.Now())
	ends, _, stop := startTestDialer(t, hub.srv.URL, clk, maxRand, nil)

	wait := func(d time.Duration) {
		t.Helper()
		require.True(t, clk.WaitForTimer(10*time.Second, clk.Now().Add(d)), "timer for %v", d)
		clk.Advance(d)
	}
	e := nextEnd(t, ends)
	assert.Equal(t, time.Second, e.delay)
	wait(e.delay)
	e = nextEnd(t, ends)
	assert.Equal(t, 2*time.Second, e.delay)
	wait(e.delay)
	e = nextEnd(t, ends)
	require.NoError(t, e.end.DialErr, "session established")
	assert.Equal(t, conduit.CloseRelayTimeout, e.end.Code())
	assert.Equal(t, time.Second, e.delay, "session backoff starts at its base")
	wait(e.delay)
	e = nextEnd(t, ends)
	require.Error(t, e.end.DialErr)
	assert.Equal(t, time.Second, e.delay, "probe backoff starts over")
	require.NoError(t, stop())
}

// TestConduitEndpointURL maps the hub endpoint to the conduit endpoint.
func TestConduitEndpointURL(t *testing.T) {
	for in, want := range map[string]string{
		"http://hub:8080":       "ws://hub:8080/api/v1/conduit",
		"https://hub.example":   "wss://hub.example/api/v1/conduit",
		"https://hub.example/x": "wss://hub.example/api/v1/conduit",
	} {
		got, err := conduitEndpointURL(in)
		require.NoError(t, err)
		assert.Equal(t, want, got, in)
	}
	_, err := NewConduitDialer(ConduitDialConfig{HubEndpoint: "http://hub"})
	assert.Error(t, err, "broker id required")
}

// TestHubConnection_ConduitDialerLifecycle: HubConnection.Start starts the
// conduit dialer alongside the control channel, with a flat instance's
// runtime target as exec_scope; Stop ends it (its session closes and
// Stop returns only after the dialer exited); Reinitialize dials afresh.
func TestHubConnection_ConduitDialerLifecycle(t *testing.T) {
	hub := newFakeConduitHub(t, hubStep{})
	creds := makeTestCreds("hub-conduit", "broker-1", hub.srv.URL)

	cfg := DefaultServerConfig()
	cfg.BrokerID = "broker-1"
	cfg.BrokerName = "test-host"
	cfg.HubEnabled = true
	cfg.HubEndpoint = hub.srv.URL
	cfg.HeartbeatEnabled = false
	cfg.ControlChannelEnabled = true
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	srv := New(cfg, &mockManager{}, rt)
	srv.config.FlatInstance = &FlatInstanceConfig{Identity: &brokeridentity.Identity{
		RuntimeBrokerID: "broker-1",
		RuntimeTarget:   api.RuntimeTargetDescriptor{ID: "target-1", Type: "docker"},
	}}

	conn, err := srv.createHubConnection("hub-conduit", creds)
	require.NoError(t, err)
	// The exit hook holds the dialer goroutine at its end until the test
	// releases it, so the test can see whether Stop waits for it.
	exiting := make(chan struct{}, 4)
	release := make(chan struct{})
	conn.conduitExitHook = func() {
		exiting <- struct{}{}
		<-release
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	require.NoError(t, conn.Start(ctx, srv))
	hello := waitHello(t, hub)
	assert.Equal(t, "broker-1", hello.GetPrincipalId())
	assert.Equal(t, "target-1", hello.GetCapabilities().GetExecScope())
	assert.Equal(t, conduitProcessIncarnation, hello.GetCapabilities().GetEndpointIncarnation())

	stopped := make(chan struct{})
	go func() {
		conn.Stop()
		close(stopped)
	}()
	select {
	case <-exiting:
	case <-time.After(10 * time.Second):
		t.Fatal("conduit dialer did not exit after Stop")
	}
	// The dialer goroutine is held at its end: Stop must still be
	// waiting for it.
	select {
	case <-stopped:
		t.Fatal("Stop returned before the conduit dialer goroutine ended")
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("Stop did not return after the dialer goroutine ended")
	}
	select {
	case <-hub.ended:
	case <-time.After(10 * time.Second):
		t.Fatal("conduit session still open after Stop")
	}
	conn.mu.RLock()
	cleared := conn.conduitCancel == nil
	conn.mu.RUnlock()
	assert.True(t, cleared, "Stop clears the conduit dialer")

	require.NoError(t, conn.Reinitialize(ctx, srv, creds))
	hello = waitHello(t, hub)
	assert.Equal(t, "target-1", hello.GetCapabilities().GetExecScope())
	conn.Stop()
}
