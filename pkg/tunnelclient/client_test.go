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
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// --- modes ---

func TestStdioCopiesBytes(t *testing.T) {
	h := newFakeHub(t)
	var out syncBuffer
	err := Run(runCtx(t), testConfig(h, newFakeREST(), credSeq(oauth("tok1"))), stdioReq(strings.NewReader("hello over stdio"), &out))
	if err != nil {
		t.Fatalf("Run: %v (exit %d)", err, CodeOf(err))
	}
	if got := out.String(); got != "hello over stdio" {
		t.Fatalf("stdout = %q", got)
	}
	h.set(func() {
		if len(h.calls) != 1 {
			t.Fatalf("calls = %d", len(h.calls))
		}
		b := h.calls[0].body
		if b.Agent != "id-a1" || b.Kind != "tcp" || b.Params["port"] != "5173" {
			t.Fatalf("request body = %+v", b)
		}
	})
}

func TestStdioSSHKind(t *testing.T) {
	h := newFakeHub(t)
	var out syncBuffer
	req := Request{Agent: "a1", Project: "proj", SSH: true, Stdio: true, Stdin: strings.NewReader("SSH-2.0-x\r\n"), Stdout: &out}
	if err := Run(runCtx(t), testConfig(h, newFakeREST(), credSeq(oauth("tok1"))), req); err != nil {
		t.Fatalf("Run: %v", err)
	}
	h.set(func() {
		b := h.calls[0].body
		if b.Kind != "ssh" || len(b.Params) != 0 {
			t.Fatalf("request body = %+v", b)
		}
	})
	if out.String() != "SSH-2.0-x\r\n" {
		t.Fatalf("stdout = %q", out.String())
	}
}

// listen runs a listener-mode tunnel and returns its address, a cancel
// for it and the channel its result arrives on.
func listen(t *testing.T, cfg Config, req Request) (string, context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(runCtx(t))
	ready := make(chan net.Addr, 1)
	req.Ready = func(a net.Addr) { ready <- a }
	done := make(chan error, 1)
	go func() { done <- Run(ctx, cfg, req) }()
	select {
	case a := <-ready:
		return a.String(), cancel, done
	case err := <-done:
		cancel()
		t.Fatalf("Run ended before listening: %v", err)
	}
	return "", cancel, done
}

// roundTrip writes msg on a new local connection and reads it back.
func roundTrip(t *testing.T, conn net.Conn, msg string) {
	t.Helper()
	if _, err := io.WriteString(conn, msg+"\n"); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil || line != msg+"\n" {
		t.Fatalf("read %q, %v", line, err)
	}
}

func TestListenerCopiesBytes(t *testing.T) {
	h := newFakeHub(t)
	addr, cancel, done := listen(t, testConfig(h, newFakeREST(), credSeq(oauth("tok1"))), Request{Agent: "a1", Project: "proj", Port: 8080})
	if !strings.HasPrefix(addr, "127.0.0.1:") {
		t.Fatalf("listener on %s", addr)
	}
	for _, msg := range []string{"first", "second"} {
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		roundTrip(t, conn, msg)
		_ = conn.Close()
	}
	dials, calls := h.counts()
	if dials != 1 || calls != 2 {
		t.Fatalf("dials %d calls %d, want one session carrying two tunnels", dials, calls)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run after cancel: %v", err)
	}
}

func TestListenerSessionLossEndsRun(t *testing.T) {
	h := newFakeHub(t)
	addr, cancel, done := listen(t, testConfig(h, newFakeREST(), credSeq(oauth("tok1"))), Request{Agent: "a1", Project: "proj", Port: 8080})
	defer cancel()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	roundTrip(t, conn, "up")
	h.set(func() {
		for _, s := range h.live {
			_ = s.Close()
		}
	})
	err = <-done
	if CodeOf(err) != ExitHubUnavailable {
		t.Fatalf("Run = %v (exit %d), want %d", err, CodeOf(err), ExitHubUnavailable)
	}
	// The local connection ends with the session.
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Fatal("local connection still open after the session was lost")
	}
}

// --- exit-code table ---

// respond answers every tunnel request with status, code and reason.
func respond(status int, code, reason string) func(rpcCall) *conduitv1.RpcResponse {
	return func(rpcCall) *conduitv1.RpcResponse {
		return jsonResp(status, errBody(code, "refused", reason))
	}
}

func TestExitCodeTableTunnelResponses(t *testing.T) {
	cases := []struct {
		name   string
		handle func(rpcCall) *conduitv1.RpcResponse
		noRPC  bool
		want   int
		calls  int
	}{
		{name: "0 clean close", handle: nil, want: ExitOK, calls: 1},
		{name: "1 internal_error, no retry", handle: respond(500, "internal_error", ""), want: ExitError, calls: 1},
		{name: "1 hub 400", handle: respond(400, "invalid_request", ""), want: ExitError, calls: 1},
		{name: "4 agent_not_found", handle: respond(404, "agent_not_found", ""), want: ExitAgentNotFound, calls: 1},
		{name: "5 forbidden", handle: respond(403, "forbidden", ""), want: ExitForbidden, calls: 1},
		{name: "8 rate_limited", handle: respond(429, "rate_limited", ""), want: ExitCapReached, calls: 1},
		{name: "9 agent_not_running", handle: respond(409, "agent_not_running", ""), want: ExitAgentNotRunning, calls: 1},
		{name: "10 runtime_broker_unavailable", handle: respond(503, "runtime_broker_unavailable", ""), want: ExitTargetUnavailable, calls: 1},
		{name: "10 agent_session_unavailable", handle: respond(503, "agent_session_unavailable", ""), want: ExitTargetUnavailable, calls: 1},
		{name: "11 404 without agent_not_found", handle: respond(404, "not_found", ""), want: ExitNoTunnelSupport, calls: 1},
		{name: "11 405", handle: respond(405, "invalid_request", ""), want: ExitNoTunnelSupport, calls: 1},
		{name: "11 unknown RPC (no handler)", noRPC: true, want: ExitNoTunnelSupport, calls: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newFakeHub(t)
			h.set(func() { h.handle, h.noRPC = tc.handle, tc.noRPC })
			err := Run(runCtx(t), testConfig(h, newFakeREST(), credSeq(oauth("tok1"))), stdioReq(strings.NewReader("x"), io.Discard))
			if got := CodeOf(err); got != tc.want {
				t.Fatalf("exit %d (%v), want %d", got, err, tc.want)
			}
			dials, calls := h.counts()
			if dials != 1 || calls != tc.calls {
				t.Fatalf("dials %d calls %d, want 1 and %d (no retry)", dials, calls, tc.calls)
			}
			if err != nil {
				assertOneLine(t, err.Error())
			}
		})
	}
}

// One row per line of the dial-status table.
func TestExitCodeTableDial(t *testing.T) {
	cases := []struct {
		name   string
		status int
		header map[string]string
		creds  func() Credential
		want   int
		has    []string
		hasNot []string
	}{
		{name: "404 gives 11 (old hub)", status: 404, want: ExitNoTunnelSupport},
		{name: "405 gives 11", status: 405, want: ExitNoTunnelSupport},
		{name: "501 gives 11", status: 501, want: ExitNoTunnelSupport},
		{name: "2xx without upgrade gives 11", status: 200, want: ExitNoTunnelSupport},
		{name: "401 gives 3", status: 401, want: ExitCredential, has: []string{"scion hub auth login"}},
		{name: "401 for an unknown credential gives 3", creds: credSeq(oauth("unknown-token")), want: ExitCredential},
		{name: "403 gives 5", status: 403, want: ExitForbidden},
		{name: "429 gives 8", status: 429, want: ExitCapReached},
		{name: "408 gives 6", status: 408, want: ExitHubUnavailable},
		{name: "500 gives 6", status: 500, want: ExitHubUnavailable},
		{name: "503 gives 6", status: 503, want: ExitHubUnavailable},
		{
			name: "3xx gives 1 naming only the redirect host", status: 302,
			header: map[string]string{"Location": "https://user:pw@Accounts.Google.com:443/o/oauth2?client=x#frag"},
			want:   ExitError,
			has:    []string{"hub redirected the dial to Accounts.Google.com; check proxy authentication"},
			hasNot: []string{"user", "pw", "oauth2", "client=x", "frag", "https"},
		},
		{name: "3xx without a host names another host", status: 307, header: map[string]string{"Location": "/login"}, want: ExitError, has: []string{"redirected the dial to another host"}},
		{name: "3xx with no Location names another host", status: 303, want: ExitError, has: []string{"redirected the dial to another host"}},
		{name: "400 gives 1", status: 400, want: ExitError},
		{name: "418 gives 1", status: 418, want: ExitError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newFakeHub(t)
			h.set(func() { h.dialStatus, h.dialHeader = tc.status, tc.header })
			creds := tc.creds
			rest := newFakeREST()
			if creds == nil {
				creds = credSeq(oauth("tok1"))
			} else {
				rest.users["unknown-token"] = User{ID: "u1"}
			}
			err := Run(runCtx(t), testConfig(h, rest, creds), stdioReq(strings.NewReader("x"), io.Discard))
			if got := CodeOf(err); got != tc.want {
				t.Fatalf("exit %d (%v), want %d", got, err, tc.want)
			}
			assertOneLine(t, err.Error())
			for _, s := range tc.has {
				if !strings.Contains(err.Error(), s) {
					t.Errorf("error %q lacks %q", err, s)
				}
			}
			for _, s := range tc.hasNot {
				if strings.Contains(err.Error(), s) {
					t.Errorf("error %q contains %q", err, s)
				}
			}
		})
	}
}

func TestRedirectHost(t *testing.T) {
	cases := map[string]string{
		"https://accounts.google.com/x":    "accounts.google.com",
		"http://u:p@proxy.example:8080/?q": "proxy.example",
		"//cdn.example.com/a":              "cdn.example.com",
		"/relative":                        "another host",
		"":                                 "another host",
		"https://bad\x7fhost.example/":     "another host",
		"https://[::1]:9/":                 "::1",
	}
	for loc, want := range cases {
		h := http.Header{}
		if loc != "" {
			h.Set("Location", loc)
		}
		if got := redirectHost(h); got != want {
			t.Errorf("redirectHost(%q) = %q, want %q", loc, got, want)
		}
	}
}

func TestExitCodeHubUnreachable(t *testing.T) {
	h := newFakeHub(t)
	cfg := testConfig(h, newFakeREST(), credSeq(oauth("tok1")))
	h.srv.Close()
	err := Run(runCtx(t), cfg, stdioReq(strings.NewReader("x"), io.Discard))
	if CodeOf(err) != ExitHubUnavailable {
		t.Fatalf("exit %d (%v), want %d", CodeOf(err), err, ExitHubUnavailable)
	}
}

func TestExitCodeHelloRefused(t *testing.T) {
	h := newFakeHub(t)
	rest := newFakeREST()
	// The REST side reports a different user than the conduit admits,
	// so the Hello principal does not match: 4403 at the handshake.
	rest.users["tok1"] = User{ID: "someone-else"}
	err := Run(runCtx(t), testConfig(h, rest, credSeq(oauth("tok1"))), stdioReq(strings.NewReader("x"), io.Discard))
	if CodeOf(err) != ExitForbidden {
		t.Fatalf("exit %d (%v), want %d", CodeOf(err), err, ExitForbidden)
	}
}

func TestExitCodeStreamCloses(t *testing.T) {
	cases := []struct {
		name string
		code uint32
		want int
	}{
		{"7 authz_expired", conduit.CloseUnauthenticated, ExitPolicyClosed},
		{"5 forbidden", conduit.CloseForbidden, ExitForbidden},
		{"4 target_not_found", closeTargetNotFound, ExitAgentNotFound},
		{"6 relay_restart", conduit.CloseRelayRestart, ExitHubUnavailable},
		{"10 relay_timeout", conduit.CloseRelayTimeout, ExitTargetUnavailable},
		{"0 normal", conduit.CloseNormal, ExitOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newFakeHub(t)
			h.set(func() {
				h.handle = func(c rpcCall) *conduitv1.RpcResponse {
					return openTunnelResp(c, func(st conduit.Stream) { _ = st.CloseWithCode(tc.code, "closed") })
				}
			})
			// stdin stays open: only the stream's close ends the run.
			pr, pw := io.Pipe()
			defer func() { _ = pw.Close() }()
			err := Run(runCtx(t), testConfig(h, newFakeREST(), credSeq(oauth("tok1"))), stdioReq(pr, io.Discard))
			if got := CodeOf(err); got != tc.want {
				t.Fatalf("exit %d (%v), want %d", got, err, tc.want)
			}
		})
	}
}

func TestExitCodeREST(t *testing.T) {
	cases := []struct {
		name       string
		whoamiErr  error
		resolveErr error
		want       int
	}{
		{name: "3 whoami 401", whoamiErr: &HTTPError{Status: 401}, want: ExitCredential},
		{name: "6 whoami unreachable", whoamiErr: errors.New("connection refused"), want: ExitHubUnavailable},
		{name: "6 whoami 502", whoamiErr: &HTTPError{Status: 502}, want: ExitHubUnavailable},
		{name: "4 unknown agent or project", resolveErr: &HTTPError{Status: 404, Code: "not_found"}, want: ExitAgentNotFound},
		{name: "5 agent lookup forbidden", resolveErr: &HTTPError{Status: 403, Code: "forbidden"}, want: ExitForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newFakeHub(t)
			rest := newFakeREST()
			rest.whoamiErr, rest.resolveErr = tc.whoamiErr, tc.resolveErr
			err := Run(runCtx(t), testConfig(h, rest, credSeq(oauth("tok1"))), stdioReq(strings.NewReader("x"), io.Discard))
			if got := CodeOf(err); got != tc.want {
				t.Fatalf("exit %d (%v), want %d", got, err, tc.want)
			}
			if dials, _ := h.counts(); dials != 0 {
				t.Fatalf("dialed %d sessions after a failed lookup", dials)
			}
			assertOneLine(t, err.Error())
		})
	}
}

func TestExitCodeUsage(t *testing.T) {
	base := Request{Agent: "a1", Project: "proj", Port: 22}
	cases := map[string]func(r *Request){
		"no agent":            func(r *Request) { r.Agent = "" },
		"bad agent form":      func(r *Request) { r.Agent = "Bad Agent" },
		"no project":          func(r *Request) { r.Project = "" },
		"bad project form":    func(r *Request) { r.Project = "../x" },
		"no port":             func(r *Request) { r.Port = 0 },
		"port out of range":   func(r *Request) { r.Port = 70000 },
		"ssh with port":       func(r *Request) { r.SSH = true },
		"local port on stdio": func(r *Request) { r.Stdio, r.LocalPort = true, 9000 },
		"local port range":    func(r *Request) { r.LocalPort = -1 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			r := base
			mutate(&r)
			err := Run(context.Background(), Config{ResolveCredential: func() Credential { t.Fatal("credential resolved for a usage error"); return Credential{} }}, r)
			if CodeOf(err) != ExitUsage {
				t.Fatalf("exit %d (%v), want %d", CodeOf(err), err, ExitUsage)
			}
		})
	}
}

func TestSignalExit(t *testing.T) {
	if SignalExit(os.Interrupt) != ExitInterrupted || SignalExit(syscall.SIGTERM) != ExitTerminated || SignalExit(syscall.SIGHUP) != ExitError {
		t.Fatal("signal exit codes")
	}
	if CodeOf(nil) != ExitOK || CodeOf(errors.New("x")) != ExitError || CodeOf(Errorf(ExitCapReached, "x")) != ExitCapReached {
		t.Fatal("CodeOf")
	}
}

// --- drain (bare 503) ---

func TestDrainRetriesOnFreshSession(t *testing.T) {
	h := newFakeHub(t)
	h.set(func() {
		h.handle = func(c rpcCall) *conduitv1.RpcResponse {
			if c.session == 1 {
				return bareDrain()
			}
			return openTunnelResp(c, echo)
		}
	})
	var out syncBuffer
	err := Run(runCtx(t), testConfig(h, newFakeREST(), credSeq(oauth("tok1"))), stdioReq(strings.NewReader("drained"), &out))
	if err != nil {
		t.Fatalf("Run: %v (exit %d), want a retry on a fresh session, not exit 10", err, CodeOf(err))
	}
	dials, calls := h.counts()
	if dials != 2 || calls != 2 || out.String() != "drained" {
		t.Fatalf("dials %d calls %d out %q", dials, calls, out.String())
	}
}

func TestDrainRacedGoAwayRetriesOnFreshSession(t *testing.T) {
	h := newFakeHub(t)
	h.set(func() {
		h.handle = func(c rpcCall) *conduitv1.RpcResponse {
			if c.session == 1 {
				_ = c.ls.GoAway(conduit.GoAwayOptions{Reason: "draining"})
				return bareDrain()
			}
			return openTunnelResp(c, echo)
		}
	})
	err := Run(runCtx(t), testConfig(h, newFakeREST(), credSeq(oauth("tok1"))), stdioReq(strings.NewReader("x"), io.Discard))
	if err != nil {
		t.Fatalf("Run: %v (exit %d)", err, CodeOf(err))
	}
	if dials, _ := h.counts(); dials != 2 {
		t.Fatalf("dials %d, want 2", dials)
	}
}

// A fresh dial after a drain maps through the dial-status table.
func TestDrainFreshDialFailureFollowsDialTable(t *testing.T) {
	cases := map[int]int{
		http.StatusServiceUnavailable: ExitHubUnavailable,
		http.StatusNotFound:           ExitNoTunnelSupport,
		http.StatusUnauthorized:       ExitCredential,
	}
	for status, want := range cases {
		t.Run(http.StatusText(status), func(t *testing.T) {
			h := newFakeHub(t)
			h.set(func() {
				h.handle = func(c rpcCall) *conduitv1.RpcResponse {
					h.dialStatus = status // the fresh dial is refused
					return bareDrain()
				}
			})
			err := Run(runCtx(t), testConfig(h, newFakeREST(), credSeq(oauth("tok1"))), stdioReq(strings.NewReader("x"), io.Discard))
			if CodeOf(err) != want {
				t.Fatalf("exit %d (%v), want %d", CodeOf(err), err, want)
			}
		})
	}
}

// A drain, then a 401 on the fresh session: the 401 still gets its own
// re-resolve and retry.
func TestDrainThen401ThenSuccess(t *testing.T) {
	h := newFakeHub(t)
	h.set(func() {
		h.handle = func(c rpcCall) *conduitv1.RpcResponse {
			switch c.session {
			case 1:
				return bareDrain()
			case 2:
				return jsonResp(401, errBody("unauthorized", "credential refused", ""))
			}
			return openTunnelResp(c, echo)
		}
	})
	var out syncBuffer
	err := Run(runCtx(t), testConfig(h, newFakeREST(), credSeq(oauth("tok1"), oauth("tok1"), oauth("tok2"))), stdioReq(strings.NewReader("third time"), &out))
	if err != nil {
		t.Fatalf("Run: %v (exit %d)", err, CodeOf(err))
	}
	if dials, calls := h.counts(); dials != 3 || calls != 3 || out.String() != "third time" {
		t.Fatalf("dials %d calls %d out %q, want two fresh sessions", dials, calls, out.String())
	}
	if got := h.bearerList(); got[2] != "tok2" {
		t.Fatalf("bearers %v, want the re-resolved credential on the last dial", got)
	}
}

// The credential is re-resolved before a drain's fresh dial too.
func TestDrainWithExpiredCredentialExits3(t *testing.T) {
	h := newFakeHub(t)
	h.set(func() { h.handle = func(rpcCall) *conduitv1.RpcResponse { return bareDrain() } })
	err := Run(runCtx(t), testConfig(h, newFakeREST(), credSeq(oauth("tok1"), oauth("tok-expired"))), stdioReq(strings.NewReader("x"), io.Discard))
	if CodeOf(err) != ExitCredential {
		t.Fatalf("exit %d (%v), want %d", CodeOf(err), err, ExitCredential)
	}
	assertNamesLogin(t, err, h.srv.URL)
	if dials, _ := h.counts(); dials != 1 {
		t.Fatalf("dials %d, want no fresh session", dials)
	}
}

func TestDrainChangedIdentityExits3(t *testing.T) {
	h := newFakeHub(t)
	h.set(func() { h.handle = func(rpcCall) *conduitv1.RpcResponse { return bareDrain() } })
	err := Run(runCtx(t), testConfig(h, newFakeREST(), credSeq(oauth("tok1"), oauth("tok3"))), stdioReq(strings.NewReader("x"), io.Discard))
	if CodeOf(err) != ExitCredential || !strings.Contains(err.Error(), "different identity") {
		t.Fatalf("exit %d (%v), want %d refusing the changed identity", CodeOf(err), err, ExitCredential)
	}
	if dials, _ := h.counts(); dials != 1 {
		t.Fatalf("dials %d: the changed identity must never dial", dials)
	}
}

// Two tunnel requests in flight on one session, one refused with 401 and
// one drained, each replace the session on their own.
func TestListenerConcurrent401AndDrain(t *testing.T) {
	h := newFakeHub(t)
	var arrived sync.WaitGroup
	arrived.Add(2)
	h.set(func() {
		h.handle = func(c rpcCall) *conduitv1.RpcResponse {
			if c.session == 1 {
				arrived.Done()
				arrived.Wait() // both requests are in flight
				if c.n == 1 {
					return jsonResp(401, errBody("unauthorized", "credential refused", ""))
				}
				return bareDrain()
			}
			return openTunnelResp(c, echo)
		}
	})
	cfg := testConfig(h, newFakeREST(), credSeq(oauth("tok1")))
	addr, cancel, done := listen(t, cfg, Request{Agent: "a1", Project: "proj", Port: 8080})
	defer cancel()
	results := make(chan error, 2)
	for _, msg := range []string{"one", "two"} {
		go func() {
			conn, err := net.Dial("tcp", addr)
			if err != nil {
				results <- err
				return
			}
			defer func() { _ = conn.Close() }()
			if _, err := io.WriteString(conn, msg+"\n"); err != nil {
				results <- err
				return
			}
			line, err := bufio.NewReader(conn).ReadString('\n')
			if err == nil && line != msg+"\n" {
				err = errors.New("read " + line)
			}
			results <- err
		}()
	}
	for i := 0; i < 2; i++ {
		if err := <-results; err != nil {
			t.Fatalf("tunnel %d: %v", i, err)
		}
	}
	if dials, calls := h.counts(); dials != 3 || calls != 4 {
		t.Fatalf("dials %d calls %d, want one fresh session per refused request", dials, calls)
	}
	cancel()
	<-done
}

func TestDrainTwiceExits6(t *testing.T) {
	h := newFakeHub(t)
	h.set(func() { h.handle = func(rpcCall) *conduitv1.RpcResponse { return bareDrain() } })
	err := Run(runCtx(t), testConfig(h, newFakeREST(), credSeq(oauth("tok1"))), stdioReq(strings.NewReader("x"), io.Discard))
	if CodeOf(err) != ExitHubUnavailable {
		t.Fatalf("exit %d (%v), want %d", CodeOf(err), err, ExitHubUnavailable)
	}
	if dials, calls := h.counts(); dials != 2 || calls != 2 {
		t.Fatalf("dials %d calls %d, want exactly one retry", dials, calls)
	}
}

// --- transient (503 unavailable) ---

func TestUnavailableRetriesOnceOnSameSession(t *testing.T) {
	h := newFakeHub(t)
	h.set(func() {
		h.handle = func(c rpcCall) *conduitv1.RpcResponse {
			if c.n == 1 {
				return jsonResp(503, errBody("unavailable", "try later", "registry_unavailable"))
			}
			return openTunnelResp(c, echo)
		}
	})
	var slept []time.Duration
	cfg := testConfig(h, newFakeREST(), credSeq(oauth("tok1")))
	cfg.Sleep = func(_ context.Context, d time.Duration) error { slept = append(slept, d); return nil }
	if err := Run(runCtx(t), cfg, stdioReq(strings.NewReader("x"), io.Discard)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	dials, calls := h.counts()
	if dials != 1 || calls != 2 {
		t.Fatalf("dials %d calls %d, want two requests on one session", dials, calls)
	}
	if len(slept) != 1 || slept[0] != 1500*time.Millisecond {
		t.Fatalf("slept %v, want one jittered delay", slept)
	}
}

func TestUnavailableTwiceExits6WithReason(t *testing.T) {
	h := newFakeHub(t)
	h.set(func() {
		h.handle = func(c rpcCall) *conduitv1.RpcResponse {
			if c.n == 1 {
				return jsonResp(503, errBody("unavailable", "try later", "registry_unavailable"))
			}
			return jsonResp(503, errBody("unavailable", "try later", "grant_keys_unavailable"))
		}
	})
	err := Run(runCtx(t), testConfig(h, newFakeREST(), credSeq(oauth("tok1"))), stdioReq(strings.NewReader("x"), io.Discard))
	if CodeOf(err) != ExitHubUnavailable || !strings.Contains(err.Error(), "grant_keys_unavailable") {
		t.Fatalf("exit %d (%v), want %d naming the latest reason", CodeOf(err), err, ExitHubUnavailable)
	}
	if strings.Contains(err.Error(), "registry_unavailable") {
		t.Fatalf("error %q names the first reason, want only the latest", err)
	}
	if dials, calls := h.counts(); dials != 1 || calls != 2 {
		t.Fatalf("dials %d calls %d, want one retry on the same session", dials, calls)
	}
	assertOneLine(t, err.Error())
}

func TestDefaultJitterIsOneToTwoSeconds(t *testing.T) {
	var c Config
	for i := 0; i < 200; i++ {
		if d := c.jitter(); d < time.Second || d >= 2*time.Second {
			t.Fatalf("jitter %v outside [1s, 2s)", d)
		}
	}
}

// --- rule 6 (criterion 25(c)) ---

func TestRule6RefusedTunnelRetriesOnFreshSessionOpenTunnelsContinue(t *testing.T) {
	h := newFakeHub(t)
	h.set(func() {
		h.handle = func(c rpcCall) *conduitv1.RpcResponse {
			if c.session == 1 && c.n > 1 {
				// The admitting credential expired: new tunnels on
				// session 1 are refused, open ones continue.
				return jsonResp(401, errBody("unauthorized", "credential refused", ""))
			}
			return openTunnelResp(c, echo)
		}
	})
	cfg := testConfig(h, newFakeREST(), credSeq(oauth("tok1"), oauth("tok2")))
	addr, cancel, done := listen(t, cfg, Request{Agent: "a1", Project: "proj", Port: 8080})
	defer cancel()
	open, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = open.Close() }()
	roundTrip(t, open, "before")

	second, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Close() }()
	roundTrip(t, second, "on the fresh session")
	// The tunnel opened on the first session still works.
	roundTrip(t, open, "after")

	if got := h.bearerList(); len(got) != 2 || got[0] != "tok1" || got[1] != "tok2" {
		t.Fatalf("dial bearers %v, want tok1 then the re-resolved tok2", got)
	}
	cancel()
	<-done
}

func TestRule6SecondRefusalExits3(t *testing.T) {
	h := newFakeHub(t)
	h.set(func() { h.handle = respond(401, "unauthorized", "") })
	err := Run(runCtx(t), testConfig(h, newFakeREST(), credSeq(oauth("tok1"), oauth("tok2"))), stdioReq(strings.NewReader("x"), io.Discard))
	if CodeOf(err) != ExitCredential {
		t.Fatalf("exit %d (%v), want %d", CodeOf(err), err, ExitCredential)
	}
	if dials, calls := h.counts(); dials != 2 || calls != 2 {
		t.Fatalf("dials %d calls %d, want exactly one retry on a fresh session", dials, calls)
	}
	assertNamesLogin(t, err, h.srv.URL)
}

func TestRule6NoCredentialOnReresolveExits3(t *testing.T) {
	h := newFakeHub(t)
	h.set(func() { h.handle = respond(401, "unauthorized", "") })
	err := Run(runCtx(t), testConfig(h, newFakeREST(), credSeq(oauth("tok1"), Credential{})), stdioReq(strings.NewReader("x"), io.Discard))
	if CodeOf(err) != ExitCredential {
		t.Fatalf("exit %d (%v), want %d", CodeOf(err), err, ExitCredential)
	}
	if dials, _ := h.counts(); dials != 1 {
		t.Fatalf("dials %d, want no fresh session", dials)
	}
	assertNamesLogin(t, err, h.srv.URL)
}

func TestRule6RefusedReresolvedCredentialExits3(t *testing.T) {
	h := newFakeHub(t)
	h.set(func() { h.handle = respond(401, "unauthorized", "") })
	// tok-expired is not known to the REST side: whoami answers 401.
	err := Run(runCtx(t), testConfig(h, newFakeREST(), credSeq(oauth("tok1"), oauth("tok-expired"))), stdioReq(strings.NewReader("x"), io.Discard))
	if CodeOf(err) != ExitCredential {
		t.Fatalf("exit %d (%v), want %d", CodeOf(err), err, ExitCredential)
	}
	if dials, _ := h.counts(); dials != 1 {
		t.Fatalf("dials %d, want no fresh session", dials)
	}
}

func TestRule6ChangedIdentityRefused(t *testing.T) {
	cases := map[string]Credential{
		"different subject": oauth("tok3"),                          // u2
		"different kind":    {Token: "tok2", Kind: CredentialToken}, // u1, but another credential kind
	}
	for name, next := range cases {
		t.Run(name, func(t *testing.T) {
			h := newFakeHub(t)
			h.set(func() { h.handle = respond(401, "unauthorized", "") })
			err := Run(runCtx(t), testConfig(h, newFakeREST(), credSeq(oauth("tok1"), next)), stdioReq(strings.NewReader("x"), io.Discard))
			if CodeOf(err) != ExitCredential || !strings.Contains(err.Error(), "different identity") {
				t.Fatalf("exit %d (%v), want %d refusing the changed identity", CodeOf(err), err, ExitCredential)
			}
			if got := h.bearerList(); len(got) != 1 {
				t.Fatalf("dial bearers %v: the changed identity must never dial", got)
			}
		})
	}
}

// --- criterion 25(e) ---

// failingReader fails the test if anything reads it.
type failingReader struct {
	t    *testing.T
	read bool
	mu   sync.Mutex
}

func (f *failingReader) Read([]byte) (int, error) {
	f.mu.Lock()
	f.read = true
	f.mu.Unlock()
	return 0, io.EOF
}

func TestNoCredentialStdioExits3WithoutReadingStdin(t *testing.T) {
	h := newFakeHub(t)
	rest := newFakeREST()
	stdin := &failingReader{t: t}
	err := Run(runCtx(t), testConfig(h, rest, credSeq(Credential{})), stdioReq(stdin, io.Discard))
	if CodeOf(err) != ExitCredential {
		t.Fatalf("exit %d (%v), want %d", CodeOf(err), err, ExitCredential)
	}
	assertOneLine(t, err.Error())
	assertNamesLogin(t, err, h.srv.URL)
	if stdin.read {
		t.Fatal("stdin was read")
	}
	if dials, _ := h.counts(); dials != 0 || rest.whoamis != 0 {
		t.Fatalf("dials %d whoamis %d, want no hub contact", dials, rest.whoamis)
	}
}

// --- helpers ---

func assertOneLine(t *testing.T, msg string) {
	t.Helper()
	if msg == "" || strings.ContainsAny(msg, "\n\r\x1b") {
		t.Fatalf("error %q is not one plain line", msg)
	}
}

func assertNamesLogin(t *testing.T, err error, endpoint string) {
	t.Helper()
	if !strings.Contains(err.Error(), endpoint) || !strings.Contains(err.Error(), "scion hub auth login") {
		t.Fatalf("error %q does not name the endpoint and the login command", err)
	}
}

// A replaced session is closed when its last tunnel ends; the current one
// stays up.
func TestReplacedSessionClosesWithItsLastTunnel(t *testing.T) {
	h := newFakeHub(t)
	h.set(func() {
		h.handle = func(c rpcCall) *conduitv1.RpcResponse {
			if c.session == 1 && c.n > 1 {
				return jsonResp(401, errBody("unauthorized", "credential refused", ""))
			}
			return openTunnelResp(c, echo)
		}
	})
	addr, cancel, done := listen(t, testConfig(h, newFakeREST(), credSeq(oauth("tok1"), oauth("tok2"))), Request{Agent: "a1", Project: "proj", Port: 8080})
	defer cancel()
	first, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	roundTrip(t, first, "on session 1")
	second, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Close() }()
	roundTrip(t, second, "on session 2")

	var s1, s2 conduit.LocalSession
	h.set(func() { s1, s2 = h.live[0], h.live[1] })
	select {
	case <-s1.Done():
		t.Fatal("session 1 closed while its tunnel was open")
	default:
	}
	_ = first.Close() // the last tunnel on session 1 ends
	select {
	case <-s1.Done():
	case <-runCtx(t).Done():
		t.Fatal("session 1 still open after its last tunnel ended")
	}
	select {
	case <-s2.Done():
		t.Fatal("the current session was closed")
	default:
	}
	roundTrip(t, second, "session 2 still works")
	cancel()
	<-done
}
