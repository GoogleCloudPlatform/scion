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

package hub

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/eventfeed"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

const (
	defaultConversationWatchWait = 30 * time.Second
	watchWriteDeadlineGrace      = 10 * time.Second
)

// conversationMessageWakeEvent is the slim payload published on
// conversation.<id>.message by feedNotifier. The database is the authoritative
// log; this event is only a coalesced wakeup doorbell (ptone/scion#8136 §3.3).
type conversationMessageWakeEvent struct {
	ConversationID string `json:"conversation_id"`
	MessageID      string `json:"message_id"`
	Seq            int64  `json:"seq"`
}

// feedNotifier implements entadapter.MessagePublisher and publishes only the
// slim conversation.<id>.message wakeup subject after MessageStore.CreateMessage
// commits. It does NOT call EventPublisher.PublishUserMessage, avoiding double
// publication of existing chat/inbox subjects.
type feedNotifier struct {
	events func() EventPublisher
}

// PublishUserMessage announces that msg was persisted in its conversation.
func (n *feedNotifier) PublishUserMessage(_ context.Context, msg *store.Message) error {
	if msg == nil || msg.ConversationID == "" {
		return nil
	}
	ep := n.events()
	if ep == nil {
		return nil
	}
	ep.PublishRaw("conversation."+msg.ConversationID+".message", conversationMessageWakeEvent{
		ConversationID: msg.ConversationID,
		MessageID:      msg.ID,
		Seq:            msg.ConversationSeq,
	})
	return nil
}

// feedWaker adapts EventPublisher.Subscribe("conversation.<id>.message") to
// eventfeed.Waker with a 1-slot coalescing channel.
type feedWaker struct {
	events func() EventPublisher
}

// Subscribe returns a 1-slot coalescing channel that receives when
// conversationID may have new messages, along with a cancel function.
func (w *feedWaker) Subscribe(conversationID string) (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	ep := w.events()
	if ep == nil {
		return ch, func() {}
	}
	evCh, unsub := ep.Subscribe("conversation." + conversationID + ".message")
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-done:
				return
			case _, ok := <-evCh:
				if !ok {
					return
				}
				select {
				case ch <- struct{}{}:
				default:
				}
			}
		}
	}()
	var once sync.Once
	cancel := func() {
		once.Do(func() {
			close(done)
			unsub()
		})
	}
	return ch, cancel
}

// getEventPublisher returns the currently configured EventPublisher safely.
func (s *Server) getEventPublisher() EventPublisher {
	s.mu.RLock()
	ep := s.events
	s.mu.RUnlock()
	return ep
}

// convReadDenial carries a structured authorization or lookup failure from
// authorizeConversationRead so HTTP handlers map it to the exact same status
// codes and error payloads without writing to ResponseWriter inside the
// authorizer itself.
type convReadDenial struct {
	status      int
	code        string
	message     string
	tokenRefuse bool
	token       *ScopedUserIdentity
	storeErr    error
	authzDenied bool
	resource    Resource
	action      Action
	decision    Decision
}

func (d *convReadDenial) Error() string {
	if d.storeErr != nil {
		return d.storeErr.Error()
	}
	if d.message != "" {
		return d.message
	}
	return eventfeed.ErrForbidden.Error()
}

func (d *convReadDenial) Unwrap() error {
	if d.storeErr != nil {
		return d.storeErr
	}
	return eventfeed.ErrForbidden
}

// authorizeConversationReadByID enforces credential, existence, and read
// authorization for conversation id, returning the conversation on success or
// a typed denial / store error without writing to w. Shared by
// handleConvListMessages and handleConvWatch for exact authz parity
// (ptone/scion#8136 §3.5).
func (s *Server) authorizeConversationReadByID(ctx context.Context, r *http.Request, id string) (*store.Conversation, error) {
	identity := GetIdentityFromContext(ctx)
	if identity == nil {
		return nil, &convReadDenial{status: http.StatusForbidden}
	}
	cls, token := inboxCredentialOf(ctx)
	if cls == inboxCredentialRefused {
		return nil, &convReadDenial{status: http.StatusForbidden}
	}
	conv, err := s.store.GetConversation(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := s.authorizeConversationReadChecked(ctx, identity, cls, token, conv); err != nil {
		return nil, err
	}
	return conv, nil
}

func (s *Server) authorizeConversationReadChecked(ctx context.Context, identity Identity, cls inboxCredential, token *ScopedUserIdentity, conv *store.Conversation) error {
	if conv.Kind == "direct" {
		if !authorizeDMRead(conv, identity.Type(), identity.ID()) {
			return &convReadDenial{status: http.StatusForbidden}
		}
		if err := s.checkCrossProjectReadGate(ctx, conv, nil); err != nil {
			return err
		}
	} else {
		if err := s.checkGroupConversationAccess(ctx, identity, conv, ActionRead); err != nil {
			return err
		}
	}

	if cls == inboxCredentialToken {
		ok, err := s.tokenMayReadConversation(ctx, token, s.newSelfScopeCheck(ctx, token, permInboxRead), conv)
		if err != nil {
			return &convReadDenial{storeErr: err}
		}
		if !ok {
			return &convReadDenial{status: http.StatusForbidden, tokenRefuse: true, token: token}
		}
	}

	return nil
}

// checkCrossProjectReadGate is the non-HTTP-writing counterpart of
// enforceCrossProjectReadGate.
func (s *Server) checkCrossProjectReadGate(ctx context.Context, conv *store.Conversation, msg *store.Message) error {
	agentIdent := GetAgentIdentityFromContext(ctx)
	if agentIdent == nil || conv.Kind != "direct" {
		return nil
	}
	peerKind, peerID, inKey, err := dmKeyPeer(conv, agentIdent.ID())
	if err != nil {
		return &convReadDenial{
			status:  http.StatusForbidden,
			code:    ErrCodeForbidden,
			message: "cross-project read denied: unparseable conversation key",
		}
	}
	if !inKey {
		return nil
	}
	var stampedPeerProject *string
	if msg != nil {
		stampedPeerProject = peerProjectFromRow(msg, peerKind, peerID)
	}
	decision, reason := s.crossProjectPeerAllowed(ctx, agentIdent.ProjectID(), peerKind, peerID, stampedPeerProject)
	switch decision {
	case peerAllowed:
		return nil
	case peerDenied403:
		return &convReadDenial{
			status:  http.StatusForbidden,
			code:    ErrCodeForbidden,
			message: reason,
		}
	default:
		return &convReadDenial{
			status:  http.StatusInternalServerError,
			code:    ErrCodeInternalError,
			message: "Internal server error verifying peer agent",
		}
	}
}

// checkGroupConversationAccess is the single non-HTTP-writing implementation of
// group conversation access control, wrapped by authorizeGroupConversationAccess
// and called by authorizeConversationReadChecked (ptone/scion#8136).
func (s *Server) checkGroupConversationAccess(ctx context.Context, identity Identity, conv *store.Conversation, action Action) error {
	if conv.ProjectID == nil || *conv.ProjectID == "" {
		if identity == nil {
			return &convReadDenial{status: http.StatusForbidden}
		}
		allowed, err := s.canReadGroupConversation(ctx, identity, conv)
		if err != nil {
			return &convReadDenial{storeErr: err}
		}
		if !allowed {
			return &convReadDenial{status: http.StatusForbidden}
		}
		return nil
	}

	if agentIdent := GetAgentIdentityFromContext(ctx); agentIdent != nil {
		if agentIdent.ProjectID() != *conv.ProjectID {
			return &convReadDenial{status: http.StatusForbidden}
		}
	}

	project, err := s.store.GetProject(ctx, *conv.ProjectID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return &convReadDenial{status: http.StatusForbidden}
		}
		return &convReadDenial{storeErr: err}
	}

	if identity == nil {
		return &convReadDenial{status: http.StatusUnauthorized}
	}

	res := projectResource(project)
	decision := s.authzService.CheckAccess(ctx, identity, res, action)
	if !decision.Allowed {
		return &convReadDenial{
			status:      http.StatusForbidden,
			authzDenied: true,
			resource:    res,
			action:      action,
			decision:    decision,
		}
	}
	return nil
}

// writeConvReadError maps an error returned by authorizeConversationReadByID,
// checkCrossProjectReadGate, or checkGroupConversationAccess to the exact HTTP
// response used across the conversation handlers.
func (s *Server) writeConvReadError(w http.ResponseWriter, r *http.Request, err error) {
	var denial *convReadDenial
	if errors.As(err, &denial) {
		if denial.storeErr != nil {
			writeErrorFromErr(w, denial.storeErr, "")
			return
		}
		if denial.tokenRefuse {
			writeTokenConversationRefusal(w, r, denial.token, permInboxRead)
			return
		}
		if denial.authzDenied {
			action := denial.action
			if action == "" {
				action = ActionRead
			}
			logAuthzDenial(r, GetIdentityFromContext(r.Context()), denial.resource, action, denial.decision.Reason)
			writeForbiddenStructuredDenialCause(w, "", denial.resource.Type, action, denial.decision.DeniedBy, denial.decision.adoptionDetailsCause())
			return
		}
		if denial.status == http.StatusUnauthorized {
			Unauthorized(w)
			return
		}
		if denial.message != "" {
			writeError(w, denial.status, denial.code, denial.message, nil)
			return
		}
		Forbidden(w)
		return
	}
	writeErrorFromErr(w, err, "Conversation")
}

// normalizeFeedPrincipalKind maps Hub identity types to the canonical message
// principal kind ("user" or "agent"), matching messageability.go:getSenderMode.
// Dev-token ("dev") and federated user ("federated_user") callers stamp rows as
// "user:..." and addressees as principal_kind="user".
func normalizeFeedPrincipalKind(identityType string) string {
	switch identityType {
	case "user", "dev", "federated_user":
		return "user"
	default:
		return identityType
	}
}

// conversationWatchResponse is the HTTP JSON wire shape for
// GET /api/v1/conversations/{id}/watch (design §3.6).
type conversationWatchResponse struct {
	Events       []eventfeed.Event `json:"events"`
	NextCursor   eventfeed.Cursor  `json:"nextCursor"`
	HasMore      bool              `json:"hasMore"`
	Truncated    bool              `json:"truncated"`
	TimedOut     bool              `json:"timedOut"`
	RetryAfterMs int64             `json:"retryAfterMs"`
}

// handleConvWatch handles GET /api/v1/conversations/{id}/watch.
func (s *Server) handleConvWatch(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w, http.MethodGet)
		return
	}
	if !s.experimentEnabled(experiments.ConversationWatch) {
		NotFound(w, "route")
		return
	}

	ctx := r.Context()
	q := r.URL.Query()

	wait, err := parseWatchWait(q.Get("wait"))
	if err != nil {
		BadRequest(w, err.Error())
		return
	}

	limit := eventfeed.DefaultLimit
	if limitStr := q.Get("limit"); limitStr != "" {
		n, parseErr := strconv.Atoi(limitStr)
		if parseErr != nil || n < 1 {
			BadRequest(w, "invalid 'limit' parameter: must be a positive integer")
			return
		}
		if n > eventfeed.MaxLimit {
			n = eventfeed.MaxLimit
		}
		limit = n
	}

	filter := eventfeed.Filter(q.Get("filter"))
	switch filter {
	case "", eventfeed.FilterOthers, eventfeed.FilterAll, eventfeed.FilterAddressed:
	default:
		BadRequest(w, fmt.Sprintf("invalid 'filter' parameter %q: must be others, all, or addressed", filter))
		return
	}

	start := eventfeed.Start(q.Get("start"))
	switch start {
	case "", eventfeed.StartHead, eventfeed.StartBeginning:
	default:
		BadRequest(w, fmt.Sprintf("invalid 'start' parameter %q: must be head or beginning", start))
		return
	}

	// Extend the per-request write deadline before blocking so waits longer
	// than Server.WriteTimeout still complete with 200 OK (design §3.6).
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Now().Add(wait + watchWriteDeadlineGrace))

	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")

	var principal eventfeed.Principal
	if identity := GetIdentityFromContext(ctx); identity != nil {
		principal = eventfeed.Principal{Kind: normalizeFeedPrincipalKind(identity.Type()), ID: identity.ID()}
	}

	var authorizedConv *store.Conversation
	authorizer := func(authCtx context.Context, convID string) (*store.Conversation, error) {
		conv, authErr := s.authorizeConversationReadByID(authCtx, r, convID)
		if authErr != nil {
			return nil, authErr
		}
		authorizedConv = conv
		return conv, nil
	}

	res, err := s.conversationFeed.Poll(ctx, principal, authorizer, eventfeed.PollRequest{
		ConversationID: id,
		Cursor:         eventfeed.Cursor(q.Get("cursor")),
		AfterMessageID: q.Get("after_message"),
		Start:          start,
		Filter:         filter,
		Limit:          limit,
		Wait:           wait,
	})
	if err != nil {
		switch {
		case errors.Is(err, eventfeed.ErrInvalidCursor):
			writeError(w, http.StatusBadRequest, "invalid_cursor", err.Error(), nil)
		case errors.Is(err, eventfeed.ErrCursorExpired):
			writeError(w, http.StatusGone, "cursor_expired", err.Error(), nil)
		case errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded):
			return
		default:
			s.writeConvReadError(w, r, err)
		}
		return
	}

	// Apply the exact same message post-processing as handleConvListMessages
	// so event.data is byte-identical to GET .../messages items (design §3.4).
	if len(res.Events) > 0 {
		if authorizedConv != nil && GetAgentIdentityFromContext(ctx) != nil {
			for i := range res.Events {
				if res.Events[i].Data != nil {
					scopeProvenanceToDMParties(authorizedConv, res.Events[i].Data)
				}
			}
		}
		msgs := make([]store.Message, len(res.Events))
		for i := range res.Events {
			if res.Events[i].Data != nil {
				msgs[i] = *res.Events[i].Data
			}
		}
		s.noteDeferredAttachments(ctx, msgs)
		for i := range res.Events {
			if res.Events[i].Data != nil {
				res.Events[i].Data.DeferredAttachments = msgs[i].DeferredAttachments
			}
		}
	}

	events := res.Events
	if events == nil {
		events = []eventfeed.Event{}
	}

	writeJSON(w, http.StatusOK, conversationWatchResponse{
		Events:       events,
		NextCursor:   res.NextCursor,
		HasMore:      res.HasMore,
		Truncated:    res.Truncated,
		TimedOut:     res.TimedOut,
		RetryAfterMs: res.RetryAfter.Milliseconds(),
	})
}

// parseWatchWait parses the wait query parameter as either seconds (number) or
// a Go duration string, defaulting to 30s and clamping to [0, 55s].
func parseWatchWait(raw string) (time.Duration, error) {
	if raw == "" {
		return defaultConversationWatchWait, nil
	}
	var d time.Duration
	if secs, err := strconv.ParseFloat(raw, 64); err == nil {
		if math.IsNaN(secs) || math.IsInf(secs, 0) || secs < 0 {
			return 0, fmt.Errorf("invalid 'wait' parameter %q: must be non-negative", raw)
		}
		if secs > float64(eventfeed.DefaultMaxWait/time.Second) {
			return eventfeed.DefaultMaxWait, nil
		}
		d = time.Duration(secs * float64(time.Second))
	} else if parsed, durErr := time.ParseDuration(raw); durErr == nil {
		if parsed < 0 {
			return 0, fmt.Errorf("invalid 'wait' parameter %q: must be non-negative", raw)
		}
		d = parsed
	} else {
		return 0, fmt.Errorf("invalid 'wait' parameter %q: must be seconds or a Go duration", raw)
	}
	if d > eventfeed.DefaultMaxWait {
		d = eventfeed.DefaultMaxWait
	}
	return d, nil
}
