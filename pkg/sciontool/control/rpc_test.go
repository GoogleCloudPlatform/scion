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

package control

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"testing"

	core "github.com/GoogleCloudPlatform/scion/pkg/conduit"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

func call(h http.Handler, req *conduitv1.RpcRequest) *conduitv1.RpcResponse {
	return RPCHandler(h).HandleRPC(context.Background(), req)
}

// TestRPCHandlerRequestMapping: the handler sees the method, path,
// query and body of the RpcRequest, and of its headers only
// Content-Type.
func TestRPCHandlerRequestMapping(t *testing.T) {
	var got *http.Request
	var body []byte
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		body, _ = io.ReadAll(r.Body)
		w.Header().Set("X-Out", "1")
		w.WriteHeader(http.StatusAccepted)
	})
	resp := call(h, &conduitv1.RpcRequest{
		Method: "POST",
		Path:   "/v1/control/x",
		Query:  "a=1",
		Body:   []byte("b"),
		Headers: map[string]string{
			"content-type":        "application/json",
			"X-Scion-Agent-Token": "not-forwarded",
			"Authorization":       "not-forwarded",
			"Cookie":              "not-forwarded",
		},
	})
	if resp.GetStatus() != 202 || resp.GetHeaders()["X-Out"] != "1" {
		t.Fatalf("resp = %v", resp)
	}
	if got.Method != "POST" || got.URL.Path != "/v1/control/x" || got.URL.RawQuery != "a=1" || string(body) != "b" {
		t.Fatalf("request = %s %s?%s body %q", got.Method, got.URL.Path, got.URL.RawQuery, body)
	}
	if got.ContentLength != 1 {
		t.Fatalf("ContentLength = %d, want 1", got.ContentLength)
	}
	if len(got.Header) != 1 || got.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("headers = %v, want only Content-Type", got.Header)
	}
}

// TestRPCHandlerStatusMapping covers defaults and failure answers.
func TestRPCHandlerStatusMapping(t *testing.T) {
	tests := []struct {
		name   string
		method string
		h      http.HandlerFunc
		want   int32
	}{
		{"no WriteHeader is 200", "POST", func(http.ResponseWriter, *http.Request) {}, 200},
		{"empty method is GET", "", func(w http.ResponseWriter, r *http.Request) {
			if r.Method != "GET" {
				w.WriteHeader(500)
			}
		}, 200},
		{"invalid method token", "PO ST", func(http.ResponseWriter, *http.Request) {}, 400},
		{"panic is 500", "POST", func(http.ResponseWriter, *http.Request) { panic("boom") }, 500},
		{"first WriteHeader wins", "POST", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(202)
			w.WriteHeader(500)
		}, 202},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := call(tt.h, &conduitv1.RpcRequest{Method: tt.method, Path: "/v1/control/x"})
			if resp.GetStatus() != tt.want {
				t.Fatalf("status %d, want %d", resp.GetStatus(), tt.want)
			}
		})
	}
}

// TestRPCHandlerBodyCap: the buffered response body stops just above
// MaxRPCBody, which the session answers 413.
func TestRPCHandlerBodyCap(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		chunk := bytes.Repeat([]byte{'a'}, 64<<10)
		for range core.MaxRPCBody/len(chunk) + 4 {
			_, _ = w.Write(chunk)
		}
	})
	resp := call(h, &conduitv1.RpcRequest{Method: "POST", Path: "/v1/control/x"})
	if n := len(resp.GetBody()); n != core.MaxRPCBody+1 {
		t.Fatalf("body length %d, want %d", n, core.MaxRPCBody+1)
	}
}

// TestRPCHandlerAllowList: paths that are not canonical /v1/control/*
// paths never reach the handler.
func TestRPCHandlerAllowList(t *testing.T) {
	for _, p := range []string{
		"", "/", "/scion/v1/exec", "/scion/v1/bootstrap", "/healthz", "/v1/control",
		"v1/control/x", "/v1/control/../scion/v1/exec", "/v1/control/./x",
		"/v1/control//x", "/v1/control/x/", "/v1/control/%2e%2e/scion/v1/exec",
		"/v1/control/..%2Fscion", "/v1/control/x%2F", "/v1/control/..\\scion",
	} {
		reached := false
		h := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true })
		resp := call(h, &conduitv1.RpcRequest{Method: "POST", Path: p})
		if resp.GetStatus() != 404 || reached {
			t.Fatalf("%q: status %d, reached %v; want 404 and not reached", p, resp.GetStatus(), reached)
		}
	}
}
