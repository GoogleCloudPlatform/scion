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

package cmd

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/brokerhost"
	"github.com/GoogleCloudPlatform/scion/pkg/brokeridentity"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/runtimebroker"
)

// The co-located launch drain (ptone/scion#4668, C2b): on a graceful
// shutdown of a process running the hub and its Runtime Broker(s), every
// in-process broker's DrainLaunches runs before the hub's HTTP drain, so
// the drain's broker_restarted reports reach a hub that still serves
// broker requests. The tests drive the real shutdown sequence
// (serverShutdownSequence: mark, then cancel), the real colocatedBrokerStop
// wait and a real hub.WebServer front (the workstation layout: the hub API
// mounted on the web server, which is where an in-process broker's reports
// arrive, see hubListenPort).

const drainTestWait = 10 * time.Second

// drainTestFreePort returns a free 127.0.0.1 port.
func drainTestFreePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := ln.Addr().(*net.TCPAddr).Port
	require.NoError(t, ln.Close())
	return port
}

// drainTestDialable reports whether addr accepts a TCP connection.
func drainTestDialable(addr string) bool {
	c, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

// drainTestAwaitListening waits until addr accepts connections.
func drainTestAwaitListening(t *testing.T, addr, what string) {
	t.Helper()
	require.Eventually(t, func() bool { return drainTestDialable(addr) }, drainTestWait, 10*time.Millisecond, "%s never listened on %s", what, addr)
}

// drainTestHold opens a connection to a broker listener that the broker's
// http.Server tracks and waits for, so the broker's Shutdown keeps waiting
// until the test closes it: the broker's shutdown, and so its goroutine in
// the broker WaitGroup, cannot finish while the hold is open.
//
// The hold sends only the first lines of a request's headers and never
// finishes them, so the handler never runs and the connection stays in
// http.StateNew, which Shutdown counts as busy for 5s after the accept
// (closeIdleConns). It does not depend on the handler, which, once the
// server is shutting down, answers without waiting for a request body.
// The hold lasts well under 5s: the order checks end within about a second
// of the accept. A hold lost before the 300 ms wait check fails the test,
// never passes it; one lost after that check is harmless.
//
// A connection still in the listen backlog when Shutdown closes the
// listener is never tracked, so the hold must be accepted before the
// shutdown begins. A probe proves it: the server's single accept loop takes
// connections in arrival order and tracks each one before it accepts the
// next, so once a second connection, dialled after the hold, has been
// served, the hold is tracked.
func drainTestHold(t *testing.T, addr string) net.Conn {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	_, err = c.Write([]byte("GET /healthz HTTP/1.1\r\nHost: hold\r\n"))
	require.NoError(t, err)

	p, err := net.DialTimeout("tcp", addr, 2*time.Second)
	require.NoError(t, err)
	defer func() { _ = p.Close() }()
	require.NoError(t, p.SetDeadline(time.Now().Add(drainTestWait)))
	_, err = p.Write([]byte("GET /healthz HTTP/1.1\r\nHost: probe\r\nConnection: close\r\n\r\n"))
	require.NoError(t, err)
	resp, err := http.ReadResponse(bufio.NewReader(p), nil)
	require.NoError(t, err, "the broker did not serve the probe connection")
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return c
}

// drainTestFront is the hub front of a co-located process, wired as
// runServerStart wires it: the mark step on the shutdown sequence, and
// ShutdownHooks whose BeforeHTTPDrain is colocatedBrokerStop's wait. The
// hook is wrapped only to observe it. This wiring is a test-side mirror of
// runServerStart (cmd/server_foreground.go: frontShutdownHooks :359-364,
// webSrv.SetShutdownHooks and the mark step :661-662, markStarted :704,
// the brokerWG wait and markStopped :706-711); those lines themselves are
// not covered by these tests.
type drainTestFront struct {
	addr       string
	web        *hub.WebServer
	ctx        context.Context
	q          *serverShutdownSequence
	brokerStop *colocatedBrokerStop

	enteredOnce sync.Once
	entered     chan struct{} // the broker wait began
	waited      chan struct{} // the broker wait returned
	served      chan error    // WebServer.Start returned

	// atWaitEnd, when set, runs as soon as the broker wait returns, before
	// the front closes its listener.
	atWaitEnd func()
	// openAtWaitEnd: the front still accepted connections when the broker
	// wait returned.
	openAtWaitEnd atomic.Bool
}

// startDrainTestFront starts the front (hub API mounted when hubSrv is
// set) and returns it. Its ctx is the process context the brokers run
// under; the shutdown sequence cancels it.
func startDrainTestFront(t *testing.T, hubSrv *hub.Server, atWaitEnd func()) *drainTestFront {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	port := drainTestFreePort(t)
	f := &drainTestFront{
		addr:       fmt.Sprintf("127.0.0.1:%d", port),
		ctx:        ctx,
		q:          newServerShutdownSequence(cancel, shutdownClock),
		brokerStop: newColocatedBrokerStop(),
		entered:    make(chan struct{}),
		waited:     make(chan struct{}),
		served:     make(chan error, 1),
		atWaitEnd:  atWaitEnd,
	}
	plan := serverShutdownPlan{q: f.q, budget: serverShutdownBudget{}}
	web := hub.NewWebServer(hub.WebServerConfig{Host: "127.0.0.1", Port: port})
	f.web = web
	if hubSrv != nil {
		web.MountHubAPI(hubSrv.Handler(), hubSrv.CleanupBackgroundResources)
	}
	wait := f.brokerStop.beforeHTTPDrain(shutdownClock, brokerStopWaitFallback, plan.brokerContext)
	var waitedOnce sync.Once
	web.SetShutdownHooks(hub.ShutdownHooks{
		Context:          plan.teardownContext,
		HTTPDrainContext: plan.httpDrainContext,
		BeforeHTTPDrain: func(ctx context.Context) {
			f.enteredOnce.Do(func() { close(f.entered) })
			wait(ctx)
			waitedOnce.Do(func() {
				f.openAtWaitEnd.Store(drainTestDialable(f.addr))
				if f.atWaitEnd != nil {
					f.atWaitEnd()
				}
				close(f.waited)
			})
		},
	})
	f.q.addStep(shutdownPhaseMark, web.BeginShutdown)
	go func() { f.served <- web.Start(ctx) }()
	drainTestAwaitListening(t, f.addr, "the hub front")
	return f
}

// The broker tracking mirrors runServerStart (cmd/server_foreground.go:
// 703-711): brokerStop.markStarted before the broker starts, then, once
// its goroutines have joined brokerWG, awaitBrokers releases the front's
// wait when every one of them has returned.
func (f *drainTestFront) awaitBrokers(brokerWG *sync.WaitGroup) {
	go func() {
		brokerWG.Wait()
		f.brokerStop.markStopped()
	}()
}

// shutdownHolding begins the shutdown while hold keeps the broker's
// shutdown from finishing, and checks the order: the front waits for the
// broker with its listener open (duringWait runs then), and closes the
// listener only after the broker stopped.
func (f *drainTestFront) shutdownHolding(t *testing.T, hold net.Conn, duringWait func()) {
	t.Helper()
	f.q.begin(syscall.SIGTERM)
	select {
	case <-f.entered:
	case <-time.After(drainTestWait):
		t.Fatal("the hub front never began its wait for the co-located broker")
	}
	select {
	case <-f.waited:
		t.Fatal("hub HTTP drain began while the co-located broker was still shutting down")
	case <-time.After(300 * time.Millisecond):
	}
	assert.True(t, drainTestDialable(f.addr), "hub front listener closed during the broker wait")
	if duringWait != nil {
		duringWait()
	}

	_ = hold.Close()
	select {
	case <-f.waited:
	case <-time.After(drainTestWait):
		t.Fatal("the hub front's broker wait did not end once the broker stopped")
	}
	assert.True(t, f.openAtWaitEnd.Load(), "hub front listener closed before the broker wait ended")
	select {
	case err := <-f.served:
		assert.NoError(t, err)
	case <-time.After(drainTestWait):
		t.Fatal("the hub front did not finish its shutdown")
	}
	assert.False(t, drainTestDialable(f.addr), "the hub front's listener is closed after its HTTP drain")
}

// drainTestDockerInstances are two co-located docker flat instances.
func drainTestDockerInstances() []config.V1RuntimeBrokerInstanceConfig {
	return []config.V1RuntimeBrokerInstanceConfig{
		{Key: "docker-a", Name: "a", RuntimeTarget: &config.V1RuntimeTargetConfig{Type: "docker"}},
		{Key: "docker-b", Name: "b", RuntimeTarget: &config.V1RuntimeTargetConfig{Type: "docker"}},
	}
}

// TestColocatedShutdown_FlatHostInBrokerWait: the production flat host
// (startFlatRuntimeBrokerHost) joins the broker WaitGroup the hub front
// waits on, so the front's HTTP drain begins only after the flat host's
// shutdown (its instance shutdown included) has finished; the front's
// listener stays open throughout. That each instance's shutdown runs
// DrainLaunches is pinned by
// TestColocatedShutdown_FlatServersDrainBeforeHubHTTPDrain.
func TestColocatedShutdown_FlatHostInBrokerWait(t *testing.T) {
	_, globalDir := brokerTestHome(t)
	// startFlatRuntimeBrokerHost builds real docker runtimes and runs the
	// ownership preflight (docker ps): a docker that lists nothing makes
	// the test independent of the host's docker and its containers.
	fakeBin := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(fakeBin, "docker"), []byte("#!/bin/sh\nexit 0\n"), 0o755))
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	origHub, origSim, origProber := enableHub, simulateRemoteBroker, flatScopeProber
	t.Cleanup(func() { enableHub, simulateRemoteBroker, flatScopeProber = origHub, origSim, origProber })
	enableHub, simulateRemoteBroker = true, false
	flatScopeProber = func(_ context.Context, inst config.V1RuntimeBrokerInstanceConfig, _ runtime.Runtime) (brokeridentity.ExecutionScope, error) {
		return brokeridentity.ExecutionScope{Type: "docker", Docker: &brokeridentity.DockerScope{DaemonID: "daemon-" + inst.Key}}, nil
	}
	s := newTestStore(t)
	hubSrv := flatAuthTestHub(t, s)
	f := startDrainTestFront(t, hubSrv, nil)

	brokerPort := drainTestFreePort(t)
	brokerAddr := fmt.Sprintf("127.0.0.1:%d", brokerPort)
	cfg := &config.GlobalConfig{RuntimeBroker: config.RuntimeBrokerConfig{
		Enabled: true, Host: "127.0.0.1", Port: brokerPort, HubEndpoint: "http://" + f.addr,
	}}
	var brokerWG sync.WaitGroup
	f.brokerStop.markStarted()
	errCh := make(chan error, 4)
	require.NoError(t, startFlatRuntimeBrokerHost(f.ctx, flatHostParams{
		cfg: cfg, hubSrv: hubSrv, webSrv: f.web, store: s, instances: drainTestDockerInstances(),
		hubEndpoint: "http://" + f.addr, settings: &config.Settings{}, globalDir: globalDir,
		wg: &brokerWG, errCh: errCh,
	}))
	f.awaitBrokers(&brokerWG)
	drainTestAwaitListening(t, brokerAddr, "the flat Runtime Broker host")
	require.Equal(t, []string{string(brokerhost.StateActive), string(brokerhost.StateActive)}, drainTestHealthStates(f.addr),
		"both flat instances serve before the shutdown")
	hold := drainTestHold(t, brokerAddr)

	f.shutdownHolding(t, hold, func() {
		// The host stops its instances before it drains its listener, so
		// while the hold keeps the host's shutdown open every instance
		// has already stopped; read through the front's /healthz (the
		// host's broker health provider), which still serves.
		require.Eventually(t, func() bool {
			states := drainTestHealthStates(f.addr)
			return len(states) == 2 && states[0] == string(brokerhost.StateStopped) && states[1] == string(brokerhost.StateStopped)
		}, drainTestWait, 20*time.Millisecond, "flat instances did not stop during the hub front's broker wait")
	})
	// Every broker goroutine has returned (each sends its error before
	// its Done), so errCh holds any error they reported.
	brokerWG.Wait()
	select {
	case err := <-errCh:
		t.Fatalf("broker error: %v", err)
	default:
	}
}

// TestColocatedShutdown_FlatServersDrainBeforeHubHTTPDrain: every active
// flat instance of a co-located host has begun its launch drain
// (DrainLaunches; Draining) and finished its shutdown by the time the hub
// front's broker wait ends, and the front still accepts connections then.
// The host's Run joins the broker WaitGroup as startFlatRuntimeBrokerHost
// does (cmd/server_flat_host.go:497-503; that join itself is pinned by
// TestColocatedShutdown_FlatHostInBrokerWait).
func TestColocatedShutdown_FlatServersDrainBeforeHubHTTPDrain(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s := newTestStore(t)
	hubSrv := flatAuthTestHub(t, s)
	globalDir := t.TempDir()

	var active []brokerhost.ActiveInstance
	drainedAtWaitEnd := map[string]bool{}
	f := startDrainTestFront(t, hubSrv, func() {
		for _, a := range active {
			drainedAtWaitEnd[a.Context.Identity.RuntimeBrokerID] = a.Server.Draining()
		}
	})

	brokerPort := drainTestFreePort(t)
	brokerAddr := fmt.Sprintf("127.0.0.1:%d", brokerPort)
	h, err := brokerhost.New(brokerhost.Config{
		GlobalDir: globalDir,
		Instances: drainTestDockerInstances(),
		Mode:      brokerhost.ModeColocated,
		NewRuntime: func(context.Context, config.V1RuntimeBrokerInstanceConfig) (runtime.Runtime, error) {
			return &runtime.MockRuntime{NameFunc: func() string { return "docker" }}, nil
		},
		ProbeScope: func(_ context.Context, inst config.V1RuntimeBrokerInstanceConfig, _ runtime.Runtime) (brokeridentity.ExecutionScope, error) {
			return brokeridentity.ExecutionScope{Type: "docker", Docker: &brokeridentity.DockerScope{DaemonID: "daemon-" + inst.Key}}, nil
		},
		Activator: &colocatedFlatActivator{hubSrv: hubSrv, endpoint: "http://" + brokerAddr},
		BuildServer: func(ic brokerhost.InstanceContext) (*runtimebroker.Server, error) {
			cfg := runtimebroker.DefaultServerConfig()
			cfg.BrokerID = ic.Identity.RuntimeBrokerID
			cfg.StateDir = filepath.Join(globalDir, "state", ic.Identity.RuntimeBrokerID)
			// A Hub connection, as every instance of a multi-instance host
			// needs to serve (Run stops one without); its endpoint is never
			// dialled successfully, so nothing else runs.
			cfg.HubEnabled = true
			cfg.HubEndpoint = "http://127.0.0.1:1"
			cfg.InMemoryCredentials = ic.Activation.InMemoryCredentials
			cfg.FlatInstance = &runtimebroker.FlatInstanceConfig{Identity: ic.Identity, Instance: ic.Instance, HubInProcess: true}
			return runtimebroker.New(cfg, ic.Manager, ic.Runtime, config.RuntimeBuildRecord{}), nil
		},
		Listener: brokerhost.ListenerConfig{Host: "127.0.0.1", Port: brokerPort},
	})
	require.NoError(t, err)
	require.NoError(t, h.Prepare(context.Background()))
	active = h.Active()
	require.Len(t, active, 2)

	var brokerWG sync.WaitGroup
	f.brokerStop.markStarted()
	// Mirrors startFlatRuntimeBrokerHost (cmd/server_flat_host.go:497-503).
	brokerWG.Add(1)
	go func() {
		defer brokerWG.Done()
		assert.NoError(t, h.Run(f.ctx))
	}()
	f.awaitBrokers(&brokerWG)
	drainTestAwaitListening(t, brokerAddr, "the flat Runtime Broker host")
	for _, st := range h.Status() {
		require.Equal(t, brokerhost.StateActive, st.State, "flat instance %s serves before the shutdown: %s", st.Key, st.Error)
	}
	for _, a := range active {
		require.False(t, a.Server.Draining(), "flat instance %s drains before the shutdown", a.Context.Identity.RuntimeBrokerID)
	}
	hold := drainTestHold(t, brokerAddr)

	f.shutdownHolding(t, hold, func() {
		// The host stops its instances before it drains its listener, so
		// while the hold keeps the host's shutdown open every instance
		// has already stopped, with the front still open.
		require.Eventually(t, func() bool {
			for _, st := range h.Status() {
				if st.State != brokerhost.StateStopped {
					return false
				}
			}
			return true
		}, drainTestWait, 10*time.Millisecond, "flat instances did not stop during the hub front's broker wait")
	})
	brokerWG.Wait()
	require.Len(t, drainedAtWaitEnd, 2)
	for id, drained := range drainedAtWaitEnd {
		assert.True(t, drained, "flat instance %s had not run DrainLaunches before the hub HTTP drain", id)
	}
}

// TestColocatedShutdown_LegacyBrokerDrainsBeforeHubHTTPDrain: the legacy
// co-located Runtime Broker has begun its launch drain (DrainLaunches;
// Draining) by the time the hub front's broker wait ends, and the front
// still accepts connections then. Its Start joins the broker WaitGroup
// as startRuntimeBroker does (cmd/server_foreground.go:4385-4391); that
// production join is covered by TestColocatedBrokerStop_BeforeHTTPDrain's
// wait helper, not here (startRuntimeBroker needs a registered broker and
// a detected runtime).
func TestColocatedShutdown_LegacyBrokerDrainsBeforeHubHTTPDrain(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	brokerPort := drainTestFreePort(t)
	brokerAddr := fmt.Sprintf("127.0.0.1:%d", brokerPort)
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	cfg := runtimebroker.DefaultServerConfig()
	cfg.Host = "127.0.0.1"
	cfg.Port = brokerPort
	cfg.BrokerID = "legacy-colocated"
	cfg.StateDir = t.TempDir()
	rhSrv := runtimebroker.New(cfg, agent.NewManager(rt), rt, config.RuntimeBuildRecord{})

	var drainedAtWaitEnd atomic.Bool
	f := startDrainTestFront(t, nil, func() { drainedAtWaitEnd.Store(rhSrv.Draining()) })
	plan := serverShutdownPlan{q: f.q}
	rhSrv.SetShutdownContext(plan.brokerContext)

	var brokerWG sync.WaitGroup
	f.brokerStop.markStarted()
	// Mirrors startRuntimeBroker (cmd/server_foreground.go:4385-4391).
	brokerWG.Add(1)
	go func() {
		defer brokerWG.Done()
		assert.NoError(t, rhSrv.Start(f.ctx))
	}()
	f.awaitBrokers(&brokerWG)
	drainTestAwaitListening(t, brokerAddr, "the legacy Runtime Broker")
	require.False(t, rhSrv.Draining(), "the legacy broker drains before the shutdown")
	hold := drainTestHold(t, brokerAddr)

	f.shutdownHolding(t, hold, nil)
	brokerWG.Wait()
	assert.True(t, drainedAtWaitEnd.Load(), "legacy broker had not run DrainLaunches before the hub HTTP drain")
}

// drainTestHealthStates reads the flat instances' states from a web
// server's /healthz broker section (nil when it cannot be read; it runs
// inside require.Eventually, so it must not fail the test itself).
func drainTestHealthStates(addr string) []string {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://" + addr + "/healthz")
	if err != nil {
		return nil
	}
	defer func() { _ = resp.Body.Close() }()
	var body struct {
		Broker struct {
			Instances []brokerhost.InstanceStatus `json:"instances"`
		} `json:"broker"`
	}
	if json.NewDecoder(resp.Body).Decode(&body) != nil {
		return nil
	}
	var states []string
	for _, st := range body.Broker.Instances {
		states = append(states, string(st.State))
	}
	return states
}
