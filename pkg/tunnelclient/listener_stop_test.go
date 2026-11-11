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

package tunnelclient

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// stopLine is the line a listener prints when it stops accepting while
// connections are still open.
const stopLine = "not accepting new connections; open tunnels continue until they end\n"

// lineWatch is a goroutine-safe stderr that closes seen once its output
// contains want.
type lineWatch struct {
	buf  syncBuffer
	want string
	once sync.Once
	seen chan struct{}
}

func newLineWatch(want string) *lineWatch {
	return &lineWatch{want: want, seen: make(chan struct{})}
}

func (w *lineWatch) Write(p []byte) (int, error) {
	n, err := w.buf.Write(p)
	if strings.Contains(w.buf.String(), w.want) {
		w.once.Do(func() { close(w.seen) })
	}
	return n, err
}

func (w *lineWatch) String() string { return w.buf.String() }

// wait blocks until the line is seen or the test's run context ends.
func (w *lineWatch) wait(t *testing.T) {
	t.Helper()
	select {
	case <-w.seen:
	case <-runCtx(t).Done():
		t.Fatalf("stderr never contained %q; got %q", w.want, w.String())
	}
}

// dialListener opens a local connection to the listener.
func dialListener(t *testing.T, addr string) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// assertRefusedConn checks that the listener closed conn without sending
// anything: its tunnel request was refused.
func assertRefusedConn(t *testing.T, conn net.Conn) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	n, err := conn.Read(make([]byte, 1))
	if n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("refused connection read %d bytes, %v; want a close with no data", n, err)
	}
}

// assertNotAccepting checks that the listening socket is closed.
func assertNotAccepting(t *testing.T, addr string) {
	t.Helper()
	if conn, err := net.Dial("tcp", addr); err == nil {
		_ = conn.Close()
		t.Fatal("the listener still accepts connections after a final refusal")
	}
}

// assertRunning checks that Run has not returned.
func assertRunning(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		t.Fatalf("Run returned %v (exit %d) while a tunnel was still open", err, CodeOf(err))
	default:
	}
}

// firstOpensThenRefuses opens the first tunnel (an echo) and answers
// every later request with refuse.
func firstOpensThenRefuses(refuse func(rpcCall) *conduitv1.RpcResponse) func(rpcCall) *conduitv1.RpcResponse {
	return func(c rpcCall) *conduitv1.RpcResponse {
		if c.n == 1 {
			return openTunnelResp(c, echo)
		}
		return refuse(c)
	}
}

// An expired credential bounds new tunnels only (design §3.8 rule 6): the
// listener refuses the new connection and stops accepting, the tunnel
// opened before the expiry keeps carrying data, and the command exits 3
// with the login line once that tunnel has ended.
func TestListenerCredentialRefusalKeepsOpenTunnels(t *testing.T) {
	for _, tc := range []struct {
		name  string
		creds func() Credential
	}{
		{"no credential on re-resolve", credSeq(oauth("tok1"), Credential{})},
		{"re-resolved credential refused", credSeq(oauth("tok1"), oauth("expired"))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newFakeHub(t)
			h.set(func() {
				h.handle = firstOpensThenRefuses(respond(401, "unauthorized", ""))
			})
			stderr := newLineWatch(stopLine)
			cfg := testConfig(h, newFakeREST(), tc.creds)
			cfg.Stderr = stderr
			addr, cancel, done := listen(t, cfg, Request{Agent: "a1", Project: "proj", Port: 8080})
			defer cancel()

			open := dialListener(t, addr)
			roundTrip(t, open, "before expiry")

			assertRefusedConn(t, dialListener(t, addr))
			stderr.wait(t)
			assertNotAccepting(t, addr)
			roundTrip(t, open, "after expiry")
			assertRunning(t, done)

			_ = open.Close()
			err := <-done
			if CodeOf(err) != ExitCredential {
				t.Fatalf("Run = %v (exit %d), want %d", err, CodeOf(err), ExitCredential)
			}
			assertNamesLogin(t, err, h.srv.URL)
			if dials, calls := h.counts(); dials != 1 || calls != 2 {
				t.Fatalf("dials %d calls %d, want one session and no dial with an unusable credential", dials, calls)
			}
			if n := strings.Count(stderr.String(), stopLine); n != 1 {
				t.Fatalf("stop line printed %d times, want once: %q", n, stderr.String())
			}
		})
	}
}

// Every final refusal stops the listener from accepting but leaves the
// other open tunnels alone (design §3.8 rule 3); the command then exits
// with the refusal's code.
func TestListenerFinalRefusalStopsAcceptingKeepsOpenTunnels(t *testing.T) {
	for _, tc := range []struct {
		name   string
		refuse func(rpcCall) *conduitv1.RpcResponse
		want   int
	}{
		{"4 agent_not_found", respond(404, "agent_not_found", ""), ExitAgentNotFound},
		{"5 forbidden", respond(403, "forbidden", ""), ExitForbidden},
		{"11 no tunnel support", respond(404, "not_found", ""), ExitNoTunnelSupport},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newFakeHub(t)
			h.set(func() { h.handle = firstOpensThenRefuses(tc.refuse) })
			stderr := newLineWatch(stopLine)
			cfg := testConfig(h, newFakeREST(), credSeq(oauth("tok1")))
			cfg.Stderr = stderr
			addr, cancel, done := listen(t, cfg, Request{Agent: "a1", Project: "proj", Port: 8080})
			defer cancel()

			open := dialListener(t, addr)
			roundTrip(t, open, "before")
			assertRefusedConn(t, dialListener(t, addr))
			stderr.wait(t)
			assertNotAccepting(t, addr)
			roundTrip(t, open, "after the refusal")
			assertRunning(t, done)

			_ = open.Close()
			if err := <-done; CodeOf(err) != tc.want {
				t.Fatalf("Run = %v (exit %d), want %d", err, CodeOf(err), tc.want)
			}
		})
	}
}

// A final refusal with no other connection open ends the command at once,
// without the stop line.
func TestListenerFinalRefusalWithNothingOpenExitsAtOnce(t *testing.T) {
	h := newFakeHub(t)
	h.set(func() { h.handle = respond(403, "forbidden", "") })
	var stderr syncBuffer
	cfg := testConfig(h, newFakeREST(), credSeq(oauth("tok1")))
	cfg.Stderr = &stderr
	addr, cancel, done := listen(t, cfg, Request{Agent: "a1", Project: "proj", Port: 8080})
	defer cancel()
	assertRefusedConn(t, dialListener(t, addr))
	if err := <-done; CodeOf(err) != ExitForbidden {
		t.Fatalf("Run = %v (exit %d), want %d", err, CodeOf(err), ExitForbidden)
	}
	if strings.Contains(stderr.String(), stopLine) {
		t.Fatalf("stop line printed with nothing open: %q", stderr.String())
	}
}

// With exactly one other connection open, a final refusal prints the stop
// line once and waits for that connection, then exits with the refusal's
// code.
func TestListenerFinalRefusalWithOneOpenWaitsForIt(t *testing.T) {
	h := newFakeHub(t)
	h.set(func() { h.handle = firstOpensThenRefuses(respond(403, "forbidden", "")) })
	stderr := newLineWatch(stopLine)
	cfg := testConfig(h, newFakeREST(), credSeq(oauth("tok1")))
	cfg.Stderr = stderr
	addr, cancel, done := listen(t, cfg, Request{Agent: "a1", Project: "proj", Port: 8080})
	defer cancel()

	open := dialListener(t, addr)
	roundTrip(t, open, "the one open tunnel")
	assertRefusedConn(t, dialListener(t, addr))
	stderr.wait(t)
	assertRunning(t, done)
	roundTrip(t, open, "still carrying data")
	assertRunning(t, done)

	_ = open.Close()
	if err := <-done; CodeOf(err) != ExitForbidden {
		t.Fatalf("Run = %v (exit %d), want %d", err, CodeOf(err), ExitForbidden)
	}
	if n := strings.Count(stderr.String(), stopLine); n != 1 {
		t.Fatalf("stop line printed %d times, want once: %q", n, stderr.String())
	}
}

// During a drain the reason, with its login hint for a credential
// refusal, is reported exactly once: in the exit error, not in the stop
// line.
func TestListenerDrainReportsTheReasonOnce(t *testing.T) {
	h := newFakeHub(t)
	h.set(func() {
		h.handle = firstOpensThenRefuses(respond(401, "unauthorized", ""))
	})
	stderr := newLineWatch(stopLine)
	cfg := testConfig(h, newFakeREST(), credSeq(oauth("tok1"), Credential{}))
	cfg.Stderr = stderr
	addr, cancel, done := listen(t, cfg, Request{Agent: "a1", Project: "proj", Port: 8080})
	defer cancel()

	open := dialListener(t, addr)
	roundTrip(t, open, "before expiry")
	assertRefusedConn(t, dialListener(t, addr))
	stderr.wait(t)
	_ = open.Close()
	err := <-done
	if CodeOf(err) != ExitCredential {
		t.Fatalf("Run = %v (exit %d), want %d", err, CodeOf(err), ExitCredential)
	}
	const hint = "log in with: scion hub auth login"
	all := stderr.String() + "Error: " + err.Error() + "\n" // what the command prints in total
	if n := strings.Count(all, hint); n != 1 {
		t.Fatalf("login hint reported %d times, want once: %q", n, all)
	}
	if n := strings.Count(all, "expired or was refused"); n != 1 {
		t.Fatalf("reason reported %d times, want once: %q", n, all)
	}
}

// With access withdrawn, the hub refuses new tunnels (403) and closes
// the open one with 4401 at its re-check. The listener stops accepting at
// the refusal; the open tunnel still ends by the hub's close, reported as
// a policy close, and the command exits 5.
func TestListenerWithdrawnAccessOpenTunnelEndsByPolicyClose(t *testing.T) {
	h := newFakeHub(t)
	held := make(chan conduit.Stream, 1)
	h.set(func() {
		h.handle = func(c rpcCall) *conduitv1.RpcResponse {
			if c.n == 1 {
				return openTunnelResp(c, func(st conduit.Stream) {
					held <- st
					echo(st)
				})
			}
			return jsonResp(403, errBody("forbidden", "not permitted", ""))
		}
	})
	stderr := newLineWatch(stopLine)
	cfg := testConfig(h, newFakeREST(), credSeq(oauth("tok1")))
	cfg.Stderr = stderr
	addr, cancel, done := listen(t, cfg, Request{Agent: "a1", Project: "proj", Port: 8080})
	defer cancel()

	open := dialListener(t, addr)
	roundTrip(t, open, "before")
	st := <-held
	assertRefusedConn(t, dialListener(t, addr))
	stderr.wait(t)
	roundTrip(t, open, "until the re-check")
	assertRunning(t, done)

	// The hub's re-check closes the open tunnel.
	_ = st.CloseWithCode(conduit.CloseUnauthenticated, "authz_expired")
	_ = open.SetReadDeadline(time.Now().Add(10 * time.Second))
	if n, err := open.Read(make([]byte, 1)); n != 0 || err == nil {
		t.Fatalf("open connection read %d bytes, %v after the policy close", n, err)
	}
	if err := <-done; CodeOf(err) != ExitForbidden {
		t.Fatalf("Run = %v (exit %d), want %d", err, CodeOf(err), ExitForbidden)
	}
	if !strings.Contains(stderr.String(), "tunnel closed: tunnel closed by policy (authz_expired)") {
		t.Fatalf("stderr %q does not report the policy close", stderr.String())
	}
}

// A signal while the listener waits for its open tunnels ends them and the
// command, as a signal always does.
func TestListenerSignalAfterFinalRefusalEndsOpenTunnels(t *testing.T) {
	h := newFakeHub(t)
	h.set(func() {
		h.handle = firstOpensThenRefuses(respond(401, "unauthorized", ""))
	})
	stderr := newLineWatch(stopLine)
	cfg := testConfig(h, newFakeREST(), credSeq(oauth("tok1"), Credential{}))
	cfg.Stderr = stderr
	addr, cancel, done := listen(t, cfg, Request{Agent: "a1", Project: "proj", Port: 8080})
	defer cancel()

	open := dialListener(t, addr)
	roundTrip(t, open, "before")
	assertRefusedConn(t, dialListener(t, addr))
	stderr.wait(t)

	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run after cancel = %v, want context.Canceled", err)
	}
	_ = open.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := open.Read(make([]byte, 1)); err == nil {
		t.Fatal("open connection still up after the signal")
	}
}

// When the session is lost while the listener waits for its open tunnels
// after a credential refusal, those tunnels end with it and the command
// still exits 3: the credential is why it stopped accepting.
func TestListenerSessionLossAfterCredentialRefusalExits3(t *testing.T) {
	h := newFakeHub(t)
	h.set(func() {
		h.handle = firstOpensThenRefuses(respond(401, "unauthorized", ""))
	})
	stderr := newLineWatch(stopLine)
	cfg := testConfig(h, newFakeREST(), credSeq(oauth("tok1"), Credential{}))
	cfg.Stderr = stderr
	addr, cancel, done := listen(t, cfg, Request{Agent: "a1", Project: "proj", Port: 8080})
	defer cancel()

	open := dialListener(t, addr)
	roundTrip(t, open, "before")
	assertRefusedConn(t, dialListener(t, addr))
	stderr.wait(t)

	h.set(func() {
		for _, s := range h.live {
			_ = s.Close()
		}
	})
	err := <-done
	if CodeOf(err) != ExitCredential {
		t.Fatalf("Run = %v (exit %d), want %d", err, CodeOf(err), ExitCredential)
	}
	assertNamesLogin(t, err, h.srv.URL)
	_ = open.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := open.Read(make([]byte, 1)); err == nil {
		t.Fatal("open connection still up after the session was lost")
	}
}

// A stopped agent refuses each new connection with the agent_not_running
// line and the listener keeps listening, so it serves again once the
// agent is started.
func TestListenerStoppedAgentKeepsListening(t *testing.T) {
	h := newFakeHub(t)
	h.set(func() {
		h.handle = func(c rpcCall) *conduitv1.RpcResponse {
			if c.n <= 2 {
				return jsonResp(409, errBody("agent_not_running", "agent is not running", ""))
			}
			return openTunnelResp(c, echo)
		}
	})
	var stderr syncBuffer
	cfg := testConfig(h, newFakeREST(), credSeq(oauth("tok1")))
	cfg.Stderr = &stderr
	addr, cancel, done := listen(t, cfg, Request{Agent: "a1", Project: "proj", Port: 8080})
	defer cancel()

	for range 2 {
		assertRefusedConn(t, dialListener(t, addr))
	}
	const line = "tunnel request failed: the agent is not running (agent_not_running)\n"
	if n := strings.Count(stderr.String(), line); n != 2 {
		t.Fatalf("refusal line printed %d times, want once per connection: %q", n, stderr.String())
	}
	if strings.Contains(stderr.String(), stopLine) {
		t.Fatalf("listener stopped accepting for a stopped agent: %q", stderr.String())
	}
	// The agent was started again.
	roundTrip(t, dialListener(t, addr), "after start")
	assertRunning(t, done)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run after cancel = %v, want context.Canceled", err)
	}
}

// A stdio tunnel to a stopped agent exits 9 with the agent_not_running
// line.
func TestStdioStoppedAgentExits9(t *testing.T) {
	h := newFakeHub(t)
	h.set(func() { h.handle = respond(409, "agent_not_running", "") })
	err := Run(runCtx(t), testConfig(h, newFakeREST(), credSeq(oauth("tok1"))), stdioReq(strings.NewReader(""), io.Discard))
	if CodeOf(err) != ExitAgentNotRunning {
		t.Fatalf("Run = %v (exit %d), want %d", err, CodeOf(err), ExitAgentNotRunning)
	}
	if got, want := err.Error(), "the agent is not running (agent_not_running)"; got != want {
		t.Fatalf("error %q, want %q", got, want)
	}
}
