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
	"io"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// spliceFakeStream is a conduit.Stream for splice tests. Read returns
// queued data first, then blocks until the stream is closed locally
// (ErrStreamClosed), half-closed by the peer (io.EOF) or closed by the
// peer (io.EOF for code 0, else *CloseError). Every CloseWithCode call is
// logged, in order, whether or not the stream was already closed, and
// signalled on calls; CloseWrite is signalled on closeWrites.
type spliceFakeStream struct {
	mu          sync.Mutex
	cond        *sync.Cond
	localClosed bool
	peerFin     bool
	peerClose   *conduit.CloseError
	pending     []byte
	// writeErr, when set, fails every Write.
	writeErr error

	log         []conduit.CloseError
	calls       chan conduit.CloseError
	closeWrites chan struct{}
}

func newSpliceFakeStream() *spliceFakeStream {
	s := &spliceFakeStream{calls: make(chan conduit.CloseError, 16), closeWrites: make(chan struct{}, 4)}
	s.cond = sync.NewCond(&s.mu)
	return s
}

func (s *spliceFakeStream) Read(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for len(s.pending) == 0 && !s.localClosed && !s.peerFin && s.peerClose == nil {
		s.cond.Wait()
	}
	switch {
	case s.localClosed:
		return 0, conduit.ErrStreamClosed
	case len(s.pending) > 0:
		n := copy(p, s.pending)
		s.pending = s.pending[n:]
		return n, nil
	case s.peerClose != nil && s.peerClose.Code != conduit.CloseNormal:
		return 0, s.peerClose
	default:
		return 0, io.EOF
	}
}

func (s *spliceFakeStream) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.writeErr != nil:
		return 0, s.writeErr
	case s.localClosed:
		return 0, conduit.ErrStreamClosed
	}
	return len(p), nil
}

func (s *spliceFakeStream) CloseWithCode(code uint32, reason string) error {
	s.mu.Lock()
	c := conduit.CloseError{Code: code, Reason: reason}
	s.log = append(s.log, c)
	s.localClosed = true
	s.cond.Broadcast()
	s.mu.Unlock()
	s.calls <- c
	return nil
}

func (s *spliceFakeStream) Close() error                   { return s.CloseWithCode(conduit.CloseNormal, "") }
func (s *spliceFakeStream) Resize(cols, rows uint16) error { return nil }
func (s *spliceFakeStream) ID() uint32                     { return 2 }

func (s *spliceFakeStream) CloseWrite() error {
	s.closeWrites <- struct{}{}
	return nil
}

// State reports closed once either side closed the stream (not on a fin).
func (s *spliceFakeStream) State() conduit.StreamState {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.peerClose != nil || s.localClosed {
		return conduit.StateClosed
	}
	return conduit.StateActive
}

// peer applies a fin, a peer close or queued data from the far side.
func (s *spliceFakeStream) peer(fin bool, closeErr *conduit.CloseError, data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.peerFin = s.peerFin || fin
	if closeErr != nil {
		s.peerClose = closeErr
	}
	s.pending = append(s.pending, data...)
	s.cond.Broadcast()
}

func (s *spliceFakeStream) closes() []conduit.CloseError {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]conduit.CloseError(nil), s.log...)
}

// waitCalls waits until s has logged n CloseWithCode calls in total.
func waitCalls(t *testing.T, s *spliceFakeStream, n int) {
	t.Helper()
	for len(s.closes()) < n {
		select {
		case <-s.calls:
		case <-time.After(10 * time.Second):
			t.Fatalf("got %d close calls, want %d", len(s.closes()), n)
		}
	}
}

// startTunnel runs the real run() for a tunnel over user and target and
// returns a channel closed when run returns (both directions ended and the
// final closes done).
func startTunnel(user, target *spliceFakeStream) (*conduitTunnel, <-chan struct{}) {
	tun := &conduitTunnel{key: conduitTunnelKey{SessionID: "s", StreamID: 2}, user: user, target: target}
	ts := newConduitTunnelSession(nil, nil, time.Time{})
	done := make(chan struct{})
	go func() { tun.run(ts); close(done) }()
	return tun, done
}

func waitRun(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("run did not return")
	}
}

var spliceAuthzClose = conduit.CloseError{Code: conduit.CloseUnauthenticated, Reason: conduitReasonAuthzExpired}

// assertAllCloses checks that every logged close on s is want.
func assertAllCloses(t *testing.T, s *spliceFakeStream, want conduit.CloseError, leg string) {
	t.Helper()
	got := s.closes()
	require.NotEmpty(t, got, leg)
	for i, c := range got {
		assert.Equal(t, want, c, "%s close %d of %d", leg, i+1, len(got))
	}
}

// TestConduitTunnel_CloseBothPassesCodeToBothLegs: once the authorization
// close is recorded, every close of either leg carries its code: the
// re-check's own close, the close the other direction's copy makes when
// its leg ends, and run's final closes. Before the code was recorded, the
// copy's close of the other leg was a normal close (code 0).
func TestConduitTunnel_CloseBothPassesCodeToBothLegs(t *testing.T) {
	user, target := newSpliceFakeStream(), newSpliceFakeStream()
	tun, done := startTunnel(user, target)

	tun.closeBoth(spliceAuthzClose.Code, spliceAuthzClose.Reason)
	waitRun(t, done)
	// Per leg: the re-check's close, the other direction's copy, and
	// run's final close.
	waitCalls(t, user, 3)
	waitCalls(t, target, 3)
	assertAllCloses(t, user, spliceAuthzClose, "user leg")
	assertAllCloses(t, target, spliceAuthzClose, "target leg")
}

// TestConduitTunnel_FirstRecordedCloseWins: a second closeBoth does not
// change the recorded close; its closes carry the first code.
func TestConduitTunnel_FirstRecordedCloseWins(t *testing.T) {
	user, target := newSpliceFakeStream(), newSpliceFakeStream()
	tun, done := startTunnel(user, target)

	tun.closeBoth(spliceAuthzClose.Code, spliceAuthzClose.Reason)
	tun.closeBoth(relay.CloseTargetNotFound, relay.ReasonTargetNotFound)
	waitRun(t, done)
	waitCalls(t, user, 4)
	waitCalls(t, target, 4)
	assertAllCloses(t, user, spliceAuthzClose, "user leg")
	assertAllCloses(t, target, spliceAuthzClose, "target leg")
}

// TestConduitTunnel_RecordedCloseOverFin: with a close recorded, a fin on
// one leg closes the other leg with the recorded code instead of
// half-closing it.
func TestConduitTunnel_RecordedCloseOverFin(t *testing.T) {
	user, target := newSpliceFakeStream(), newSpliceFakeStream()
	tun, done := startTunnel(user, target)
	tun.closing.Store(&spliceAuthzClose)

	target.peer(true, nil, nil)
	waitCalls(t, user, 1)
	assert.Equal(t, spliceAuthzClose, user.closes()[0], "a full close with the recorded code")
	waitRun(t, done)
	select {
	case <-user.closeWrites:
		t.Fatal("the user leg was half-closed")
	default:
	}
	assertAllCloses(t, user, spliceAuthzClose, "user leg")
	assertAllCloses(t, target, spliceAuthzClose, "target leg")
}

// TestConduitTunnel_RecordedCloseOnWriteError: with a close recorded, a
// write that fails on one leg closes the source leg with the recorded
// code.
func TestConduitTunnel_RecordedCloseOnWriteError(t *testing.T) {
	user, target := newSpliceFakeStream(), newSpliceFakeStream()
	user.writeErr = &conduit.CloseError{Code: conduit.CloseRelayRestart, Reason: "relay_restart"}
	tun, done := startTunnel(user, target)
	tun.closing.Store(&spliceAuthzClose)

	target.peer(false, nil, []byte("data"))
	waitCalls(t, target, 1)
	assert.Equal(t, spliceAuthzClose, target.closes()[0], "the source leg closes with the recorded code")
	user.peer(true, nil, nil)
	waitRun(t, done)
	assertAllCloses(t, user, spliceAuthzClose, "user leg")
	assertAllCloses(t, target, spliceAuthzClose, "target leg")
}

// TestConduitTunnel_SpliceWithoutRecordedClose: with no recorded close the
// splice passes a leg's own end on, as before: a peer close code goes to
// the other leg, a normal close stays normal, a fin half-closes, a failed
// write closes the source with the destination's code, and run's final
// closes are normal.
func TestConduitTunnel_SpliceWithoutRecordedClose(t *testing.T) {
	normal := conduit.CloseError{Code: conduit.CloseNormal}
	restart := conduit.CloseError{Code: conduit.CloseRelayRestart, Reason: "relay_restart"}

	t.Run("peer close code", func(t *testing.T) {
		user, target := newSpliceFakeStream(), newSpliceFakeStream()
		_, done := startTunnel(user, target)
		target.peer(false, &restart, nil)
		waitCalls(t, user, 1)
		assert.Equal(t, restart, user.closes()[0])
		waitRun(t, done)
		assert.Equal(t, normal, user.closes()[len(user.closes())-1], "run's final close")
	})
	t.Run("normal close", func(t *testing.T) {
		user, target := newSpliceFakeStream(), newSpliceFakeStream()
		_, done := startTunnel(user, target)
		target.peer(false, &normal, nil)
		waitCalls(t, user, 1)
		assert.Equal(t, normal, user.closes()[0])
		waitRun(t, done)
		assertAllCloses(t, user, normal, "user leg")
		assertAllCloses(t, target, normal, "target leg")
	})
	t.Run("half-close", func(t *testing.T) {
		user, target := newSpliceFakeStream(), newSpliceFakeStream()
		_, done := startTunnel(user, target)
		target.peer(true, nil, nil)
		select {
		case <-user.closeWrites:
		case <-time.After(10 * time.Second):
			t.Fatal("the user leg was not half-closed")
		}
		assert.Empty(t, user.closes(), "a fin only half-closes the other leg")
		user.peer(true, nil, nil)
		waitRun(t, done)
		assertAllCloses(t, user, normal, "user leg")
		assertAllCloses(t, target, normal, "target leg")
	})
	t.Run("write error", func(t *testing.T) {
		user, target := newSpliceFakeStream(), newSpliceFakeStream()
		user.writeErr = &restart
		_, done := startTunnel(user, target)
		target.peer(false, nil, []byte("data"))
		waitCalls(t, target, 1)
		assert.Equal(t, restart, target.closes()[0], "the source leg closes with the destination's code")
		user.peer(true, nil, nil)
		waitRun(t, done)
	})
}
