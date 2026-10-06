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

package conduit

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	core "github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/clock"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/grant"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/transport/ws"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// TestClassifyCloseCodes pins the dialer's action for every close code
// and upgrade status of the 1e close-code table.
func TestClassifyCloseCodes(t *testing.T) {
	ce := func(code uint32) error { return &core.CloseError{Code: code} }
	ga := func(code uint32) *conduitv1.GoAway { return &conduitv1.GoAway{Code: code} }
	de := func(status int) error { return &ws.DialError{StatusCode: status, Err: errors.New("bad handshake")} }
	tests := []struct {
		name string
		end  core.End
		want Action
	}{
		{"4409 at handshake", core.End{DialErr: ce(4409)}, ActionRefused},
		{"4409 on a live session", core.End{GoAway: ga(4409)}, ActionRefused},
		{"4403", core.End{DialErr: ce(4403)}, ActionStop},
		{"4404", core.End{GoAway: ga(4404)}, ActionStop},
		{"4401 at handshake", core.End{DialErr: ce(4401)}, ActionRefreshCredential},
		{"4401 on a live session", core.End{GoAway: ga(4401)}, ActionRefreshCredential},
		{"4400", core.End{GoAway: ga(4400)}, ActionMaxBackoff},
		{"4503", core.End{GoAway: ga(4503)}, ActionBackoff},
		{"4504", core.End{DialErr: ce(4504)}, ActionBackoff},
		{"1011", core.End{SessionErr: ce(1011)}, ActionBackoff},
		{"transport loss", core.End{SessionErr: core.ErrKeepaliveTimeout}, ActionBackoff},
		{"network error", core.End{DialErr: &ws.DialError{Err: errors.New("connection refused")}}, ActionBackoff},
		{"HTTP 404", core.End{DialErr: de(http.StatusNotFound)}, ActionUnsupported},
		{"HTTP 401", core.End{DialErr: de(http.StatusUnauthorized)}, ActionRefreshCredential},
		{"HTTP 403", core.End{DialErr: de(http.StatusForbidden)}, ActionStop},
		{"HTTP 503", core.End{DialErr: de(http.StatusServiceUnavailable)}, ActionBackoff},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Classify(tt.end); got != tt.want {
				t.Fatalf("Classify = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestAgentDecideDelays: 4400 waits BackoffMax; backoff codes keep the
// default delay.
func TestAgentDecideDelays(t *testing.T) {
	a, err := New(Options{HubURL: "http://hub", AgentID: testAgentID, ProjectID: testProjectID, Token: func() string { return "t" }})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		end  core.End
		want time.Duration
	}{
		{"4400 max backoff", core.End{GoAway: &conduitv1.GoAway{Code: core.CloseProtocolError}}, core.BackoffMax},
		{"4504 default", core.End{DialErr: &core.CloseError{Code: core.CloseRelayTimeout}}, 3 * time.Second},
		{"1011 default", core.End{SessionErr: &core.CloseError{Code: closeInternalError}}, 3 * time.Second},
		{"4503 default (window draw)", core.End{GoAway: &conduitv1.GoAway{Code: core.CloseRelayRestart}}, 3 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := a.decide(context.Background(), tt.end, 3*time.Second)
			if err != nil || got != tt.want {
				t.Fatalf("decide = %v, %v; want %v", got, err, tt.want)
			}
		})
	}
}

// TestAgentHelloCarriesLaunchID: the Hello presents SCION_LAUNCH_ID as
// the endpoint incarnation, offers only tcp streams, and the upgrade
// carries the agent token.
func TestAgentHelloCarriesLaunchID(t *testing.T) {
	h := newFakeHub(t)
	startAgent(t, h, func(o *Options) { o.LaunchID = "launch-42" })
	hello := h.nextHello(t)
	if hello.GetPrincipalKind() != conduitv1.PrincipalKind_PRINCIPAL_KIND_AGENT || hello.GetPrincipalId() != testAgentID {
		t.Fatalf("principal = %v %q", hello.GetPrincipalKind(), hello.GetPrincipalId())
	}
	caps := hello.GetCapabilities()
	if caps.GetEndpointIncarnation() != "launch-42" {
		t.Fatalf("endpoint_incarnation = %q, want launch-42", caps.GetEndpointIncarnation())
	}
	if len(caps.GetStreamKinds()) != 1 || caps.GetStreamKinds()[0] != grant.StreamKindTCP {
		t.Fatalf("stream kinds = %v, want [tcp]", caps.GetStreamKinds())
	}
	h.nextSession(t)
	h.mu.Lock()
	tok := h.tokens[0]
	h.mu.Unlock()
	if tok != "token-1" {
		t.Fatalf("upgrade token = %q", tok)
	}
}

// TestAgentStopsOnTerminalCodes: 4403 and 4404 stop the dialer with a
// *TerminalError after one attempt; an HTTP 404 stops it with
// ErrUnsupported.
func TestAgentStopsOnTerminalCodes(t *testing.T) {
	tests := []struct {
		name     string
		set      func(h *fakeHub)
		wantCode uint32
		wantErr  error
	}{
		{"4403 forbidden", func(h *fakeHub) { h.reject = core.Reject(core.CloseForbidden, "forbidden") }, core.CloseForbidden, nil},
		{"4404 not found", func(h *fakeHub) { h.reject = core.Reject(closeTargetNotFound, "target_not_found") }, closeTargetNotFound, nil},
		{"HTTP 404 old hub", func(h *fakeHub) { h.status = http.StatusNotFound }, 0, ErrUnsupported},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newFakeHub(t)
			h.set(tt.set)
			_, done := startAgent(t, h, nil)
			err := runResult(t, done)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("Run = %v, want %v", err, tt.wantErr)
				}
			} else {
				var te *TerminalError
				if !errors.As(err, &te) || te.Code != tt.wantCode {
					t.Fatalf("Run = %v, want TerminalError code %d", err, tt.wantCode)
				}
			}
			if n := h.conduitHits.Load(); n != 1 {
				t.Fatalf("%d conduit attempts, want 1", n)
			}
		})
	}
}

// TestAgentRefusalWindow pins the 4409 rule: each refusal is retried
// after BackoffMax, the dialer stops once refusals have persisted for
// RefusalStopAfter, an admission starts a new window, and other failures
// in between neither stop nor reset it.
func TestAgentRefusalWindow(t *testing.T) {
	type event struct {
		kind      string // refuse | refuse_live | admit | transient
		at        time.Duration
		wantDelay time.Duration
		wantStop  bool
	}
	refusals := func(kind string, from, to time.Duration) []event {
		var evs []event
		for at := from; at < from+RefusalStopAfter && at <= to; at += core.BackoffMax {
			evs = append(evs, event{kind: kind, at: at, wantDelay: core.BackoffMax})
		}
		return evs
	}
	stop := func(at time.Duration) event { return event{kind: "refuse", at: at, wantStop: true} }
	cat := func(parts ...[]event) []event {
		var evs []event
		for _, p := range parts {
			evs = append(evs, p...)
		}
		return evs
	}
	tests := []struct {
		name   string
		events []event
	}{
		{"zombie: retried every BackoffMax, stops after RefusalStopAfter",
			cat(refusals("refuse", 0, RefusalStopAfter), []event{stop(RefusalStopAfter)})},
		{"refused live session counts like a handshake refusal",
			cat(refusals("refuse_live", 0, RefusalStopAfter), []event{stop(RefusalStopAfter)})},
		{"admission inside the window starts a new one",
			cat(refusals("refuse", 0, 3*time.Minute), []event{{kind: "admit", at: 4 * time.Minute}},
				refusals("refuse", 5*time.Minute, 15*time.Minute), []event{stop(15 * time.Minute)})},
		{"transient failures neither stop nor reset the window",
			cat(refusals("refuse", 0, time.Minute),
				[]event{{kind: "transient", at: 90 * time.Second, wantDelay: 3 * time.Second}},
				refusals("refuse", 2*time.Minute, RefusalStopAfter-time.Minute), []event{stop(RefusalStopAfter)})},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			start := time.Unix(1_700_000_000, 0)
			clk := clock.NewFake(start)
			a, err := New(Options{HubURL: "http://hub", AgentID: testAgentID, ProjectID: testProjectID, Token: func() string { return "t" }, Clock: clk})
			if err != nil {
				t.Fatal(err)
			}
			for _, ev := range tt.events {
				clk.Advance(start.Add(ev.at).Sub(clk.Now()))
				var end core.End
				switch ev.kind {
				case "admit":
					a.admitted()
					continue
				case "refuse":
					end = core.End{DialErr: &core.CloseError{Code: closeSuperseded, Reason: "superseded_incarnation"}}
				case "refuse_live":
					end = core.End{GoAway: &conduitv1.GoAway{Code: closeSuperseded, Reason: "superseded_incarnation"}}
				case "transient":
					end = core.End{DialErr: &core.CloseError{Code: core.CloseRelayTimeout}}
				}
				d, err := a.decide(context.Background(), end, 3*time.Second)
				if ev.wantStop {
					var te *TerminalError
					if !errors.As(err, &te) || te.Code != closeSuperseded {
						t.Fatalf("at %v: decide = %v, %v; want TerminalError 4409", ev.at, d, err)
					}
					continue
				}
				if err != nil || d != ev.wantDelay {
					t.Fatalf("at %v (%s): decide = %v, %v; want %v", ev.at, ev.kind, d, err, ev.wantDelay)
				}
			}
		})
	}
}

type decision struct {
	end   core.End
	delay time.Duration
	err   error
}

// refusalAgent runs an Agent against h on a fake clock and returns the
// clock, its decisions and Run's result.
func refusalAgent(t *testing.T, h *fakeHub) (*clock.Fake, <-chan decision, <-chan error) {
	t.Helper()
	clk := clock.NewFake(time.Unix(1_700_000_000, 0))
	decisions := make(chan decision, 32)
	_, done := startAgent(t, h, func(o *Options) {
		o.Clock = clk
		o.OnEnd = func(end core.End, d time.Duration, err error) { decisions <- decision{end, d, err} }
	})
	return clk, decisions, done
}

func nextDecision(t *testing.T, decisions <-chan decision) decision {
	t.Helper()
	select {
	case d := <-decisions:
		return d
	case <-time.After(waitTimeout):
		t.Fatal("no reconnect decision")
		return decision{}
	}
}

// TestAgentSupersededRedialsOncePerMinute: a container the hub keeps
// refusing with 4409 (a zombie) redials once per BackoffMax, never
// sooner, and stops with a *TerminalError once the refusals have
// persisted for RefusalStopAfter.
func TestAgentSupersededRedialsOncePerMinute(t *testing.T) {
	h := newFakeHub(t)
	h.set(func(h *fakeHub) { h.reject = core.Reject(closeSuperseded, "superseded_incarnation") })
	clk, decisions, done := refusalAgent(t, h)
	start := clk.Now()
	for attempt := int64(1); ; attempt++ {
		d := nextDecision(t, decisions)
		if d.err != nil {
			break
		}
		if d.delay != core.BackoffMax {
			t.Fatalf("attempt %d: redial in %v, want %v", attempt, d.delay, core.BackoffMax)
		}
		waitPending(t, clk, 2) // the redial timer and the key refresh
		clk.Advance(core.BackoffMax - time.Second)
		if n := h.conduitHits.Load(); n != attempt {
			t.Fatalf("redialed early: %d attempts after attempt %d", n, attempt)
		}
		clk.Advance(time.Second)
	}
	var te *TerminalError
	if err := runResult(t, done); !errors.As(err, &te) || te.Code != closeSuperseded {
		t.Fatalf("Run = %v, want TerminalError 4409", err)
	}
	if got := clk.Now().Sub(start); got != RefusalStopAfter {
		t.Fatalf("stopped after %v, want %v", got, RefusalStopAfter)
	}
	if n, want := h.conduitHits.Load(), int64(RefusalStopAfter/core.BackoffMax)+1; n != want {
		t.Fatalf("%d attempts, want %d", n, want)
	}
}

// TestAgentRecoversWhenRefusalClears: a container whose 4409 refusals
// clear inside the window is admitted on its next redial and keeps
// running; a later 4409 on the live session is retried, not terminal.
func TestAgentRecoversWhenRefusalClears(t *testing.T) {
	h := newFakeHub(t)
	refuse := func(h *fakeHub) { h.reject = core.Reject(closeSuperseded, "superseded_incarnation") }
	h.set(refuse)
	clk, decisions, done := refusalAgent(t, h)
	for i := range 3 {
		if d := nextDecision(t, decisions); d.err != nil || d.delay != core.BackoffMax {
			t.Fatalf("decision = %v, %v; want redial in %v", d.delay, d.err, core.BackoffMax)
		}
		waitPending(t, clk, 2)
		if i == 2 {
			h.set(func(h *fakeHub) { h.reject = nil }) // the refusal clears
		}
		clk.Advance(core.BackoffMax)
	}
	s := h.nextSession(t)
	if n := h.conduitHits.Load(); n != 4 {
		t.Fatalf("%d attempts, want 4", n)
	}
	select {
	case err := <-done:
		t.Fatalf("Run returned %v after the refusal cleared", err)
	default:
	}

	h.set(refuse)
	if err := s.CloseWithCode(closeSuperseded, "superseded_incarnation"); err != nil {
		t.Fatal(err)
	}
	if d := nextDecision(t, decisions); d.err != nil || d.delay != core.BackoffMax || d.end.Code() != closeSuperseded {
		t.Fatalf("live 4409: decision = %v, %v (code %d); want redial in %v", d.delay, d.err, d.end.Code(), core.BackoffMax)
	}
}

// TestAgentRefreshesCredentialOn4401: a 4401 refresh the credential and
// the next attempt presents the new token; refreshes are rate-limited.
func TestAgentRefreshesCredentialOn4401(t *testing.T) {
	h := newFakeHub(t)
	var token atomic.Value
	token.Store("old")
	var refreshes atomic.Int64
	h.set(func(h *fakeHub) { h.reject = core.Reject(core.CloseUnauthenticated, "unauthenticated") })
	startAgent(t, h, func(o *Options) {
		o.Token = func() string { return token.Load().(string) }
		o.RefreshCredential = func(context.Context) error {
			if refreshes.Add(1) == 1 {
				token.Store("new")
				h.set(func(h *fakeHub) { h.reject = nil })
			}
			return nil
		}
	})
	h.nextSession(t)
	if n := refreshes.Load(); n != 1 {
		t.Fatalf("%d credential refreshes, want 1", n)
	}
	h.mu.Lock()
	tokens := append([]string(nil), h.tokens...)
	h.mu.Unlock()
	if len(tokens) != 2 || tokens[0] != "old" || tokens[1] != "new" {
		t.Fatalf("tokens presented = %v, want [old new]", tokens)
	}
}

// TestAgentReconnectsAfterTransientClose: 4504 and 1011 closes redial.
func TestAgentReconnectsAfterTransientClose(t *testing.T) {
	for _, code := range []uint32{core.CloseRelayTimeout, closeInternalError} {
		t.Run(strconv.Itoa(int(code)), func(t *testing.T) {
			h := newFakeHub(t)
			startAgent(t, h, nil)
			s := h.nextSession(t)
			if err := s.CloseWithCode(code, "internal"); err != nil {
				t.Fatal(err)
			}
			h.nextSession(t)
		})
	}
}

// echoListener serves a loopback echo server and returns its port.
func echoListener(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = c.Close() }()
				_, _ = io.Copy(c, c)
			}()
		}
	}()
	return l.Addr().(*net.TCPAddr).Port
}

// TestAgentTCPStream: a granted TCP stream reaches the loopback port and
// echoes; half-close propagates.
func TestAgentTCPStream(t *testing.T) {
	key := newTestKey(t, "k1")
	h := newFakeHub(t, key.public)
	startAgent(t, h, nil)
	s := h.nextSession(t)
	port := echoListener(t)

	st, err := s.OpenStream(context.Background(), tcpOpen(t, key, s.Info(), "launch-1", port))
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	if _, err := st.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	if err := st.(interface{ CloseWrite() error }).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(st)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "ping" {
		t.Fatalf("echo = %q", got)
	}
}

// TestAgentTCPStreamRefusals: the target refuses a stream whose grant
// does not verify against this session's Welcome, and never dials for it.
func TestAgentTCPStreamRefusals(t *testing.T) {
	key := newTestKey(t, "k1")
	other := newTestKey(t, "k2")
	port := 8080
	tests := []struct {
		name string
		open func(t *testing.T, info core.SessionInfo) *conduitv1.StreamOpen
	}{
		{"unknown key", func(t *testing.T, info core.SessionInfo) *conduitv1.StreamOpen {
			return tcpOpen(t, other, info, "launch-1", port)
		}},
		{"incarnation from Hello, not Welcome", func(t *testing.T, info core.SessionInfo) *conduitv1.StreamOpen {
			return tcpOpen(t, key, info, "launch-presented", port)
		}},
		{"other session", func(t *testing.T, info core.SessionInfo) *conduitv1.StreamOpen {
			info.SessionID = "sess-other"
			return tcpOpen(t, key, info, "launch-1", port)
		}},
		{"other connection epoch", func(t *testing.T, info core.SessionInfo) *conduitv1.StreamOpen {
			info.ConnectionEpoch++
			return tcpOpen(t, key, info, "launch-1", port)
		}},
		{"params altered after signing", func(t *testing.T, info core.SessionInfo) *conduitv1.StreamOpen {
			o := tcpOpen(t, key, info, "launch-1", port)
			o.Params[grant.ParamPort] = "9090"
			return o
		}},
		{"no grant", func(t *testing.T, info core.SessionInfo) *conduitv1.StreamOpen {
			o := tcpOpen(t, key, info, "launch-1", port)
			o.Grant = nil
			return o
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newFakeHub(t, key.public)
			// The admitted incarnation is the Welcome's, which differs from
			// the presented launch id here.
			h.set(func(h *fakeHub) {
				inner := h.welcome
				h.welcome = func(hello *conduitv1.Hello) *conduitv1.Welcome {
					w := inner(hello)
					w.EndpointIncarnation = "launch-1"
					return w
				}
			})
			startAgent(t, h, func(o *Options) {
				o.LaunchID = "launch-presented"
				o.DialLocal = func(context.Context, string, string) (net.Conn, error) {
					t.Error("DialLocal called for a refused stream")
					return nil, errors.New("refused")
				}
			})
			s := h.nextSession(t)
			_, err := s.OpenStream(context.Background(), tt.open(t, s.Info()))
			if core.CodeOf(err, 0) != core.CloseForbidden {
				t.Fatalf("OpenStream = %v, want 4403", err)
			}
		})
	}
}

// TestAgentGrantUsesWelcomeIncarnation: grants are verified against the
// incarnation the hub admitted (Welcome), e.g. "gen-N" for a container
// that presented no launch id.
func TestAgentGrantUsesWelcomeIncarnation(t *testing.T) {
	key := newTestKey(t, "k1")
	h := newFakeHub(t, key.public)
	startAgent(t, h, func(o *Options) { o.LaunchID = "" })
	s := h.nextSession(t)
	port := echoListener(t)
	st, err := s.OpenStream(context.Background(), tcpOpen(t, key, s.Info(), "gen-1", port))
	if err != nil {
		t.Fatalf("OpenStream with the admitted incarnation: %v", err)
	}
	_ = st.Close()
}

// TestAgentTCPLoopbackOnly: even with a valid grant, the target dials only
// 127.0.0.1 and refuses the reserved ports and non-loopback hosts.
func TestAgentTCPLoopbackOnly(t *testing.T) {
	key := newTestKey(t, "k1")
	tests := []struct {
		name   string
		params map[string]string
	}{
		{"hub API port", map[string]string{"host": "127.0.0.1", "port": "9810"}},
		{"metadata port", map[string]string{"host": "127.0.0.1", "port": "18380"}},
		{"non-loopback host", map[string]string{"host": "10.0.0.1", "port": "8080"}},
		{"localhost name", map[string]string{"host": "localhost", "port": "8080"}},
		{"ipv6 loopback", map[string]string{"host": "::1", "port": "8080"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newFakeHub(t, key.public)
			startAgent(t, h, func(o *Options) {
				o.DialLocal = func(context.Context, string, string) (net.Conn, error) {
					t.Error("DialLocal called for a refused target")
					return nil, errors.New("refused")
				}
			})
			s := h.nextSession(t)
			open := tcpOpenParams(t, key, s.Info(), "launch-1", tt.params)
			if _, err := s.OpenStream(context.Background(), open); core.CodeOf(err, 0) != core.CloseForbidden {
				t.Fatalf("OpenStream = %v, want 4403", err)
			}
		})
	}
}

// TestAgentTCPDialsOnlyLoopback: the only address the target dials is
// 127.0.0.1:<granted port>.
func TestAgentTCPDialsOnlyLoopback(t *testing.T) {
	key := newTestKey(t, "k1")
	h := newFakeHub(t, key.public)
	dialed := make(chan string, 1)
	startAgent(t, h, func(o *Options) {
		o.DialLocal = func(_ context.Context, network, addr string) (net.Conn, error) {
			dialed <- network + " " + addr
			return nil, errors.New("connection refused")
		}
	})
	s := h.nextSession(t)
	_, err := s.OpenStream(context.Background(), tcpOpen(t, key, s.Info(), "launch-1", 8080))
	if core.CodeOf(err, 0) != core.CloseRelayTimeout {
		t.Fatalf("OpenStream = %v, want 4504 upstream_unreachable", err)
	}
	if got := <-dialed; got != "tcp 127.0.0.1:8080" {
		t.Fatalf("dialed %q", got)
	}
}

// TestTCPTarget pins the local target policy.
func TestTCPTarget(t *testing.T) {
	tests := []struct {
		name    string
		params  map[string]string
		want    int
		wantErr bool
	}{
		{"ok", map[string]string{"host": "127.0.0.1", "port": "8080"}, 8080, false},
		{"reserved 9810", map[string]string{"host": "127.0.0.1", "port": "9810"}, 0, true},
		{"reserved 18380", map[string]string{"host": "127.0.0.1", "port": "18380"}, 0, true},
		{"port 0", map[string]string{"host": "127.0.0.1", "port": "0"}, 0, true},
		{"port too large", map[string]string{"host": "127.0.0.1", "port": "65536"}, 0, true},
		{"non-canonical port", map[string]string{"host": "127.0.0.1", "port": "08080"}, 0, true},
		{"other host", map[string]string{"host": "192.168.1.1", "port": "8080"}, 0, true},
		{"missing host", map[string]string{"port": "8080"}, 0, true},
		{"extra param", map[string]string{"host": "127.0.0.1", "port": "8080", "agent_id": "a"}, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := TCPTarget(tt.params)
			if (err != nil) != tt.wantErr || got != tt.want {
				t.Fatalf("TCPTarget = %d, %v; want %d (err %v)", got, err, tt.want, tt.wantErr)
			}
		})
	}
}

// tcpOpenParams is tcpOpen with arbitrary (signed) params.
func tcpOpenParams(t *testing.T, key testKey, info core.SessionInfo, incarnation string, params map[string]string) *conduitv1.StreamOpen {
	t.Helper()
	now := time.Now().Truncate(time.Second)
	tok, err := grant.Mint(&key.signer, grant.Claims{
		Issuer:    "scion-hub",
		Subject:   "user:u1",
		ProjectID: testProjectID,
		Target: grant.Target{
			Kind: grant.TargetKindAgent, ID: testAgentID, EndpointIncarnation: incarnation,
			SessionID: info.SessionID, ConnectionEpoch: info.ConnectionEpoch,
		},
		Stream:    grant.StreamHeader{Kind: grant.StreamKindTCP, Params: params},
		NotBefore: now,
		Expiry:    now.Add(50 * time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	return &conduitv1.StreamOpen{Kind: conduitv1.StreamKind_STREAM_KIND_TCP, Params: params, Grant: tok}
}
