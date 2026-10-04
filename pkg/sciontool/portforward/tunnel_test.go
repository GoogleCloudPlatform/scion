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

package portforward

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	scionhub "github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/transportauth"
	"github.com/gorilla/websocket"
)

func TestIsLoopbackHost(t *testing.T) {
	tests := []struct {
		host string
		want bool
	}{
		{"localhost", true},
		{"LOCALHOST", true},
		{" localhost ", true},
		{"127.0.0.1", true},
		{"::1", true},
		{"127.0.0.2", true},
		{"10.0.0.1", false},
		{"169.254.169.254", false},
		{"0.0.0.0", false},
		{"example.com", false},
		{"localhost.evil.com", false},
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.host, func(t *testing.T) {
			if got := isLoopbackHost(tt.host); got != tt.want {
				t.Errorf("isLoopbackHost(%q) = %v, want %v", tt.host, got, tt.want)
			}
		})
	}
}

func TestRunOnce_DialCarriesTransportHeader(t *testing.T) {
	tests := []struct {
		name   string
		mode   transportauth.HeaderMode
		header string
	}{
		{"authorization", transportauth.HeaderAuthorization, "Authorization"},
		{"iap", transportauth.HeaderProxyAuthorization, "Proxy-Authorization"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := make(chan http.Header, 1)
			upgrader := websocket.Upgrader{}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got <- r.Header.Clone()
				conn, err := upgrader.Upgrade(w, r, nil)
				if err != nil {
					return
				}
				_ = conn.Close()
			}))
			defer srv.Close()

			client := scionhub.NewClientWithConfig(srv.URL, "agent-credential", "agent-1")
			src := transportauth.NewInjectedSource()
			src.SetToken("transport-credential", time.Now().Add(time.Hour))
			client.SetTransportAuth(src, tt.mode)

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = NewManager(client).runOnce(ctx)

			select {
			case h := <-got:
				if v := h.Get(tt.header); v != "Bearer transport-credential" {
					t.Errorf("%s = %q, want transport credential", tt.header, v)
				}
				if v := h.Get("X-Scion-Agent-Token"); v != "agent-credential" {
					t.Errorf("X-Scion-Agent-Token = %q", v)
				}
			case <-ctx.Done():
				t.Fatal("tunnel dial never reached the server")
			}
		})
	}
}

func TestRunOnce_NoTransportSourceStillDials(t *testing.T) {
	got := make(chan http.Header, 1)
	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- r.Header.Clone()
		if conn, err := upgrader.Upgrade(w, r, nil); err == nil {
			_ = conn.Close()
		}
	}))
	defer srv.Close()

	client := scionhub.NewClientWithConfig(srv.URL, "agent-credential", "agent-1")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = NewManager(client).runOnce(ctx)

	h := <-got
	if v := h.Get("Authorization"); v != "" {
		t.Errorf("unexpected Authorization header without a transport source")
	}
}
