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

package agent

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

// MessageBuffer implements a debounce-style buffer for agent message delivery.
//
// Because messages are delivered to agents by effectively "typing" them into the
// agent's terminal input, a rapid burst of arriving messages can cause contention
// and garbled input. The buffer introduces a short delay window to coalesce
// consecutive messages into a single delivery.
//
// Behavior:
//   - When a message arrives for an agent, a 2-second timer starts.
//   - If additional messages arrive before the timer fires, they are appended
//     to the buffer and the timer is reset (debounce).
//   - When the timer finally fires (2 seconds after the LAST message), all
//     buffered messages are concatenated and delivered as a single string.
//   - Interrupt messages bypass the buffer entirely for immediate delivery.
type MessageBuffer struct {
	// bufferDelay is the debounce window duration. Each new message resets
	// the timer to this duration from the current time.
	bufferDelay time.Duration

	// deliverFunc is the callback that performs actual message delivery via tmux.
	// It receives the agent ID, project ID, the concatenated message text, and the interrupt flag.
	deliverFunc func(agentID, projectID string, message string, interrupt bool) error

	// maxFlushAttempts bounds how many times flush retries a coalesced
	// delivery before giving up and reporting it to onFailure handlers. It
	// is small: a transient failure (container briefly unreachable, "docker
	// ps failed") is worth a couple of quick retries, but flush runs
	// synchronously in the buffer's timer goroutine and holds up delivery
	// of this agent's messages while retrying, so it must not retry for
	// long (ptone/scion#1866). Set to a default by NewMessageBuffer; tests
	// may override it per instance.
	maxFlushAttempts int

	// flushRetryBackoff is the delay between retry attempts. Set to a
	// default by NewMessageBuffer; tests may override it per instance.
	flushRetryBackoff time.Duration

	// waitReady, when set, is called once at the start of each timer-driven
	// flush, before the messages are taken and before any delivery attempt,
	// to wait until the agent's harness can read input. It must return by
	// its own deadline, and promptly once ctx is cancelled (Close cancels it
	// so shutdown never waits). It runs in the flush goroutine, so senders
	// never block on it; flushes for other agents run in their own timer
	// goroutines and are not delayed by it.
	waitReady func(ctx context.Context, agentID, projectID string)

	// closeCtx is cancelled when Close starts. It cuts short a readiness
	// wait and the backoff between delivery retries.
	closeCtx    context.Context
	closeCancel context.CancelFunc

	// closeWaitTimeout bounds how long Close waits for flushes that are
	// already running (ptone/scion#4589). Set to a default by
	// NewMessageBuffer; tests may override it per instance.
	closeWaitTimeout time.Duration

	// inflight counts running flushes: timer-driven ones and Close's own.
	// Every Add runs with mu held: a timer flush adds only while closed is
	// still false, and Close adds its own flushes in the same critical
	// section that sets closed. The single Wait starts after that section,
	// so no Add can race with it.
	inflight sync.WaitGroup

	mu     sync.Mutex
	closed bool // set by Close; guarded by mu
	// inflightDone is created by the first Close (guarded by mu) and closed
	// by the one goroutine that waits on inflight, so every Close shares a
	// single waiter instead of starting its own.
	inflightDone chan struct{}
	// closeDeadline is set by the first Close (guarded by mu) to its start
	// plus closeWaitTimeout. Every Close waits only until this one deadline.
	closeDeadline time.Time
	buffers       map[string]*agentBuffer // keyed by agentID + "\x00" + projectID

	// gates serializes flushes per buffer key, so a later flush for the
	// same agent can never paste before an earlier one that is still
	// waiting for readiness. Guarded by mu; an entry is removed when no
	// flush holds or waits for it.
	gates map[string]*flushGate
}

// flushGate is the per-key flush lock; refs counts holders and waiters.
type flushGate struct {
	mu   sync.Mutex
	refs int
}

// defaultMaxFlushAttempts and defaultFlushRetryBackoff are the production
// values NewMessageBuffer uses for a MessageBuffer's retry bounds.
const (
	defaultMaxFlushAttempts  = 3
	defaultFlushRetryBackoff = 500 * time.Millisecond
)

// defaultCloseWaitTimeout is how long Close waits for in-flight flushes
// before it gives up, logs a warning and returns.
const defaultCloseWaitTimeout = 10 * time.Second

// ErrMessageBufferClosed is passed to the failure handler of a message sent
// after Close started; such a message is never delivered.
var ErrMessageBufferClosed = errors.New("msgbuffer: message buffer is closed")

// agentBuffer holds the pending messages and timer for a single agent.
type agentBuffer struct {
	messages  []string    // accumulated messages waiting for delivery
	timer     *time.Timer // debounce timer; fires to trigger delivery
	projectID string      // project scope for delivery

	// onFailure holds per-message failure callbacks registered by the
	// sender (nil entries allowed). They are invoked when the coalesced
	// delivery fails, because the caller was already told the message was
	// accepted and has no other way to learn it was lost (#1820).
	onFailure []DeliveryFailureHandler
}

// DeliveryFailureHandler is called when a buffered message could not be
// delivered after the caller was told it was accepted. It is invoked from
// the buffer's timer goroutine and must not block for long.
type DeliveryFailureHandler func(err error)

// PartialDeliveryError marks a deliverFunc failure as unsafe to retry: some
// of the coalesced message content already reached the agent's terminal
// before the failure occurred (e.g. the text was pasted but the trailing
// Enter keypress failed). Retrying such a failure would re-run the whole
// delivery — including the paste — and the agent would see the same text
// twice (ptone/scion#1866). deliverFunc implementations should wrap errors
// this way once they can no longer guarantee a retry is side-effect free;
// flush's bounded retry only retries errors that are not wrapped this way.
type PartialDeliveryError struct {
	Err error
}

func (e *PartialDeliveryError) Error() string { return e.Err.Error() }
func (e *PartialDeliveryError) Unwrap() error { return e.Err }

type deliveryFailureHandlerKey struct{}

// WithDeliveryFailureHandler returns a context carrying fn. When passed to
// AgentManager.Message for a non-interrupt message, fn is invoked if the
// buffered delivery later fails. Only the value is read from ctx; its
// cancellation does not affect the buffered delivery.
func WithDeliveryFailureHandler(ctx context.Context, fn DeliveryFailureHandler) context.Context {
	if fn == nil {
		return ctx
	}
	return context.WithValue(ctx, deliveryFailureHandlerKey{}, fn)
}

// DeliveryFailureHandlerFromContext returns the handler attached with
// WithDeliveryFailureHandler, or nil.
func DeliveryFailureHandlerFromContext(ctx context.Context) DeliveryFailureHandler {
	if ctx == nil {
		return nil
	}
	fn, _ := ctx.Value(deliveryFailureHandlerKey{}).(DeliveryFailureHandler)
	return fn
}

// NewMessageBuffer creates a new MessageBuffer with the given debounce delay
// and delivery function. The deliverFunc is called asynchronously when the
// buffer flushes — it should perform the actual tmux send-keys delivery.
func NewMessageBuffer(delay time.Duration, deliverFunc func(agentID, projectID string, message string, interrupt bool) error) *MessageBuffer {
	closeCtx, closeCancel := context.WithCancel(context.Background())
	return &MessageBuffer{
		bufferDelay:       delay,
		deliverFunc:       deliverFunc,
		maxFlushAttempts:  defaultMaxFlushAttempts,
		flushRetryBackoff: defaultFlushRetryBackoff,
		closeWaitTimeout:  defaultCloseWaitTimeout,
		closeCtx:          closeCtx,
		closeCancel:       closeCancel,
		buffers:           make(map[string]*agentBuffer),
		gates:             make(map[string]*flushGate),
	}
}

// Send queues a message for buffered delivery to the given agent.
// The message is added to the agent's buffer and the debounce timer is
// started (or reset if already running). The actual delivery occurs
// asynchronously once the timer fires.
// projectID scopes delivery to a specific project.
func (mb *MessageBuffer) Send(agentID, projectID string, message string) {
	mb.SendWithFailureHandler(agentID, projectID, message, nil)
}

// SendWithFailureHandler is Send with a callback that is invoked if the
// eventual coalesced delivery containing this message fails. A message sent
// after Close started is dropped and onFailure, if set, is called at once
// with ErrMessageBufferClosed.
func (mb *MessageBuffer) SendWithFailureHandler(agentID, projectID string, message string, onFailure DeliveryFailureHandler) {
	mb.mu.Lock()
	if mb.closed {
		mb.mu.Unlock()
		slog.Warn("msgbuffer: message rejected, buffer is closed",
			"agent_id", agentID,
			"project_id", projectID,
		)
		if onFailure != nil {
			onFailure(ErrMessageBufferClosed)
		}
		return
	}
	defer mb.mu.Unlock()

	key := bufferKey(agentID, projectID)
	buf, exists := mb.buffers[key]
	if !exists {
		buf = &agentBuffer{projectID: projectID}
		mb.buffers[key] = buf
	}

	// Append the message to the pending list.
	buf.messages = append(buf.messages, message)
	buf.onFailure = append(buf.onFailure, onFailure)
	util.Debugf("msgbuffer: queued message for agent %s project %s (%d pending)", agentID, projectID, len(buf.messages))

	// Reset or start the debounce timer. If a timer is already running,
	// stop it first so we can restart with a fresh delay window.
	if buf.timer != nil {
		buf.timer.Stop()
	}
	buf.timer = time.AfterFunc(mb.bufferDelay, func() {
		// Register with inflight under mu, so Close either sees this
		// flush and waits for it, or this flush sees closed and leaves
		// the messages to Close's own flush.
		mb.mu.Lock()
		if mb.closed {
			mb.mu.Unlock()
			return
		}
		mb.inflight.Add(1)
		mb.mu.Unlock()
		defer mb.inflight.Done()
		mb.flush(agentID, key, true)
	})
}

func bufferKey(agentID, projectID string) string {
	return agentID + "\x00" + projectID
}

// flush delivers all buffered messages for the given agent as a single
// concatenated string. Called when the debounce timer fires (waitForReady
// true) and from Close (false, so shutdown never waits for readiness).
//
// Flushes for one key are serialized by its flushGate. When waitForReady is
// set and mb.waitReady is configured, the readiness wait runs once, before
// the messages are taken: messages that arrive during the wait join this
// delivery, and the flush their own timer starts finds nothing left. The
// wait runs once per flush, never per retry, and mb.mu is not held while
// waiting, so Send never blocks.
//
// A failure is retried up to mb.maxFlushAttempts times with a short backoff,
// unless it is a PartialDeliveryError: once some of the coalesced text has
// reached the agent's terminal, retrying would re-deliver it and the agent
// would see it twice, so that class of failure is reported immediately
// instead (ptone/scion#1866).
func (mb *MessageBuffer) flush(agentID, key string, waitForReady bool) {
	gate := mb.acquireGate(key)
	defer mb.releaseGate(key, gate)

	if waitForReady && mb.waitReady != nil {
		mb.mu.Lock()
		buf, exists := mb.buffers[key]
		hasPending := exists && len(buf.messages) > 0
		var projectID string
		if exists {
			projectID = buf.projectID
		}
		mb.mu.Unlock()
		if !hasPending {
			return
		}
		mb.waitReady(mb.closeCtx, agentID, projectID)
	}

	mb.mu.Lock()
	buf, exists := mb.buffers[key]
	if !exists || len(buf.messages) == 0 {
		mb.mu.Unlock()
		return
	}

	// Take ownership of the pending messages and clean up the buffer entry.
	pending := buf.messages
	handlers := buf.onFailure
	projectID := buf.projectID
	delete(mb.buffers, key)
	mb.mu.Unlock()

	// Concatenate all buffered messages with double-newline separators so
	// each original message remains visually distinct in the agent's input.
	combined := strings.Join(pending, "\n\n")
	util.Debugf("msgbuffer: flushing %d message(s) for agent %s project %s", len(pending), agentID, projectID)

	var err error
	for attempt := 1; attempt <= mb.maxFlushAttempts; attempt++ {
		err = mb.deliverFunc(agentID, projectID, combined, false)
		if err == nil {
			return
		}

		var partial *PartialDeliveryError
		if errors.As(err, &partial) {
			break
		}
		if attempt < mb.maxFlushAttempts {
			slog.Warn("msgbuffer: delivery attempt failed, retrying",
				"agent_id", agentID,
				"project_id", projectID,
				"attempt", attempt,
				"max_attempts", mb.maxFlushAttempts,
				"error", err,
			)
			mb.retryBackoff()
		}
	}

	slog.Warn("msgbuffer: message delivery failed",
		"agent_id", agentID,
		"project_id", projectID,
		"pending_count", len(pending),
		"error", err,
	)
	for _, fn := range handlers {
		if fn != nil {
			fn(err)
		}
	}
}

// retryBackoff sleeps for flushRetryBackoff between delivery attempts, but
// returns early once Close starts so shutdown is not held up by it.
func (mb *MessageBuffer) retryBackoff() {
	if mb.flushRetryBackoff <= 0 {
		return
	}
	t := time.NewTimer(mb.flushRetryBackoff)
	defer t.Stop()
	select {
	case <-t.C:
	case <-mb.closeCtx.Done():
	}
}

// acquireGate locks and returns the flush gate for key.
func (mb *MessageBuffer) acquireGate(key string) *flushGate {
	mb.mu.Lock()
	g, ok := mb.gates[key]
	if !ok {
		g = &flushGate{}
		mb.gates[key] = g
	}
	g.refs++
	mb.mu.Unlock()
	g.mu.Lock()
	return g
}

// releaseGate unlocks g and drops it once nothing else holds or waits on it.
func (mb *MessageBuffer) releaseGate(key string, g *flushGate) {
	g.mu.Unlock()
	mb.mu.Lock()
	g.refs--
	if g.refs == 0 && mb.gates[key] == g {
		delete(mb.gates, key)
	}
	mb.mu.Unlock()
}

// Close flushes all pending buffers immediately and stops all timers.
// Call this during shutdown to ensure no messages are lost. Pending
// messages are pasted without waiting for harness readiness, and a
// readiness wait already in progress is cut short, so shutdown is never
// held up by it.
//
// Close also waits for timer-driven flushes that are already delivering,
// so their retries, logs and failure handlers run before Close returns
// (ptone/scion#4589). The backoff between retries ends early once Close
// starts. The whole wait is bounded by closeWaitTimeout; if a delivery is
// still stuck then, Close logs a warning and returns anyway. The trade-off:
// after such a timeout the stuck key's flush can still retry, log and run
// its failure handlers, and Close's own queued flush for that key delivers
// once the key's gate frees, all after Close returned. Messages sent after
// Close started are rejected through their failure handler. A repeated
// Close waits for the same flushes until the same deadline as the first,
// so it returns at once if that deadline has passed.
func (mb *MessageBuffer) Close() {
	mb.closeCancel()
	mb.mu.Lock()
	if mb.closed {
		// A repeated Close only waits, on the first Close's waiter and
		// until the first Close's deadline, so it never extends the
		// bound: Sends are rejected and timers do nothing once closed,
		// so there is nothing new to flush.
		done, deadline := mb.inflightDone, mb.closeDeadline
		mb.mu.Unlock()
		mb.waitInflight(done, deadline, false)
		return
	}
	mb.closed = true
	done := make(chan struct{})
	mb.inflightDone = done
	deadline := time.Now().Add(mb.closeWaitTimeout)
	mb.closeDeadline = deadline
	type pendingEntry struct {
		agentID string
		key     string
	}
	entries := make([]pendingEntry, 0, len(mb.buffers))
	for key, buf := range mb.buffers {
		if buf.timer != nil {
			buf.timer.Stop()
		}
		agentID := key
		if idx := strings.IndexByte(key, '\x00'); idx >= 0 {
			agentID = key[:idx]
		}
		entries = append(entries, pendingEntry{agentID: agentID, key: key})
	}
	// Flush each key in its own goroutine, outside the lock, so a key
	// whose gate is held by a stuck timer flush cannot hold up the
	// others. Each is counted in inflight here, still under mu, so the
	// bounded wait below covers Close's own flushes too.
	for _, e := range entries {
		mb.inflight.Add(1)
		go func() {
			defer mb.inflight.Done()
			mb.flush(e.agentID, e.key, false)
		}()
	}
	mb.mu.Unlock()

	go func() {
		mb.inflight.Wait()
		close(done)
	}()
	mb.waitInflight(done, deadline, true)
}

// waitInflight waits for done (all in-flight flushes finished), but not
// past deadline. If deadline has already passed it only checks done. Only
// the first Close (first true) logs the timeout as a warning; a repeated
// Close logs it at debug level, so the timeout is reported once.
func (mb *MessageBuffer) waitInflight(done <-chan struct{}, deadline time.Time, first bool) {
	select {
	case <-done:
		return
	default:
	}
	if remaining := time.Until(deadline); remaining > 0 {
		t := time.NewTimer(remaining)
		defer t.Stop()
		select {
		case <-done:
			return
		case <-t.C:
		}
	}
	if !first {
		util.Debugf("msgbuffer: repeated Close returned at the first Close's deadline with deliveries still in flight")
		return
	}
	slog.Warn("msgbuffer: Close timed out waiting for in-flight message deliveries",
		"timeout", mb.closeWaitTimeout,
	)
}
