package discord

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
)

const (
	defaultSendQueueSize = 100
	defaultSendMinDelay  = 50 * time.Millisecond
	defaultSendIdleTimeout = 5 * time.Minute
)

// sendRequest represents a message waiting to be sent through the queue.
type sendRequest struct {
	channelID  string
	content    string
	embeds     []*discordgo.MessageEmbed
	components []discordgo.MessageComponent
	result     chan *sendResult
}

// sendResult carries the outcome of a queued send back to the caller.
type sendResult struct {
	msg *discordgo.Message
	err error
}

// channelQueue holds a per-channel buffered channel.
type channelQueue struct {
	ch chan *sendRequest
}

// SendQueue manages per-channel outbound message workers to prevent
// Discord API rate-limit errors. Each channel gets its own goroutine
// that serializes sends with a configurable minimum delay.
type SendQueue struct {
	session  *discordgo.Session
	log      *slog.Logger
	mu       sync.Mutex
	queues   map[string]*channelQueue
	maxSize  int
	minDelay time.Duration
	closed   bool
	wg       sync.WaitGroup
}

// NewSendQueue creates a new SendQueue. Pass 0 for queueSize or minDelay
// to use the defaults (100 messages, 50ms).
func NewSendQueue(session *discordgo.Session, log *slog.Logger, queueSize int, minDelay time.Duration) *SendQueue {
	if queueSize <= 0 {
		queueSize = defaultSendQueueSize
	}
	if minDelay <= 0 {
		minDelay = defaultSendMinDelay
	}
	if log == nil {
		log = slog.Default()
	}
	return &SendQueue{
		session:  session,
		log:      log,
		queues:   make(map[string]*channelQueue),
		maxSize:  queueSize,
		minDelay: minDelay,
	}
}

// Send enqueues a message and blocks until it is sent (or the context is
// cancelled). It returns the Discord API response or an error.
func (sq *SendQueue) Send(ctx context.Context, channelID, content string, embeds []*discordgo.MessageEmbed, components []discordgo.MessageComponent) (*discordgo.Message, error) {
	resultCh := make(chan *sendResult, 1)

	req := &sendRequest{
		channelID:  channelID,
		content:    content,
		embeds:     embeds,
		components: components,
		result:     resultCh,
	}

	if err := sq.enqueue(channelID, req); err != nil {
		return nil, err
	}

	select {
	case res := <-resultCh:
		return res.msg, res.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// enqueue gets-or-creates the per-channel queue and writes the request to it.
func (sq *SendQueue) enqueue(channelID string, req *sendRequest) error {
	sq.mu.Lock()
	defer sq.mu.Unlock()

	if sq.closed {
		return errors.New("send queue is closed")
	}

	cq, ok := sq.queues[channelID]
	if !ok {
		cq = &channelQueue{
			ch: make(chan *sendRequest, sq.maxSize),
		}
		sq.queues[channelID] = cq

		sq.wg.Add(1)
		go sq.worker(channelID, cq)
	}

	// Try non-blocking send; if full, drop the oldest and retry.
	select {
	case cq.ch <- req:
	default:
		select {
		case dropped := <-cq.ch:
			if dropped.result != nil {
				dropped.result <- &sendResult{err: errors.New("dropped: send queue overflow")}
			}
			sq.log.Warn("Send queue overflow, dropped oldest message",
				"channel_id", channelID, "queue_size", sq.maxSize)
		default:
		}
		cq.ch <- req
	}

	return nil
}

// worker is the per-channel send goroutine. It reads messages from the channel
// and sends them via the API with rate limiting. It exits after an idle timeout.
func (sq *SendQueue) worker(channelID string, cq *channelQueue) {
	defer sq.wg.Done()
	defer sq.removeQueue(channelID)

	idleTimer := time.NewTimer(defaultSendIdleTimeout)
	defer idleTimer.Stop()

	for {
		select {
		case req, ok := <-cq.ch:
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

			// Send the message.
			msg, err := sq.sendOne(req)
			if req.result != nil {
				req.result <- &sendResult{msg: msg, err: err}
			}

			// Enforce minimum delay between sends.
			time.Sleep(sq.minDelay)

		case <-idleTimer.C:
			sq.log.Debug("Send queue worker idle, exiting", "channel_id", channelID)
			return
		}
	}
}

// sendOne dispatches a single outbound message to the Discord API.
func (sq *SendQueue) sendOne(req *sendRequest) (*discordgo.Message, error) {
	data := &discordgo.MessageSend{
		Content:    req.content,
		Embeds:     req.embeds,
		Components: req.components,
	}
	return sq.session.ChannelMessageSendComplex(req.channelID, data)
}

// removeQueue removes the per-channel queue from the map when the worker exits.
func (sq *SendQueue) removeQueue(channelID string) {
	sq.mu.Lock()
	defer sq.mu.Unlock()
	delete(sq.queues, channelID)
}

// Close shuts down all worker goroutines and waits for them to finish.
// Messages still in the queues are drained with errors.
func (sq *SendQueue) Close() {
	sq.mu.Lock()
	sq.closed = true
	for channelID, cq := range sq.queues {
		close(cq.ch)
		delete(sq.queues, channelID)
	}
	sq.mu.Unlock()

	sq.wg.Wait()
}
