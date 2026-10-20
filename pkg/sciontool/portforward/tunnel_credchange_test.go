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

package portforward

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	scionhub "github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
)

// runUntilConnects runs m.run with connect answering from answer, where the
// delay timer never fires, and returns once connect was called n times
// (or fails the test after a real-time bound).
func runUntilConnects(t *testing.T, m *Manager, n int, answer func(call int) (bool, error)) []time.Duration {
	t.Helper()
	m.backoff.Rand = func() float64 { return 0 }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	reached := make(chan struct{})
	m.connect = func(context.Context) (bool, error) {
		calls++
		if calls == n {
			close(reached)
			<-ctx.Done()
			return false, ctx.Err()
		}
		return answer(calls)
	}
	var delays []time.Duration
	m.after = func(d time.Duration) <-chan time.Time {
		delays = append(delays, d)
		return nil // never fires: only a credential change can wake the loop
	}
	done := make(chan struct{})
	go func() {
		m.run(ctx)
		close(done)
	}()
	select {
	case <-reached:
	case <-time.After(10 * time.Second):
		cancel()
		<-done
		t.Fatalf("connect called %d times, want %d: the loop did not wake", calls, n)
	}
	cancel()
	<-done
	return delays
}

// A credential change during a refused connect wakes the 401 wait at once.
func TestRun_UnauthorizedWakesOnCredentialChangeDuringConnect(t *testing.T) {
	client := scionhub.NewClientWithConfig("http://127.0.0.1:1", "old-credential", "agent-1")
	m := NewManager(client)
	err401 := &scionhub.StatusError{StatusCode: http.StatusUnauthorized}
	delays := runUntilConnects(t, m, 2, func(call int) (bool, error) {
		// The credential changes while the refused request is in flight.
		client.SetToken("new-credential")
		return false, err401
	})
	if len(delays) != 1 || delays[0] != maxReconnectDelay {
		t.Fatalf("delays = %v, want one wait of %s", delays, maxReconnectDelay)
	}
}

// A credential change after the loop has parked on a 401 also wakes it.
func TestRun_UnauthorizedWakesOnLaterCredentialChange(t *testing.T) {
	client := scionhub.NewClientWithConfig("http://127.0.0.1:1", "old-credential", "agent-1")
	m := NewManager(client)
	err401 := &scionhub.StatusError{StatusCode: http.StatusUnauthorized}
	parked := make(chan struct{})
	go func() {
		<-parked
		client.SetToken("new-credential")
	}()
	m.backoff.Rand = func() float64 { return 0 }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	second := make(chan struct{})
	m.connect = func(context.Context) (bool, error) {
		calls++
		if calls == 2 {
			close(second)
			<-ctx.Done()
			return false, ctx.Err()
		}
		return false, err401
	}
	m.after = func(time.Duration) <-chan time.Time {
		close(parked)
		return nil
	}
	done := make(chan struct{})
	go func() { m.run(ctx); close(done) }()
	select {
	case <-second:
	case <-time.After(10 * time.Second):
		t.Fatal("the loop did not wake on the credential change")
	}
	cancel()
	<-done
}

// A credential change does not cut a backoff for a failure other than 401:
// the second connect happens only after the test fires the backoff timer.
func TestRun_CredentialChangeDoesNotCutOtherBackoff(t *testing.T) {
	client := scionhub.NewClientWithConfig("http://127.0.0.1:1", "old-credential", "agent-1")
	m := NewManager(client)
	m.backoff.Rand = func() float64 { return 0 }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var fired atomic.Bool
	firedAtSecond := make(chan bool, 1)
	calls := 0
	m.connect = func(ctx context.Context) (bool, error) {
		calls++
		if calls == 2 {
			firedAtSecond <- fired.Load()
			<-ctx.Done()
			return false, ctx.Err()
		}
		// The credential changes during the failing connect.
		client.SetToken("new-credential")
		return false, errors.New("dial failed")
	}
	timer := make(chan time.Time, 1)
	waiting := make(chan struct{})
	m.after = func(time.Duration) <-chan time.Time {
		close(waiting)
		return timer
	}
	done := make(chan struct{})
	go func() { m.run(ctx); close(done) }()
	<-waiting
	fired.Store(true)
	timer <- time.Time{}
	select {
	case ok := <-firedAtSecond:
		if !ok {
			t.Fatal("a credential change cut a non-401 backoff")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no second connect after the backoff timer fired")
	}
	cancel()
	<-done
}
