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

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/clock"
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

// newTestShutdown returns a shutdown sequence on the real clock whose
// cancel is cancel.
func newTestShutdown(cancel context.CancelFunc) *serverShutdownSequence {
	return newServerShutdownSequence(cancel, clock.Real())
}

// PS-1: SIGTERM and SIGINT each start the graceful shutdown by calling
// cancel exactly once; the subscribed set is exactly {Interrupt, SIGTERM},
// and the run-once shutdown is published after notify.
func TestInstallServerShutdownSignals_SingleCancel(t *testing.T) {
	for _, tc := range []struct {
		name   string
		first  os.Signal
		logged string
	}{
		{"SIGTERM", syscall.SIGTERM, "Received signal terminated, shutting down..."},
		{"SIGINT", os.Interrupt, "Received signal interrupt, shutting down..."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetServerShutdownForTest(t)
			logs := captureStdLog(t)
			var f fakeSignalNotify
			var cancels atomic.Int32
			cancelled := make(chan struct{}, 2)
			ctx, stop := context.WithCancel(context.Background())
			q := newTestShutdown(func() {
				cancels.Add(1)
				cancelled <- struct{}{}
			})
			done := installServerShutdownSignals(ctx, f.notify, q)

			require.Equal(t, 1, f.calls, "notify is called once")
			assert.ElementsMatch(t, []os.Signal{os.Interrupt, syscall.SIGTERM}, f.sigs)
			require.NotNil(t, f.ch)
			assert.NotNil(t, serverShutdown.Load(), "the run-once shutdown is published after notify")

			f.deliver(tc.first)
			<-cancelled
			assert.Contains(t, logs.String(), tc.logged)
			stop()
			<-done
			assert.Equal(t, int32(1), cancels.Load(), "cancel is called exactly once")
			assert.Equal(t, 1, strings.Count(logs.String(), "Received signal"), "the shutdown line is logged once")
			select {
			case <-q.forced():
				t.Fatal("one signal forced the shutdown")
			default:
			}
		})
	}
}

// R6 (ptone/scion#5054): a second signal from outside the process on the
// server's subscription forces the shutdown (an interactive Ctrl+C never
// waits out a drain): the remaining pre-cancel steps are skipped, cancel
// runs at once and q.forced is closed.
func TestInstallServerShutdownSignals_SecondSignalForces(t *testing.T) {
	for _, tc := range []struct {
		name          string
		first, second os.Signal
	}{
		{"SIGTERM then SIGINT", syscall.SIGTERM, os.Interrupt},
		{"SIGINT then SIGINT", os.Interrupt, os.Interrupt},
		{"SIGTERM then SIGTERM", syscall.SIGTERM, syscall.SIGTERM},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetServerShutdownForTest(t)
			logs := captureStdLog(t)
			var f fakeSignalNotify
			cancelled := make(chan struct{}, 4)
			ctx, stop := context.WithCancel(context.Background())
			t.Cleanup(stop)
			q := newTestShutdown(func() { cancelled <- struct{}{} })
			// A drain step that blocks until the test releases it, as a
			// slow drain would.
			drainEntered, releaseDrain := make(chan struct{}), make(chan struct{})
			var laterRan atomic.Bool
			require.True(t, q.addStep(shutdownPhaseMark, func() {
				close(drainEntered)
				<-releaseDrain
			}))
			require.True(t, q.addStep(shutdownPhaseDrain, func() { laterRan.Store(true) }))
			done := installServerShutdownSignals(ctx, f.notify, q)

			f.deliver(tc.first)
			<-drainEntered
			f.deliver(tc.second)
			<-q.forced()
			<-cancelled // force cancels at once, while the step still blocks
			assert.Contains(t, logs.String(), "again, forcing shutdown")
			close(releaseDrain)
			<-cancelled // the run's own cancel after the step returns
			assert.False(t, laterRan.Load(), "steps after the force are skipped")
			stop()
			<-done
		})
	}
}

// N3-a (R11a): when the handler's lifetime ctx is done first (runServerStart
// cancels it on every return), the signal goroutine exits without starting
// a shutdown; a later signal stays unconsumed in the still-subscribed
// channel (so it is swallowed, not handled by the default action), with no
// cancel and no log line.
func TestInstallServerShutdownSignals_CtxDoneFirstExitsWithoutShutdown(t *testing.T) {
	resetServerShutdownForTest(t)
	logs := captureStdLog(t)
	var f fakeSignalNotify
	var cancels atomic.Int32
	ctx, stop := context.WithCancel(context.Background())
	done := installServerShutdownSignals(ctx, f.notify, newTestShutdown(func() { cancels.Add(1) }))

	stop()
	<-done // the goroutine has exited

	f.deliver(syscall.SIGTERM)
	assert.Equal(t, 1, len(f.ch), "the signal is not consumed")
	assert.Equal(t, int32(0), cancels.Load(), "no shutdown after ctx is done")
	assert.NotContains(t, logs.String(), "Received signal")
}

// The handler keeps reading after the first signal until its lifetime ctx
// ends, which is not the server context the shutdown cancels: the
// shutdown's own cancel does not stop it, so a second signal during the
// teardown is still handled.
func TestInstallServerShutdownSignals_HandlesSignalsAfterCancel(t *testing.T) {
	resetServerShutdownForTest(t)
	captureStdLog(t)
	var f fakeSignalNotify
	serverCtx, serverCancel := context.WithCancel(context.Background())
	lifetime, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)
	q := newTestShutdown(serverCancel)
	done := installServerShutdownSignals(lifetime, f.notify, q)

	f.deliver(syscall.SIGTERM)
	<-serverCtx.Done()
	select {
	case <-done:
		t.Fatal("the handler exited with the server context")
	default:
	}
	f.deliver(os.Interrupt)
	<-q.forced()
	stop()
	<-done
}

// The pre-cancel steps run in phase order (mark before the conduit drain,
// whatever the registration order), all before cancel, and a step
// registered after the shutdown began is refused and never runs.
func TestServerShutdownSequence_StepsInPhaseOrderBeforeCancel(t *testing.T) {
	captureStdLog(t)
	var mu sync.Mutex
	var order []string
	record := func(s string) func() {
		return func() {
			mu.Lock()
			defer mu.Unlock()
			order = append(order, s)
		}
	}
	q := newTestShutdown(record("cancel"))
	require.True(t, q.addStep(shutdownPhaseDrain, record("drain")))
	require.True(t, q.addStep(shutdownPhaseMark, record("mark-hub")))
	require.True(t, q.addStep(shutdownPhaseMark, record("mark-web")))

	q.begin(syscall.SIGTERM)
	assert.False(t, q.addStep(shutdownPhaseMark, record("late")), "a step registered after the shutdown began is refused")
	q.begin(syscall.SIGTERM) // run-once

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []string{"mark-hub", "mark-web", "drain", "cancel"}, order, "pre-cancel steps in phase order, then cancel")
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
		lifetime, stop := context.WithCancel(context.Background())
		defer stop()
		_ = installServerShutdownSignals(lifetime, f.notify, newTestShutdown(func() {
			events <- "cancel"
			cancel()
		}))
		close(installed)
		return awaitServerExit(ctx, make(chan error), cancel,
			func() { events <- "wait-return" },
			func(context.Context) error {
				events <- "close"
				return nil
			}, nil, nil, nil)
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
