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
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	core "github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/control"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// controlAgent runs an Agent whose session serves h through
// control.RPCHandler and returns the hub's side of the session with the
// agent's Hello.
func controlAgent(t *testing.T, h http.Handler, routes []string) (core.LocalSession, *conduitv1.Hello) {
	t.Helper()
	hub := newFakeHub(t)
	startAgent(t, hub, func(o *Options) {
		o.Session.RPCHandler = control.RPCHandler(h)
		o.RPCRoutes = routes
	})
	hello := hub.nextHello(t)
	return hub.nextSession(t), hello
}

// TestControlHandlerServedOverConduitAndHTTP: one handler serves conduit
// RPCs and router-fronted HTTP. The same handler value, served from a
// plain net/http server and as the agent session's RPC handler, gives
// identical results for every request, and the agent advertises exactly
// the implemented routes.
func TestControlHandlerServedOverConduitAndHTTP(t *testing.T) {
	var kicks atomic.Int64
	ctl := control.New(control.Options{KickTokenRefresh: func() { kicks.Add(1) }})
	srv := httptest.NewServer(ctl)
	t.Cleanup(srv.Close)
	hubSide, hello := controlAgent(t, ctl, ctl.Routes())

	if got := hello.GetCapabilities().GetRpc(); !slices.Equal(got, []string{control.RouteRotateToken}) {
		t.Fatalf("Hello rpc = %v, want [%s]", got, control.RouteRotateToken)
	}

	tests := []struct {
		method, path string
		want         int
		kick         bool
	}{
		{"POST", "/v1/control/rotate-token", http.StatusAccepted, true},
		{"GET", "/v1/control/rotate-token", http.StatusMethodNotAllowed, false},
		{"PUT", "/v1/control/rotate-token", http.StatusMethodNotAllowed, false},
		{"DELETE", "/v1/control/rotate-token", http.StatusMethodNotAllowed, false},
		{"POST", "/v1/control/wake", http.StatusNotFound, false},
		{"POST", "/v1/control/reload-config", http.StatusNotFound, false},
		{"POST", "/v1/control/unknown", http.StatusNotFound, false},
		{"GET", "/v1/control/wake", http.StatusNotFound, false},
		{"POST", "/v1/control/", http.StatusNotFound, false},
		{"POST", "/v1/control/rotate-token/", http.StatusNotFound, false},
		{"POST", "/scion/v1/exec", http.StatusNotFound, false},
		{"POST", "/scion/v1/bootstrap", http.StatusNotFound, false},
		{"GET", "/healthz", http.StatusNotFound, false},
		{"POST", "/", http.StatusNotFound, false},
	}
	for _, tt := range tests {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			// Plain net/http.
			before := kicks.Load()
			req, err := http.NewRequest(tt.method, srv.URL+tt.path, nil)
			if err != nil {
				t.Fatal(err)
			}
			hr, err := srv.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			httpBody, _ := io.ReadAll(hr.Body)
			_ = hr.Body.Close()
			httpKicked := kicks.Load() != before

			// Conduit RpcRequest from the hub side of the agent session.
			before = kicks.Load()
			resp, err := hubSide.Call(context.Background(), &conduitv1.RpcRequest{Method: tt.method, Path: tt.path})
			if err != nil {
				t.Fatal(err)
			}
			rpcKicked := kicks.Load() != before

			if hr.StatusCode != tt.want || int(resp.GetStatus()) != tt.want {
				t.Fatalf("status: http %d, conduit %d; want %d", hr.StatusCode, resp.GetStatus(), tt.want)
			}
			if httpKicked != tt.kick || rpcKicked != tt.kick {
				t.Fatalf("kicked: http %v, conduit %v; want %v", httpKicked, rpcKicked, tt.kick)
			}
			if string(httpBody) != string(resp.GetBody()) {
				t.Fatalf("body: http %q, conduit %q", httpBody, resp.GetBody())
			}
			if tt.want == http.StatusAccepted && len(resp.GetBody()) != 0 {
				t.Fatalf("202 body = %q, want empty", resp.GetBody())
			}
			if tt.want == http.StatusMethodNotAllowed &&
				(hr.Header.Get("Allow") != "POST" || resp.GetHeaders()["Allow"] != "POST") {
				t.Fatalf("Allow: http %q, conduit %q; want POST", hr.Header.Get("Allow"), resp.GetHeaders()["Allow"])
			}
		})
	}
}

// TestControlRPCAllowList: over conduit RPC only canonical /v1/control/*
// paths reach the handler. Everything else, including dot segments and
// percent-encoded forms that would decode into another path, is answered
// 404 without calling the handler.
func TestControlRPCAllowList(t *testing.T) {
	var kicks, reached atomic.Int64
	ctl := control.New(control.Options{KickTokenRefresh: func() { kicks.Add(1) }})
	counting := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached.Add(1)
		ctl.ServeHTTP(w, r)
	})
	hubSide, _ := controlAgent(t, counting, ctl.Routes())

	tests := []struct {
		path  string
		want  int32
		reach bool // the handler is called
	}{
		{"/v1/control/rotate-token", 202, true},
		{"/v1/control/wake", 404, true},
		{"/v1/control/reload-config", 404, true},
		{"/v1/control/unknown", 404, true},
		{"/scion/v1/exec", 404, false},
		{"/scion/v1/bootstrap", 404, false},
		{"/healthz", 404, false},
		{"/", 404, false},
		{"", 404, false},
		{"v1/control/rotate-token", 404, false},
		{"/v1/control", 404, false},
		{"/v1/control/../scion/v1/exec", 404, false},
		{"/v1/control/./rotate-token", 404, false},
		{"/v1/control//rotate-token", 404, false},
		{"/v1/control/rotate-token/", 404, false},
		{"/v1/control/rotate-token/.", 404, false},
		{"/v1/control/%2e%2e/scion/v1/exec", 404, false},
		{"/v1/control/%2E%2E%2Fscion%2Fv1%2Fexec", 404, false},
		{"/v1/control/..%2fscion/v1/exec", 404, false},
		{"/v1/control/rotate-token%2F", 404, false},
		{"/v1/control/rotate%2Dtoken", 404, false},
		{"/v1/control/..\\scion\\v1\\exec", 404, false},
		{"/V1/control/rotate-token", 404, false},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			k, r := kicks.Load(), reached.Load()
			resp, err := hubSide.Call(context.Background(), &conduitv1.RpcRequest{Method: "POST", Path: tt.path})
			if err != nil {
				t.Fatal(err)
			}
			if resp.GetStatus() != tt.want {
				t.Fatalf("status %d, want %d", resp.GetStatus(), tt.want)
			}
			if got := reached.Load() != r; got != tt.reach {
				t.Fatalf("handler reached = %v, want %v", got, tt.reach)
			}
			if kicked := kicks.Load() != k; kicked != (tt.want == 202) {
				t.Fatalf("kicked = %v", kicked)
			}
		})
	}
}

// TestAgentRPCWithoutControlHandler: an agent built without a control
// handler (as with conduit's control routes not wired) advertises no rpc
// routes and answers every RPC 501, as before.
func TestAgentRPCWithoutControlHandler(t *testing.T) {
	hub := newFakeHub(t)
	startAgent(t, hub, nil)
	if got := hub.nextHello(t).GetCapabilities().GetRpc(); len(got) != 0 {
		t.Fatalf("Hello rpc = %v, want none", got)
	}
	s := hub.nextSession(t)
	for _, p := range []string{"/v1/control/rotate-token", "/scion/v1/exec"} {
		resp, err := s.Call(context.Background(), &conduitv1.RpcRequest{Method: "POST", Path: p})
		if err != nil {
			t.Fatal(err)
		}
		if resp.GetStatus() != http.StatusNotImplemented {
			t.Fatalf("%s: status %d, want 501", p, resp.GetStatus())
		}
	}
}

// TestNewRejectsRPCRoutesWithoutHandler: rpc routes are advertised only
// with a handler to serve them.
func TestNewRejectsRPCRoutesWithoutHandler(t *testing.T) {
	_, err := New(Options{
		HubURL:    "http://hub",
		AgentID:   testAgentID,
		ProjectID: testProjectID,
		Token:     func() string { return "t" },
		RPCRoutes: []string{control.RouteRotateToken},
	})
	if err == nil || !strings.Contains(err.Error(), "RPCRoutes") {
		t.Fatalf("New = %v, want an RPCRoutes error", err)
	}
}
