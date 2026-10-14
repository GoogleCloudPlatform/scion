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
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// GrantKeysChanged (design v2.21.27 §3.5, Key-set freshness; criterion 29).

// TestGrantKeysChangedFromRelayReachesTarget: each GrantKeysChanged the
// relay sends reaches the target's hook, and the session stays up.
func TestGrantKeysChangedFromRelayReachesTarget(t *testing.T) {
	var got atomic.Int32
	p := newPair(t, Config{OnGrantKeysChanged: func(context.Context) { got.Add(1) }}, Config{})
	for range 2 {
		if err := p.relay.NotifyGrantKeysChanged(); err != nil {
			t.Fatal(err)
		}
	}
	eventually(t, "two hints delivered", func() bool { return got.Load() == 2 })
	if p.dialer.isDone() || p.relay.isDone() {
		t.Fatal("a session ended after GrantKeysChanged")
	}
}

// TestGrantKeysChangedFromDialerIsBadFrame: GrantKeysChanged only travels
// relay → target. One sent by a dialer ends the session with 4400
// bad_frame, is logged, and never reaches a hook on the relay side.
func TestGrantKeysChangedFromDialerIsBadFrame(t *testing.T) {
	var logBuf syncBuffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	var relayHook atomic.Int32
	p := newPair(t, Config{}, Config{Logger: logger, OnGrantKeysChanged: func(context.Context) { relayHook.Add(1) }})
	if err := p.dialer.sendControl(context.Background(), &conduitv1.Frame{Body: &conduitv1.Frame_GrantKeysChanged{GrantKeysChanged: &conduitv1.GrantKeysChanged{}}}); err != nil {
		t.Fatal(err)
	}
	err := waitDone(t, p.relay)
	var ce *CloseError
	if !errors.As(err, &ce) || ce.Code != CloseProtocolError || !strings.HasPrefix(ce.Reason, "bad_frame") {
		t.Fatalf("relay err = %v, want 4400 bad_frame", err)
	}
	if err := waitDone(t, p.dialer); CodeOf(err, 0) != CloseProtocolError {
		t.Fatalf("dialer err = %v, want 4400", err)
	}
	if n := relayHook.Load(); n != 0 {
		t.Fatalf("relay-side hook called %d times", n)
	}
	if logged := logBuf.String(); !strings.Contains(logged, "dialer sent GrantKeysChanged") || !strings.Contains(logged, "principal_id=agent-1") {
		t.Errorf("log %q does not record the dialer's frame", logged)
	}
}

// TestNotifyGrantKeysChangedOnDialerRefused: a dialer cannot send the hint.
func TestNotifyGrantKeysChangedOnDialerRefused(t *testing.T) {
	p := newPair(t, Config{}, Config{})
	if err := p.dialer.NotifyGrantKeysChanged(); !errors.Is(err, ErrNotRelaySide) {
		t.Fatalf("err = %v, want ErrNotRelaySide", err)
	}
	if got := FrameType(&conduitv1.Frame{Body: &conduitv1.Frame_GrantKeysChanged{GrantKeysChanged: &conduitv1.GrantKeysChanged{}}}); got != "grant_keys_changed" {
		t.Fatalf("FrameType = %q", got)
	}
}

// TestNotifyGrantKeysChangedNeverWaits: with the relay side's writer blocked
// and its outbound buffer full, NotifyGrantKeysChanged returns
// ErrBufferBudget at once and the session stays up, so a relay notifying
// many sessions is never held up by one.
func TestNotifyGrantKeysChangedNeverWaits(t *testing.T) {
	release := make(chan struct{})
	p := newPair(t, Config{}, Config{Interceptor: func(dir Direction, f *conduitv1.Frame) []*conduitv1.Frame {
		if dir == Outbound && f.GetPing().GetNonce() == 777 {
			<-release
		}
		return []*conduitv1.Frame{f}
	}})
	t.Cleanup(func() { close(release) })
	sc := p.relay.sched
	ping := func(nonce uint64, size int64) *outFrame {
		return &outFrame{f: &conduitv1.Frame{Body: &conduitv1.Frame_Ping{Ping: &conduitv1.Ping{Nonce: nonce}}}, class: classControl, size: size}
	}
	if err := sc.enqueueInternal(ping(777, 1)); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the writer holds the blocking frame", func() bool {
		sc.mu.Lock()
		defer sc.mu.Unlock()
		return sc.queuedLocked() == 0
	})
	if err := sc.enqueueInternal(ping(778, sc.budget+controlReserve)); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- p.relay.NotifyGrantKeysChanged() }()
	select {
	case err := <-done:
		if !errors.Is(err, ErrBufferBudget) {
			t.Fatalf("err = %v, want ErrBufferBudget", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("NotifyGrantKeysChanged waited on a session with a full buffer")
	}
	if p.relay.isDone() {
		t.Fatal("the session ended")
	}
}
