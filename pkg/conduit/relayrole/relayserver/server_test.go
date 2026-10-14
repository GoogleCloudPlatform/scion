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

package relayserver

import (
	"context"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/clock"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay/relaytest"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relayrole"
)

// fakeExperiment answers hub.conduit reads from a script: each read takes
// the next answer, and the last answer repeats. Every read is signalled on
// reads.
type fakeExperiment struct {
	mu      sync.Mutex
	answers []expAnswer
	n       int
	reads   chan struct{}
}

type expAnswer struct {
	on  bool
	err error
}

var errUnreadable = errors.New("database unavailable")

func newFakeExperiment(answers ...expAnswer) *fakeExperiment {
	return &fakeExperiment{answers: answers, reads: make(chan struct{}, 64)}
}

func (f *fakeExperiment) ConduitEnabled(context.Context) (bool, error) {
	f.mu.Lock()
	a := f.answers[min(f.n, len(f.answers)-1)]
	f.n++
	f.mu.Unlock()
	select {
	case f.reads <- struct{}{}:
	default:
	}
	return a.on, a.err
}

func (f *fakeExperiment) set(answers ...expAnswer) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answers, f.n = answers, 0
}

func (f *fakeExperiment) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.n
}

const testPeerSecret = "relay-peer-secret-0123456789-abcdefghijkl"

func testConfig(t *testing.T, w *relaytest.World, exp ExperimentSource) Config {
	t.Helper()
	auth, err := relay.NewHMACPeerAuthFromPeerSecret(relay.HMACPeerAuthConfig{Secret: []byte(testPeerSecret), SelfID: "relay-0"})
	if err != nil {
		t.Fatal(err)
	}
	return Config{
		InstanceID:           "relay-0",
		PublicURL:            "https://relay-0.example.com",
		HubInternalURL:       "http://127.0.0.1:1",
		PublicListen:         "127.0.0.1:0",
		InternalListen:       "127.0.0.1:0",
		PeerAuth:             auth,
		Registry:             w.Store,
		Experiment:           exp,
		ExperimentPollPeriod: time.Hour, // reads in these tests are explicit
		StartupDeadline:      time.Second,
		StartupRetry:         100 * time.Millisecond,
		Clock:                clock.NewFake(time.Now()),
	}
}

func startServer(t *testing.T, cfg Config) *Server {
	t.Helper()
	r := awaitStart(t, startAsync(t, cfg))
	if r.err != nil {
		t.Fatalf("start: %v", r.err)
	}
	t.Cleanup(func() {
		sctx, scancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer scancel()
		_ = r.s.Shutdown(sctx, true, time.Second)
	})
	return r.s
}

func relayDraining(t *testing.T, w *relaytest.World) bool {
	t.Helper()
	relays, err := w.Store.ListRelays(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, ri := range relays {
		if ri.InstanceID == "relay-0" {
			return ri.Draining
		}
	}
	t.Fatal("no relay row")
	return false
}

// TestStartRequiresExperiment: hub.conduit off refuses to start and
// registers nothing.
func TestStartRequiresExperiment(t *testing.T) {
	w := relaytest.NewWorld(t)
	_, err := Start(context.Background(), testConfig(t, w, newFakeExperiment(expAnswer{on: false})))
	if !errors.Is(err, ErrExperimentOff) {
		t.Fatalf("start with hub.conduit off: %v", err)
	}
	relays, err := w.Store.ListRelays(context.Background())
	if err != nil || len(relays) != 0 {
		t.Fatalf("relay rows %v (%v)", relays, err)
	}
}

// startAsync runs Start in a goroutine and returns its result channel. The
// context Start gets ends when the test ends, which stops the relay's
// background work (the experiment follower among it).
func startAsync(t *testing.T, cfg Config) <-chan startResult {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ch := make(chan startResult, 1)
	go func() {
		s, err := Start(ctx, cfg)
		ch <- startResult{s, err}
	}()
	return ch
}

type startResult struct {
	s   *Server
	err error
}

// awaitStart returns Start's result, failing the test if it does not come
// within a bounded wait.
func awaitStart(t *testing.T, res <-chan startResult) startResult {
	t.Helper()
	select {
	case r := <-res:
		return r
	case <-time.After(30 * time.Second):
		t.Fatal("Start did not return")
	}
	return startResult{}
}

// awaitRead waits for one hub.conduit read, failing the test if none comes
// within a bounded wait.
func awaitRead(t *testing.T, exp *fakeExperiment, what string) {
	t.Helper()
	select {
	case <-exp.reads:
	case <-time.After(10 * time.Second):
		t.Fatalf("no %s", what)
	}
}

// drainReads discards the hub.conduit reads signalled so far, without
// waiting for more.
func drainReads(exp *fakeExperiment) {
	for {
		select {
		case <-exp.reads:
		default:
			return
		}
	}
}

// sendEvent delivers one settings event, failing the test if the follower
// does not take it within a bounded wait.
func sendEvent(t *testing.T, ev *fakeEvents, subject string) {
	t.Helper()
	select {
	case ev.ch <- subject:
	case <-time.After(10 * time.Second):
		t.Fatalf("settings event %s not taken", subject)
	}
}

// advanceRetry waits for the startup retry timer, then fires it.
func advanceRetry(t *testing.T, clk *clock.Fake, retry time.Duration) {
	t.Helper()
	if !clk.WaitForTimer(10*time.Second, clk.Now().Add(retry)) {
		t.Fatal("no startup retry timer armed")
	}
	clk.Advance(retry)
}

// TestStartRetriesUnreadableExperiment: an unreadable hub.conduit is
// retried at startup on the relay's clock; the relay starts once it reads
// on, and fails when the deadline passes first.
func TestStartRetriesUnreadableExperiment(t *testing.T) {
	t.Run("readable before the deadline", func(t *testing.T) {
		w := relaytest.NewWorld(t)
		exp := newFakeExperiment(expAnswer{err: errUnreadable}, expAnswer{err: errUnreadable}, expAnswer{on: true})
		cfg := testConfig(t, w, exp)
		clk := cfg.Clock.(*clock.Fake)
		res := startAsync(t, cfg)
		advanceRetry(t, clk, cfg.StartupRetry)
		advanceRetry(t, clk, cfg.StartupRetry)
		r := awaitStart(t, res)
		if r.err != nil {
			t.Fatalf("start: %v", r.err)
		}
		t.Cleanup(func() {
			sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = r.s.Shutdown(sctx, true, time.Second)
		})
		if n := exp.count(); n != 3 {
			t.Fatalf("%d reads, want 3", n)
		}
		if !r.s.Relay.Admitting() {
			t.Fatal("not admitting")
		}
	})
	t.Run("deadline passes", func(t *testing.T) {
		w := relaytest.NewWorld(t)
		exp := newFakeExperiment(expAnswer{err: errUnreadable})
		cfg := testConfig(t, w, exp)
		cfg.StartupDeadline = 3 * cfg.StartupRetry
		clk := cfg.Clock.(*clock.Fake)
		res := startAsync(t, cfg)
		for i := 0; i < 3; i++ {
			advanceRetry(t, clk, cfg.StartupRetry)
		}
		r := awaitStart(t, res)
		if r.err == nil || !errors.Is(r.err, errUnreadable) || !strings.Contains(r.err.Error(), "startup deadline") {
			t.Fatalf("start: %v", r.err)
		}
		if n := exp.count(); n != 4 {
			t.Fatalf("%d reads, want 4 (one per retry, and the first)", n)
		}
	})
}

// TestExperimentPolledOnTheClock: hub.conduit is re-read every
// ExperimentPollPeriod on the relay's clock.
func TestExperimentPolledOnTheClock(t *testing.T) {
	w := relaytest.NewWorld(t)
	exp := newFakeExperiment(expAnswer{on: true})
	cfg := testConfig(t, w, exp)
	// The relay's heartbeat timer runs on the same fake clock. The waits
	// below target the poll timer by its own deadline, so that deadline
	// must not coincide with a heartbeat deadline.
	cfg.ExperimentPollPeriod = 7 * time.Second
	if relay.DefaultHeartbeatInterval%cfg.ExperimentPollPeriod == 0 || cfg.ExperimentPollPeriod%relay.DefaultHeartbeatInterval == 0 {
		t.Fatalf("poll period %v shares deadlines with the heartbeat interval %v", cfg.ExperimentPollPeriod, relay.DefaultHeartbeatInterval)
	}
	clk := cfg.Clock.(*clock.Fake)
	s := startServer(t, cfg)
	drainReads(exp)
	exp.set(expAnswer{on: false})
	if !clk.WaitForTimer(10*time.Second, clk.Now().Add(cfg.ExperimentPollPeriod)) {
		t.Fatal("no poll timer armed")
	}
	clk.Advance(cfg.ExperimentPollPeriod)
	awaitRead(t, exp, "the poll read")
	// The next poll timer is armed after the read is applied.
	if !clk.WaitForTimer(10*time.Second, clk.Now().Add(cfg.ExperimentPollPeriod)) {
		t.Fatal("no next poll timer armed")
	}
	if s.Relay.Admitting() {
		t.Fatal("still admitting after the poll read off")
	}
}

// TestApplyExperimentRetriesFailedDrainingWrite: a draining flag write that
// failed is written again by the next ApplyExperiment.
func TestApplyExperimentRetriesFailedDrainingWrite(t *testing.T) {
	w := relaytest.NewWorld(t)
	exp := newFakeExperiment(expAnswer{on: true})
	s := startServer(t, testConfig(t, w, exp))
	w.SetFault(func(op string) error {
		if op == registry.OpSetRelayDraining {
			return errUnreadable
		}
		return nil
	})
	exp.set(expAnswer{on: false})
	s.ApplyExperiment(context.Background())
	if s.Relay.Admitting() || relayDraining(t, w) {
		t.Fatal("want admission off with the draining write failed")
	}
	w.SetFault(nil)
	s.ApplyExperiment(context.Background())
	if !relayDraining(t, w) {
		t.Fatal("the draining flag was not written again")
	}
}

// orderedEvents records whether Start registered its subscriptions and
// listen hooks before starting the events.
type orderedEvents struct {
	mu         sync.Mutex
	subscribed []string
	hooks      int
	started    bool
	early      []string
}

func (e *orderedEvents) Subscribe(subjects ...string) (<-chan string, func()) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.started {
		e.early = append(e.early, "subscribe after start")
	}
	e.subscribed = append(e.subscribed, subjects...)
	return make(chan string), func() {}
}

func (e *orderedEvents) AddOnListen(func()) func() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.started {
		e.early = append(e.early, "listen hook after start")
	}
	e.hooks++
	return func() {}
}

func (e *orderedEvents) start() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.started = true
}

// TestEventsStartAfterRegistration: the events start only once the
// revocation and settings subscriptions and the listen hook are registered.
func TestEventsStartAfterRegistration(t *testing.T) {
	w := relaytest.NewWorld(t)
	cfg := testConfig(t, w, newFakeExperiment(expAnswer{on: true}))
	ev := &orderedEvents{}
	cfg.Events, cfg.StartEvents = ev, ev.start
	startServer(t, cfg)
	ev.mu.Lock()
	defer ev.mu.Unlock()
	if !ev.started {
		t.Fatal("events never started")
	}
	if len(ev.early) != 0 {
		t.Fatalf("registered after the events started: %v", ev.early)
	}
	if ev.hooks == 0 {
		t.Fatal("no listen hook registered")
	}
	joined := strings.Join(ev.subscribed, " ")
	for _, want := range append(append([]string(nil), RevocationSubjects...), SettingsUpdatedSubject) {
		if !strings.Contains(joined, want) {
			t.Errorf("no subscription to %s", want)
		}
	}
}

// TestApplyExperimentFollowsSetting: at run time, off stops admission and
// marks the relay draining while the process keeps running, on resumes,
// and a failed read changes nothing.
func TestApplyExperimentFollowsSetting(t *testing.T) {
	w := relaytest.NewWorld(t)
	exp := newFakeExperiment(expAnswer{on: true})
	s := startServer(t, testConfig(t, w, exp))
	ctx := context.Background()

	exp.set(expAnswer{on: false})
	s.ApplyExperiment(ctx)
	if s.Relay.Admitting() || !relayDraining(t, w) {
		t.Fatal("off: still admitting")
	}
	exp.set(expAnswer{err: errUnreadable})
	s.ApplyExperiment(ctx)
	if s.Relay.Admitting() {
		t.Fatal("a failed read resumed admission")
	}
	exp.set(expAnswer{on: true})
	s.ApplyExperiment(ctx)
	if !s.Relay.Admitting() || relayDraining(t, w) {
		t.Fatal("on: admission not resumed")
	}
	exp.set(expAnswer{err: errUnreadable})
	s.ApplyExperiment(ctx)
	if !s.Relay.Admitting() {
		t.Fatal("a failed read stopped admission")
	}
	select {
	case err := <-s.Relay.Fatal():
		t.Fatalf("the relay stopped: %v", err)
	default:
	}
}

// fakeEvents delivers settings events the test sends.
type fakeEvents struct{ ch chan string }

func (e *fakeEvents) Subscribe(subjects ...string) (<-chan string, func()) {
	for _, s := range subjects {
		if s == SettingsUpdatedSubject {
			return e.ch, func() {}
		}
	}
	return make(chan string), func() {}
}

func (e *fakeEvents) AddOnListen(func()) func() { return func() {} }

// TestSettingsEventRereadsExperiment: a settings change event re-reads
// hub.conduit at once.
func TestSettingsEventRereadsExperiment(t *testing.T) {
	w := relaytest.NewWorld(t)
	exp := newFakeExperiment(expAnswer{on: true})
	cfg := testConfig(t, w, exp)
	ev := &fakeEvents{ch: make(chan string, 1)}
	cfg.Events = ev
	s := startServer(t, cfg)
	drainReads(exp)
	exp.set(expAnswer{on: false})
	sendEvent(t, ev, SettingsUpdatedSubject)
	awaitRead(t, exp, "re-read after a settings event")
	// The read and SetAdmitting run in one step of the follower; a second
	// event is handled only after the first finished.
	sendEvent(t, ev, SettingsUpdatedSubject)
	awaitRead(t, exp, "re-read after the second settings event")
	if s.Relay.Admitting() {
		t.Fatal("still admitting after hub.conduit turned off")
	}
}

// TestNoSessionSecretInput: neither the relay's nor its front's
// configuration has a session secret input, and the relay starts with no
// session secret in the environment.
func TestNoSessionSecretInput(t *testing.T) {
	for _, typ := range []reflect.Type{reflect.TypeOf(Config{}), reflect.TypeOf(relayrole.Config{})} {
		for i := 0; i < typ.NumField(); i++ {
			name := strings.ToLower(typ.Field(i).Name)
			if strings.Contains(name, "secret") || strings.Contains(name, "signing") {
				t.Errorf("%s has a %s field", typ, typ.Field(i).Name)
			}
		}
	}
	t.Setenv("SCION_SERVER_SESSION_SECRET", "")
	t.Setenv("SESSION_SECRET", "")
	w := relaytest.NewWorld(t)
	s := startServer(t, testConfig(t, w, newFakeExperiment(expAnswer{on: true})))
	if !s.Relay.Admitting() {
		t.Fatal("not admitting")
	}
}

// cancellingExperiment fails every read with the context's error and, on
// the first read, cancels the context and moves the clock past the startup
// deadline: a read that ran until the relay was told to stop.
type cancellingExperiment struct {
	cancel  context.CancelFunc
	clk     *clock.Fake
	advance time.Duration
	n       int
}

func (e *cancellingExperiment) ConduitEnabled(ctx context.Context) (bool, error) {
	e.n++
	if e.n == 1 {
		e.cancel()
		e.clk.Advance(e.advance)
	}
	return false, ctx.Err()
}

// TestWaitExperimentReturnsContextErrorWhenCancelled: a hub.conduit read
// that fails once ctx is done returns ctx's error at once, unwrapped: no
// retry, no wait and no startup-deadline error.
func TestWaitExperimentReturnsContextErrorWhenCancelled(t *testing.T) {
	t.Run("cancelled during the read, past the deadline", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		clk := clock.NewFake(time.Now())
		exp := &cancellingExperiment{cancel: cancel, clk: clk, advance: 2 * time.Second}
		cfg := &Config{Experiment: exp, Clock: clk, StartupDeadline: time.Second, StartupRetry: 100 * time.Millisecond, Logger: slog.New(slog.DiscardHandler)}
		err := waitExperiment(ctx, cfg)
		if err != context.Canceled {
			t.Fatalf("waitExperiment = %v, want context.Canceled unwrapped", err)
		}
		if exp.n != 1 {
			t.Fatalf("%d reads, want 1", exp.n)
		}
		if p := clk.Pending(); p != 0 {
			t.Fatalf("%d timers armed, want none", p)
		}
	})
	t.Run("already cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		clk := clock.NewFake(time.Now())
		exp := newFakeExperiment(expAnswer{err: errUnreadable})
		cfg := &Config{Experiment: exp, Clock: clk, StartupDeadline: time.Second, StartupRetry: 100 * time.Millisecond, Logger: slog.New(slog.DiscardHandler)}
		err := waitExperiment(ctx, cfg)
		if err != context.Canceled {
			t.Fatalf("waitExperiment = %v, want context.Canceled unwrapped", err)
		}
		if n := exp.count(); n != 1 {
			t.Fatalf("%d reads, want 1", n)
		}
		if p := clk.Pending(); p != 0 {
			t.Fatalf("%d timers armed, want none", p)
		}
	})
}
