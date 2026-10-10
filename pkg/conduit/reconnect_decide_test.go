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

package conduit

import (
	"context"
	"errors"
	"testing"
	"time"

	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// decideCall is one Reconnector.Decide invocation.
type decideCall struct {
	end   End
	delay time.Duration
}

// TestReconnectorDecideSeesEnds: Decide is told about a refused handshake
// (with its close code) and about a session the relay closed with a code,
// together with the default delay.
func TestReconnectorDecideSeesEnds(t *testing.T) {
	calls := make(chan decideCall, 4)
	h := newReconnectHarnessWith(t, func(r *Reconnector) {
		r.Decide = func(_ context.Context, end End, d time.Duration) (time.Duration, error) {
			calls <- decideCall{end, d}
			return d, nil
		}
	})
	next := func() decideCall {
		t.Helper()
		select {
		case c := <-calls:
			return c
		case <-time.After(waitTimeout):
			t.Fatal("Decide not called")
			return decideCall{}
		}
	}

	h.plan <- &CloseError{Code: CloseUnauthenticated, Reason: "unauthenticated"}
	h.expectCeiling(time.Second)
	c := next()
	if c.end.DialErr == nil || c.end.Code() != CloseUnauthenticated || c.delay != time.Second {
		t.Fatalf("dial end = %+v (code %d), delay %v", c.end, c.end.Code(), c.delay)
	}
	settle(t, h.clk, 1)
	h.clk.Advance(time.Second)

	_, relay := h.connect()
	h.age(time.Second)
	if err := relay.CloseWithCode(CloseForbidden, "forbidden"); err != nil {
		t.Fatal(err)
	}
	c = next()
	if c.end.DialErr != nil || c.end.Code() != CloseForbidden || c.end.Lived != time.Second {
		t.Fatalf("session end = %+v (code %d)", c.end, c.end.Code())
	}
}

// TestReconnectorDecideOverridesDelay: the delay Decide returns replaces
// the default one.
func TestReconnectorDecideOverridesDelay(t *testing.T) {
	const override = 45 * time.Second
	decided := make(chan struct{}, 1)
	h := newReconnectHarnessWith(t, func(r *Reconnector) {
		r.Decide = func(context.Context, End, time.Duration) (time.Duration, error) {
			decided <- struct{}{}
			return override, nil
		}
	})
	h.plan <- errors.New("refused")
	select {
	case <-decided:
	case <-time.After(waitTimeout):
		t.Fatal("Decide not called")
	}
	settle(t, h.clk, 1)
	h.clk.Advance(override - time.Millisecond)
	if n := h.clk.Pending(); n != 1 {
		t.Fatalf("redial timer fired before the overridden delay (pending %d)", n)
	}
	h.clk.Advance(time.Millisecond)
	h.connect()
}

// TestReconnectorDecideStops: a Decide error ends Run with that error,
// both for a refused handshake and for a live session closed by the relay.
func TestReconnectorDecideStops(t *testing.T) {
	terminal := errors.New("terminal")
	stopOn := func(_ context.Context, end End, d time.Duration) (time.Duration, error) {
		if end.Code() == closeSupersededTest {
			return 0, terminal
		}
		return d, nil
	}
	tests := []struct {
		name string
		end  func(h *reconnectHarness)
	}{
		{"refused handshake", func(h *reconnectHarness) {
			h.plan <- &CloseError{Code: closeSupersededTest, Reason: "superseded_incarnation"}
		}},
		{"session closed", func(h *reconnectHarness) {
			_, relay := h.connect()
			if err := relay.CloseWithCode(closeSupersededTest, "superseded_incarnation"); err != nil {
				h.t.Fatal(err)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newReconnectHarnessWith(t, func(r *Reconnector) { r.Decide = stopOn })
			tt.end(h)
			select {
			case err := <-h.done:
				h.done <- err // for the cleanup
				if !errors.Is(err, terminal) {
					t.Fatalf("Run = %v, want %v", err, terminal)
				}
			case <-time.After(waitTimeout):
				t.Fatal("Run did not stop")
			}
		})
	}
}

// closeSupersededTest stands in for the relay's 4409 (defined in
// pkg/conduit/relay, which this package cannot import).
const closeSupersededTest uint32 = 4409

// TestEndCode pins End.Code's precedence: GoAway, then DialErr, then
// SessionErr.
func TestEndCode(t *testing.T) {
	tests := []struct {
		name string
		end  End
		want uint32
	}{
		{"empty", End{}, 0},
		{"plain dial error", End{DialErr: errors.New("x")}, 0},
		{"dial close error", End{DialErr: &CloseError{Code: 4403}}, 4403},
		{"goaway wins", End{GoAway: &conduitv1.GoAway{Code: 4503}, SessionErr: &CloseError{Code: 4401}}, 4503},
		{"session error", End{SessionErr: &CloseError{Code: 4401}}, 4401},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.end.Code(); got != tt.want {
				t.Fatalf("Code() = %d, want %d", got, tt.want)
			}
		})
	}
}
