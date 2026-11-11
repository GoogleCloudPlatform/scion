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

package bridge

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/GoogleCloudPlatform/scion/extras/scion-a2a-bridge/internal/state"
	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	hubstore "github.com/GoogleCloudPlatform/scion/pkg/store"
)

// The reply poller returns an agent's reply to an A2A call by reading the
// caller's own direct conversation with the agent through the hub API, with
// the caller's own credential. The hub's read checks decide what it can see.
//
// It finds the conversation in the caller's list of direct conversations
// (the one whose dmPeer is the agent), reads the anchor (the message the
// bridge sent, by the ID the send returned) for its creation time, and then
// reads the conversation's newest messages after that time, following the
// page cursor up to replyPollMaxPages pages. The reply is the oldest agent
// message of a reply or question type that is newer than the anchor on the
// pages read. It is appended to the task's event log with the dedup key
// "poll:<hub message ID>", which only the poller produces, and the
// executor's wait accepts only such events (and a cancel).

const (
	// replyPollDedupPrefix prefixes the dedup key of every event the reply
	// poller appends. The executor's wait keys on it; the push path never
	// produces it (processAndAppendEvent).
	replyPollDedupPrefix = "poll:"

	// replyPollPageSize is the number of messages one conversation read asks for.
	replyPollPageSize = 50
	// replyPollMaxPages bounds the pages one poll reads (200 messages).
	replyPollMaxPages = 4

	// replyPollFirstDelay is the delay before the first read after the send.
	replyPollFirstDelay = time.Second
	// replyPollStep is added to the delay after each read that found no reply.
	replyPollStep = 500 * time.Millisecond
	// replyPollMaxDelay caps the delay between reads.
	replyPollMaxDelay = 3 * time.Second

	// replyAppendTimeout bounds the append of a reply that passed the
	// append fence. The append does not end when the call stops the poller;
	// stop waits for it instead.
	replyAppendTimeout = 10 * time.Second
)

// Messages of the calls that end on a reply poll outcome or refusal.
const (
	replyMsgUnauthorized = "The hub no longer accepts this caller's credential; the agent's reply can't be returned."
	replyMsgForbidden    = "This access token can't read your conversation with this agent (it needs the inbox:read scope and access to the agent's project); agent replies can't be returned."
	replyMsgNotListed    = "Your conversation with this agent can't be found; the agent's reply can't be returned."
	replyMsgTooMany      = "Too many messages arrived in this conversation after the request; the agent's reply can't be located."
	replyMsgNotRecorded  = "The hub did not record this message; the agent's reply can't be returned."
	replyMsgBusy         = "Another request to this agent is still waiting for its reply; try again when it completes."
)

// sendStatusNotPersisted is the send status of a message the hub delivered
// but did not store.
const sendStatusNotPersisted = "delivered_not_persisted"

// replyReader is the part of the hub conversation API the reply poller uses.
// hubclient.ConversationService satisfies it.
type replyReader interface {
	List(ctx context.Context, opts *hubclient.ListConversationsOptions) (*hubclient.ConversationListResult, error)
	GetMessage(ctx context.Context, conversationID, messageID string) (*hubstore.Message, error)
	ListMessages(ctx context.Context, conversationID string, opts *hubclient.ConversationMessagesOptions) (*hubstore.ListResult[hubstore.Message], error)
}

// pollOutcome is the result of one read of the caller's conversation. Its
// value is the reason code that log lines carry.
type pollOutcome string

const (
	pollFound        pollOutcome = "found"
	pollNotYet       pollOutcome = "not_yet"
	pollForbidden    pollOutcome = "forbidden"
	pollUnauthorized pollOutcome = "unauthorized"
	pollNotListed    pollOutcome = "not_listed"
	pollTooMany      pollOutcome = "too_many"
	pollError        pollOutcome = "error"
	pollNoAnchor     pollOutcome = "no_anchor"
	// pollRefused is a federated user's read refused by the hub (401, 403
	// or 404). It ends the call with the uniform refusal, so a federated
	// user cannot tell refusal reasons apart.
	pollRefused pollOutcome = "refused"
)

// terminalMessage returns the message a call ends with on a terminal
// outcome, or "" for an outcome that does not end the call.
func (o pollOutcome) terminalMessage() string {
	switch o {
	case pollUnauthorized:
		return replyMsgUnauthorized
	case pollForbidden:
		return replyMsgForbidden
	case pollNotListed:
		return replyMsgNotListed
	case pollTooMany:
		return replyMsgTooMany
	case pollRefused:
		return federatedUserRefusalText
	default:
		return ""
	}
}

// readOutcome maps a failed hub read to its outcome: 401 and 403 are
// terminal refusals; anything else (404 on the anchor or a page, 503 and
// other 5xx, transport errors and timeouts) is retried until the deadline.
func readOutcome(err error) pollOutcome {
	var apiErr *apiclient.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.StatusCode {
		case http.StatusUnauthorized:
			return pollUnauthorized
		case http.StatusForbidden:
			return pollForbidden
		}
	}
	return pollError
}

// errNoReadResult is a conversation read that returned neither a result nor
// an error. It is taken as a failed read (pollError): the poll retries it
// until the deadline, and the preflight goes on to the send. It is never
// taken as an empty answer, because "not listed" and "anchor not seen" end
// or keep a call on what the hub answered, and here it answered nothing.
var errNoReadResult = errors.New("conversation read returned no result")

// replyPollEnded is the cause with which the reply poller ends the call's
// wait on a terminal outcome.
type replyPollEnded struct {
	outcome pollOutcome
}

func (e *replyPollEnded) Error() string {
	return e.outcome.terminalMessage()
}

// replyRowKind classifies a conversation message for reply selection.
type replyRowKind int

const (
	// replyRowSkip is a message that is neither a reply nor a question;
	// scanning continues past it.
	replyRowSkip replyRowKind = iota
	// replyRowAnswer is a reply that completes the call.
	replyRowAnswer
	// replyRowQuestion is the agent asking the caller a question; the call
	// ends input-required.
	replyRowQuestion
)

// classifyReplyRow returns how a message from the agent counts for reply
// selection. The message types are the closed list of pkg/messages.
//
// An input-needed message is a question only when it carries a thread ID:
// the agent's own outbound messages filed in a direct conversation always
// do, while the hub's subscriber notifications (sent on WAITING_FOR_INPUT
// to every subscriber of the agent, whatever conversation it came from) do
// not, and are skipped.
func classifyReplyRow(m *hubstore.Message) replyRowKind {
	switch m.Type {
	case "", messages.TypeInstruction, messages.TypeReply, messages.TypeChat, messages.TypeGroupSet:
		return replyRowAnswer
	case messages.TypeInputNeeded:
		if m.ThreadID != "" {
			return replyRowQuestion
		}
		return replyRowSkip
	default:
		// state-change, system, mention and the retired assistant-reply.
		return replyRowSkip
	}
}

// selectReply scans newest-first conversation messages (one page, or
// consecutive pages in order). It reports whether the anchor is among them
// and, if it is, returns the oldest message newer than the anchor that the
// agent sent with a reply or question type. A message is never taken as
// newer than the anchor unless the anchor itself is among the messages: the
// order (created, id) descending is the hub's keyset order, so the messages
// before the anchor are exactly the newer ones.
func selectReply(items []hubstore.Message, anchorID, agentID string) (reply *hubstore.Message, anchorSeen bool) {
	anchorIdx := -1
	for i := range items {
		if items[i].ID == anchorID {
			anchorIdx = i
			break
		}
	}
	if anchorIdx < 0 {
		return nil, false
	}
	// Walk from just above the anchor towards the newest message, so the
	// first match is the oldest candidate.
	for i := anchorIdx - 1; i >= 0; i-- {
		m := &items[i]
		if m.SenderID != agentID {
			continue
		}
		if classifyReplyRow(m) != replyRowSkip {
			return m, true
		}
	}
	return nil, true
}

// polledReplyMessage converts a conversation message to the structured
// message the event pipeline appends.
func polledReplyMessage(m *hubstore.Message) *messages.StructuredMessage {
	return &messages.StructuredMessage{
		Version:   messages.Version,
		Type:      m.Type,
		Sender:    m.Sender,
		SenderID:  m.SenderID,
		Msg:       m.Msg,
		Timestamp: m.CreatedAt.UTC().Format(time.RFC3339Nano),
	}
}

// pollOnlyEvent is the wait predicate for a call with a live reply poller:
// it accepts only the poller's events and a canceled final event. Push
// events (agent-wide status, pushed content) are skipped.
func pollOnlyEvent(ev state.TaskEvent) bool {
	return strings.HasPrefix(ev.DedupKey, replyPollDedupPrefix) || isCanceledEvent(ev)
}

// findDirectConversation returns the ID of the caller's direct conversation
// with the agent, or "" when it is not listed. The list carries no limit:
// the hub truncates after sorting, so with a limit the conversation could
// fall off the list.
func findDirectConversation(ctx context.Context, reader replyReader, agentID string) (string, error) {
	list, err := reader.List(ctx, &hubclient.ListConversationsOptions{Kind: "direct"})
	if err != nil {
		return "", err
	}
	if list == nil {
		return "", errNoReadResult
	}
	for _, c := range list.Conversations {
		if c.DMPeer != nil && c.DMPeer.Kind == "agent" && c.DMPeer.ID == agentID {
			return c.ID, nil
		}
	}
	return "", nil
}

// replyPreflight checks, before the send, that the caller can read its
// conversation with the agent: it lists the caller's direct conversations
// and, when the conversation with the agent is listed, reads from it with
// the same checks a poll makes (after now-1s, limit 1). It returns the
// conversation ID when listed, and the outcome of a refusal (401 or 403),
// which ends the call before anything is sent. Other failures are not
// refusals: the call goes on, and the poll maps them again after the send.
// A conversation that is not listed is first contact; the send creates it.
func replyPreflight(ctx context.Context, reader replyReader, agentID string, now time.Time) (string, pollOutcome) {
	id, err := findDirectConversation(ctx, reader, agentID)
	if err != nil {
		if o := readOutcome(err); o.terminalMessage() != "" {
			return "", o
		}
		return "", ""
	}
	if id == "" {
		return "", ""
	}
	_, err = reader.ListMessages(ctx, id, &hubclient.ConversationMessagesOptions{
		After: now.Add(-time.Second).UTC().Format(time.RFC3339),
		Limit: 1,
	})
	if err != nil {
		if o := readOutcome(err); o.terminalMessage() != "" {
			return "", o
		}
	}
	return id, ""
}

// replyWaitKey is the same-caller key of a call: the principal whose
// conversation is read, and the agent. That is the caller's user ID for a
// user caller and the admin topic user for a call without a caller.
func replyWaitKey(caller *CallerIdentity, adminUserID, agentID string) string {
	principal := adminUserID
	if caller != nil {
		principal = caller.UserID
	}
	return principal + "\x00" + agentID
}

// replyPoller reads one call's reply from the caller's conversation with
// the agent. It runs in its own goroutine from start until stop.
type replyPoller struct {
	bridge    *Bridge
	reader    replyReader
	log       *slog.Logger
	taskID    string
	agentSlug string
	agentID   string
	anchorID  string

	// visitor, when set, reads a federated user's own conversation with
	// the agent instead of reader (visitorDMReader).
	visitor *visitorDMReader

	// endCall ends the call's wait with a cause, on a terminal outcome.
	endCall func(error)
	// fence, when set, is checked before the reply is appended, in addition
	// to the done flag and the poller's context (standalone mode: the SDK
	// snapshot is not terminal and the execution lease is held).
	fence func(ctx context.Context) bool
	// sem caps the reads in flight across the process; nil means no cap.
	sem chan struct{}
	// sleep waits for d or until ctx ends; it reports whether the full
	// delay elapsed. Tests replace it.
	sleep func(ctx context.Context, d time.Duration) bool

	// Resolved on the first reads (or by the preflight), then reused.
	conversationID string
	afterBound     string

	lastOutcome pollOutcome

	done   atomic.Bool
	cancel context.CancelFunc
	exited chan struct{}
}

// newReplyPoller returns a poller for one call. anchorID is the hub message
// ID the send returned; it must not be empty. conversationID is the
// caller's conversation with the agent when the preflight found it, else "".
func newReplyPoller(b *Bridge, reader replyReader, taskID, agentSlug, agentID, anchorID, conversationID string, endCall func(error)) *replyPoller {
	return &replyPoller{
		bridge:         b,
		reader:         reader,
		log:            b.log,
		taskID:         taskID,
		agentSlug:      agentSlug,
		agentID:        agentID,
		anchorID:       anchorID,
		conversationID: conversationID,
		endCall:        endCall,
		sem:            b.replyPollSem,
		sleep:          sleepContext,
		exited:         make(chan struct{}),
	}
}

// start runs the poller in a goroutine bound to ctx.
func (p *replyPoller) start(ctx context.Context) {
	ctx, p.cancel = context.WithCancel(ctx)
	go func() {
		defer close(p.exited)
		p.run(ctx)
	}()
}

// stop ends the poller and waits for its goroutine to exit, so that no
// poller event can be appended after stop returns. It may be called more
// than once, and does nothing for a poller that was never started.
func (p *replyPoller) stop() {
	p.done.Store(true)
	if p.cancel == nil {
		return
	}
	p.cancel()
	<-p.exited
}

// run reads the conversation on the backoff schedule until it finds the
// reply, reaches a terminal outcome, or ctx ends.
func (p *replyPoller) run(ctx context.Context) {
	delay := replyPollFirstDelay
	for {
		if !p.sleep(ctx, delay) {
			return
		}
		outcome, reply := p.pollCapped(ctx)
		if ctx.Err() != nil {
			return
		}
		p.logOutcome(outcome)
		switch {
		case outcome == pollFound:
			p.appendReply(ctx, reply)
			return
		case outcome.terminalMessage() != "":
			if p.endCall != nil {
				p.endCall(&replyPollEnded{outcome: outcome})
			}
			return
		}
		if delay += replyPollStep; delay > replyPollMaxDelay {
			delay = replyPollMaxDelay
		}
	}
}

// pollCapped makes one poll while holding a slot of the process-wide cap.
func (p *replyPoller) pollCapped(ctx context.Context) (pollOutcome, *hubstore.Message) {
	if p.sem != nil {
		select {
		case p.sem <- struct{}{}:
			defer func() { <-p.sem }()
		case <-ctx.Done():
			return pollError, nil
		}
	}
	return p.pollOnce(ctx)
}

// pollOnce makes one poll of the caller's conversation.
func (p *replyPoller) pollOnce(ctx context.Context) (pollOutcome, *hubstore.Message) {
	if p.visitor != nil {
		return p.scanPages(ctx, visitorReadOutcome, p.visitor.page)
	}
	if p.conversationID == "" {
		// Listed once per call: after the preflight (or the first poll) has
		// found the conversation, its ID is reused. Only while the list
		// itself fails with a retryable error does a later poll list again,
		// on the same backoff schedule as the reads.
		id, err := findDirectConversation(ctx, p.reader, p.agentID)
		if err != nil {
			return readOutcome(err), nil
		}
		if id == "" {
			// The send creates the conversation and registers both parties
			// before it answers, so after a send that returned an anchor a
			// conversation that is not listed is final.
			return pollNotListed, nil
		}
		p.conversationID = id
	}
	if p.afterBound == "" {
		anchor, err := p.reader.GetMessage(ctx, p.conversationID, p.anchorID)
		if err != nil {
			return readOutcome(err), nil
		}
		if anchor == nil {
			return readOutcome(errNoReadResult), nil
		}
		// The handler parses whole-second RFC3339; one second before the
		// anchor, truncated, only widens the window.
		p.afterBound = anchor.CreatedAt.Add(-time.Second).UTC().Format(time.RFC3339)
	}
	return p.scanPages(ctx, readOutcome, func(ctx context.Context, cursor string) ([]hubstore.Message, string, error) {
		res, err := p.reader.ListMessages(ctx, p.conversationID, &hubclient.ConversationMessagesOptions{
			After:  p.afterBound,
			Limit:  replyPollPageSize,
			Cursor: cursor,
		})
		if err != nil {
			return nil, "", err
		}
		if res == nil {
			return nil, "", errNoReadResult
		}
		return res.Items, res.NextCursor, nil
	})
}

// replyPageFetch reads one newest-first page of the caller's conversation
// with the agent, from cursor ("" for the newest page). It returns the
// page's messages and the cursor of the next (older) page, "" when there is
// none.
type replyPageFetch func(ctx context.Context, cursor string) ([]hubstore.Message, string, error)

// scanPages reads newest-first pages with fetch, following the page cursor
// up to replyPollMaxPages pages, until the anchor is on the pages read. It
// maps a failed read with outcomeOf.
func (p *replyPoller) scanPages(ctx context.Context, outcomeOf func(error) pollOutcome, fetch replyPageFetch) (pollOutcome, *hubstore.Message) {
	var rows []hubstore.Message
	cursor := ""
	for page := 0; page < replyPollMaxPages; page++ {
		items, next, err := fetch(ctx, cursor)
		if err != nil {
			return outcomeOf(err), nil
		}
		rows = append(rows, items...)
		reply, anchorSeen := selectReply(rows, p.anchorID, p.agentID)
		if anchorSeen {
			if reply == nil {
				return pollNotYet, nil
			}
			return pollFound, reply
		}
		if next == "" {
			// The anchor is not on the pages and there is no further page:
			// nothing is taken as newer than an anchor not seen.
			return pollNotYet, nil
		}
		cursor = next
	}
	// replyPollMaxPages full pages after the anchor and more to come: every
	// later poll sees even more newer messages, so the anchor can no longer
	// be reached.
	return pollTooMany, nil
}

// appendReply appends the reply to the task's event log, unless the call
// has ended or the fence refuses. Once the fence has passed, the append
// (the artifact and message events and their push dispatches) runs on a
// bounded context that stop does not cancel: the wait can return on the
// first of these events, and stop then waits for the rest of the append
// instead of cutting it short.
func (p *replyPoller) appendReply(ctx context.Context, reply *hubstore.Message) {
	if !p.mayAppend(ctx) {
		return
	}
	actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), replyAppendTimeout)
	defer cancel()
	p.bridge.processAndAppendEvent(actx, p.taskID, p.agentSlug, polledReplyMessage(reply), replyPollDedupPrefix+reply.ID)
}

// mayAppend is the append fence: the call has not ended (done not set, the
// poller's context live) and, in standalone mode, the fence function agrees.
func (p *replyPoller) mayAppend(ctx context.Context) bool {
	if p.done.Load() || ctx.Err() != nil {
		return false
	}
	if p.fence != nil && !p.fence(ctx) {
		return false
	}
	return true
}

// logOutcome logs a read outcome when it differs from the previous one, so
// repeated outcomes produce one line. The line carries the reason code
// only: no message text and no identifiers.
func (p *replyPoller) logOutcome(outcome pollOutcome) {
	if outcome == p.lastOutcome {
		return
	}
	p.lastOutcome = outcome
	p.log.Debug("reply poll", "reason", string(outcome))
}

// standaloneAppendFence returns the standalone append fence of a task: its
// SDK snapshot is not terminal and this replica still holds its execution
// lease.
func (b *Bridge) standaloneAppendFence(taskID string) func(ctx context.Context) bool {
	return func(ctx context.Context) bool {
		if b.sdkTaskStore == nil {
			return false
		}
		ownerKey, ok, err := buildOwnerKey(ctx)
		if err != nil || !ok {
			return false
		}
		stored, _, err := b.sdkTaskStore.GetOwnedTaskSnapshotAndCursor(ctx, taskID, ownerKey)
		if err != nil || stored == nil || stored.Task == nil || isFinalSDKState(stored.Task.Status.State) {
			return false
		}
		return b.sdkTaskStore.HeartbeatExecution(ctx, taskID, OwnerID()) == nil
	}
}

// isFinalSDKState reports whether an SDK task state is terminal.
func isFinalSDKState(s a2a.TaskState) bool {
	switch s {
	case a2a.TaskStateCompleted, a2a.TaskStateFailed, a2a.TaskStateCanceled, a2a.TaskStateRejected:
		return true
	default:
		return false
	}
}

// sleepContext waits for d or until ctx ends, and reports whether the full
// delay elapsed.
func sleepContext(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}
