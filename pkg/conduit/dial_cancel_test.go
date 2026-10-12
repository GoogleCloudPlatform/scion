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
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/transport"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// setAfterDialHook installs f as Dial's after-connect hook for the test.
func setAfterDialHook(t *testing.T, f func(context.Context)) {
	t.Helper()
	prev := afterDialHook
	afterDialHook = f
	t.Cleanup(func() { afterDialHook = prev })
}

// countingAdmitter counts Admit calls: an admitted Hello is a session the
// relay registers.
type countingAdmitter struct {
	testAdmitter
	admits atomic.Int32
}

func (a *countingAdmitter) Admit(ctx context.Context, h *conduitv1.Hello) (*conduitv1.Welcome, error) {
	a.admits.Add(1)
	return a.testAdmitter.Admit(ctx, h)
}

// assertNoHello reads the relay end of a pipe the dialer has already
// closed: it must report the close, not a buffered Hello.
func assertNoHello(t *testing.T, relayEnd transport.Conn) {
	t.Helper()
	b, err := relayEnd.ReadFrame()
	if err == nil {
		t.Fatalf("relay end received a frame (%d bytes) after a cancelled dial, want none", len(b))
	}
	if !errors.Is(err, transport.ErrClosed) {
		t.Fatalf("relay end read: %v, want %v", err, transport.ErrClosed)
	}
}

// A connect that completes after its context is cancelled is closed and
// no Hello is sent.
func TestDialCancelledBeforeConnectReturnsSendsNoHello(t *testing.T) {
	// Buffered, so a Hello written by Dial would sit in the pipe and be
	// read back below even though Dial closes the connection.
	a, b := transport.Pipe(transport.MemoryOptions{Buffer: 1})
	defer func() { _ = b.Close() }()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d := transport.DialerFunc(func(context.Context) (transport.Conn, error) {
		cancel() // the client stops while the connect is completing
		return a, nil
	})
	s, _, err := Dial(ctx, d, Config{}, testHello())
	if s != nil {
		_ = s.Close()
		t.Fatal("Dial returned a session for a cancelled context")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Dial: %v, want %v", err, context.Canceled)
	}
	assertNoHello(t, b)
}

// Cancelling at the hook between the connect and the Hello sends no Hello
// and leaves the relay with no admitted session.
func TestDialCancelAtHookSendsNoHello(t *testing.T) {
	l := transport.NewMemoryListener(transport.MemoryOptions{Buffer: 1})
	defer func() { _ = l.Close() }()
	adm := &countingAdmitter{}
	acceptErr := make(chan error, 1)
	go func() {
		c, err := l.Accept(context.Background())
		if err != nil {
			acceptErr <- err
			return
		}
		s, err := Accept(context.Background(), c, Config{}, adm)
		if s != nil {
			_ = s.Close()
		}
		acceptErr <- err
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hooked := make(chan struct{})
	setAfterDialHook(t, func(hctx context.Context) {
		if hctx != ctx {
			t.Error("hook got a context other than Dial's")
		}
		cancel()
		close(hooked)
	})

	s, _, err := Dial(ctx, l, Config{}, testHello())
	if s != nil {
		_ = s.Close()
		t.Fatal("Dial returned a session for a context cancelled at the hook")
	}
	select {
	case <-hooked:
	default:
		t.Fatal("after-dial hook did not run")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Dial: %v, want %v", err, context.Canceled)
	}
	select {
	case err := <-acceptErr:
		if err == nil {
			t.Fatal("relay Accept succeeded for a cancelled dial")
		}
	case <-time.After(waitTimeout):
		t.Fatal("relay Accept did not return after the dialer closed")
	}
	if n := adm.admits.Load(); n != 0 {
		t.Fatalf("relay admitted %d sessions, want 0 (a Hello was sent)", n)
	}
}

// Dial returns promptly when the connect never completes until its
// context is cancelled.
func TestDialBlockedUntilCancelReturnsPromptly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered := make(chan struct{})
	d := transport.DialerFunc(func(ctx context.Context) (transport.Conn, error) {
		close(entered)
		<-ctx.Done()
		return nil, ctx.Err()
	})
	res := make(chan error, 1)
	go func() {
		s, _, err := Dial(ctx, d, Config{}, testHello())
		if s != nil {
			_ = s.Close()
		}
		res <- err
	}()
	<-entered
	cancel()
	select {
	case err := <-res:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Dial: %v, want %v", err, context.Canceled)
		}
	case <-time.After(waitTimeout):
		t.Fatal("Dial did not return after its context was cancelled")
	}
}

// Stopping a Reconnector whose connect only completes once it is stopped
// returns promptly and sends no Hello on that late connection.
func TestReconnectorStopsWhileDialBlocked(t *testing.T) {
	a, b := transport.Pipe(transport.MemoryOptions{Buffer: 1})
	defer func() { _ = b.Close() }()
	entered := make(chan struct{})
	var dials atomic.Int32
	r := &Reconnector{
		Dialer: transport.DialerFunc(func(ctx context.Context) (transport.Conn, error) {
			if dials.Add(1) != 1 {
				t.Error("Reconnector redialed after it was stopped")
				return nil, ctx.Err()
			}
			close(entered)
			<-ctx.Done()
			return a, nil // the connect completes only after the stop
		}),
		Hello: testHello,
		OnSession: func(_ context.Context, s Session, _ *conduitv1.Welcome) {
			t.Error("Reconnector established a session after it was stopped")
			_ = s.Close()
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	res := make(chan error, 1)
	go func() { res <- r.Run(ctx) }()
	<-entered
	cancel()
	select {
	case err := <-res:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run: %v, want %v", err, context.Canceled)
		}
	case <-time.After(waitTimeout):
		t.Fatal("Run did not return after it was stopped")
	}
	assertNoHello(t, b)
}
