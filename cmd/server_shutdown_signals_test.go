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

package cmd

import (
	"bytes"
	"context"
	"log"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// lockedBuffer is a bytes.Buffer safe for the log package's writes from
// the signal goroutine.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// resetServerShutdownForTest clears the published run-once server
// shutdown now and when the test ends, for tests that call
// installServerShutdownSignals (directly or through runServerStart).
func resetServerShutdownForTest(t *testing.T) {
	t.Helper()
	serverShutdown.Store(nil)
	t.Cleanup(func() { serverShutdown.Store(nil) })
}

// captureStdLog sends the standard logger's output to a buffer for the
// rest of the test.
func captureStdLog(t *testing.T) *lockedBuffer {
	t.Helper()
	buf := &lockedBuffer{}
	savedOut, savedFlags := log.Writer(), log.Flags()
	log.SetOutput(buf)
	t.Cleanup(func() {
		log.SetOutput(savedOut)
		log.SetFlags(savedFlags)
	})
	return buf
}

// fakeSignalNotify records what installServerShutdownSignals subscribes to
// and hands the test the channel, so signals are delivered without
// signalling the process. Like signal.Notify, delivery never blocks.
type fakeSignalNotify struct {
	calls int
	ch    chan<- os.Signal
	sigs  []os.Signal
}

func (f *fakeSignalNotify) notify(c chan<- os.Signal, sigs ...os.Signal) {
	f.calls++
	f.ch = c
	f.sigs = append([]os.Signal(nil), sigs...)
}

func (f *fakeSignalNotify) deliver(sig os.Signal) {
	select {
	case f.ch <- sig:
	default:
	}
}

// PS-1: SIGTERM and SIGINT each start the graceful shutdown by calling
// cancel exactly once; a second signal of either kind causes no second
// cancel and no panic; the subscribed set is exactly {Interrupt, SIGTERM}.
func TestInstallServerShutdownSignals_SingleCancel(t *testing.T) {
	for _, tc := range []struct {
		name          string
		first, second os.Signal
		logged        string
	}{
		{"SIGTERM then SIGINT", syscall.SIGTERM, os.Interrupt, "Received signal terminated, shutting down..."},
		{"SIGINT then SIGTERM", os.Interrupt, syscall.SIGTERM, "Received signal interrupt, shutting down..."},
		{"SIGTERM twice", syscall.SIGTERM, syscall.SIGTERM, "Received signal terminated, shutting down..."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetServerShutdownForTest(t)
			logs := captureStdLog(t)
			var f fakeSignalNotify
			var cancels atomic.Int32
			cancelled := make(chan struct{}, 2)
			ctx, stop := context.WithCancel(context.Background())
			t.Cleanup(stop)
			done := installServerShutdownSignals(ctx, f.notify, func() {
				cancels.Add(1)
				cancelled <- struct{}{}
			})

			require.Equal(t, 1, f.calls, "notify is called once")
			assert.ElementsMatch(t, []os.Signal{os.Interrupt, syscall.SIGTERM}, f.sigs)
			require.NotNil(t, f.ch)
			assert.NotNil(t, serverShutdown.Load(), "the run-once shutdown is published after notify")

			f.deliver(tc.first)
			<-cancelled
			assert.Contains(t, logs.String(), tc.logged)
			<-done // the signal goroutine has exited after its one shutdown

			// A second signal stays unread in the (still subscribed)
			// channel and can never reach cancel.
			f.deliver(tc.second)
			assert.Equal(t, 1, len(f.ch), "the second signal is not consumed")
			f.deliver(tc.second) // channel full: dropped, as signal.Notify does
			assert.Equal(t, int32(1), cancels.Load(), "cancel is called exactly once")
			assert.Equal(t, 1, strings.Count(logs.String(), "Received signal"), "the shutdown line is logged once")
		})
	}
}

// N3-a (R11a): when ctx is done first (runServerStart cancels it on every
// return), the signal goroutine exits without starting a shutdown; a later
// signal stays unconsumed in the still-subscribed channel (so it is
// swallowed, not handled by the default action), with no cancel and no log
// line.
func TestInstallServerShutdownSignals_CtxDoneFirstExitsWithoutShutdown(t *testing.T) {
	resetServerShutdownForTest(t)
	logs := captureStdLog(t)
	var f fakeSignalNotify
	var cancels atomic.Int32
	ctx, stop := context.WithCancel(context.Background())
	done := installServerShutdownSignals(ctx, f.notify, func() { cancels.Add(1) })

	stop()
	<-done // the goroutine has exited

	f.deliver(syscall.SIGTERM)
	assert.Equal(t, 1, len(f.ch), "the signal is not consumed")
	assert.Equal(t, int32(0), cancels.Load(), "no shutdown after ctx is done")
	assert.NotContains(t, logs.String(), "Received signal")
}

// N3-b (R11a): a signal first runs the shutdown once (one log line), then
// the goroutine exits; a second signal stays unconsumed.
func TestInstallServerShutdownSignals_SignalFirstExitsAfterShutdown(t *testing.T) {
	resetServerShutdownForTest(t)
	logs := captureStdLog(t)
	var f fakeSignalNotify
	var cancels atomic.Int32
	cancelled := make(chan struct{}, 2)
	ctx, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)
	done := installServerShutdownSignals(ctx, f.notify, func() {
		cancels.Add(1)
		cancelled <- struct{}{}
	})

	f.deliver(syscall.SIGTERM)
	<-cancelled
	<-done

	f.deliver(os.Interrupt)
	assert.Equal(t, 1, len(f.ch), "the second signal is not consumed")
	assert.Equal(t, int32(1), cancels.Load())
	assert.Equal(t, 1, strings.Count(logs.String(), "Received signal"), "the shutdown line is logged once")
}

// blockingLogWriter blocks every write until released, reporting the first
// write on entered.
type blockingLogWriter struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (w *blockingLogWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.entered) })
	<-w.release
	return len(p), nil
}

// N3-c (R11b): while the shutdown's log write is blocked, the signal
// goroutine has not exited (done stays open) even though cancel has not
// run yet; once the write returns, cancel runs and the goroutine exits. Its
// lifetime is bounded by the logger, as on base. Production never waits on
// done.
func TestInstallServerShutdownSignals_GoroutineBoundedByLogWrite(t *testing.T) {
	resetServerShutdownForTest(t)
	w := &blockingLogWriter{entered: make(chan struct{}), release: make(chan struct{})}
	savedOut, savedFlags := log.Writer(), log.Flags()
	log.SetOutput(w)
	t.Cleanup(func() {
		log.SetOutput(savedOut)
		log.SetFlags(savedFlags)
	})
	var f fakeSignalNotify
	var cancels atomic.Int32
	ctx, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)
	done := installServerShutdownSignals(ctx, f.notify, func() { cancels.Add(1) })

	f.deliver(syscall.SIGTERM)
	<-w.entered
	select {
	case <-done:
		t.Fatal("the goroutine exited while its log write was blocked")
	default:
	}
	assert.Equal(t, int32(0), cancels.Load(), "cancel runs after the log line")

	close(w.release)
	<-done
	assert.Equal(t, int32(1), cancels.Load())
}

// PS-2: a SIGTERM-initiated shutdown runs the step-16 tail in order:
// cancel, then the server wait returns, then the audit writer closes, then
// runServerStart's deferred log cleanups. The harness mirrors
// runServerStart's structure and calls the real installServerShutdownSignals
// and awaitServerExit; only notify, wait, close and the log cleanup are fakes.
func TestServerShutdown_SIGTERMOrderedPath(t *testing.T) {
	resetServerShutdownForTest(t)
	events := make(chan string, 8)
	var f fakeSignalNotify
	installed := make(chan struct{})

	run := func() error {
		defer func() { events <- "log-cleanup" }() // step 1's deferred log cleanups
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		_ = installServerShutdownSignals(ctx, f.notify, func() {
			events <- "cancel"
			cancel()
		})
		close(installed)
		return awaitServerExit(ctx, make(chan error), cancel,
			func() { events <- "wait-return" },
			func(context.Context) error {
				events <- "close"
				return nil
			})
	}

	done := make(chan error, 1)
	go func() { done <- run() }()
	<-installed
	f.deliver(syscall.SIGTERM)
	require.NoError(t, <-done)

	close(events)
	var got []string
	for e := range events {
		got = append(got, e)
	}
	assert.Equal(t, []string{"cancel", "wait-return", "close", "log-cleanup"}, got)
}
