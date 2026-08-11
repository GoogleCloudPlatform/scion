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

package chatapp

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

const (
	defaultSendQueueSize   = 100
	defaultSendMinDelay    = 100 * time.Millisecond
	defaultSendIdleTimeout = 5 * time.Minute
)

// sendRequest represents a message waiting to be sent through the queue.
type sendRequest struct {
	ctx    context.Context
	req    SendMessageRequest
	result chan<- sendResult
}

// sendResult carries the outcome of a queued send back to the caller.
type sendResult struct {
	messageID string
	err       error
}

// spaceWorker holds a per-space buffered channel.
type spaceWorker struct {
	ch chan sendRequest
}

// SendQueue manages per-space outbound message workers to prevent
// Chat API rate-limit errors. Each space gets its own goroutine
// that serializes sends with a configurable minimum delay.
type SendQueue struct {
	messenger Messenger
	log       *slog.Logger
	bufSize   int
	minDelay  time.Duration

	mu      sync.Mutex
	workers map[string]*spaceWorker
	closed  bool
	wg      sync.WaitGroup
}

// NewSendQueue creates a new SendQueue. Pass 0 for bufSize or minDelay
// to use the defaults (100 messages, 100ms).
func NewSendQueue(messenger Messenger, log *slog.Logger, bufSize int, minDelay time.Duration) *SendQueue {
	if bufSize <= 0 {
		bufSize = defaultSendQueueSize
	}
	if minDelay <= 0 {
		minDelay = defaultSendMinDelay
	}
	if log == nil {
		log = slog.Default()
	}
	return &SendQueue{
		messenger: messenger,
		log:       log,
		bufSize:   bufSize,
		minDelay:  minDelay,
		workers:   make(map[string]*spaceWorker),
	}
}

// Send enqueues a message and blocks until it is sent (or the context is
// cancelled). It returns the message ID from the Chat API or an error.
func (q *SendQueue) Send(ctx context.Context, req SendMessageRequest) (string, error) {
	resultCh := make(chan sendResult, 1)

	sr := sendRequest{
		ctx:    ctx,
		req:    req,
		result: resultCh,
	}

	if err := q.enqueue(req.SpaceID, sr); err != nil {
		return "", err
	}

	select {
	case res := <-resultCh:
		return res.messageID, res.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// Close stops all workers and waits for them to drain remaining messages.
func (q *SendQueue) Close() {
	q.mu.Lock()
	q.closed = true
	for spaceID, w := range q.workers {
		close(w.ch)
		delete(q.workers, spaceID)
	}
	q.mu.Unlock()

	q.wg.Wait()
}

// enqueue gets-or-creates the per-space worker and writes the request to it.
func (q *SendQueue) enqueue(spaceID string, sr sendRequest) error {
	q.mu.Lock()
	defer q.mu.Unlock()

	if q.closed {
		return errors.New("send queue is closed")
	}

	w := q.getOrCreateWorkerLocked(spaceID)

	// Try non-blocking send; if full, drop the oldest and retry.
	select {
	case w.ch <- sr:
	default:
		select {
		case dropped := <-w.ch:
			dropped.result <- sendResult{err: errors.New("dropped: send queue overflow")}
			q.log.Warn("send queue overflow, dropped oldest message",
				"space_id", spaceID, "queue_size", q.bufSize)
		default:
		}
		w.ch <- sr
	}

	return nil
}

// getOrCreateWorkerLocked returns the existing worker for a space, or creates
// a new one. Must be called with q.mu held.
func (q *SendQueue) getOrCreateWorkerLocked(spaceID string) *spaceWorker {
	w, ok := q.workers[spaceID]
	if ok {
		return w
	}

	w = &spaceWorker{
		ch: make(chan sendRequest, q.bufSize),
	}
	q.workers[spaceID] = w

	q.wg.Add(1)
	go q.worker(spaceID, w)

	return w
}

// worker is the per-space send goroutine. It reads messages from the channel
// and sends them via the Messenger with rate limiting. It exits after an idle
// timeout and is cleaned up.
func (q *SendQueue) worker(spaceID string, w *spaceWorker) {
	defer q.wg.Done()

	idleTimer := time.NewTimer(defaultSendIdleTimeout)
	defer idleTimer.Stop()

	for {
		select {
		case sr, ok := <-w.ch:
			if !ok {
				// Channel closed — worker should exit.
				return
			}

			// Reset idle timer on activity.
			if !idleTimer.Stop() {
				select {
				case <-idleTimer.C:
				default:
				}
			}
			idleTimer.Reset(defaultSendIdleTimeout)

			// Check if context is already cancelled before sending.
			if err := sr.ctx.Err(); err != nil {
				sr.result <- sendResult{err: err}
				continue
			}

			// Send the message via the Messenger.
			msgID, err := q.sendOne(sr)
			sr.result <- sendResult{messageID: msgID, err: err}

			if err != nil {
				q.log.Error("send queue delivery failed",
					"space_id", spaceID, "error", err)
			}

			// Enforce minimum delay between sends.
			time.Sleep(q.minDelay)

		case <-idleTimer.C:
			// Check under the lock that the channel is still empty.
			// Between the timer firing and here, enqueue() may have
			// written a message — if so, reset the timer and continue.
			q.mu.Lock()
			if len(w.ch) > 0 {
				q.mu.Unlock()
				idleTimer.Reset(defaultSendIdleTimeout)
				continue
			}
			// Still empty — remove the worker while holding the lock
			// so no new messages can be enqueued to a dead worker.
			delete(q.workers, spaceID)
			q.mu.Unlock()
			q.log.Debug("send queue worker idle, exiting", "space_id", spaceID)
			return
		}
	}
}

// sendOne dispatches a single outbound message to the Chat API via the
// Messenger.SendMessage method.
func (q *SendQueue) sendOne(sr sendRequest) (string, error) {
	return q.messenger.SendMessage(sr.ctx, sr.req)
}
