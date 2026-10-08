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

package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/clock"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/grant"
)

// Unit tests of the user-stream re-check tracker (conduit_stream_authz.go)
// with a stub check and a fake clock.

// authzRecorder collects re-check log lines and metric increments.
type authzRecorder struct {
	mu      sync.Mutex
	buf     bytes.Buffer
	metrics []string // trigger/outcome/kind
}

func (r *authzRecorder) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.buf.Write(p)
}

func (r *authzRecorder) RecordConduitStreamAuthz(trigger, outcome, kind string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.metrics = append(r.metrics, trigger+"/"+outcome+"/"+kind)
}

// lines returns the decoded conduit_stream_authz log lines.
func (r *authzRecorder) lines(t *testing.T) []map[string]any {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(r.buf.String()), "\n") {
		if l == "" {
			continue
		}
		var m map[string]any
		require.NoError(t, json.Unmarshal([]byte(l), &m), l)
		if m["msg"] == "conduit_stream_authz" {
			out = append(out, m)
		}
	}
	return out
}

func (r *authzRecorder) text() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.buf.String()
}

func (r *authzRecorder) metricList() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.metrics...)
}

func newAuthzRecorder() (*authzRecorder, *slog.Logger) {
	r := &authzRecorder{}
	return r, slog.New(slog.NewJSONHandler(r, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// closeRecorder records a stream's Close calls.
type closeRecorder struct {
	mu     sync.Mutex
	closes []string
}

func (c *closeRecorder) close(code uint32, reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closes = append(c.closes, strconv.FormatUint(uint64(code), 10)+" "+reason)
}

func (c *closeRecorder) list() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.closes...)
}

// stubCheck returns a settable verdict per user.
type stubCheck struct {
	mu       sync.Mutex
	verdicts map[string]conduitAuthzVerdict
	calls    int
	block    chan struct{} // when set, checks wait on it (or ctx)
}

func (s *stubCheck) set(user string, v conduitAuthzVerdict) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.verdicts[user] = v
}

func (s *stubCheck) check(ctx context.Context, st *conduitUserStream) (conduitAuthzVerdict, string) {
	s.mu.Lock()
	s.calls++
	v := s.verdicts[st.UserID]
	block := s.block
	s.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
		}
	}
	return v, "stub"
}

func (s *stubCheck) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

type authzTrackerFixture struct {
	clk   *clock.Fake
	check *stubCheck
	rec   *authzRecorder
	a     *conduitStreamAuthz
}

func newAuthzTrackerFixture(t *testing.T, interval time.Duration) *authzTrackerFixture {
	t.Helper()
	rec, logger := newAuthzRecorder()
	f := &authzTrackerFixture{
		clk:   clock.NewFake(time.Date(2026, 10, 8, 1, 0, 0, 0, time.UTC)),
		check: &stubCheck{verdicts: map[string]conduitAuthzVerdict{}},
		rec:   rec,
	}
	f.a = newConduitStreamAuthz(conduitStreamAuthzConfig{
		Check:           f.check.check,
		Clock:           f.clk,
		RecheckInterval: interval,
		Metrics:         rec,
		Logger:          logger,
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	f.a.Start(ctx)
	return f
}

func (f *authzTrackerFixture) track(user, agent, project, kind string, id uint32) *closeRecorder {
	c := &closeRecorder{}
	f.a.Track(&conduitUserStream{
		Kind: kind, Identity: NewAuthenticatedUser(user, user+"@x", user, "member", "api"),
		UserID: user, AgentID: agent, ProjectID: project, SessionID: "sess-1", StreamID: id,
		Close: c.close,
	})
	return c
}

// TestConduitStreamAuthz_SweepClosesRevokedStream: with no event at all,
// the periodic sweep re-checks every tracked stream within one
// authz_recheck_interval, closes a revoked one with 4401 authz_expired and
// records trigger=sweep; a still-permitted stream records passed and stays
// open.
func TestConduitStreamAuthz_SweepClosesRevokedStream(t *testing.T) {
	f := newAuthzTrackerFixture(t, 60*time.Second)
	revoked := f.track("u1", "agent-x", "p1", grant.StreamKindPTY, 1)
	kept := f.track("u2", "agent-x", "p1", grant.StreamKindPTY, 3)
	f.check.set("u1", conduitAuthzDenied)

	f.clk.Advance(59 * time.Second)
	assert.Zero(t, f.check.callCount(), "sweep ran before its interval")
	f.clk.Advance(time.Second)

	assert.Equal(t, []string{"4401 authz_expired"}, revoked.list())
	assert.Empty(t, kept.list())
	assert.Equal(t, 1, f.a.Len(), "the closed stream is no longer tracked")
	assert.ElementsMatch(t, []string{"sweep/closed/pty", "sweep/passed/pty"}, f.rec.metricList())

	// The sweep re-arms: the next interval checks the remaining stream.
	f.clk.Advance(60 * time.Second)
	assert.ElementsMatch(t, []string{"sweep/closed/pty", "sweep/passed/pty", "sweep/passed/pty"}, f.rec.metricList())
}

// TestConduitStreamAuthz_SweepDisabled: a negative interval (test hook)
// disables the sweep.
func TestConduitStreamAuthz_SweepDisabled(t *testing.T) {
	f := newAuthzTrackerFixture(t, -1)
	f.track("u1", "agent-x", "p1", grant.StreamKindPTY, 1)
	f.clk.Advance(time.Hour)
	assert.Zero(t, f.check.callCount())
}

// TestConduitStreamAuthz_TargetGoneCloses4404 (R6): a deleted agent closes
// the stream with 4404 target_not_found, never 4401.
func TestConduitStreamAuthz_TargetGoneCloses4404(t *testing.T) {
	f := newAuthzTrackerFixture(t, -1)
	c := f.track("u1", "agent-x", "p1", grant.StreamKindPTY, 1)
	f.check.set("u1", conduitAuthzTargetGone)
	f.a.Recheck(context.Background(), conduitAuthzTriggerNotify, conduitAuthzMatch{AgentID: "agent-x"})
	assert.Equal(t, []string{"4404 target_not_found"}, c.list())
	assert.Equal(t, []string{"notify/target_gone/pty"}, f.rec.metricList())
}

// TestConduitStreamAuthz_UnavailableNeverClosesOrRenews (O3): a check that
// cannot be evaluated records deferred_unavailable on every trigger and
// neither closes nor renews the stream; once evaluation recovers the next
// check decides normally.
func TestConduitStreamAuthz_UnavailableNeverClosesOrRenews(t *testing.T) {
	f := newAuthzTrackerFixture(t, 60*time.Second)
	c := f.track("u1", "agent-x", "p1", grant.StreamKindPTY, 1)
	f.check.set("u1", conduitAuthzUnavailable)

	for i := 0; i < 3; i++ {
		f.clk.Advance(60 * time.Second)
	}
	f.a.Recheck(context.Background(), conduitAuthzTriggerNotify, conduitAuthzMatch{})
	assert.Empty(t, c.list(), "an unavailable check closed the stream")
	assert.Equal(t, 1, f.a.Len())
	for _, m := range f.rec.metricList() {
		assert.True(t, strings.HasSuffix(m, "/deferred_unavailable/pty"), m)
	}
	for _, l := range f.rec.lines(t) {
		assert.Equal(t, "deferred_unavailable", l["outcome"], "an unavailable check renewed or closed the stream")
		assert.Equal(t, "WARN", l["level"])
	}

	// Recovery: the next check decides on the real state.
	f.check.set("u1", conduitAuthzDenied)
	f.clk.Advance(60 * time.Second)
	assert.Equal(t, []string{"4401 authz_expired"}, c.list())
}

// TestConduitStreamAuthz_LateVerdictIsUnavailable: a check that outlives
// its timeout is unavailable even if it then returns a verdict.
func TestConduitStreamAuthz_LateVerdictIsUnavailable(t *testing.T) {
	rec, logger := newAuthzRecorder()
	check := &stubCheck{verdicts: map[string]conduitAuthzVerdict{"u1": conduitAuthzDenied}, block: make(chan struct{})}
	a := newConduitStreamAuthz(conduitStreamAuthzConfig{
		Check: check.check, Clock: clock.NewFake(time.Now()), RecheckInterval: -1,
		CheckTimeout: time.Millisecond, Metrics: rec, Logger: logger,
	})
	c := &closeRecorder{}
	a.Track(&conduitUserStream{Kind: grant.StreamKindTCP, UserID: "u1", Close: c.close})
	a.Recheck(context.Background(), conduitAuthzTriggerNotify, conduitAuthzMatch{})
	assert.Empty(t, c.list())
	assert.Equal(t, []string{"notify/deferred_unavailable/tcp"}, rec.metricList())
}

// TestConduitStreamAuthz_NotifyPrecision (R3, R4 at the tracker): a
// revocation event re-checks only the streams it selects; other users'
// streams and streams to other agents see no check at all.
func TestConduitStreamAuthz_NotifyPrecision(t *testing.T) {
	f := newAuthzTrackerFixture(t, -1)
	u1 := f.track("u1", "agent-x", "p1", grant.StreamKindPTY, 1)
	u2 := f.track("u2", "agent-x", "p1", grant.StreamKindPTY, 3)
	other := f.track("u1", "agent-y", "p2", grant.StreamKindTCP, 5)
	f.check.set("u1", conduitAuthzDenied)

	f.a.Recheck(context.Background(), conduitAuthzTriggerNotify, conduitAuthzMatch{UserID: "u1", ProjectID: "p1"})
	assert.Equal(t, []string{"4401 authz_expired"}, u1.list())
	assert.Empty(t, u2.list())
	assert.Empty(t, other.list())
	assert.Equal(t, 1, f.check.callCount(), "streams outside the match were checked")
	assert.Equal(t, []string{"notify/closed/pty"}, f.rec.metricList())
}

// TestConduitStreamAuthz_LogLine: each re-check emits one structured log
// line with the 16b evidence fields and a matching metric increment; the
// trigger is recorded server-side.
func TestConduitStreamAuthz_LogLine(t *testing.T) {
	f := newAuthzTrackerFixture(t, -1)
	f.track("u1", "agent-x", "p1", grant.StreamKindTCP, 7)
	f.check.set("u1", conduitAuthzDenied)
	f.a.Recheck(context.Background(), conduitAuthzTriggerResync, conduitAuthzMatch{})

	lines := f.rec.lines(t)
	require.Len(t, lines, 1)
	l := lines[0]
	assert.Equal(t, "resync", l["trigger"])
	assert.Equal(t, "closed", l["outcome"])
	assert.Equal(t, "tcp", l["kind"])
	assert.EqualValues(t, 7, l["stream_id"])
	assert.Equal(t, "sess-1", l["session_id"])
	assert.Equal(t, "agent-x", l["agent_id"])
	assert.Equal(t, "p1", l["project_id"])
	assert.Equal(t, "user", l["principal_kind"])
	for _, k := range []string{"check_start", "check_end"} {
		assert.Contains(t, l, k)
	}
	assert.Equal(t, []string{"resync/closed/tcp"}, f.rec.metricList())
}

// TestConduitStreamAuthz_ClosedOnce: concurrent triggers close a stream
// once; a stream already closed is not checked again.
func TestConduitStreamAuthz_ClosedOnce(t *testing.T) {
	f := newAuthzTrackerFixture(t, -1)
	c := f.track("u1", "agent-x", "p1", grant.StreamKindPTY, 1)
	f.check.set("u1", conduitAuthzDenied)
	var wg sync.WaitGroup
	for _, trig := range []string{conduitAuthzTriggerNotify, conduitAuthzTriggerSweep, conduitAuthzTriggerResync} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f.a.Recheck(context.Background(), trig, conduitAuthzMatch{})
		}()
	}
	wg.Wait()
	assert.Equal(t, []string{"4401 authz_expired"}, c.list())
}

// TestConduitAuthzMatchForEvent maps trigger events to the streams they
// select.
func TestConduitAuthzMatchForEvent(t *testing.T) {
	data, err := json.Marshal(conduitAuthzMatch{UserID: "u1"})
	require.NoError(t, err)
	for _, tc := range []struct {
		evt  Event
		want conduitAuthzMatch
		ok   bool
	}{
		{Event{Subject: conduitAuthzChangedSubject, Data: data}, conduitAuthzMatch{UserID: "u1"}, true},
		{Event{Subject: conduitAuthzChangedSubject, Data: []byte("{")}, conduitAuthzMatch{}, true},
		{Event{Subject: "agent.a1.deleted"}, conduitAuthzMatch{AgentID: "a1"}, true},
		{Event{Subject: "agent.a1.ports"}, conduitAuthzMatch{AgentID: "a1"}, true},
		{Event{Subject: "project.p1.deleted"}, conduitAuthzMatch{ProjectID: "p1"}, true},
		{Event{Subject: "agent.a1.status"}, conduitAuthzMatch{}, false},
		{Event{Subject: "project.p1.agent.deleted"}, conduitAuthzMatch{}, false},
	} {
		got, ok := conduitAuthzMatchForEvent(tc.evt)
		assert.Equal(t, tc.ok, ok, tc.evt.Subject)
		assert.Equal(t, tc.want, got, tc.evt.Subject)
	}
}

// TestConduitStreamAuthz_SweepFixedPeriod: the next sweep tick is armed
// before a sweep runs, so a slow sweep does not push later ticks back; a
// tick that finds the previous sweep still running is skipped and logged.
func TestConduitStreamAuthz_SweepFixedPeriod(t *testing.T) {
	f := newAuthzTrackerFixture(t, 60*time.Second)
	f.track("u1", "agent-x", "p1", grant.StreamKindPTY, 1)
	entered := make(chan struct{}, 4)
	release := make(chan struct{})
	f.check.mu.Lock()
	f.check.block = release
	f.check.mu.Unlock()
	f.a.cfg.Check = func(ctx context.Context, st *conduitUserStream) (conduitAuthzVerdict, string) {
		entered <- struct{}{}
		return f.check.check(ctx, st)
	}

	first := make(chan struct{})
	go func() {
		defer close(first)
		f.clk.Advance(60 * time.Second) // runs the first, slow sweep
	}()
	<-entered
	require.True(t, f.clk.WaitFor(recheckWait, func(pending int) bool { return pending == 1 }),
		"the next tick was not armed before the sweep ran")

	f.clk.Advance(60 * time.Second) // the next tick, while the first sweep runs
	assert.Contains(t, f.rec.text(), "conduit_stream_authz_sweep_skipped")
	close(release)
	<-first
	assert.Equal(t, 1, f.check.callCount(), "the skipped tick re-checked the stream")
	assert.Contains(t, f.rec.text(), `"msg":"conduit_stream_authz_sweep"`)
}

// TestConduitNotifyQueue_Coalesces: notify matches recorded while a
// re-check runs are deduplicated, a match-all absorbs the rest, and too
// many distinct matches collapse into one re-check of every stream.
func TestConduitNotifyQueue_Coalesces(t *testing.T) {
	q := newConduitNotifyQueue()
	q.add(conduitAuthzMatch{UserID: "u1"})
	q.add(conduitAuthzMatch{UserID: "u1"})
	q.add(conduitAuthzMatch{AgentID: "a1"})
	all, ms := q.take()
	assert.False(t, all)
	assert.ElementsMatch(t, []conduitAuthzMatch{{UserID: "u1"}, {AgentID: "a1"}}, ms)

	q.add(conduitAuthzMatch{UserID: "u1"})
	q.add(conduitAuthzMatch{})
	q.add(conduitAuthzMatch{UserID: "u2"})
	all, ms = q.take()
	assert.True(t, all)
	assert.Empty(t, ms)

	for i := 0; i <= conduitNotifyMaxPending; i++ {
		q.add(conduitAuthzMatch{UserID: strconv.Itoa(i)})
	}
	all, ms = q.take()
	assert.True(t, all, "the pending set was not bounded")
	assert.Empty(t, ms)

	all, ms = q.take()
	assert.False(t, all)
	assert.Empty(t, ms)
}
