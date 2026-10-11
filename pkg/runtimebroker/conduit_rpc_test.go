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
	"io"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/clock"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/envelope"
	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// TestConduitRPCHandler_RequestReachesHandlerUnchanged checks that a
// request arriving on the broker's conduit session reaches the HTTP
// handlers with its method, path, query, headers and body unchanged, plus
// the hub connection name, and that the handler's status, headers and body
// come back unchanged.
func TestConduitRPCHandler_RequestReachesHandlerUnchanged(t *testing.T) {
	binary := make([]byte, 256)
	for i := range binary {
		binary[i] = byte(i)
	}
	type seen struct {
		method, path, rawQuery, conn string
		header                       http.Header
		body                         []byte
	}
	got := make(chan seen, 1)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- seen{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Get("X-Scion-Hub-Connection"), r.Header.Clone(), b}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Vary", "Accept, Origin")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write(b)
	})
	h := newConduitRPCHandler(handler, "hub-a", 0, nil)
	hub := newConduitSessionPair(t, h).hub

	tests := []struct {
		name string
		req  *wsprotocol.RequestEnvelope
	}{
		{"binary body and query", wsprotocol.NewRequestEnvelope("q-1", http.MethodPost, "/api/v1/agents/a1/files", "projectId=p%201&x=1&x=2", map[string]string{"Accept": "application/json, text/plain", "X-Scion-Broker-ID": "broker-1"}, binary)},
		{"empty body", wsprotocol.NewRequestEnvelope("q-2", http.MethodGet, "/api/v1/agents", "", map[string]string{"Accept": "*/*"}, nil)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := hub.Call(context.Background(), envelope.RequestToRPC(tc.req))
			if err != nil {
				t.Fatalf("Call: %v", err)
			}
			s := <-got
			if s.method != tc.req.Method || s.path != tc.req.Path || s.rawQuery != tc.req.Query {
				t.Errorf("handler saw %s %s?%s, want %s %s?%s", s.method, s.path, s.rawQuery, tc.req.Method, tc.req.Path, tc.req.Query)
			}
			for k, v := range tc.req.Headers {
				if s.header.Get(k) != v {
					t.Errorf("header %s = %q, want %q", k, s.header.Get(k), v)
				}
			}
			if s.conn != "hub-a" {
				t.Errorf("X-Scion-Hub-Connection = %q, want hub-a", s.conn)
			}
			if len(tc.req.Body) == 0 && len(s.body) != 0 || len(tc.req.Body) != 0 && !reflect.DeepEqual(s.body, tc.req.Body) {
				t.Errorf("handler saw body %q, want %q", s.body, tc.req.Body)
			}
			env := envelope.RPCToResponse(resp)
			if env.RequestID != tc.req.RequestID || env.StatusCode != http.StatusAccepted {
				t.Errorf("response %+v", env)
			}
			if env.Headers["Vary"] != "Accept, Origin" || env.Headers["Content-Type"] != "application/octet-stream" {
				t.Errorf("response headers %v", env.Headers)
			}
			if len(tc.req.Body) != 0 && !reflect.DeepEqual(env.Body, tc.req.Body) {
				t.Errorf("response body %q, want %q", env.Body, tc.req.Body)
			}
		})
	}
}

// TestConduitRPCHandler_OverLimitIsAnswered413 checks that a request body
// over the RPC limit is answered 413 without reaching the handlers.
func TestConduitRPCHandler_OverLimitIsAnswered413(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler must not run for an over-limit request")
	})
	hub := newConduitSessionPair(t, newConduitRPCHandler(handler, "", 0, nil)).hub
	resp, err := hub.Call(context.Background(), &conduitv1.RpcRequest{Method: http.MethodPost, Path: "/api/v1/agents", Body: make([]byte, conduit.MaxRPCBody+1)})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if resp.GetStatus() != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", resp.GetStatus())
	}
}

// TestConduitRPCHandler_NonUTF8StaysUp checks that a request path and a
// response header value that are not valid UTF-8 cross the session in
// each direction (with UTF-8 normalisation for parity with the JSON
// envelope) and that the session stays up.
func TestConduitRPCHandler_NonUTF8StaysUp(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Disposition", "caf\xe9")
		w.WriteHeader(http.StatusOK)
	})
	pair := newConduitSessionPair(t, newConduitRPCHandler(handler, "", 0, nil))
	hub := pair.hub

	resp, err := hub.Call(context.Background(), envelope.RequestToRPC(
		wsprotocol.NewRequestEnvelope("u-1", http.MethodGet, "/api/v1/agents/\xff", "", map[string]string{"X-Name": "caf\xe9"}, nil)))
	if err != nil {
		t.Fatalf("first Call: %v", err)
	}
	if got := envelope.RPCToResponse(resp).Headers["Content-Disposition"]; got != "caf�" {
		t.Fatalf("response header = %q, want %q", got, "caf�")
	}

	// Both ends are still up: the hub's session has not ended, the
	// dialer's session has not ended, and a second call succeeds.
	if err := hub.Err(); err != nil {
		t.Fatalf("hub session ended: %v", err)
	}
	select {
	case end := <-pair.ends:
		t.Fatalf("broker session ended: %+v", end)
	default:
	}
	if _, err := hub.Call(context.Background(), envelope.RequestToRPC(
		wsprotocol.NewRequestEnvelope("u-2", http.MethodGet, "/api/v1/agents", "", nil, nil))); err != nil {
		t.Fatalf("second Call: %v", err)
	}
}

// TestConduitRPCHandler_ServesOnTheDialerSession checks that the handler,
// set as the RPC handler of the broker's conduit dialer, answers the hub's
// RpcRequests on the session the dialer keeps.
func TestConduitRPCHandler_ServesOnTheDialerSession(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Path", r.URL.Path)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(r.URL.RawQuery))
	})
	answered := make(chan *conduitv1.RpcResponse, 1)
	callErr := make(chan error, 1)
	hub := newFakeConduitHub(t, hubStep{after: func(ls conduit.LocalSession) {
		resp, err := ls.Call(context.Background(), envelope.RequestToRPC(
			wsprotocol.NewRequestEnvelope("d-1", http.MethodGet, "/api/v1/agents", "projectId=p1", nil, nil)))
		if err != nil {
			callErr <- err
			return
		}
		answered <- resp
	}})
	_, _, stop := startTestDialer(t, hub.srv.URL, clock.Real(), maxRand, func(c *ConduitDialConfig) {
		c.Session.RPCHandler = newConduitRPCHandler(handler, "hub-a", 0, nil)
	})
	select {
	case resp := <-answered:
		env := envelope.RPCToResponse(resp)
		if env.StatusCode != http.StatusOK || env.Headers["X-Path"] != "/api/v1/agents" || string(env.Body) != "projectId=p1" {
			t.Fatalf("response %+v", env)
		}
	case err := <-callErr:
		t.Fatalf("Call: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("hub never got an answer")
	}
	if err := stop(); err != nil {
		t.Fatalf("dialer: %v", err)
	}
}
