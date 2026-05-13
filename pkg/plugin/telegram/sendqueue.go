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

package telegram

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

const (
	defaultMaxQueueSize = 100
	defaultMinDelay     = 50 * time.Millisecond
	defaultIdleTimeout  = 5 * time.Minute
	maxBackoff          = 60 * time.Second
)

// SendQueue manages per-chat outbound message workers to prevent
// Telegram 429 rate-limit errors. Each chat gets its own goroutine
// that serializes sends with a configurable minimum delay.
type SendQueue struct {
	mu       sync.Mutex
	queues   map[int64]*chatQueue // chatID -> queue
	api      *TelegramAPIClient
	log      *slog.Logger
	maxSize  int           // max messages queued per chat
	minDelay time.Duration // minimum delay between sends to same chat

	closed bool
	wg     sync.WaitGroup
}

// chatQueue holds a per-chat buffered channel and the current backoff state.
type chatQueue struct {
	ch chan *outboundMessage
}

// outboundMessage represents a message waiting to be sent through the queue.
type outboundMessage struct {
	chatID    int64
	text      string
	parseMode string
	keyboard  *InlineKeyboardMarkup
	replyTo   int64
	result    chan<- *sendResult // caller blocks on this to receive the outcome
}

// sendResult carries the outcome of a queued send back to the caller.
type sendResult struct {
	msg *TGMessage
	err error
}

// NewSendQueue creates a new SendQueue. Pass 0 for maxSize or minDelay to
// use the defaults (100 messages, 50ms).
func NewSendQueue(api *TelegramAPIClient, log *slog.Logger, maxSize int, minDelay time.Duration) *SendQueue {
	if maxSize <= 0 {
		maxSize = defaultMaxQueueSize
	}
	if minDelay <= 0 {
		minDelay = defaultMinDelay
	}
	if log == nil {
		log = slog.Default()
	}
	return &SendQueue{
		queues:   make(map[int64]*chatQueue),
		api:      api,
		log:      log,
		maxSize:  maxSize,
		minDelay: minDelay,
	}
}

// Send enqueues a message and blocks until it is sent (or fails).
// It returns the Telegram API response or an error.
func (sq *SendQueue) Send(ctx context.Context, chatID int64, text, parseMode string, keyboard *InlineKeyboardMarkup, replyTo int64) (*TGMessage, error) {
	resultCh := make(chan *sendResult, 1)

	om := &outboundMessage{
		chatID:    chatID,
		text:      text,
		parseMode: parseMode,
		keyboard:  keyboard,
		replyTo:   replyTo,
		result:    resultCh,
	}

	ch, err := sq.enqueue(chatID, om)
	if err != nil {
		return nil, err
	}
	_ = ch // enqueue already wrote to the channel

	select {
	case res := <-resultCh:
		return res.msg, res.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// enqueue gets-or-creates the per-chat queue and writes the message to it.
// If the queue is full, the oldest message is dropped.
func (sq *SendQueue) enqueue(chatID int64, om *outboundMessage) (chan *outboundMessage, error) {
	sq.mu.Lock()
	defer sq.mu.Unlock()

	if sq.closed {
		return nil, errors.New("send queue is closed")
	}

	cq, ok := sq.queues[chatID]
	if !ok {
		cq = &chatQueue{
			ch: make(chan *outboundMessage, sq.maxSize),
		}
		sq.queues[chatID] = cq

		sq.wg.Add(1)
		go sq.worker(chatID, cq)
	}

	// Try non-blocking send; if full, drop the oldest and retry.
	select {
	case cq.ch <- om:
	default:
		// Queue overflow: drop oldest.
		dropped := <-cq.ch
		if dropped.result != nil {
			dropped.result <- &sendResult{err: errors.New("dropped: send queue overflow")}
		}
		sq.log.Warn("Send queue overflow, dropped oldest message",
			"chat_id", chatID, "queue_size", sq.maxSize)
		cq.ch <- om
	}

	return cq.ch, nil
}

// worker is the per-chat send goroutine. It reads messages from the channel
// and sends them via the API with rate limiting. It exits after idleTimeout
// of inactivity.
func (sq *SendQueue) worker(chatID int64, cq *chatQueue) {
	defer sq.wg.Done()
	defer sq.removeQueue(chatID)

	backoff := sq.minDelay
	idleTimer := time.NewTimer(defaultIdleTimeout)
	defer idleTimer.Stop()

	for {
		select {
		case om, ok := <-cq.ch:
			if !ok {
				// Channel closed — drain remaining.
				return
			}

			// Reset idle timer on activity.
			if !idleTimer.Stop() {
				select {
				case <-idleTimer.C:
				default:
				}
			}
			idleTimer.Reset(defaultIdleTimeout)

			// Send the message.
			msg, err := sq.sendOne(om)

			if err != nil {
				var apiErr *APIError
				if errors.As(err, &apiErr) && apiErr.Code == 429 {
					// Respect Telegram's retry_after with exponential backoff.
					retryAfter := time.Duration(apiErr.RetryAfterSec) * time.Second
					if retryAfter < backoff {
						retryAfter = backoff
					}
					sq.log.Warn("Rate limited by Telegram, backing off",
						"chat_id", chatID,
						"retry_after_sec", apiErr.RetryAfterSec,
						"backoff", retryAfter)
					time.Sleep(retryAfter)
					// Exponential backoff: double for next 429.
					backoff = retryAfter * 2
					if backoff > maxBackoff {
						backoff = maxBackoff
					}
					// Retry this message once after backoff.
					retryMsg, retryErr := sq.sendOne(om)
					if om.result != nil {
						om.result <- &sendResult{msg: retryMsg, err: retryErr}
					}
					if retryErr == nil {
						backoff = sq.minDelay
					}
					continue
				}
			} else {
				// Success — reset backoff.
				backoff = sq.minDelay
			}

			if om.result != nil {
				om.result <- &sendResult{msg: msg, err: err}
			}

			// Enforce minimum delay between sends.
			time.Sleep(sq.minDelay)

		case <-idleTimer.C:
			sq.log.Debug("Send queue worker idle, exiting", "chat_id", chatID)
			return
		}
	}
}

// sendOne dispatches a single outbound message to the Telegram API.
func (sq *SendQueue) sendOne(om *outboundMessage) (*TGMessage, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if om.keyboard != nil || om.replyTo > 0 {
		return sq.api.SendMessageWithKeyboard(ctx, om.chatID, om.text, om.parseMode, om.keyboard, om.replyTo)
	}
	return sq.api.SendMessage(ctx, om.chatID, om.text, om.parseMode)
}

// removeQueue removes the per-chat queue from the map when the worker exits.
func (sq *SendQueue) removeQueue(chatID int64) {
	sq.mu.Lock()
	defer sq.mu.Unlock()
	delete(sq.queues, chatID)
}

// Close shuts down all worker goroutines and waits for them to finish.
// Messages still in the queues are drained with errors.
func (sq *SendQueue) Close() {
	sq.mu.Lock()
	sq.closed = true
	// Close all channels to signal workers to drain and exit.
	for chatID, cq := range sq.queues {
		close(cq.ch)
		delete(sq.queues, chatID)
	}
	sq.mu.Unlock()

	sq.wg.Wait()
}
