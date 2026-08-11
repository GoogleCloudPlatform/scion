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

package teams

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

const (
	defaultTeamsSendQueueSize = 100
	defaultTeamsSendMinDelay  = 200 * time.Millisecond
	defaultTeamsIdleTimeout   = 5 * time.Minute
	maxSendRetries            = 3
)

// sendQueueRequest represents a message waiting to be sent through the queue.
type sendQueueRequest struct {
	conversationID string
	serviceURL     string
	activity       *Activity
	result         chan *sendQueueResult
}

// sendQueueResult carries the outcome of a queued send back to the caller.
type sendQueueResult struct {
	activityID string
	err        error
}

// conversationQueue holds a per-conversation buffered channel.
type conversationQueue struct {
	ch chan *sendQueueRequest
}

// SendQueue manages per-conversation outbound message workers to prevent
// rate-limit errors from the Bot Connector API. Each conversation gets its
// own goroutine that serializes sends with a configurable minimum delay.
type SendQueue struct {
	sender   *Sender
	log      *slog.Logger
	mu       sync.Mutex
	queues   map[string]*conversationQueue
	maxSize  int
	minDelay time.Duration
	closed   bool
	wg       sync.WaitGroup
}

// NewSendQueue creates a new SendQueue.
// Pass 0 for queueSize or minDelay to use defaults (100 messages, 200ms).
func NewSendQueue(sender *Sender, queueSize int, minDelay time.Duration, log *slog.Logger) *SendQueue {
	if queueSize <= 0 {
		queueSize = defaultTeamsSendQueueSize
	}
	if minDelay <= 0 {
		minDelay = defaultTeamsSendMinDelay
	}
	if log == nil {
		log = slog.Default()
	}
	return &SendQueue{
		sender:   sender,
		log:      log,
		queues:   make(map[string]*conversationQueue),
		maxSize:  queueSize,
		minDelay: minDelay,
	}
}

// Enqueue queues an activity for sending and blocks until it is sent
// (or the context is cancelled). Returns the created activity ID.
func (sq *SendQueue) Enqueue(ctx context.Context, conversationID, serviceURL string, activity *Activity) (string, error) {
	resultCh := make(chan *sendQueueResult, 1)

	req := &sendQueueRequest{
		conversationID: conversationID,
		serviceURL:     serviceURL,
		activity:       activity,
		result:         resultCh,
	}

	if err := sq.enqueue(conversationID, req); err != nil {
		return "", err
	}

	select {
	case res := <-resultCh:
		return res.activityID, res.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// enqueue gets-or-creates the per-conversation queue and writes the request.
func (sq *SendQueue) enqueue(conversationID string, req *sendQueueRequest) error {
	sq.mu.Lock()
	defer sq.mu.Unlock()

	if sq.closed {
		return errors.New("send queue is closed")
	}

	cq, ok := sq.queues[conversationID]
	if !ok {
		cq = &conversationQueue{
			ch: make(chan *sendQueueRequest, sq.maxSize),
		}
		sq.queues[conversationID] = cq

		sq.wg.Add(1)
		go sq.worker(conversationID, cq)
	}

	// Non-blocking send; if full, drop the oldest and retry.
	select {
	case cq.ch <- req:
	default:
		select {
		case dropped := <-cq.ch:
			if dropped.result != nil {
				dropped.result <- &sendQueueResult{err: errors.New("dropped: send queue overflow")}
			}
			sq.log.Warn("Send queue overflow, dropped oldest message",
				"conversation_id", conversationID, "queue_size", sq.maxSize)
		default:
		}
		cq.ch <- req
	}

	return nil
}

// worker is the per-conversation send goroutine. It reads messages from the
// channel and sends them via the Sender with rate limiting.
func (sq *SendQueue) worker(conversationID string, cq *conversationQueue) {
	defer sq.wg.Done()
	defer sq.removeQueue(conversationID)

	idleTimer := time.NewTimer(defaultTeamsIdleTimeout)
	defer idleTimer.Stop()

	for {
		select {
		case req, ok := <-cq.ch:
			if !ok {
				return
			}

			// Reset idle timer on activity.
			if !idleTimer.Stop() {
				select {
				case <-idleTimer.C:
				default:
				}
			}
			idleTimer.Reset(defaultTeamsIdleTimeout)

			// Send the message, retrying on 429 up to maxSendRetries times.
			retries := 0
			for {
				activityID, err := sq.sendOne(req)
				var retryErr *RetryAfterError
				if errors.As(err, &retryErr) && retries < maxSendRetries {
					retries++
					sq.log.Warn("Rate limited, retrying message",
						"conversation_id", conversationID,
						"retry_after", retryErr.RetryAfter,
						"attempt", retries,
					)
					time.Sleep(retryErr.RetryAfter)
					continue
				}
				if req.result != nil {
					req.result <- &sendQueueResult{activityID: activityID, err: err}
				}
				break
			}

			// Enforce minimum delay between sends.
			time.Sleep(sq.minDelay)

		case <-idleTimer.C:
			sq.log.Debug("Send queue worker idle, exiting",
				"conversation_id", conversationID)
			return
		}
	}
}

// sendOne dispatches a single outbound activity via the Sender.
func (sq *SendQueue) sendOne(req *sendQueueRequest) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	return sq.sender.sendActivity(ctx, req.serviceURL, req.conversationID, req.activity)
}

// removeQueue removes the per-conversation queue from the map.
func (sq *SendQueue) removeQueue(conversationID string) {
	sq.mu.Lock()
	defer sq.mu.Unlock()
	delete(sq.queues, conversationID)
}

// Len returns the total number of messages buffered across all conversation queues.
func (sq *SendQueue) Len() int {
	sq.mu.Lock()
	defer sq.mu.Unlock()
	n := 0
	for _, cq := range sq.queues {
		n += len(cq.ch)
	}
	return n
}

// Close shuts down all worker goroutines and waits for them to finish.
// Messages still in the queues are drained with errors.
func (sq *SendQueue) Close() {
	sq.mu.Lock()
	sq.closed = true
	for conversationID, cq := range sq.queues {
		close(cq.ch)
		delete(sq.queues, conversationID)
	}
	sq.mu.Unlock()

	sq.wg.Wait()
}
