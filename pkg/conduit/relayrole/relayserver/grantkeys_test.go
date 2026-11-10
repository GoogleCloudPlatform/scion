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

package relayserver

import (
	"context"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"
)

// TestListenReconnectHook: the first LISTEN signals nothing; every later
// (re)connect signals once, without blocking when a signal is pending.
func TestListenReconnectHook(t *testing.T) {
	reconnected := make(chan struct{}, 1)
	hook := listenReconnectHook(reconnected)
	hook()
	select {
	case <-reconnected:
		t.Fatal("the first LISTEN signalled a reconnect")
	default:
	}
	hook()
	hook() // pending already: must not block
	select {
	case <-reconnected:
	default:
		t.Fatal("a reconnect after a drop did not signal")
	}
	select {
	case <-reconnected:
		t.Fatal("two reconnects with one pending signalled twice")
	default:
	}
}

// TestFollowGrantKeys: each grant key change notice and each reconnect
// signal makes the relay hint its targets once; the loop stops (and
// releases its subscription) when the context ends or the event channel
// closes.
func TestFollowGrantKeys(t *testing.T) {
	keys := make(chan string)
	reconnected := make(chan struct{})
	var hints atomic.Int32
	notified := make(chan struct{}, 4)
	notify := func() int { hints.Add(1); notified <- struct{}{}; return 1 }
	stopped := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go followGrantKeys(ctx, slog.New(slog.DiscardHandler), keys, reconnected, notify, func() { close(stopped) })

	wait := func(what string) {
		t.Helper()
		select {
		case <-notified:
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: no hint", what)
		}
	}
	keys <- GrantKeysChangedSubject
	wait("grant key change notice")
	reconnected <- struct{}{}
	wait("LISTEN reconnect")
	if n := hints.Load(); n != 2 {
		t.Fatalf("hints = %d, want 2", n)
	}

	close(keys)
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("the loop did not stop when the event channel closed")
	}

	// A cancelled context stops it too.
	ctx2, cancel2 := context.WithCancel(context.Background())
	stopped2 := make(chan struct{})
	go followGrantKeys(ctx2, slog.New(slog.DiscardHandler), make(chan string), make(chan struct{}), notify, func() { close(stopped2) })
	cancel2()
	select {
	case <-stopped2:
	case <-time.After(5 * time.Second):
		t.Fatal("the loop did not stop when the context ended")
	}
}
