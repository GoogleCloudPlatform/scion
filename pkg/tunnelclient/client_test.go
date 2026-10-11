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

func TestExitCodeTableDial(t *testing.T) {
	cases := []struct {
		name   string
		status int
		creds  func() Credential
		want   int
	}{
		{name: "3 dial 401", creds: credSeq(oauth("unknown-token")), want: ExitCredential},
		{name: "5 dial 403", status: http.StatusForbidden, want: ExitForbidden},
		{name: "6 dial 503", status: http.StatusServiceUnavailable, want: ExitHubUnavailable},
		{name: "11 dial 404 (old hub)", status: http.StatusNotFound, want: ExitNoTunnelSupport},
		{name: "11 dial 200 without upgrade", status: http.StatusOK, want: ExitNoTunnelSupport},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newFakeHub(t)
			h.set(func() { h.dialStatus = tc.status })
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
		})
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

func TestDrainFreshDialFailsExits6(t *testing.T) {
	h := newFakeHub(t)
	h.set(func() {
		h.handle = func(c rpcCall) *conduitv1.RpcResponse {
			h.dialStatus = http.StatusNotFound // the fresh dial fails
			return bareDrain()
		}
	})
	err := Run(runCtx(t), testConfig(h, newFakeREST(), credSeq(oauth("tok1"))), stdioReq(strings.NewReader("x"), io.Discard))
	if CodeOf(err) != ExitHubUnavailable {
		t.Fatalf("exit %d (%v), want %d", CodeOf(err), err, ExitHubUnavailable)
	}
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
	h.set(func() { h.handle = respond(503, "unavailable", "grant_keys_unavailable") })
	err := Run(runCtx(t), testConfig(h, newFakeREST(), credSeq(oauth("tok1"))), stdioReq(strings.NewReader("x"), io.Discard))
	if CodeOf(err) != ExitHubUnavailable || !strings.Contains(err.Error(), "grant_keys_unavailable") {
		t.Fatalf("exit %d (%v), want %d naming the reason", CodeOf(err), err, ExitHubUnavailable)
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
		"different subject": oauth("tok3"),                                   // u2
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
