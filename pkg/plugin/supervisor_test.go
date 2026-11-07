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

// These tests use Linux process facilities (/proc, SIGSTOP).

//go:build linux

package plugin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/eventbus"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	goplugin "github.com/hashicorp/go-plugin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// supervisorHelperEnv makes the test binary run as a fake broker plugin
// (see TestMain). Its value is irrelevant.
const supervisorHelperEnv = "PLUGIN_SUPERVISOR_TEST_HELPER"

// supervisorPidDirEnv, when set, makes each fake plugin process record its
// pid as a file in that directory.
const supervisorPidDirEnv = "PLUGIN_SUPERVISOR_TEST_PIDDIR"

// Publish topics the fake broker plugin acts on.
const (
	fakeTopicExit  = "test.exit"  // exit the plugin process
	fakeTopicSever = "test.sever" // shut down the plugin's sockets, process stays alive
	fakeTopicBlock = "test.block" // block until the plugin is closed
	fakeTopicFail  = "test.fail"  // return an error from the plugin
)

func TestMain(m *testing.M) {
	if os.Getenv(supervisorHelperEnv) != "" {
		if dir := os.Getenv(supervisorPidDirEnv); dir != "" {
			_ = os.WriteFile(dir+"/"+strconv.Itoa(os.Getpid()), nil, 0o600)
		}
		goplugin.Serve(&goplugin.ServeConfig{
			HandshakeConfig: goplugin.HandshakeConfig{
				ProtocolVersion:  BrokerPluginProtocolVersion,
				MagicCookieKey:   MagicCookieKey,
				MagicCookieValue: MagicCookieValue,
			},
			Plugins: map[string]goplugin.Plugin{
				BrokerPluginName: &BrokerPlugin{Impl: newFakeBroker()},
			},
		})
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// fakeBroker is a MessageBrokerPluginInterface that records what it was
// told and reports it in its HealthCheck details: pid, the configured
// "token", the subscribed patterns and the number of Configure calls.
type fakeBroker struct {
	mu         sync.Mutex
	token      string
	configures int
	subs       map[string]bool
	published  int
	block      chan struct{}
	// closed is set by Close and makes Subscribe and Publish fail, as in
	// the real chat plugins (a closed broker stays closed).
	closed bool
	closes int
}

func newFakeBroker() *fakeBroker {
	return &fakeBroker{subs: map[string]bool{}, block: make(chan struct{})}
}

// fakeTokenFailConfigure makes the fake broker's Configure fail.
const fakeTokenFailConfigure = "fail-configure"

func (f *fakeBroker) Configure(cfg map[string]string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.configures++
	if cfg["token"] == fakeTokenFailConfigure {
		return errors.New("configure refused")
	}
	f.token = cfg["token"]
	return nil
}

func (f *fakeBroker) Publish(_ context.Context, topic string, _ *messages.StructuredMessage) error {
	f.mu.Lock()
	closed := f.closed
	f.mu.Unlock()
	if closed {
		return errors.New("broker is closed")
	}
	switch topic {
	case fakeTopicExit:
		os.Exit(3)
	case fakeTopicSever:
		severSockets()
		return nil
	case fakeTopicBlock:
		<-f.block
		return nil
	case fakeTopicFail:
		return errors.New("plugin refused the message")
	}
	f.mu.Lock()
	f.published++
	f.mu.Unlock()
	return nil
}

func (f *fakeBroker) Subscribe(pattern string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return errors.New("broker is closed")
	}
	f.subs[pattern] = true
	return nil
}

func (f *fakeBroker) Unsubscribe(pattern string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.subs, pattern)
	return nil
}

func (f *fakeBroker) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	f.closes++
	return nil
}

func (f *fakeBroker) closeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closes
}

func (f *fakeBroker) GetInfo() (*PluginInfo, error) {
	return &PluginInfo{Name: "fake", Version: "1.0.0"}, nil
}

func (f *fakeBroker) HealthCheck() (*HealthStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	subs := make([]string, 0, len(f.subs))
	for s := range f.subs {
		subs = append(subs, s)
	}
	sort.Strings(subs)
	return &HealthStatus{Status: "healthy", Details: map[string]string{
		"pid":        strconv.Itoa(os.Getpid()),
		"token":      f.token,
		"subs":       strings.Join(subs, ","),
		"configures": strconv.Itoa(f.configures),
	}}, nil
}

// severSockets shuts down every socket of this process, which ends the
// go-plugin session while the process keeps running.
func severSockets() {
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return
	}
	for _, e := range entries {
		target, err := os.Readlink("/proc/self/fd/" + e.Name())
		if err != nil || !strings.HasPrefix(target, "socket:") {
			continue
		}
		if fd, err := strconv.Atoi(e.Name()); err == nil {
			_ = syscall.Shutdown(fd, syscall.SHUT_RDWR)
		}
	}
}

// shortenSupervisorTiming makes the supervisor fast for a test. Its
// cleanup restores the values; register it before startSupervisorForTest,
// whose cleanup (run first) waits for the supervisor to stop reading them.
func shortenSupervisorTiming(t *testing.T) {
	t.Helper()
	oldCheck, oldPing, oldInit, oldMax, oldStep, oldStop := supervisorCheckInterval, supervisorPingTimeout, backoffInitial, backoffMax, restartStepTimeout, gracefulStopTimeout
	supervisorCheckInterval = 50 * time.Millisecond
	supervisorPingTimeout = time.Second
	backoffInitial = 20 * time.Millisecond
	backoffMax = 200 * time.Millisecond
	restartStepTimeout = 5 * time.Second
	gracefulStopTimeout = time.Second
	t.Cleanup(func() {
		supervisorCheckInterval, supervisorPingTimeout, backoffInitial, backoffMax, restartStepTimeout, gracefulStopTimeout = oldCheck, oldPing, oldInit, oldMax, oldStep, oldStop
	})
}

// startSupervisorForTest starts m's supervisor and stops it, waiting for
// its goroutines, at the end of the test.
func startSupervisorForTest(t *testing.T, m *Manager) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		m.BeginShutdown()
		m.kickSupervisor()
		m.waitSupervisorStopped()
	})
	m.StartSupervisor(ctx)
}

// fakeHealth returns the fake broker's HealthCheck details through the
// manager's current client.
func fakeHealth(t *testing.T, m *Manager, name string) map[string]string {
	t.Helper()
	status, _, details, err := m.BrokerHealthCheck(name)
	require.NoError(t, err)
	require.Equal(t, "healthy", status)
	return details
}

// waitFor polls cond until it is true or the timeout passes.
func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// waitRestarted waits until the plugin is running again after n restarts.
func waitRestarted(t *testing.T, m *Manager, name string, n int) {
	t.Helper()
	waitFor(t, 30*time.Second, fmt.Sprintf("%s restart %d", name, n), func() bool {
		st, ok := m.BrokerSupervision(name)
		return ok && st.State == SupervisionRunning && st.Restarts >= n
	})
}

// hubManagedFixture loads the test binary as a hub-managed fake broker
// plugin "fake" with a fan-out bus around it, a config provider and a
// restart hook that replaces the fan-out spoke (as the hub does).
type hubManagedFixture struct {
	m         *Manager
	fanout    *eventbus.FanOutEventBus
	hookCalls atomic.Int32
	hookErr   atomic.Pointer[error]
}

func newHubManagedFixture(t *testing.T) *hubManagedFixture {
	t.Helper()
	return newHubManagedFixtureWithInterval(t, 0)
}

// newHubManagedFixtureWithInterval is newHubManagedFixture with the given
// supervisor check interval (0 keeps the shortened default).
func newHubManagedFixtureWithInterval(t *testing.T, interval time.Duration) *hubManagedFixture {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("helper-process plugin tests need Linux")
	}
	shortenSupervisorTiming(t)
	if interval > 0 {
		supervisorCheckInterval = interval
	}
	t.Setenv(supervisorHelperEnv, "1")

	fx := &hubManagedFixture{m: NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))}
	t.Cleanup(fx.m.Shutdown)
	exe, err := os.Executable()
	require.NoError(t, err)
	require.NoError(t, fx.m.LoadOne(PluginTypeBroker, "fake", PluginEntry{
		Path:   exe,
		Config: map[string]string{"token": "boot"},
	}, ""))

	fx.m.SetBrokerConfigProvider(func(_ context.Context, name string) (map[string]string, error) {
		return map[string]string{"token": "provided-" + name}, nil
	})
	bus, err := fx.m.GetBroker("fake")
	require.NoError(t, err)
	fx.fanout = eventbus.NewFanOutEventBus([]eventbus.NamedEventBus{
		{Name: eventbus.InProcessBusName, Bus: eventbus.NewInProcessEventBus(slog.New(slog.NewTextHandler(io.Discard, nil)))},
		{Name: "fake", Bus: bus},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	_, err = fx.fanout.Subscribe("agent.>", func(context.Context, string, *messages.StructuredMessage) {})
	require.NoError(t, err)

	fx.m.OnBrokerRestart(func(name string) error {
		fx.hookCalls.Add(1)
		if p := fx.hookErr.Load(); p != nil && *p != nil {
			return *p
		}
		nb, err := fx.m.GetBroker(name)
		if err != nil {
			return err
		}
		return fx.fanout.ReplaceSpoke(name, eventbus.NamedEventBus{Name: name, Bus: nb})
	})
	startSupervisorForTest(t, fx.m)
	return fx
}

func (fx *hubManagedFixture) publish(topic string) error {
	return fx.fanout.Publish(context.Background(), topic, messages.NewInstruction("user:alice", "agent:bot", "hi"))
}

func processGone(pid int) bool {
	err := syscall.Kill(pid, 0)
	return errors.Is(err, syscall.ESRCH)
}

func TestSupervisor_RespawnsAfterProcessExit(t *testing.T) {
	fx := newHubManagedFixture(t)
	before := fakeHealth(t, fx.m, "fake")
	require.Equal(t, "boot", before["token"])
	require.Equal(t, "agent.>", before["subs"])

	require.Error(t, fx.publish(fakeTopicExit))
	waitRestarted(t, fx.m, "fake", 1)

	after := fakeHealth(t, fx.m, "fake")
	assert.NotEqual(t, before["pid"], after["pid"], "a new process serves the plugin")
	assert.Equal(t, "provided-fake", after["token"], "the respawn uses the provider's config")
	assert.Equal(t, int32(1), fx.hookCalls.Load())
	assert.NoError(t, fx.publish("agent.bot.message"), "publish through the replaced spoke")
}

func TestSupervisor_RespawnsAfterSessionClose(t *testing.T) {
	fx := newHubManagedFixture(t)
	before := fakeHealth(t, fx.m, "fake")
	oldPid, err := strconv.Atoi(before["pid"])
	require.NoError(t, err)

	require.Error(t, fx.publish(fakeTopicSever))
	waitRestarted(t, fx.m, "fake", 1)

	after := fakeHealth(t, fx.m, "fake")
	assert.NotEqual(t, before["pid"], after["pid"])
	waitFor(t, 10*time.Second, "old plugin process killed", func() bool { return processGone(oldPid) })
	assert.NoError(t, fx.publish("agent.bot.message"))
}

func TestSupervisor_RestartReplaysSubscriptions(t *testing.T) {
	fx := newHubManagedFixture(t)
	require.Error(t, fx.publish(fakeTopicExit))
	waitRestarted(t, fx.m, "fake", 1)
	assert.Equal(t, "agent.>", fakeHealth(t, fx.m, "fake")["subs"],
		"the new process received the fan-out subscription")
}

func TestSupervisor_HookFailureKeepsRestarting(t *testing.T) {
	fx := newHubManagedFixture(t)
	hookErr := errors.New("spoke not replaced")
	fx.hookErr.Store(&hookErr)

	require.Error(t, fx.publish(fakeTopicExit))
	waitFor(t, 30*time.Second, "a failed restart attempt", func() bool {
		st, _ := fx.m.BrokerSupervision("fake")
		return fx.hookCalls.Load() >= 1 && st.State != SupervisionRunning && st.Attempt >= 2
	})
	st, _ := fx.m.BrokerSupervision("fake")
	assert.Equal(t, 0, st.Restarts)

	var none error
	fx.hookErr.Store(&none)
	waitRestarted(t, fx.m, "fake", 1)
	assert.NoError(t, fx.publish("agent.bot.message"))
}

func TestBrokerSupervision_DegradedThenRecovered(t *testing.T) {
	fx := newHubManagedFixture(t)
	st, ok := fx.m.BrokerSupervision("fake")
	require.True(t, ok)
	require.Equal(t, SupervisionRunning, st.State)

	// Hold the restart back so the outage is observable.
	hookErr := errors.New("hold")
	fx.hookErr.Store(&hookErr)
	require.Error(t, fx.publish(fakeTopicExit))
	waitFor(t, 30*time.Second, "restarting state", func() bool {
		st, _ := fx.m.BrokerSupervision("fake")
		return st.State == SupervisionRestarting || st.State == SupervisionFailing
	})
	st, _ = fx.m.BrokerSupervision("fake")
	assert.GreaterOrEqual(t, st.DroppedTotal, int64(1))
	assert.GreaterOrEqual(t, st.DroppedThisOutage, int64(1))
	assert.NotEmpty(t, st.LastError)

	var none error
	fx.hookErr.Store(&none)
	waitRestarted(t, fx.m, "fake", 1)
	st, _ = fx.m.BrokerSupervision("fake")
	assert.Equal(t, int64(0), st.DroppedThisOutage, "reset on recovery")
	assert.GreaterOrEqual(t, st.DroppedTotal, int64(1), "cumulative")
}

// selfManagedServer serves a fakeBroker on TCP as a self-managed plugin
// does (see extras/scion-broker-log), and can drop its connections while
// it keeps listening.
type selfManagedServer struct {
	impl  *fakeBroker
	addr  string
	mu    sync.Mutex
	conns []net.Conn
	lis   net.Listener
}

type trackingListener struct {
	net.Listener
	s *selfManagedServer
}

func (l trackingListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err == nil {
		l.s.mu.Lock()
		l.s.conns = append(l.s.conns, c)
		l.s.mu.Unlock()
	}
	return c, err
}

func startSelfManagedServer(t *testing.T) *selfManagedServer {
	t.Helper()
	s := &selfManagedServer{impl: newFakeBroker(), addr: "127.0.0.1:0"}
	require.NoError(t, s.listen())
	t.Cleanup(func() {
		close(s.impl.block)
		s.stop()
	})
	return s
}

// listen starts serving on s.addr (the first call picks a free port).
func (s *selfManagedServer) listen() error {
	lis, err := net.Listen("tcp", s.addr)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.lis = lis
	s.addr = lis.Addr().String()
	s.mu.Unlock()
	stdoutR, _ := io.Pipe()
	stderrR, _ := io.Pipe()
	srv := &goplugin.RPCServer{
		Plugins: map[string]goplugin.Plugin{BrokerPluginName: &BrokerPlugin{Impl: s.impl}},
		Stdout:  stdoutR,
		Stderr:  stderrR,
		DoneCh:  make(chan struct{}),
	}
	go srv.Serve(trackingListener{Listener: lis, s: s})
	return nil
}

// stop closes the listener and every connection, as a plugin process
// that went away does.
func (s *selfManagedServer) stop() {
	s.mu.Lock()
	lis := s.lis
	s.lis = nil
	s.mu.Unlock()
	if lis != nil {
		_ = lis.Close()
	}
	s.dropConns()
}

func (s *selfManagedServer) dropConns() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.conns {
		_ = c.Close()
	}
	s.conns = nil
}

func (s *selfManagedServer) configures() int {
	s.impl.mu.Lock()
	defer s.impl.mu.Unlock()
	return s.impl.configures
}

func newSelfManagedManager(t *testing.T, s *selfManagedServer) (*Manager, *atomic.Int32) {
	t.Helper()
	shortenSupervisorTiming(t)
	m := NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(m.Shutdown)
	require.NoError(t, m.LoadOne(PluginTypeBroker, "ext", PluginEntry{
		SelfManaged: true,
		Address:     s.addr,
		Config:      map[string]string{"token": "boot"},
	}, ""))
	m.SetBrokerConfigProvider(func(context.Context, string) (map[string]string, error) {
		return map[string]string{"token": "provided"}, nil
	})
	hooks := &atomic.Int32{}
	m.OnBrokerRestart(func(string) error { hooks.Add(1); return nil })
	startSupervisorForTest(t, m)
	return m, hooks
}

func TestSupervisor_SelfManagedRedialsWithProvidedConfig(t *testing.T) {
	s := startSelfManagedServer(t)
	m, hooks := newSelfManagedManager(t, s)
	require.Equal(t, 1, s.configures())

	s.dropConns()
	waitRestarted(t, m, "ext", 1)
	assert.Equal(t, "provided", fakeHealth(t, m, "ext")["token"])
	assert.Equal(t, int32(1), hooks.Load())
}

func TestSupervisor_SelfManagedSingleRedial(t *testing.T) {
	s := startSelfManagedServer(t)
	m, _ := newSelfManagedManager(t, s)
	bus, err := m.GetBroker("ext")
	require.NoError(t, err)

	s.dropConns()
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = bus.Publish(context.Background(), "agent.x", messages.NewInstruction("user:a", "agent:b", "hi"))
		}()
	}
	wg.Wait()
	waitRestarted(t, m, "ext", 1)
	time.Sleep(5 * supervisorCheckInterval)
	st, _ := m.BrokerSupervision("ext")
	assert.Equal(t, 1, st.Restarts)
	assert.Equal(t, 2, s.configures(), "one Configure at load and one for the single re-dial")
}

func TestBrokerPluginAdapter_PublishHonoursContext(t *testing.T) {
	s := startSelfManagedServer(t)
	m, _ := newSelfManagedManager(t, s)
	bus, err := m.GetBroker("ext")
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	err = bus.Publish(ctx, fakeTopicBlock, messages.NewInstruction("user:a", "agent:b", "hi"))
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, time.Since(start), 5*time.Second)

	// A deadline expiry does not restart the plugin.
	time.Sleep(10 * supervisorCheckInterval)
	st, _ := m.BrokerSupervision("ext")
	assert.Equal(t, SupervisionRunning, st.State)
	assert.Equal(t, 0, st.Restarts)
	assert.Equal(t, int64(0), st.DroppedTotal)
}

func TestBrokerPluginAdapter_BurstNoLostReplies(t *testing.T) {
	s := startSelfManagedServer(t)
	m, _ := newSelfManagedManager(t, s)
	bus, err := m.GetBroker("ext")
	require.NoError(t, err)

	var wg sync.WaitGroup
	errs := make([]error, 200)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx := context.Background()
			if i%2 == 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, time.Microsecond)
				defer cancel()
			}
			errs[i] = bus.Publish(ctx, "agent.x", messages.NewInstruction("user:a", "agent:b", "hi"))
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if i%2 == 1 {
			assert.NoError(t, err, "caller %d waiting for its reply", i)
		} else if err != nil {
			assert.ErrorIs(t, err, context.DeadlineExceeded)
		}
	}
	waitFor(t, 10*time.Second, "all publishes answered", func() bool {
		s.impl.mu.Lock()
		defer s.impl.mu.Unlock()
		return s.impl.published == len(errs)
	})
	st, _ := m.BrokerSupervision("ext")
	assert.Equal(t, SupervisionRunning, st.State)
}

func TestBrokerPluginAdapter_TransportErrorKicks(t *testing.T) {
	impl := newFakeBroker()
	client := startTestBrokerRPCServer(t, impl)
	var calls []string
	var mu sync.Mutex
	client.onTransportError = func(method, topic, _ string, _ bool, err error) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, method+" "+topic)
	}
	msg := messages.NewInstruction("user:a", "agent:b", "hi")

	// An error returned by the plugin is not a transport error.
	require.Error(t, client.Publish(context.Background(), fakeTopicFail, msg))
	mu.Lock()
	assert.Empty(t, calls)
	mu.Unlock()

	// A closed connection is.
	require.NoError(t, client.client.Close())
	err := client.Publish(context.Background(), "agent.x", msg)
	require.Error(t, err)
	mu.Lock()
	assert.Equal(t, []string{"Publish agent.x"}, calls)
	mu.Unlock()

	assert.True(t, isTransportError(io.ErrUnexpectedEOF))
	assert.True(t, isTransportError(io.EOF))
	assert.False(t, isTransportError(context.DeadlineExceeded))
	assert.False(t, isTransportError(fmt.Errorf("wrapped: %w", context.Canceled)))
}

func TestBackoffDelay(t *testing.T) {
	old := [2]time.Duration{backoffInitial, backoffMax}
	backoffInitial, backoffMax = time.Second, 5*time.Minute
	t.Cleanup(func() { backoffInitial, backoffMax = old[0], old[1] })

	for n, want := range map[int]time.Duration{1: time.Second, 2: 2 * time.Second, 3: 4 * time.Second, 9: 256 * time.Second, 10: 5 * time.Minute, 50: 5 * time.Minute} {
		assert.Equal(t, want, backoffDelay(n, 0.5), "attempt %d without jitter", n)
		assert.Equal(t, time.Duration(float64(want)*0.8), backoffDelay(n, 0), "attempt %d low jitter", n)
		assert.InDelta(t, float64(want)*1.2, float64(backoffDelay(n, 0.999999)), float64(time.Millisecond), "attempt %d high jitter", n)
	}
}

// fakeClock is a settable clock for the supervisor.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func TestSupervisor_BackoffCapAndReset(t *testing.T) {
	old := [2]time.Duration{backoffInitial, backoffMax}
	backoffInitial, backoffMax = time.Second, 5*time.Minute
	t.Cleanup(func() { backoffInitial, backoffMax = old[0], old[1] })

	m := NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
	clock := &fakeClock{t: time.Unix(1_000_000, 0)}
	m.sup.now = clock.now
	m.sup.jitter = func() float64 { return 0.5 }
	key := PluginTypeBroker + ":fake"
	lost := &goplugin.Client{}
	m.clients[key] = lost
	m.supervisedLoaded(key)

	// First loss: attempt due at once.
	m.markLost(key, lost, "plugin session lost")
	sp := m.sup.plugins[key]
	assert.Equal(t, clock.now(), sp.nextAttempt)

	// Failed attempts back off 1 s, 2 s, 4 s, ... up to 5 min; failing
	// after 5.
	for attempt := 1; attempt <= 12; attempt++ {
		sp.status.Attempt = attempt
		m.attemptFailed(key, attempt, errors.New("boom"))
		if attempt == 1 {
			assert.Equal(t, time.Second, sp.nextAttempt.Sub(clock.now()), "first retry after backoffInitial")
		}
		assert.Equal(t, backoffDelay(attempt, 0.5), sp.nextAttempt.Sub(clock.now()), "after attempt %d", attempt)
		if attempt >= failingAfter {
			assert.Equal(t, SupervisionFailing, sp.status.State)
		} else {
			assert.Equal(t, SupervisionRestarting, sp.status.State)
		}
	}
	assert.Equal(t, 5*time.Minute, sp.nextAttempt.Sub(clock.now()), "capped")

	// A kick or check before the scheduled time never starts an attempt
	// early (a crash-looping plugin keeps its backoff).
	clock.advance(time.Minute)
	for i := 0; i < 5; i++ {
		m.kickSupervisor()
		m.superviseOnce(context.Background())
	}
	assert.False(t, sp.busy)
	assert.Equal(t, 12, sp.status.Attempt)

	// After a successful restart the counter is kept, so a plugin dying
	// again soon waits the next backoff instead of restarting at once.
	m.supervisedLoaded(key)
	m.markLost(key, lost, "plugin process exited")
	assert.Equal(t, 5*time.Minute, sp.nextAttempt.Sub(clock.now()))

	// It resets once the plugin has stayed up backoffResetAfter.
	m.supervisedLoaded(key)
	clock.advance(backoffResetAfter)
	assert.True(t, sp.resetAttemptsIfStable(clock.now()))
	assert.Equal(t, 0, sp.status.Attempt)
	m.markLost(key, lost, "plugin process exited")
	assert.Equal(t, clock.now(), sp.nextAttempt)
}

func TestSupervisor_SkipsReplacedClient(t *testing.T) {
	m := NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
	key := PluginTypeBroker + ":fake"
	lost, replacement := &goplugin.Client{}, &goplugin.Client{}
	m.clients[key] = lost
	m.supervisedLoaded(key)
	m.markLost(key, lost, "plugin session lost")

	m.clients[key] = replacement // an admin Restart replaced it
	m.restartAttempt(context.Background(), key)
	st, _ := m.BrokerSupervision("fake")
	assert.Equal(t, SupervisionRunning, st.State)
	assert.Equal(t, 0, st.Restarts)
	assert.Same(t, replacement, m.clients[key])
}

func TestSupervisor_StopsOnShutdown(t *testing.T) {
	m := NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
	key := PluginTypeBroker + ":fake"
	lost := &goplugin.Client{}
	m.clients[key] = lost
	m.supervisedLoaded(key)
	m.markLost(key, lost, "plugin session lost")

	m.BeginShutdown()
	m.restartAttempt(context.Background(), key)
	st, _ := m.BrokerSupervision("fake")
	assert.Equal(t, SupervisionRestarting, st.State)
	assert.Equal(t, 0, st.Attempt, "no attempt made after shutdown began")
}

// setFailingAfter lowers failingAfter for a test.
func setFailingAfter(t *testing.T, n int) {
	t.Helper()
	old := failingAfter
	failingAfter = n
	t.Cleanup(func() { failingAfter = old })
}

func TestSupervisor_FailedLoadIsRetriedSelfManaged(t *testing.T) {
	setFailingAfter(t, 2)
	s := startSelfManagedServer(t)
	m, hooks := newSelfManagedManager(t, s)

	// The plugin goes away and stays away: every re-dial fails.
	s.stop()
	waitFor(t, 30*time.Second, "repeated failed re-dials reaching failing", func() bool {
		st, _ := m.BrokerSupervision("ext")
		return st.State == SupervisionFailing && st.Attempt > 2
	})
	assert.Equal(t, int32(0), hooks.Load())

	// It comes back on the same address: the next attempt succeeds.
	require.NoError(t, s.listen())
	waitRestarted(t, m, "ext", 1)
	assert.Equal(t, "provided", fakeHealth(t, m, "ext")["token"])
	assert.Equal(t, int32(1), hooks.Load())
}

func TestSupervisor_FailedLoadIsRetriedHubManaged(t *testing.T) {
	setFailingAfter(t, 2)
	fx := newHubManagedFixture(t)
	var failing atomic.Bool
	failing.Store(true)
	fx.m.SetBrokerConfigProvider(func(_ context.Context, name string) (map[string]string, error) {
		if failing.Load() {
			return map[string]string{"token": fakeTokenFailConfigure}, nil
		}
		return map[string]string{"token": "provided-" + name}, nil
	})

	require.Error(t, fx.publish(fakeTopicExit))
	waitFor(t, 30*time.Second, "respawns failing in Configure reaching failing", func() bool {
		st, _ := fx.m.BrokerSupervision("fake")
		return st.State == SupervisionFailing && st.Attempt > 2
	})

	failing.Store(false)
	waitRestarted(t, fx.m, "fake", 1)
	assert.Equal(t, "provided-fake", fakeHealth(t, fx.m, "fake")["token"])
	assert.NoError(t, fx.publish("agent.bot.message"))
}

func TestSupervisor_FailedReconnectIsRecovered(t *testing.T) {
	s := startSelfManagedServer(t)
	m, _ := newSelfManagedManager(t, s)

	s.stop()
	require.Error(t, m.Reconnect(PluginTypeBroker, "ext"), "re-dial to a stopped plugin fails")
	waitFor(t, 30*time.Second, "the supervisor taking over the failed reconnect", func() bool {
		st, _ := m.BrokerSupervision("ext")
		return st.State != SupervisionRunning
	})
	require.NoError(t, s.listen())
	waitRestarted(t, m, "ext", 1)
}

func TestSupervisor_RecoversThroughKicksAlone(t *testing.T) {
	// No periodic check within the test: recovery must come from the kick
	// chain (transport error -> probe -> lost -> attempt).
	fx := newHubManagedFixtureWithInterval(t, time.Hour)
	require.Error(t, fx.publish(fakeTopicExit))
	waitRestarted(t, fx.m, "fake", 1)
}

// freezingRelay forwards TCP connections to target; freeze makes every
// connection open at that moment stop forwarding without closing, as a
// hung session does, and unfreeze makes them forward again (the bytes sent
// meanwhile are lost). Connections accepted later forward normally.
type freezingRelay struct {
	lis    net.Listener
	target string
	mu     sync.Mutex
	open   []*atomic.Bool // per connection: frozen
	frozen []*atomic.Bool
	conns  []net.Conn
}

func startFreezingRelay(t *testing.T, target string) *freezingRelay {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	r := &freezingRelay{lis: lis, target: target}
	go func() {
		for {
			c, err := lis.Accept()
			if err != nil {
				return
			}
			up, err := net.Dial("tcp", target)
			if err != nil {
				_ = c.Close()
				continue
			}
			stop := &atomic.Bool{}
			r.mu.Lock()
			r.open = append(r.open, stop)
			r.conns = append(r.conns, c, up)
			r.mu.Unlock()
			go relayCopy(up, c, stop)
			go relayCopy(c, up, stop)
		}
	}()
	t.Cleanup(func() {
		_ = lis.Close()
		r.mu.Lock()
		defer r.mu.Unlock()
		for _, c := range r.conns {
			_ = c.Close()
		}
	})
	return r
}

// relayCopy copies src to dst until src fails; while frozen it keeps
// reading and discards, so the peer sees no close.
func relayCopy(dst, src net.Conn, frozen *atomic.Bool) {
	buf := make([]byte, 32*1024)
	for {
		n, err := src.Read(buf)
		if err != nil {
			return
		}
		if frozen.Load() {
			continue
		}
		if _, err := dst.Write(buf[:n]); err != nil {
			return
		}
	}
}

func (r *freezingRelay) freeze() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, f := range r.open {
		f.Store(true)
	}
	r.frozen = r.open
	r.open = nil
}

func (r *freezingRelay) unfreeze() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, f := range r.frozen {
		f.Store(false)
	}
}

func TestSupervisor_HungSessionNeedsConsecutivePingTimeouts(t *testing.T) {
	s := startSelfManagedServer(t)
	relay := startFreezingRelay(t, s.addr)
	shortenSupervisorTiming(t)
	supervisorPingTimeout = 300 * time.Millisecond

	m := NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(m.Shutdown)
	require.NoError(t, m.LoadOne(PluginTypeBroker, "ext", PluginEntry{
		SelfManaged: true,
		Address:     relay.lis.Addr().String(),
	}, ""))
	startSupervisorForTest(t, m)

	relay.freeze()
	key := PluginTypeBroker + ":ext"
	waitFor(t, 10*time.Second, "one Ping timeout", func() bool {
		m.sup.mu.Lock()
		defer m.sup.mu.Unlock()
		return m.sup.plugins[key].pingTimeouts >= 1
	})
	st, _ := m.BrokerSupervision("ext")
	if supervisorPingTimeouts > 1 {
		assert.Equal(t, SupervisionRunning, st.State, "a single Ping timeout does not mark the session lost")
		assert.Equal(t, 0, st.Restarts)
	}
	waitRestarted(t, m, "ext", 1)
}

func TestSupervisor_NoAttemptAfterShutdownBegins(t *testing.T) {
	shortenSupervisorTiming(t)
	m := NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
	clock := &fakeClock{t: time.Unix(1_000_000, 0)}
	m.sup.now = clock.now
	key := PluginTypeBroker + ":fake"
	lost := &goplugin.Client{}
	m.clients[key] = lost
	m.configs[key] = DiscoveredPlugin{Name: "fake", Type: PluginTypeBroker}
	m.supervisedLoaded(key)
	m.sup.plugins[key].status.Attempt = 3 // the next attempt waits its backoff
	m.markLost(key, lost, "plugin session lost")
	require.True(t, m.sup.plugins[key].nextAttempt.After(clock.now()))

	// The supervisor runs, shutdown begins, then the attempt becomes due.
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		m.kickSupervisor()
		m.waitSupervisorStopped()
	})
	m.StartSupervisor(ctx)
	time.Sleep(3 * supervisorCheckInterval)
	m.BeginShutdown()
	clock.advance(time.Hour)
	for i := 0; i < 5; i++ {
		m.kickSupervisor()
		time.Sleep(supervisorCheckInterval)
	}
	st, _ := m.BrokerSupervision("fake")
	assert.Equal(t, 3, st.Attempt, "no attempt once shutdown has begun")
	assert.Equal(t, SupervisionRestarting, st.State)
}

func TestSupervisor_MarkLostIgnoresReplacedClient(t *testing.T) {
	m := NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
	key := PluginTypeBroker + ":fake"
	old, current := &goplugin.Client{}, &goplugin.Client{}
	m.clients[key] = current
	m.supervisedLoaded(key)

	m.markLost(key, old, "plugin session lost")
	st, _ := m.BrokerSupervision("fake")
	assert.Equal(t, SupervisionRunning, st.State)
	assert.Equal(t, 0, st.Attempt)

	// A replaced client found at attempt time is not counted either.
	m.markLost(key, current, "plugin session lost")
	m.clients[key] = &goplugin.Client{}
	m.restartAttempt(context.Background(), key)
	st, _ = m.BrokerSupervision("fake")
	assert.Equal(t, SupervisionRunning, st.State)
	assert.Equal(t, 0, st.Attempt)
}

// spokeHook returns a restart hook that replaces the plugin's spoke in
// fanout, as the hub's does, and fails the first failFirst calls after
// replacing it (as a failed subscription replay does).
func spokeHook(m *Manager, fanout *eventbus.FanOutEventBus, failFirst int32, calls *atomic.Int32) BrokerRestartHook {
	return func(name string) error {
		n := calls.Add(1)
		nb, err := m.GetBroker(name)
		if err != nil {
			return err
		}
		if err := fanout.ReplaceSpoke(name, eventbus.NamedEventBus{Name: name, Bus: nb}); err != nil {
			return err
		}
		if n <= failFirst {
			return errors.New("subscription replay failed")
		}
		return nil
	}
}

// newSelfManagedFanout loads the self-managed plugin "ext" at addr behind a
// fan-out bus with one subscription, and wires spokeHook.
func newSelfManagedFanout(t *testing.T, addr string, failFirst int32, setup ...func()) (*Manager, *eventbus.FanOutEventBus, *atomic.Int32) {
	t.Helper()
	shortenSupervisorTiming(t)
	for _, f := range setup {
		f()
	}
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	m := NewManager(quiet)
	t.Cleanup(m.Shutdown)
	require.NoError(t, m.LoadOne(PluginTypeBroker, "ext", PluginEntry{SelfManaged: true, Address: addr}, ""))
	bus, err := m.GetBroker("ext")
	require.NoError(t, err)
	fanout := eventbus.NewFanOutEventBus([]eventbus.NamedEventBus{
		{Name: eventbus.InProcessBusName, Bus: eventbus.NewInProcessEventBus(quiet)},
		{Name: "ext", Bus: bus},
	}, quiet)
	_, err = fanout.Subscribe("agent.>", func(context.Context, string, *messages.StructuredMessage) {})
	require.NoError(t, err)
	calls := &atomic.Int32{}
	m.OnBrokerRestart(spokeHook(m, fanout, failFirst, calls))
	startSupervisorForTest(t, m)
	return m, fanout, calls
}

func TestSupervisor_SelfManagedStaleSessionNeverClosesPlugin(t *testing.T) {
	s := startSelfManagedServer(t)
	m, fanout, calls := newSelfManagedFanout(t, s.addr, 1)

	s.dropConns()
	// Attempt 1 re-dials and its hook fails after replacing the spoke;
	// attempt 2 replaces the client the failed attempt left, whose
	// session is still healthy.
	waitRestarted(t, m, "ext", 1)
	require.GreaterOrEqual(t, calls.Load(), int32(2))
	assert.Equal(t, 0, s.impl.closeCount(), "the shared plugin instance was never closed")
	assert.NoError(t, fanout.Publish(context.Background(), "agent.bot.message", messages.NewInstruction("user:a", "agent:b", "hi")))
	assert.Equal(t, "agent.>", fakeHealth(t, m, "ext")["subs"])
}

func TestSupervisor_HungSelfManagedSessionNeverClosesPlugin(t *testing.T) {
	s := startSelfManagedServer(t)
	relay := startFreezingRelay(t, s.addr)
	m, fanout, _ := newSelfManagedFanout(t, relay.lis.Addr().String(), 0, func() {
		supervisorPingTimeout = 200 * time.Millisecond
	})

	relay.freeze()
	waitRestarted(t, m, "ext", 1)
	relay.unfreeze()
	time.Sleep(20 * supervisorCheckInterval)
	assert.Equal(t, 0, s.impl.closeCount(), "nothing queued on the hung session closed the plugin")
	assert.NoError(t, fanout.Publish(context.Background(), "agent.bot.message", messages.NewInstruction("user:a", "agent:b", "hi")))
}

func TestReconnect_HookFailureLeavesPluginToSupervisor(t *testing.T) {
	s := startSelfManagedServer(t)
	m, _, calls := newSelfManagedFanout(t, s.addr, 1)

	require.Error(t, m.Reconnect(PluginTypeBroker, "ext"))
	st, _ := m.BrokerSupervision("ext")
	assert.NotEqual(t, SupervisionRunning, st.State, "a failed hook does not report running")
	waitRestarted(t, m, "ext", 1)
	assert.GreaterOrEqual(t, calls.Load(), int32(2), "the supervisor ran the hook again")
	assert.Equal(t, 0, s.impl.closeCount())
}

// publishThrough publishes one message through the fan-out and ignores the
// result (a dropped publish returns an error).
func publishThrough(fanout *eventbus.FanOutEventBus) {
	_ = fanout.Publish(context.Background(), "agent.bot.message", messages.NewInstruction("user:a", "agent:b", "hi"))
}

func TestSupervisor_OutageDropsCountedHubManaged(t *testing.T) {
	fx := newHubManagedFixture(t)
	fx.m.SetBrokerConfigProvider(func(context.Context, string) (map[string]string, error) {
		return map[string]string{"token": fakeTokenFailConfigure}, nil // every respawn fails
	})
	require.Error(t, fx.publish(fakeTopicExit))
	waitFor(t, 30*time.Second, "the old client discarded by failed attempts", func() bool {
		st, _ := fx.m.BrokerSupervision("fake")
		return st.Attempt >= 2
	})

	before, _ := fx.m.BrokerSupervision("fake")
	for i := 0; i < 3; i++ {
		publishThrough(fx.fanout)
	}
	after, _ := fx.m.BrokerSupervision("fake")
	assert.Equal(t, before.DroppedThisOutage+3, after.DroppedThisOutage)
	assert.Equal(t, before.DroppedTotal+3, after.DroppedTotal)
}

func TestSupervisor_OutageDropsCountedSelfManaged(t *testing.T) {
	s := startSelfManagedServer(t)
	m, fanout, _ := newSelfManagedFanout(t, s.addr, 0)
	s.stop()
	waitFor(t, 30*time.Second, "the old client discarded by failed attempts", func() bool {
		st, _ := m.BrokerSupervision("ext")
		return st.Attempt >= 2
	})

	before, _ := m.BrokerSupervision("ext")
	for i := 0; i < 3; i++ {
		publishThrough(fanout)
	}
	after, _ := m.BrokerSupervision("ext")
	assert.Equal(t, before.DroppedThisOutage+3, after.DroppedThisOutage)
	assert.Equal(t, before.DroppedTotal+3, after.DroppedTotal)
}

// TestClientReplacementPaths_NeverClosePlugin replaces a self-managed
// plugin's client through every path and refreshes the fan-out spoke as
// the hub does: the plugin instance, shared by all connections, never
// receives Close, the new connection works, and a publish through the old
// adapter is counted as a drop. (UpdatePlugin refuses self-managed
// plugins; it shares replaceClient with RestartBrokerPlugin.)
func TestClientReplacementPaths_NeverClosePlugin(t *testing.T) {
	for _, tc := range []struct {
		name    string
		replace func(t *testing.T, m *Manager, s *selfManagedServer)
		// counted reports whether drops are still counted afterwards
		// (Shutdown forgets every plugin).
		counted bool
		// refresh: the hub refreshes the spoke itself after this path
		// (the supervisor and Reconnect paths do it in the restart hook).
		refresh bool
	}{
		{name: "supervisor re-dial", counted: true, replace: func(t *testing.T, m *Manager, s *selfManagedServer) {
			s.dropConns()
			waitRestarted(t, m, "ext", 1)
		}},
		{name: "Reconnect", counted: true, replace: func(t *testing.T, m *Manager, _ *selfManagedServer) {
			require.NoError(t, m.Reconnect(PluginTypeBroker, "ext"))
		}},
		{name: "admin RestartBrokerPlugin", counted: true, refresh: true, replace: func(t *testing.T, m *Manager, _ *selfManagedServer) {
			require.NoError(t, m.RestartBrokerPlugin("ext", map[string]string{"token": "admin"}))
		}},
		{name: "superseding LoadOne", counted: true, refresh: true, replace: func(t *testing.T, m *Manager, s *selfManagedServer) {
			require.NoError(t, m.LoadOne(PluginTypeBroker, "ext", PluginEntry{SelfManaged: true, Address: s.addr}, ""))
		}},
		{name: "admin RestartBrokerPlugin failing to load", counted: true, refresh: false, replace: func(t *testing.T, m *Manager, _ *selfManagedServer) {
			require.Error(t, m.RestartBrokerPlugin("ext", map[string]string{"token": fakeTokenFailConfigure}))
			// No client is left; the supervisor recovers it from the
			// stored config (and its hook refreshes the spoke).
			waitRestarted(t, m, "ext", 1)
		}},
		{name: "Shutdown", replace: func(_ *testing.T, m *Manager, _ *selfManagedServer) {
			m.Shutdown()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := startSelfManagedServer(t)
			m, fanout, _ := newSelfManagedFanout(t, s.addr, 0)
			oldBus, err := m.GetBroker("ext")
			require.NoError(t, err)

			tc.replace(t, m, s)
			if nb, err := m.GetBroker("ext"); err == nil {
				if tc.refresh {
					// handleRestartIntegration's refreshBrokerSpoke.
					require.NoError(t, fanout.ReplaceSpoke("ext", eventbus.NamedEventBus{Name: "ext", Bus: nb}))
				}
				assert.NoError(t, fanout.Publish(context.Background(), "agent.bot.message", messages.NewInstruction("user:a", "agent:b", "hi")))
				assert.Equal(t, "agent.>", fakeHealth(t, m, "ext")["subs"])
			}
			before, _ := m.BrokerSupervision("ext")
			assert.Error(t, oldBus.Publish(context.Background(), "agent.x", messages.NewInstruction("user:a", "agent:b", "hi")))
			assert.NoError(t, oldBus.Close(), "closing an old adapter is a no-op")
			if tc.counted {
				after, _ := m.BrokerSupervision("ext")
				assert.Equal(t, before.DroppedTotal+1, after.DroppedTotal, "a publish through the old client is a counted drop")
			}
			assert.Equal(t, 0, s.impl.closeCount(), "the shared plugin instance never received Close")
		})
	}
}

// hubManagedPid returns the pid of the fixture plugin's current process.
func hubManagedPid(t *testing.T, fx *hubManagedFixture) int {
	t.Helper()
	pid, err := strconv.Atoi(fakeHealth(t, fx.m, "fake")["pid"])
	require.NoError(t, err)
	return pid
}

func TestClientReplacementPaths_HubManagedAdminRestart(t *testing.T) {
	fx := newHubManagedFixture(t)
	oldPid := hubManagedPid(t, fx)
	oldBus, err := fx.m.GetBroker("fake")
	require.NoError(t, err)

	require.NoError(t, fx.m.RestartBrokerPlugin("fake", map[string]string{"token": "admin"}))
	nb, err := fx.m.GetBroker("fake")
	require.NoError(t, err)
	require.NoError(t, fx.fanout.ReplaceSpoke("fake", eventbus.NamedEventBus{Name: "fake", Bus: nb}))

	waitFor(t, 10*time.Second, "old plugin process gone", func() bool { return processGone(oldPid) })
	assert.NotEqual(t, oldPid, hubManagedPid(t, fx))
	assert.NoError(t, fx.publish("agent.bot.message"))
	assert.Equal(t, "agent.>", fakeHealth(t, fx.m, "fake")["subs"])
	assert.NoError(t, oldBus.Close(), "closing the old adapter is a no-op")
}

// TestDiscardClient_BoundedStopOfHungPlugin stops the plugin process
// (SIGSTOP), so its session hangs without the process exiting, and checks
// that the graceful stop of the admin restart and of Shutdown is bounded.
func TestDiscardClient_BoundedStopOfHungPlugin(t *testing.T) {
	for _, tc := range []struct {
		name string
		// stop runs off the test goroutine, so it reports failure as an
		// error instead of failing the test itself.
		stop func(fx *hubManagedFixture) error
	}{
		{name: "admin RestartBrokerPlugin", stop: func(fx *hubManagedFixture) error {
			if err := fx.m.RestartBrokerPlugin("fake", map[string]string{"token": "admin"}); err != nil {
				return fmt.Errorf("RestartBrokerPlugin: %w", err)
			}
			nb, err := fx.m.GetBroker("fake")
			if err != nil {
				return fmt.Errorf("GetBroker: %w", err)
			}
			if err := fx.fanout.ReplaceSpoke("fake", eventbus.NamedEventBus{Name: "fake", Bus: nb}); err != nil {
				return fmt.Errorf("ReplaceSpoke: %w", err)
			}
			if err := fx.publish("agent.bot.message"); err != nil {
				return fmt.Errorf("publish through the new client: %w", err)
			}
			return nil
		}},
		{name: "Shutdown", stop: func(fx *hubManagedFixture) error {
			fx.m.Shutdown()
			return nil
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newHubManagedFixtureWithInterval(t, time.Hour)
			pid := hubManagedPid(t, fx)
			require.NoError(t, syscall.Kill(pid, syscall.SIGSTOP))
			t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGCONT) })

			done := make(chan error, 1)
			go func() { done <- tc.stop(fx) }()
			select {
			case err := <-done:
				require.NoError(t, err)
			case <-time.After(15 * time.Second):
				t.Fatal("stopping a hung plugin did not finish within 15 s")
			}
			waitFor(t, 10*time.Second, "hung plugin process killed", func() bool { return processGone(pid) })
		})
	}
}

func TestShutdown_DiscardsPluginStartedByInFlightAttempt(t *testing.T) {
	pidDir := t.TempDir()
	t.Setenv(supervisorPidDirEnv, pidDir)
	fx := newHubManagedFixtureWithInterval(t, time.Hour)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	fx.m.SetBrokerConfigProvider(func(_ context.Context, name string) (map[string]string, error) {
		once.Do(func() { close(entered) })
		<-release
		return map[string]string{"token": "provided-" + name}, nil
	})

	require.Error(t, fx.publish(fakeTopicExit))
	select {
	case <-entered: // the attempt is in progress
	case <-time.After(10 * time.Second):
		t.Fatal("restart attempt did not start")
	}
	shutdownDone := make(chan struct{})
	go func() {
		fx.m.Shutdown()
		close(shutdownDone)
	}()
	waitFor(t, 5*time.Second, "shutdown begun", fx.m.stopping.Load)
	close(release) // the attempt now starts a new plugin process
	select {
	case <-shutdownDone:
	case <-time.After(20 * time.Second):
		t.Fatal("Shutdown did not return")
	}

	// When Shutdown returns, the attempt has finished: the process it
	// started exists and has already been killed.
	entries, err := os.ReadDir(pidDir)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(entries), 2, "the boot process and the one the attempt started, by the time Shutdown returns")
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		require.NoError(t, err)
		waitFor(t, 2*time.Second, fmt.Sprintf("plugin process %d dead", pid), func() bool { return processDead(pid) })
	}
	fx.m.mu.RLock()
	assert.Empty(t, fx.m.clients, "no client stored after shutdown")
	fx.m.mu.RUnlock()
}

// processDead reports whether pid has exited (it may not be reaped yet).
func processDead(pid int) bool {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return true
	}
	// The state follows the parenthesized command name.
	if i := strings.LastIndexByte(string(b), ')'); i >= 0 && i+2 < len(b) {
		return b[i+2] == 'Z' || b[i+2] == 'X'
	}
	return false
}
