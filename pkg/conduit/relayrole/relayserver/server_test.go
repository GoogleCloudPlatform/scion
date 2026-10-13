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
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

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
		StartupRetry:         10 * time.Millisecond,
	}
}

func startServer(t *testing.T, cfg Config) *Server {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	s, err := Start(ctx, cfg)
	if err != nil {
		cancel()
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() {
		sctx, scancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer scancel()
		_ = s.Shutdown(sctx, true, time.Second)
		cancel()
	})
	return s
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

// TestStartRetriesUnreadableExperiment: an unreadable hub.conduit is
// retried at startup; the relay starts once it reads on, and fails when
// the deadline passes first.
func TestStartRetriesUnreadableExperiment(t *testing.T) {
	t.Run("readable before the deadline", func(t *testing.T) {
		w := relaytest.NewWorld(t)
		exp := newFakeExperiment(expAnswer{err: errUnreadable}, expAnswer{err: errUnreadable}, expAnswer{on: true})
		s := startServer(t, testConfig(t, w, exp))
		if n := exp.count(); n != 3 {
			t.Fatalf("%d reads, want 3", n)
		}
		if !s.Relay.Admitting() {
			t.Fatal("not admitting")
		}
	})
	t.Run("deadline passes", func(t *testing.T) {
		w := relaytest.NewWorld(t)
		cfg := testConfig(t, w, newFakeExperiment(expAnswer{err: errUnreadable}))
		cfg.StartupDeadline = 100 * time.Millisecond
		_, err := Start(context.Background(), cfg)
		if err == nil || !errors.Is(err, errUnreadable) || !strings.Contains(err.Error(), "startup deadline") {
			t.Fatalf("start: %v", err)
		}
	})
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
	for len(exp.reads) > 0 {
		<-exp.reads
	}
	exp.set(expAnswer{on: false})
	ev.ch <- SettingsUpdatedSubject
	select {
	case <-exp.reads:
	case <-time.After(10 * time.Second):
		t.Fatal("no re-read after a settings event")
	}
	// The read and SetAdmitting run in one step of the follower; a second
	// event is handled only after the first finished.
	ev.ch <- SettingsUpdatedSubject
	select {
	case <-exp.reads:
	case <-time.After(10 * time.Second):
		t.Fatal("no re-read after the second settings event")
	}
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
