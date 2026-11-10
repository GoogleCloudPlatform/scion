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

package tunnelclient

import (
	"sync/atomic"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
)

// closeCountingSession is a conduit.LocalSession that only counts Close
// calls; it never ends and never receives GoAway.
type closeCountingSession struct {
	conduit.LocalSession
	closes atomic.Int32
	done   chan struct{}
}

func newCloseCountingSession() *closeCountingSession {
	return &closeCountingSession{done: make(chan struct{})}
}

func (s *closeCountingSession) Close() error                    { s.closes.Add(1); return nil }
func (s *closeCountingSession) Done() <-chan struct{}           { return s.done }
func (s *closeCountingSession) GoAwayReceived() <-chan struct{} { return nil }

// TestAcquireCurrentHoldsSessionAcrossReplacement (ptone/scion#4634): a
// session taken for a tunnel request by acquireCurrent is already counted
// as in use, so a replacement (setCurrent) that lands before the request
// is sent leaves it open; the request's release then closes it.
func TestAcquireCurrentHoldsSessionAcrossReplacement(t *testing.T) {
	ls1, ls2 := newCloseCountingSession(), newCloseCountingSession()
	// setCurrent starts a watch goroutine per session; ending the sessions
	// at cleanup lets them return.
	t.Cleanup(func() { close(ls1.done); close(ls2.done) })
	s1, s2 := &session{ls: ls1}, &session{ls: ls2}
	c := &client{cfg: Config{Endpoint: "https://hub.example.com"}, tunnels: map[*tunnelEntry]struct{}{}}
	c.setCurrent(s1)

	got := c.acquireCurrent()
	if got != s1 {
		t.Fatalf("acquireCurrent returned %p, want the current session %p", got, s1)
	}
	c.setCurrent(s2)
	if n := ls1.closes.Load(); n != 0 {
		t.Fatalf("the replaced session was closed %d times while a request held it", n)
	}
	c.release(got)
	if n := ls1.closes.Load(); n != 1 {
		t.Fatalf("the replaced session was closed %d times after its last request ended, want 1", n)
	}
	if n := ls2.closes.Load(); n != 0 {
		t.Fatalf("the current session was closed %d times", n)
	}
}
