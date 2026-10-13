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

package brokerhost

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/brokercredentials"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/runtimebroker"
)

// runHost starts h.Run on port 0 and returns its address and a function
// that cancels it and returns Run's result and duration.
func runHost(t *testing.T, h *Host) (string, func() (time.Duration, error)) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.Run(ctx) }()
	require.Eventually(t, func() bool {
		select {
		case err := <-done:
			t.Fatalf("Run returned early: %v", err)
		default:
		}
		return h.Addr() != ""
	}, 20*time.Second, 10*time.Millisecond)
	stop := func() (time.Duration, error) {
		start := time.Now()
		cancel()
		select {
		case err := <-done:
			return time.Since(start), err
		case <-time.After(30 * time.Second):
			t.Fatal("Run did not return")
			return 0, nil
		}
	}
	t.Cleanup(func() { cancel() })
	return "http://" + h.Addr(), stop
}

func getStatus(t *testing.T, url string) int {
	t.Helper()
	resp, err := http.Get(url)
	require.NoError(t, err)
	_ = resp.Body.Close()
	return resp.StatusCode
}

// TestHost_RunServesAndShutsDown: Run starts every active instance's
// services and the listener; on cancel every instance is stopped and Run
// returns.
func TestHost_RunServesAndShutsDown(t *testing.T) {
	f := newFixture(t)
	cfg := authConfig(t, f, dockerInstance("docker-a", "a"), dockerInstance("docker-b", "b"))
	cfg.Listener = ListenerConfig{Host: "127.0.0.1", Port: 0}
	h, err := New(cfg)
	require.NoError(t, err)
	require.NoError(t, h.Prepare(context.Background()))

	base, stop := runHost(t, h)
	assert.Equal(t, http.StatusOK, getStatus(t, base+"/readyz"), "both instances serving (each has a Hub connection)")
	assert.Equal(t, http.StatusOK, getStatus(t, base+"/healthz"))

	took, err := stop()
	require.NoError(t, err)
	assert.Less(t, took, 10*time.Second)
	for _, st := range h.Status() {
		assert.Equal(t, StateStopped, st.State, st.Key)
	}
}

// TestHost_ShutdownDrainIsBounded: an open request does not keep Run from
// returning past the shutdown bound.
func TestHost_ShutdownDrainIsBounded(t *testing.T) {
	f := newFixture(t)
	listing := make(chan struct{}, 1)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	cfg := authConfig(t, f, dockerInstance("docker-a", "a"))
	cfg.NewRuntime = func(context.Context, config.V1RuntimeBrokerInstanceConfig) (runtime.Runtime, error) {
		return &runtime.MockRuntime{
			NameFunc: func() string { return "docker" },
			ListFunc: func(ctx context.Context, _ map[string]string) ([]api.AgentInfo, error) {
				select {
				case listing <- struct{}{}:
				default:
				}
				<-release // a request that never finishes on its own
				return nil, nil
			},
		}, nil
	}
	cfg.Listener = ListenerConfig{Host: "127.0.0.1", Port: 0}
	cfg.ShutdownTimeout = 300 * time.Millisecond
	h, err := New(cfg)
	require.NoError(t, err)
	require.NoError(t, h.Prepare(context.Background()))
	id := h.Status()[0].RuntimeBrokerID

	base, stop := runHost(t, h)
	go func() {
		req, _ := http.NewRequest(http.MethodGet, base+"/api/v1/agents", nil)
		_ = (&apiclient.HMACAuth{BrokerID: id, SecretKey: secretFor("docker-a")}).ApplyAuth(req)
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			_ = resp.Body.Close()
		}
	}()
	select {
	case <-listing:
	case <-time.After(5 * time.Second):
		t.Fatal("the request never reached the instance")
	}

	took, _ := stop()
	assert.Less(t, took, 5*time.Second, "the drain is bounded by the shutdown timeout")
}

// TestHost_RunInstanceNotServingIsStopped: an instance whose services fail
// to start, or (several configured) that has no Hub connection, is stopped,
// reported refused, and readiness is false; the sibling keeps serving.
func TestHost_RunInstanceNotServingIsStopped(t *testing.T) {
	f := newFixture(t)
	cfg := authConfig(t, f, dockerInstance("docker-a", "a"), dockerInstance("docker-b", "b"), dockerInstance("docker-c", "c"))
	authBuild := cfg.BuildServer
	cfg.BuildServer = func(ic InstanceContext) (*runtimebroker.Server, error) {
		switch ic.Instance.Key {
		case "docker-b":
			// Hub mode without keys on a non-loopback listener: StartServices refuses.
			sc := runtimebroker.DefaultServerConfig()
			sc.BrokerID = ic.Identity.RuntimeBrokerID
			sc.StateDir = t.TempDir()
			sc.Host = "0.0.0.0"
			sc.HubEnabled = true
			sc.HubEndpoint = "http://127.0.0.1:1"
			sc.FlatInstance = &runtimebroker.FlatInstanceConfig{Identity: ic.Identity, Instance: ic.Instance,
				RemoteCredentials: []brokercredentials.BrokerCredentials{{Name: "h", BrokerID: ic.Identity.RuntimeBrokerID,
					SecretKey: "!!not-base64", HubEndpoint: "http://127.0.0.1:1"}}}
			return runtimebroker.New(sc, ic.Manager, ic.Runtime), nil
		case "docker-c":
			// No Hub connection at all.
			sc := runtimebroker.DefaultServerConfig()
			sc.BrokerID = ic.Identity.RuntimeBrokerID
			sc.Host = "127.0.0.1"
			sc.StateDir = t.TempDir()
			sc.FlatInstance = &runtimebroker.FlatInstanceConfig{Identity: ic.Identity, Instance: ic.Instance, HubInProcess: true}
			return runtimebroker.New(sc, ic.Manager, ic.Runtime), nil
		}
		return authBuild(ic)
	}
	cfg.Listener = ListenerConfig{Host: "127.0.0.1", Port: 0}
	h, err := New(cfg)
	require.NoError(t, err)
	require.NoError(t, h.Prepare(context.Background()))

	base, stop := runHost(t, h)
	st := statusByKey(h)
	assert.Equal(t, StateActive, st["docker-a"].State)
	assert.Equal(t, StateStopped, st["docker-b"].State, "services failed to start")
	assert.Equal(t, StateStopped, st["docker-c"].State, "no Hub connection with several configured")
	assert.Contains(t, st["docker-c"].Error, "no Hub connection")
	assert.Contains(t, f.activator.refused, "docker-b", "reported refused")
	assert.Contains(t, f.activator.refused, "docker-c")
	assert.Equal(t, http.StatusServiceUnavailable, getStatus(t, base+"/readyz"))

	// The serving sibling is still reachable on its instance route.
	id := st["docker-a"].RuntimeBrokerID
	req, _ := http.NewRequest(http.MethodGet, base+InstancePrefix(id)+"/api/v1/info", nil)
	require.NoError(t, (&apiclient.HMACAuth{BrokerID: id, SecretKey: secretFor("docker-a")}).ApplyAuth(req))
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	_, err = stop()
	require.NoError(t, err)
}

// orderService records, when started and stopped, what the instances were
// doing.
type orderService struct {
	h                     *Host
	mu                    sync.Mutex
	startedWithInstanceUp bool
	stoppedWithInstanceUp bool
	starts, stops         int
}

func (s *orderService) Start(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.starts++
	for _, a := range s.h.Active() {
		if a.Server.Started() {
			s.startedWithInstanceUp = true
		}
	}
	return nil
}

func (s *orderService) Stop(context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stops++
	for _, st := range s.h.Status() {
		if st.State == StateActive {
			s.stoppedWithInstanceUp = true
		}
	}
}

// TestHost_ServicesStartBeforeAndStopAfterInstances: a host service (the
// single NFS mount owner) starts before any instance's services and stops,
// once, after every instance has shut down.
func TestHost_ServicesStartBeforeAndStopAfterInstances(t *testing.T) {
	f := newFixture(t)
	cfg := authConfig(t, f, dockerInstance("docker-a", "a"), dockerInstance("docker-b", "b"))
	cfg.Listener = ListenerConfig{Host: "127.0.0.1", Port: 0}
	svc := &orderService{}
	cfg.Services = []Service{svc}
	h, err := New(cfg)
	require.NoError(t, err)
	svc.h = h
	require.NoError(t, h.Prepare(context.Background()))

	_, stop := runHost(t, h)
	for _, a := range h.Active() {
		assert.True(t, a.Server.Started(), "instances started after the service")
	}
	_, err = stop()
	require.NoError(t, err)
	svc.mu.Lock()
	defer svc.mu.Unlock()
	assert.Equal(t, 1, svc.starts)
	assert.Equal(t, 1, svc.stops, "stopped once")
	assert.False(t, svc.startedWithInstanceUp, "the service starts before any instance's services")
	assert.False(t, svc.stoppedWithInstanceUp, "the service stops after every instance shut down")
}

// failingService fails to start.
type failingService struct{ stopped bool }

func (s *failingService) Start(context.Context) error { return errors.New("cannot start") }
func (s *failingService) Stop(context.Context)        { s.stopped = true }

// TestHost_ServiceStartFailureStopsEarlierServices: a host service that
// fails to start ends Run before any instance starts, and the services
// already started are stopped.
func TestHost_ServiceStartFailureStopsEarlierServices(t *testing.T) {
	f := newFixture(t)
	cfg := authConfig(t, f, dockerInstance("docker-a", "a"))
	first := &orderService{}
	cfg.Services = []Service{first, &failingService{}}
	h, err := New(cfg)
	require.NoError(t, err)
	first.h = h
	require.NoError(t, h.Prepare(context.Background()))
	require.Error(t, h.Run(context.Background()))
	assert.Equal(t, 1, first.stops, "the started service is stopped")
	for _, a := range h.Active() {
		assert.False(t, a.Server.Started(), "no instance starts")
	}
}
