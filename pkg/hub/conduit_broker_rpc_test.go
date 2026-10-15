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
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// The control channel request/response tests run over both connections
// ControlChannelBrokerClient can tunnel through: the control channel
// (ControlChannelManager, or its mock) and a broker's conduit session
// (conduitBrokerTunnel). Each hubTunnelTransport wraps the same mock
// broker: over conduit, the mock answers on the broker's end of a real
// session pair, so every request and response crosses the session.

// ---------------------------------------------------------------- silent broker

// ---------------------------------------------------------------- adapter

// TestConduitBrokerTunnel_EnvelopeRoundTrip checks that a request envelope
// crosses the session to the broker unchanged (method, path, query, every
// header, binary and empty bodies) and the broker's response comes back
// unchanged.
func TestConduitBrokerTunnel_EnvelopeRoundTrip(t *testing.T) {
	binary := make([]byte, 256)
	for i := range binary {
		binary[i] = byte(i)
	}
	tests := []struct {
		name string
		req  *wsprotocol.RequestEnvelope
	}{
		{"get with query", wsprotocol.NewRequestEnvelope("rt-1", http.MethodGet, "/api/v1/agents/a1/logs", "tail=50&projectId=p%201", map[string]string{"X-Scion-Broker-ID": "broker-1"}, nil)},
		{"post json", wsprotocol.NewRequestEnvelope("rt-2", http.MethodPost, "/api/v1/agents", "", map[string]string{"Content-Type": "application/json"}, []byte(`{"name":"a"}`))},
		{"binary body", wsprotocol.NewRequestEnvelope("rt-3", http.MethodPut, "/api/v1/blob", "", map[string]string{"Content-Type": "application/octet-stream"}, binary)},
		{"multi-value header", wsprotocol.NewRequestEnvelope("rt-4", http.MethodDelete, "/api/v1/agents/a1", "deleteFiles=true&x=1&x=2", map[string]string{"Accept": "application/json, text/plain"}, nil)},
		{"empty body", wsprotocol.NewRequestEnvelope("rt-5", http.MethodPost, "/api/v1/agents/a1/stop", "", map[string]string{"X-Empty": ""}, []byte{})},
	}
	tun, echo, _ := newEchoTunnel(t)
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			wantHeaders := map[string]string{}
			for k, v := range tc.req.Headers {
				wantHeaders[k] = v
			}
			resp, err := tun.TunnelRequest(context.Background(), "broker-1", tc.req)
			if err != nil {
				t.Fatalf("TunnelRequest: %v", err)
			}
			seen := echo.requests()
			if len(seen) != i+1 {
				t.Fatalf("broker saw %d requests, want %d", len(seen), i+1)
			}
			got := seen[i]
			if got.RequestID != tc.req.RequestID || got.Method != tc.req.Method || got.Path != tc.req.Path || got.Query != tc.req.Query {
				t.Errorf("broker saw %s %s?%s (id %s), want %s %s?%s (id %s)", got.Method, got.Path, got.Query, got.RequestID, tc.req.Method, tc.req.Path, tc.req.Query, tc.req.RequestID)
			}
			// The hub adds the trace propagation headers, as on the
			// control channel; every caller header arrives unchanged.
			for k, v := range wantHeaders {
				if got.Headers[k] != v {
					t.Errorf("header %s = %q at the broker, want %q", k, got.Headers[k], v)
				}
			}
			if len(tc.req.Body) == 0 {
				if len(got.Body) != 0 || len(resp.Body) != 0 {
					t.Errorf("empty body arrived as %q, returned as %q", got.Body, resp.Body)
				}
			} else if !reflect.DeepEqual(got.Body, tc.req.Body) || !reflect.DeepEqual(resp.Body, tc.req.Body) {
				t.Errorf("body changed: broker saw %q, response %q, want %q", got.Body, resp.Body, tc.req.Body)
			}
			if resp.Type != wsprotocol.TypeResponse || resp.RequestID != tc.req.RequestID || resp.StatusCode != http.StatusOK {
				t.Errorf("response envelope = %+v", resp)
			}
			if !reflect.DeepEqual(resp.Headers, got.Headers) {
				t.Errorf("response headers %v, want %v", resp.Headers, got.Headers)
			}
		})
	}
}

// TestConduitBrokerTunnel_NotConnected checks that a broker without a live
// session is reported as not connected, before anything is sent.
func TestConduitBrokerTunnel_NotConnected(t *testing.T) {
	t.Run("no session", func(t *testing.T) {
		tun := newConduitBrokerTunnel(func(string) conduit.Session { return nil }, 0)
		if tun.IsConnected("broker-1") {
			t.Fatal("IsConnected = true without a session")
		}
		_, err := tun.TunnelRequest(context.Background(), "broker-1", wsprotocol.NewRequestEnvelope("nc-1", http.MethodGet, "/x", "", nil, nil))
		if !errors.Is(err, errStartBrokerNotConnected) {
			t.Fatalf("err = %v, want errStartBrokerNotConnected", err)
		}
	})
	t.Run("ended session", func(t *testing.T) {
		tun, echo, hub := newEchoTunnel(t)
		if !tun.IsConnected("broker-1") {
			t.Fatal("IsConnected = false with a live session")
		}
		_ = hub.Close()
		<-hub.Done()
		if tun.IsConnected("broker-1") {
			t.Fatal("IsConnected = true after the session ended")
		}
		_, err := tun.TunnelRequest(context.Background(), "broker-1", wsprotocol.NewRequestEnvelope("nc-2", http.MethodGet, "/x", "", nil, nil))
		if !errors.Is(err, errStartBrokerNotConnected) {
			t.Fatalf("err = %v, want errStartBrokerNotConnected", err)
		}
		if n := len(echo.requests()); n != 0 {
			t.Fatalf("broker saw %d requests, want 0", n)
		}
	})
}

// TestConduitBrokerTunnel_PayloadTooLarge checks that a request a session
// would answer with 413 without sending it (a body over the RPC limit, or
// a frame over the frame limit, here through a large query) never reaches
// the broker and is reported as *ErrPayloadTooLarge marked not sent; a 413
// the broker itself answers is passed through as a status.
func TestConduitBrokerTunnel_PayloadTooLarge(t *testing.T) {
	tun, echo, hub := newEchoTunnel(t)
	big := make([]byte, conduit.MaxRPCBody+1)

	// The session's own answer, before anything is sent.
	resp, err := hub.Call(context.Background(), &conduitv1.RpcRequest{Method: http.MethodPost, Path: "/api/v1/agents", Body: big})
	if err != nil || resp.GetStatus() != http.StatusRequestEntityTooLarge {
		t.Fatalf("Session.Call = %v, %v; want status 413", resp, err)
	}

	_, err = tun.TunnelRequest(context.Background(), "broker-1", wsprotocol.NewRequestEnvelope("big-1", http.MethodPost, "/api/v1/agents", "", nil, big))
	var tooLarge *ErrPayloadTooLarge
	if !errors.As(err, &tooLarge) {
		t.Fatalf("err = %v, want *ErrPayloadTooLarge", err)
	}
	if !errors.Is(err, errStartRequestNotSent) {
		t.Fatalf("err = %v, want it marked errStartRequestNotSent", err)
	}
	if tooLarge.Size != len(big) || tooLarge.Limit != conduit.MaxRPCBody {
		t.Errorf("ErrPayloadTooLarge = %+v", tooLarge)
	}
	if n := len(echo.requests()); n != 0 {
		t.Fatalf("broker saw %d requests, want 0", n)
	}

	// A small body with a query that takes the frame over its limit.
	longQuery := "q=" + strings.Repeat("x", conduit.DefaultMaxRPCFrame)
	_, err = tun.TunnelRequest(context.Background(), "broker-1", wsprotocol.NewRequestEnvelope("big-2", http.MethodGet, "/api/v1/agents", longQuery, nil, []byte("small")))
	tooLarge = nil
	if !errors.As(err, &tooLarge) || !errors.Is(err, errStartRequestNotSent) {
		t.Fatalf("frame over the limit: err = %v, want *ErrPayloadTooLarge marked errStartRequestNotSent", err)
	}
	if tooLarge.Limit != conduit.DefaultMaxRPCFrame || tooLarge.Size <= tooLarge.Limit {
		t.Errorf("frame over the limit: ErrPayloadTooLarge = %+v, want Size > Limit = %d", tooLarge, conduit.DefaultMaxRPCFrame)
	}
	if n := len(echo.requests()); n != 0 {
		t.Fatalf("broker saw %d requests, want 0", n)
	}

	// A broker's own 413 is a response, not a fallback.
	hub413, _ := newBrokerConduitPair(t, conduit.Config{RPCHandler: conduit.RPCHandlerFunc(func(context.Context, *conduitv1.RpcRequest) *conduitv1.RpcResponse {
		return &conduitv1.RpcResponse{Status: http.StatusRequestEntityTooLarge, Body: []byte(`{"error":"too large"}`)}
	})})
	tun413 := newConduitBrokerTunnel(func(string) conduit.Session { return hub413 }, 0)
	resp413, err := tun413.TunnelRequest(context.Background(), "broker-1", wsprotocol.NewRequestEnvelope("small-1", http.MethodPost, "/x", "", nil, []byte("small")))
	if err != nil {
		t.Fatalf("broker 413: err = %v, want a response", err)
	}
	if resp413.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("broker 413: status = %d", resp413.StatusCode)
	}
}

// TestConduitBrokerTunnel_DrainingNotSent checks that a request refused by
// a draining session is marked not sent. A request still in flight keeps
// the session draining (rather than closed) while the second one is made.
func TestConduitBrokerTunnel_DrainingNotSent(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	rpc := conduit.RPCHandlerFunc(func(context.Context, *conduitv1.RpcRequest) *conduitv1.RpcResponse {
		if calls.Add(1) == 1 {
			close(started)
			<-release
		}
		return &conduitv1.RpcResponse{Status: http.StatusOK}
	})
	hub, _ := newBrokerConduitPair(t, conduit.Config{RPCHandler: rpc})
	tun := newConduitBrokerTunnel(func(string) conduit.Session { return hub }, 0)

	firstDone := make(chan error, 1)
	go func() {
		_, err := tun.TunnelRequest(context.Background(), "broker-1", wsprotocol.NewRequestEnvelope("dr-0", http.MethodGet, "/x", "", nil, nil))
		firstDone <- err
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("first request never reached the broker")
	}
	if err := hub.GoAway(conduit.GoAwayOptions{DrainDeadline: time.Minute}); err != nil {
		t.Fatalf("GoAway: %v", err)
	}

	_, err := tun.TunnelRequest(context.Background(), "broker-1", wsprotocol.NewRequestEnvelope("dr-1", http.MethodPost, "/x", "", nil, nil))
	if !errors.Is(err, errStartRequestNotSent) || !errors.Is(err, conduit.ErrDraining) {
		t.Fatalf("err = %v, want ErrDraining marked errStartRequestNotSent", err)
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatalf("in-flight request during the drain: %v", err)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("broker handled %d requests, want 1", n)
	}
}

// TestConduitBrokerTunnel_LostResponseIsOutcomeUnknown checks that a
// session lost after the request reached the broker is reported with
// neither not-connected nor not-sent marker, and the request is sent once.
func TestConduitBrokerTunnel_LostResponseIsOutcomeUnknown(t *testing.T) {
	var calls atomic.Int32
	var drop func()
	ready := make(chan struct{})
	rpc := conduit.RPCHandlerFunc(func(ctx context.Context, _ *conduitv1.RpcRequest) *conduitv1.RpcResponse {
		calls.Add(1)
		<-ready
		drop()       // the broker goes away before answering
		<-ctx.Done() // the session has ended: no response is sent
		return nil
	})
	hub, d := newBrokerConduitPair(t, conduit.Config{RPCHandler: rpc})
	drop = d
	close(ready)
	tun := newConduitBrokerTunnel(func(string) conduit.Session { return hub }, 0)

	_, err := tun.TunnelRequest(context.Background(), "broker-1", wsprotocol.NewRequestEnvelope("lost-1", http.MethodPost, "/api/v1/agents/a1/exec", "", nil, []byte(`{"command":["ls"]}`)))
	if err == nil {
		t.Fatal("expected an error")
	}
	if errors.Is(err, errStartBrokerNotConnected) || errors.Is(err, errStartRequestNotSent) {
		t.Fatalf("a lost response must not be marked not sent: %v", err)
	}
	if isConfirmedStartNotActedOnError(err) {
		t.Fatalf("a lost response must not be confirmed not acted on: %v", err)
	}
	if !errors.Is(err, errBrokerConnectionLost) || !isUnknownDispatchOutcome(err) {
		t.Fatalf("a session lost after the send must be an unknown outcome (errBrokerConnectionLost): %v", err)
	}
	if !strings.HasPrefix(err.Error(), "conduit request failed: ") {
		t.Fatalf("the error keeps its text: %v", err)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("broker handled %d requests, want 1", n)
	}
}

// TestConduitBrokerTunnel_CallerCancelReachesBrokerHandler checks that
// cancelling the caller's context mid-call sends RpcCancel and the
// broker's handler sees its context cancelled.
func TestConduitBrokerTunnel_CallerCancelReachesBrokerHandler(t *testing.T) {
	started := make(chan struct{})
	handlerCancelled := make(chan error, 1)
	var cancelFrames atomic.Int32
	rpc := conduit.RPCHandlerFunc(func(ctx context.Context, _ *conduitv1.RpcRequest) *conduitv1.RpcResponse {
		close(started)
		<-ctx.Done()
		handlerCancelled <- ctx.Err()
		return nil
	})
	intercept := func(dir conduit.Direction, f *conduitv1.Frame) []*conduitv1.Frame {
		if dir == conduit.Inbound && f.GetRpcCancel().GetRequestId() == "cx-1" {
			cancelFrames.Add(1)
		}
		return []*conduitv1.Frame{f}
	}
	hub, _ := newBrokerConduitPair(t, conduit.Config{RPCHandler: rpc, Interceptor: intercept})
	tun := newConduitBrokerTunnel(func(string) conduit.Session { return hub }, 0)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := tun.TunnelRequest(ctx, "broker-1", wsprotocol.NewRequestEnvelope("cx-1", http.MethodPost, "/api/v1/agents", "", nil, nil))
		errCh <- err
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("broker handler never started")
	}
	cancel()
	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("TunnelRequest err = %v, want context.Canceled", err)
		}
		if errors.Is(err, errStartRequestNotSent) || errors.Is(err, errStartBrokerNotConnected) {
			t.Fatalf("a cancelled in-flight request must not be marked not sent: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("TunnelRequest did not return after cancel")
	}
	select {
	case err := <-handlerCancelled:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("broker handler ctx err = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("broker handler never saw its context cancelled")
	}
	if n := cancelFrames.Load(); n != 1 {
		t.Fatalf("broker received %d RpcCancel frames for the request, want 1", n)
	}
}

// TestConduitBrokerTunnel_NotConstructedInProduction checks where
// production code constructs the conduit broker tunnel and the broker's
// conduit RPC handler. The hub-side local tunnel is constructed only by
// its own constructor: broker requests reach conduit sessions through the
// router (routedBrokerTunnel). The broker's RPC handler is built only
// through its constructor chain, and (*HubConnection).startConduit may use
// only the guarded constructor NewConduitBrokerRPCHandler.
func TestConduitBrokerTunnel_NotConstructedInProduction(t *testing.T) {
	// guarded maps each directory to the guarded names (a constructor
	// called, or a type built with a composite literal) and to the free
	// functions and receiver methods allowed to use them.
	guarded := map[string]struct {
		names map[string]bool
		// allowed maps a free function ("Func") or receiver method
		// ("(*Recv).Method") to the guarded names it may use (nil: all).
		allowed map[string]map[string]bool
	}{
		".": {
			names:   map[string]bool{"newConduitBrokerTunnel": true, "conduitBrokerTunnel": true},
			allowed: map[string]map[string]bool{"newConduitBrokerTunnel": nil},
		},
		"../runtimebroker": {
			names: map[string]bool{
				"newConduitRPCHandler": true, "conduitRPCHandler": true,
				"newGuardedConduitRPCHandler": true, "NewConduitBrokerRPCHandler": true,
			},
			allowed: map[string]map[string]bool{
				"newConduitRPCHandler":          {"conduitRPCHandler": true},
				"newGuardedConduitRPCHandler":   {"newConduitRPCHandler": true},
				"NewConduitBrokerRPCHandler":    {"newGuardedConduitRPCHandler": true},
				"(*HubConnection).startConduit": {"NewConduitBrokerRPCHandler": true},
			},
		},
	}
	for dir, g := range guarded {
		// pkgmove:scan-covers pkg/hub/apierr
		// scanned through guardScanSubdirs; guards conduitBrokerTunnel construction (pkg/hub and its subpackages; ../runtimebroker is scanned on its own for the broker RPC handler).
		// pkgmove:scan-covers pkg/hub/internal/principal
		// scanned through guardScanSubdirs; guards conduitBrokerTunnel construction (pkg/hub and its subpackages; ../runtimebroker is scanned on its own for the broker RPC handler).
		// pkgmove:scan-covers pkg/hub/maintenance
		// scanned through guardScanSubdirs once it exists; guards conduitBrokerTunnel construction (pkg/hub and its subpackages; ../runtimebroker is scanned on its own for the broker RPC handler).
		var files []string
		var err error
		if dir == "." {
			files, err = globGuardScanDirs(dir, "*.go")
		} else {
			files, err = filepath.Glob(filepath.Join(dir, "*.go"))
		}
		if err != nil {
			t.Fatal(err)
		}
		fset := token.NewFileSet()
		for _, path := range files {
			if strings.HasSuffix(path, "_test.go") {
				continue
			}
			src, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			f, err := parser.ParseFile(fset, path, src, 0)
			if err != nil {
				t.Fatal(err)
			}
			for _, uses := range guardedUsesAllowing(f, g.names, g.allowed) {
				t.Errorf("%s: production code uses %s", fset.Position(uses.Pos()), uses.Name)
			}
		}
	}
}

// guardedUses returns the calls of, and composite literals of, the names
// in f outside the function named constructor.
func guardedUses(f *ast.File, names map[string]bool, constructor string) []*ast.Ident {
	return guardedUsesAllowing(f, names, map[string]map[string]bool{constructor: nil})
}

// guardFuncDeclKey names fd as allowed lists it: "Func" for a free function,
// "(*Recv).Method" or "(Recv).Method" for a method.
func guardFuncDeclKey(fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return fd.Name.Name
	}
	switch rt := fd.Recv.List[0].Type.(type) {
	case *ast.StarExpr:
		if id, ok := rt.X.(*ast.Ident); ok {
			return "(*" + id.Name + ")." + fd.Name.Name
		}
	case *ast.Ident:
		return "(" + rt.Name + ")." + fd.Name.Name
	}
	return "?." + fd.Name.Name
}

// guardedUsesAllowing returns the calls of, and composite literals of, the
// names in f, except where a function or method allowed lists (keys as
// guardFuncDeclKey gives them) may use that name (a nil set allows every
// name).
func guardedUsesAllowing(f *ast.File, names map[string]bool, allowed map[string]map[string]bool) []*ast.Ident {
	var out []*ast.Ident
	for _, decl := range f.Decls {
		var permitted map[string]bool
		all := false
		if fd, ok := decl.(*ast.FuncDecl); ok {
			if set, listed := allowed[guardFuncDeclKey(fd)]; listed {
				permitted, all = set, set == nil
			}
		}
		ast.Inspect(decl, func(n ast.Node) bool {
			var id *ast.Ident
			switch n := n.(type) {
			case *ast.CallExpr:
				id, _ = n.Fun.(*ast.Ident)
			case *ast.CompositeLit:
				id, _ = n.Type.(*ast.Ident)
			}
			if id != nil && names[id.Name] && !all && !permitted[id.Name] {
				out = append(out, id)
			}
			return true
		})
	}
	return out
}

// TestGuardedUses checks the guard itself: a use inside the constructor is
// allowed, and a use anywhere else in the same file is reported.
func TestGuardedUses(t *testing.T) {
	const src = `package hub
func newConduitBrokerTunnel() *conduitBrokerTunnel { return &conduitBrokerTunnel{} }
func (t *conduitBrokerTunnel) other() { _ = newConduitBrokerTunnel() }
var _ = conduitBrokerTunnel{}
`
	f, err := parser.ParseFile(token.NewFileSet(), "x.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	got := guardedUses(f, map[string]bool{"newConduitBrokerTunnel": true, "conduitBrokerTunnel": true}, "newConduitBrokerTunnel")
	if len(got) != 2 {
		t.Fatalf("guardedUses found %d uses, want 2 (the method call and the package-level literal)", len(got))
	}
}

// TestGuardedUsesAllowsListedMethod checks the receiver-method allowance:
// a use inside an allowed method is not reported, a use in another method
// of the same type is.
func TestGuardedUsesAllowsListedMethod(t *testing.T) {
	const src = `package runtimebroker
func (hc *HubConnection) startConduit() { _ = newConduitRPCHandler() }
func (hc *HubConnection) other() { _ = newConduitRPCHandler() }
`
	f, err := parser.ParseFile(token.NewFileSet(), "x.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	got := guardedUsesAllowing(f, map[string]bool{"newConduitRPCHandler": true}, map[string]map[string]bool{"(*HubConnection).startConduit": nil})
	if len(got) != 1 {
		t.Fatalf("guardedUsesAllowing found %d uses, want 1 (the call in other)", len(got))
	}
}

// TestGuardedUsesStartConduitOnlyGuarded checks the production allowance
// for startConduit: the guarded constructor passes, the unguarded one is
// reported.
func TestGuardedUsesStartConduitOnlyGuarded(t *testing.T) {
	names := map[string]bool{"newConduitRPCHandler": true, "newGuardedConduitRPCHandler": true, "NewConduitBrokerRPCHandler": true}
	allowed := map[string]map[string]bool{"(*HubConnection).startConduit": {"NewConduitBrokerRPCHandler": true}}
	for _, tc := range []struct {
		call string
		want int
	}{
		{"NewConduitBrokerRPCHandler", 0},
		{"newConduitRPCHandler", 1},
		{"newGuardedConduitRPCHandler", 1},
	} {
		src := "package runtimebroker\nfunc (hc *HubConnection) startConduit() { _ = " + tc.call + "() }\n"
		f, err := parser.ParseFile(token.NewFileSet(), "x.go", src, 0)
		if err != nil {
			t.Fatal(err)
		}
		if got := guardedUsesAllowing(f, names, allowed); len(got) != tc.want {
			t.Errorf("startConduit calling %s: %d uses reported, want %d", tc.call, len(got), tc.want)
		}
	}
}
