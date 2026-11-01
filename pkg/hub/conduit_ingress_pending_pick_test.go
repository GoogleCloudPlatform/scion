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

//go:build !no_sqlite

package hub

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
)

// pendingTotal is the number of pending placements on rt across relays.
func pendingTotal(rt *conduitExternalRuntime) int {
	rt.ingressPending.mu.Lock()
	defer rt.ingressPending.mu.Unlock()
	n := 0
	for _, m := range rt.ingressPending.entries {
		n += len(m)
	}
	return n
}

// TestConduitIngress_PickBurstSpreads: concurrent picks through
// pickConduitIngressRelay at equal session counts (no dial admitted yet)
// split across the two relays instead of herding onto one
// (ptone/scion#6641).
func TestConduitIngress_PickBurstSpreads(t *testing.T) {
	f := newIngressFixture(t, "relay-a", "relay-b")
	f.s.conduitIngressSampleTestHook = func(c []registry.RelayInstance) []registry.RelayInstance {
		require.Len(t, c, 2)
		return c
	}
	f.rt.store = &ingressCountStore{Store: f.st, counts: map[string]int{}}

	const n = 50
	var mu sync.Mutex
	got := map[string]int{}
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			target, _, ok := f.s.pickConduitIngressRelay(context.Background(), f.rt, map[string]bool{})
			if !assert.True(t, ok) {
				return
			}
			mu.Lock()
			got[target.InstanceID]++
			mu.Unlock()
		}()
	}
	wg.Wait()
	assert.Equal(t, n, got["relay-a"]+got["relay-b"])
	assert.LessOrEqual(t, abs(got["relay-a"]-got["relay-b"]), 1, "picks %v", got)
	assert.Equal(t, n, pendingTotal(f.rt), "every unreleased pick is pending")
}

// TestConduitIngress_PendingReleased: a dial leaves no pending placement
// once its outcome is known: a non-upgrade answer, a re-pick (a refusal or
// an unusable endpoint), or an upgraded leg whose relay has answered while
// the session is still open.
func TestConduitIngress_PendingReleased(t *testing.T) {
	t.Run("non-upgrade answer", func(t *testing.T) {
		f := newIngressFixture(t, "relay-a", "relay-b")
		rec := f.dial(t, nil)
		require.Equal(t, http.StatusAccepted, rec.Code)
		assert.Zero(t, pendingTotal(f.rt))
	})
	t.Run("re-pick after a refusal", func(t *testing.T) {
		f := newIngressFixture(t, "relay-a", "relay-b")
		f.preferRelays("relay-a", "relay-b")
		f.relays["relay-a"].answer = func(w http.ResponseWriter, _ *http.Request) {
			writeRelayUnavailable(w, relay.ReasonRelayNotAdmitting)
		}
		rec := f.dial(t, nil)
		require.Equal(t, "reached relay-b", rec.Body.String())
		assert.Zero(t, pendingTotal(f.rt))
	})
	t.Run("re-pick after an unusable endpoint", func(t *testing.T) {
		f := newIngressFixture(t, "relay-a")
		now := time.Now()
		_, err := f.st.RegisterRelay(context.Background(), registry.RelayInstance{InstanceID: "relay-bad", InternalEndpoint: "://no-host", StartedAt: now, LastSeen: now})
		require.NoError(t, err)
		f.preferRelays("relay-bad", "relay-a")
		rec := f.dial(t, nil)
		require.Equal(t, "reached relay-a", rec.Body.String())
		assert.Zero(t, pendingTotal(f.rt))
	})
	t.Run("upgraded leg after the relay answers", func(t *testing.T) {
		f := newIngressFixture(t, "relay-a")
		hold := make(chan struct{})
		t.Cleanup(func() { close(hold) })
		f.relays["relay-a"].answer = func(w http.ResponseWriter, _ *http.Request) {
			conn, bufrw, err := http.NewResponseController(w).Hijack()
			if err != nil {
				return
			}
			defer conn.Close()
			_, _ = bufrw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\nW")
			_ = bufrw.Flush()
			<-hold // the session stays open
		}
		hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			f.s.serveConduitIngress(w, r, f.rt)
		}))
		t.Cleanup(hub.Close)

		conn, err := net.Dial("tcp", strings.TrimPrefix(hub.URL, "http://"))
		require.NoError(t, err)
		t.Cleanup(func() { _ = conn.Close() })
		_, err = io.WriteString(conn, "GET /api/v1/conduit?v=1 HTTP/1.1\r\nHost: hub.example.com\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n"+
			"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nAuthorization: Bearer client-credential\r\n\r\n")
		require.NoError(t, err)
		br := bufio.NewReader(conn)
		resp, err := http.ReadResponse(br, nil)
		require.NoError(t, err)
		require.Equal(t, http.StatusSwitchingProtocols, resp.StatusCode)
		b, err := br.ReadByte()
		require.NoError(t, err)
		require.Equal(t, byte('W'), b, "the relay's first byte reached the client")
		assert.Zero(t, pendingTotal(f.rt), "released on the relay's answer while the session is open")
	})
}
