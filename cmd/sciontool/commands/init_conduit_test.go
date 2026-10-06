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

package commands

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	core "github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/transport/ws"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

const pfWait = 10 * time.Second

// pfHub serves the conduit endpoint (or 404s it, like an older hub) and
// the legacy tunnel endpoint, recording which ones sciontool calls.
type pfHub struct {
	srv          *httptest.Server
	conduitHits  atomic.Int64
	tunnelHits   atomic.Int64
	tunnel       chan struct{}
	conduitAdmit chan struct{}
}

func newPFHub(t *testing.T, servesConduit bool) *pfHub {
	h := &pfHub{tunnel: make(chan struct{}, 16), conduitAdmit: make(chan struct{}, 16)}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/conduit", func(w http.ResponseWriter, r *http.Request) {
		h.conduitHits.Add(1)
		if !servesConduit {
			http.NotFound(w, r)
			return
		}
		conn, err := ws.Upgrade(w, r, nil, ws.Options{})
		if err != nil {
			return
		}
		s, err := core.Accept(r.Context(), conn, core.Config{}, pfAdmit(func(hello *conduitv1.Hello) *conduitv1.Welcome {
			h.conduitAdmit <- struct{}{}
			return &conduitv1.Welcome{SessionId: "s1", RelayInstanceId: "r1", ConnectionEpoch: 1,
				EndpointIncarnation: hello.GetCapabilities().GetEndpointIncarnation()}
		}))
		if err != nil {
			return
		}
		<-s.(core.LocalSession).Done()
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/ports/tunnel") {
			h.tunnelHits.Add(1)
			h.tunnel <- struct{}{}
		}
		http.Error(w, "no", http.StatusServiceUnavailable)
	})
	h.srv = httptest.NewServer(mux)
	t.Cleanup(h.srv.Close)
	return h
}

type pfAdmit func(*conduitv1.Hello) *conduitv1.Welcome

func (f pfAdmit) Admit(_ context.Context, h *conduitv1.Hello) (*conduitv1.Welcome, error) {
	return f(h), nil
}

func (pfAdmit) Refresh(context.Context, *conduitv1.AuthRefresh) error {
	return core.Reject(core.CloseUnauthenticated, "unauthenticated")
}

func waitSignal(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(pfWait):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// TestPortForwardingConduitGate is the sciontool half of the old/new
// matrix. Without hub.conduit in SCION_HUB_EXPERIMENTS (an older hub, or
// hub.conduit off) sciontool runs only the legacy tunnel and never calls
// /api/v1/conduit; with it, it dials conduit and falls back to the tunnel
// on a 404. An older hub's SCION_HUB_CONDUIT is not read.
func TestPortForwardingConduitGate(t *testing.T) {
	tests := []struct {
		name           string
		experiments    string // SCION_HUB_EXPERIMENTS
		legacy         string // SCION_HUB_CONDUIT, set by older hubs
		disableConduit bool
		servesConduit  bool
		wantConduit    int64 // conduit attempts
		wantTunnel     bool
	}{
		{name: "old hub, no capability", servesConduit: false, wantTunnel: true},
		{name: "old hub, SCION_HUB_CONDUIT ignored", legacy: "true", servesConduit: true, wantTunnel: true},
		{name: "new hub, conduit off", servesConduit: true, wantTunnel: true},
		{name: "conduit not listed", experiments: "hub.k8s_nfs_home", servesConduit: true, wantTunnel: true},
		{name: "conduit name prefix only", experiments: "hub.conduit_x", servesConduit: true, wantTunnel: true},
		{name: "DisableConduit", experiments: conduit.ExperimentConduit, disableConduit: true, servesConduit: true, wantTunnel: true},
		{name: "capability but endpoint 404", experiments: conduit.ExperimentConduit, servesConduit: false, wantConduit: 1, wantTunnel: true},
		{name: "new hub, conduit on", experiments: conduit.ExperimentConduit, servesConduit: true, wantConduit: 1},
		{name: "conduit listed with others", experiments: "hub.other, hub.conduit", servesConduit: true, wantConduit: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newPFHub(t, tt.servesConduit)
			client := hub.NewClientWithConfig(h.srv.URL, "token-1", "agent-1")
			env := map[string]string{
				conduit.EnvHubExperiments: tt.experiments,
				"SCION_HUB_CONDUIT":       tt.legacy,
				envProjectID:              "project-1",
				conduit.EnvLaunchID:       "launch-1",
			}
			p := newPortForwarding(client, tt.disableConduit, func(k string) string { return env[k] })
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() { p.run(ctx); close(done) }()

			if tt.wantTunnel {
				waitSignal(t, h.tunnel, "the legacy tunnel")
			} else {
				waitSignal(t, h.conduitAdmit, "a conduit session")
			}
			cancel()
			select {
			case <-done:
			case <-time.After(pfWait):
				t.Fatal("run did not return after cancel")
			}
			if n := h.conduitHits.Load(); n != tt.wantConduit {
				t.Fatalf("/api/v1/conduit called %d times, want %d", n, tt.wantConduit)
			}
			if !tt.wantTunnel && h.tunnelHits.Load() != 0 {
				t.Fatalf("legacy tunnel dialed %d times while conduit was up", h.tunnelHits.Load())
			}
		})
	}
}
