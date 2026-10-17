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

package relaydb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// ConduitChannel is the dedicated PostgreSQL channel the hub notifies the
// subjects a relay acts on, with identifier-only payloads. The relay
// LISTENs on it alone, never on the hub's global channel. A hub test keeps
// it equal to the hub's.
const ConduitChannel = "scion_ev_conduit"

// ListenerApplicationName is the application_name of the LISTEN connection.
const ListenerApplicationName = "scion-conduit-relay-events"

// Listener is a LISTEN source for a fixed set of hub event subjects. It
// reads only the subject of each notification, never its data, and
// delivers nothing outside its subjects.
type Listener struct {
	dsn      string
	patterns []string
	log      *slog.Logger
	ctx      context.Context
	cancel   context.CancelFunc
	done     chan struct{}

	startOnce sync.Once
	started   bool // set inside startOnce

	mu       sync.Mutex
	subs     map[*subscription]struct{}
	onListen map[int]func()
	next     int
}

type subscription struct {
	patterns []string
	ch       chan string
}

// envelope is the part of a hub event notification the listener reads.
type envelope struct {
	Subject string `json:"subject"`
}

// NewListener returns a listener for the events whose subject matches one
// of patterns. It does not connect until Start, so subscriptions and
// listen hooks registered before Start see the first connection.
func NewListener(dsn string, patterns []string, log *slog.Logger) (*Listener, error) {
	if _, err := pgx.ParseConfig(dsn); err != nil {
		return nil, fmt.Errorf("conduit relay events: %w", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Listener{
		dsn: dsn, patterns: append([]string(nil), patterns...), log: log,
		ctx: ctx, cancel: cancel, done: make(chan struct{}),
		subs: map[*subscription]struct{}{}, onListen: map[int]func(){},
	}, nil
}

// Start connects and LISTENs on ConduitChannel; the listener reconnects on
// its own after a connection loss. Start runs once; later calls do
// nothing.
func (l *Listener) Start() {
	l.startOnce.Do(func() {
		l.started = true
		go l.run()
	})
}

// Close stops the listener.
func (l *Listener) Close() {
	l.cancel()
	l.startOnce.Do(func() {}) // a listener never started has nothing to wait for
	if l.started {
		<-l.done
	}
}

// Subscribe returns the subjects of delivered events matching subjects
// (each also one of the listener's). A subscriber that falls behind loses
// events; the re-check it triggers covers everything anyway.
func (l *Listener) Subscribe(subjects ...string) (<-chan string, func()) {
	s := &subscription{patterns: append([]string(nil), subjects...), ch: make(chan string, 16)}
	l.mu.Lock()
	l.subs[s] = struct{}{}
	l.mu.Unlock()
	var once sync.Once
	return s.ch, func() {
		once.Do(func() {
			l.mu.Lock()
			delete(l.subs, s)
			l.mu.Unlock()
		})
	}
}

// AddOnListen runs fn each time the LISTEN connection is (re)established.
func (l *Listener) AddOnListen(fn func()) func() {
	l.mu.Lock()
	id := l.next
	l.next++
	l.onListen[id] = fn
	l.mu.Unlock()
	return func() {
		l.mu.Lock()
		delete(l.onListen, id)
		l.mu.Unlock()
	}
}

func (l *Listener) run() {
	defer close(l.done)
	backoff := time.Second
	for l.ctx.Err() == nil {
		err := l.listenOnce()
		if l.ctx.Err() != nil {
			return
		}
		l.log.Warn("Conduit relay event listener lost its connection; reconnecting", "error", err, "backoff", backoff)
		t := time.NewTimer(backoff)
		select {
		case <-l.ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

// listenOnce connects, LISTENs and delivers notifications until the
// connection fails.
func (l *Listener) listenOnce() error {
	cc, err := pgx.ParseConfig(l.dsn)
	if err != nil {
		return err
	}
	// Keepalives and a connect timeout, so a silently dropped peer is
	// detected instead of blocking forever.
	cc.DialFunc = (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	cc.RuntimeParams["application_name"] = ListenerApplicationName
	conn, err := pgx.ConnectConfig(l.ctx, cc)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(context.Background()) }()
	if _, err := conn.Exec(l.ctx, "LISTEN "+pgx.Identifier{ConduitChannel}.Sanitize()); err != nil {
		return fmt.Errorf("LISTEN %s: %w", ConduitChannel, err)
	}
	l.fireOnListen()
	for {
		wctx, cancel := context.WithTimeout(l.ctx, 30*time.Second)
		n, err := conn.WaitForNotification(wctx)
		cancel()
		switch {
		case l.ctx.Err() != nil:
			return l.ctx.Err()
		case errors.Is(err, context.DeadlineExceeded):
			// Idle: check the connection is still alive.
			if err := conn.Ping(l.ctx); err != nil {
				return err
			}
			continue
		case err != nil:
			return err
		}
		var env envelope
		if json.Unmarshal([]byte(n.Payload), &env) != nil || env.Subject == "" {
			continue
		}
		l.deliver(env.Subject)
	}
}

// fireOnListen runs the listen hooks. They re-check over the network, so
// they run off the read loop and notifications keep flowing meanwhile.
func (l *Listener) fireOnListen() {
	l.mu.Lock()
	fns := make([]func(), 0, len(l.onListen))
	for _, fn := range l.onListen {
		fns = append(fns, fn)
	}
	l.mu.Unlock()
	go func() {
		for _, fn := range fns {
			fn()
		}
	}()
}

// deliver hands subject to every subscriber whose patterns match it, if it
// matches the listener's own subjects.
func (l *Listener) deliver(subject string) {
	if !matchesAny(l.patterns, subject) {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for s := range l.subs {
		if !matchesAny(s.patterns, subject) {
			continue
		}
		select {
		case s.ch <- subject:
		default:
		}
	}
}

func matchesAny(patterns []string, subject string) bool {
	for _, p := range patterns {
		if subjectMatches(p, subject) {
			return true
		}
	}
	return false
}

// subjectMatches matches a dotted subject against a pattern, as the hub's
// event publisher does: "*" matches one token, a final ">" one or more.
func subjectMatches(pattern, subject string) bool {
	pp := strings.Split(pattern, ".")
	sp := strings.Split(subject, ".")
	for i, p := range pp {
		if p == ">" {
			return i < len(sp)
		}
		if i >= len(sp) {
			return false
		}
		if p != "*" && p != sp[i] {
			return false
		}
	}
	return len(pp) == len(sp)
}
