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
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/transportauth"
)

// newLifecycleTestHub returns a stand-in hub that answers heartbeats and
// counts them. Every other path (control channel, conduit) gets a 404, so
// those clients keep retrying until they are stopped.
func newLifecycleTestHub(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var heartbeats atomic.Int64
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/heartbeat") {
			heartbeats.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte("{}"))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(hub.Close)
	return hub, &heartbeats
}

// TestHubConnection_OverlappingStartAndReinitialize runs Start and
// Reinitialize on one connection from several goroutines at once (run it
// under -race), then checks that a final Stop returns and leaves no
// control channel or heartbeat from an earlier Start running.
func TestHubConnection_OverlappingStartAndReinitialize(t *testing.T) {
	t.Setenv(transportauth.EnvTransportMode, "")
	t.Setenv(transportauth.EnvTransportAudience, "")

	hub, heartbeats := newLifecycleTestHub(t)
	creds := makeTestCreds("hub-overlap", "broker-1", hub.URL)

	cfg := DefaultServerConfig()
	cfg.BrokerID = "broker-1"
	cfg.BrokerName = "test-host"
	cfg.HeartbeatEnabled = true
	cfg.HeartbeatInterval = 10 * time.Millisecond
	cfg.ControlChannelEnabled = true
	cfg.BrokerAuthEnabled = false
	srv := New(cfg, &mockManager{}, &runtime.MockRuntime{NameFunc: func() string { return "docker" }})

	conn, err := srv.createHubConnection("hub-overlap", creds)
	if err != nil {
		t.Fatal(err)
	}
	srv.hubMu.Lock()
	srv.hubConnections[conn.Name] = conn
	srv.hubMu.Unlock()

	// The context stays live until the checks below are done, so anything
	// left running by an earlier Start keeps running and is visible.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const workers, rounds = 4, 5
	begin := make(chan struct{})
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			<-begin
			for i := 0; i < rounds; i++ {
				if (w+i)%2 == 0 {
					if err := conn.Start(ctx, srv); err != nil {
						t.Errorf("Start: %v", err)
					}
				} else if err := conn.Reinitialize(ctx, srv, creds); err != nil {
					t.Errorf("Reinitialize: %v", err)
				}
			}
		}(w)
	}
	close(begin)
	workersDone := make(chan struct{})
	go func() {
		wg.Wait()
		close(workersDone)
	}()
	select {
	case <-workersDone:
	case <-time.After(30 * time.Second):
		t.Fatal("Start/Reinitialize calls did not finish: a Stop inside one of them is waiting on a control channel it could not stop")
	}

	// Stop waits for every control channel goroutine Start launched. One
	// whose client was overwritten by a later Start is never closed, so
	// Stop would not return.
	stopped := make(chan struct{})
	go func() {
		conn.Stop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(15 * time.Second):
		t.Fatal("Stop did not return: a control channel from an earlier Start is still running")
	}

	snap := conn.snapshot()
	if snap.Heartbeat != nil || snap.HasControlChannel {
		t.Errorf("services still set after Stop: heartbeat=%v controlChannel=%v", snap.Heartbeat != nil, snap.HasControlChannel)
	}

	// No heartbeat may keep running after Stop. Allow one in flight.
	time.Sleep(50 * time.Millisecond)
	before := heartbeats.Load()
	time.Sleep(300 * time.Millisecond)
	if after := heartbeats.Load(); after != before {
		t.Errorf("heartbeats still sent after Stop (%d -> %d): a heartbeat from an earlier Start is still running", before, after)
	}
}

// TestHubConnection_RequestedReinitializeAppliesLatest checks that when
// several credential changes are queued for one connection, the latest one
// is applied whatever order the queued calls run in.
func TestHubConnection_RequestedReinitializeAppliesLatest(t *testing.T) {
	t.Setenv(transportauth.EnvTransportMode, "")
	t.Setenv(transportauth.EnvTransportAudience, "")

	cfg := DefaultServerConfig()
	cfg.BrokerID = "broker-1"
	cfg.HeartbeatEnabled = false
	cfg.ControlChannelEnabled = false
	cfg.BrokerAuthEnabled = false
	srv := New(cfg, &mockManager{}, &runtime.MockRuntime{NameFunc: func() string { return "docker" }})

	first := makeTestCreds("hub-latest", "broker-1", "http://hub1.example.com")
	conn, err := srv.createHubConnection("hub-latest", first)
	if err != nil {
		t.Fatal(err)
	}
	second := makeTestCreds("hub-latest", "broker-2", "http://hub2.example.com")
	third := makeTestCreds("hub-latest", "broker-3", "http://hub3.example.com")
	conn.requestReinitialize(second)
	conn.requestReinitialize(third)

	ctx := context.Background()
	applied, err := conn.applyRequestedReinitialize(ctx, srv)
	if err != nil || !applied {
		t.Fatalf("first apply: applied=%v err=%v", applied, err)
	}
	// The second queued call finds the latest request already applied.
	applied, err = conn.applyRequestedReinitialize(ctx, srv)
	if err != nil || applied {
		t.Fatalf("second apply: applied=%v err=%v, want a no-op", applied, err)
	}
	if got := conn.snapshot().Credentials; got != third {
		t.Errorf("Credentials = %+v, want the latest requested set", got)
	}
	if got := conn.latestCredentials(); got != third {
		t.Errorf("latestCredentials = %+v, want the latest requested set", got)
	}
}
