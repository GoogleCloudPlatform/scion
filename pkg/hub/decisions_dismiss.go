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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	"github.com/GoogleCloudPlatform/scion/pkg/msgaction"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Error codes of POST /api/v1/decision-prompts/{messageId}/dismiss (design 8194, 4.3–4.4a).
const (
	ErrCodeUsersOnly         = "users_only"
	ErrCodeDecisionTooRecent = "too_recent"
	ErrCodeDismissChanged    = "dismiss_changed"
)

// DismissDecisionPromptRequest is the body of
// POST /api/v1/decision-prompts/{messageId}/dismiss (design 8194, 4.3).
type DismissDecisionPromptRequest struct {
	DecisionIDs    []string `json:"decisionIds,omitempty"`
	Reason         string   `json:"reason,omitempty"`
	Surface        string   `json:"surface,omitempty"`
	IdempotencyKey string   `json:"idempotencyKey,omitempty"`
}

// DismissDecisionPromptResponse is the 200 response of
// POST /api/v1/decision-prompts/{messageId}/dismiss.
type DismissDecisionPromptResponse struct {
	Dismissed         []DecisionView `json:"dismissed"`
	AlreadyDismissed  []DecisionView `json:"alreadyDismissed"`
	ResponseMessageID string         `json:"responseMessageId"`
	// Delivered is true when the response was dispatched to the asking agent
	// or held for its next session (DeliveryStatus delivered or held).
	Delivered bool `json:"delivered"`
	// DeliveryStatus says what happened to the asking agent's
	// decision-response: one of the DismissDelivery* values.
	DeliveryStatus string `json:"deliveryStatus"`
}

// DismissDecisionPromptResponse.DeliveryStatus values.
const (
	// DismissDeliveryDelivered: dispatched to the asking agent.
	DismissDeliveryDelivered = "delivered"
	// DismissDeliveryHeld: held until the asking agent's next session (or
	// queued behind an auto-wake), and delivered then.
	DismissDeliveryHeld = "held"
	// DismissDeliveryGone: the asking agent is deleted; nothing was sent.
	DismissDeliveryGone = "gone"
	// DismissDeliveryUnreachable: the asking agent is unreachable
	// (user-suspended, held, stopping, stopped, or in error) and will not
	// auto-wake; the response is stored, marked failed, not sent.
	DismissDeliveryUnreachable = "unreachable"
	// DismissDeliveryFailed: dispatch was attempted and failed, or the result
	// cannot be confirmed.
	DismissDeliveryFailed = "failed"
	// DismissDeliveryNone: nothing new was dismissed, so no response exists
	// for this call (every listed row was already dismissed).
	DismissDeliveryNone = "none"
)

// agentUnreachableReasonPrefix starts every agentUnreachableReason with a
// phase suffix (handlers_chat_v2.go), the failure reason stored on a
// decision-response for an unreachable asker.
const agentUnreachableReasonPrefix = "Agent unreachable ("

func dismissDeliveryAccepted(status string) bool {
	return status == DismissDeliveryDelivered || status == DismissDeliveryHeld
}

// dismissReplayDeliveryStatus rebuilds the delivery status of a replayed
// dismissal from its stored decision-response.
func dismissReplayDeliveryStatus(askerGone bool, m *store.Message) string {
	if askerGone {
		return DismissDeliveryGone
	}
	if m == nil {
		return DismissDeliveryFailed
	}
	switch m.DispatchState {
	case store.MessageDispatchDispatched, store.MessageDispatchDelivered:
		return DismissDeliveryDelivered
	case store.MessageDispatchAwaitingSession, store.MessageDispatchPending,
		store.MessageDispatchDeferred, store.MessageDispatchDelivering:
		return DismissDeliveryHeld
	case store.MessageDispatchFailed:
		if m.DispatchFailureReason != nil && strings.HasPrefix(*m.DispatchFailureReason, agentUnreachableReasonPrefix) {
			return DismissDeliveryUnreachable
		}
	}
	return DismissDeliveryFailed
}

type decisionDismissAnswerMeta struct {
	Key        string `json:"key"`
	Version    int    `json:"version"`
	ChoiceID   string `json:"choice_id"`
	Provenance string `json:"provenance"`
	Outcome    string `json:"outcome"`
}

func (s *Server) nowDecision() time.Time {
	if s.decisionClock != nil {
		return s.decisionClock().UTC()
	}
	return time.Now().UTC()
}

// decisionDismissMinAge returns the effective minimum age before a decision
// version may be dismissed. Outside an active --test-mode hub it always
// returns the production 5-minute store.DecisionDismissMinAge.
func (s *Server) decisionDismissMinAge() time.Duration {
	if s != nil && s.TestModeLevel() != TestModeOff && s.config.TestDecisionDismissMinAge > 0 {
		return s.config.TestDecisionDismissMinAge
	}
	return store.DecisionDismissMinAge
}

// dismissCaller verifies the caller is a human user (web session, dev
// credential, or UAT with inbox:write) and never an agent, visitor, or
// federated user.
func (s *Server) dismissCaller(w http.ResponseWriter, r *http.Request) (UserIdentity, *ScopedUserIdentity, bool) {
	ctx := r.Context()
	if GetAgentIdentityFromContext(ctx) != nil {
		writeError(w, http.StatusForbidden, ErrCodeUsersOnly, "this endpoint is only available to users", nil)
		return nil, nil, false
	}
	identity := GetIdentityFromContext(ctx)
	if isNilIdentity(identity) {
		Unauthorized(w)
		return nil, nil, false
	}
	if isVisitorIdentity(identity) {
		logAuthzDenial(r, identity, Resource{Type: "inbox"}, ActionWrite, "decision dismissal is not open to visitors")
		writeVisitorRefusal(w)
		return nil, nil, false
	}
	cls, token := inboxCredentialOf(ctx)
	if cls != inboxCredentialSession && cls != inboxCredentialToken {
		logAuthzDenial(r, identity, Resource{Type: "inbox"}, ActionWrite, "credential cannot dismiss decisions")
		Forbidden(w)
		return nil, nil, false
	}
	user, ok := identity.(UserIdentity)
	if !ok {
		Forbidden(w)
		return nil, nil, false
	}
	if token != nil && !s.authorizeInboxToken(w, r, token, permInboxWrite) {
		return nil, nil, false
	}
	return user, token, true
}

// handleDecisionPromptDismiss serves POST /api/v1/decision-prompts/{messageId}/dismiss.
func (s *Server) handleDecisionPromptDismiss(w http.ResponseWriter, r *http.Request, msgID string) {
	ctx := r.Context()
	user, token, ok := s.dismissCaller(w, r)
	if !ok {
		return
	}

	var bodyReader io.Reader
	if r.Body != nil && r.ContentLength != 0 {
		bodyReader = r.Body
	}
	req, ferr := msgaction.ParseDismissRequest(bodyReader, msgaction.DismissParseOptions{
		RowIDsField:      "decisionIds",
		MaxRowIDs:        messages.MaxDecisionsPerMessage,
		NormalizeSurface: normalizeAnswerSurface,
		SurfaceError: func(raw string) string {
			return fmt.Sprintf("surface %q is not supported; expected %q or %q", raw, store.DecisionSurfaceNative, store.DecisionSurfaceDecisionsPage)
		},
	})
	if ferr != nil {
		if ferr.InvalidJSON {
			BadRequest(w, ferr.Message)
			return
		}
		ValidationError(w, ferr.Message, map[string]interface{}{"field": ferr.Field})
		return
	}
	surface := req.Surface
	trimmedReason := req.Reason

	// Authorize on the prompt's conversation using the exact answer check (D5).
	ds, chatKey, ok := s.loadAuthorizedDecisionPrompt(w, ctx, user, msgID)
	if !ok {
		return
	}
	// Enforce UAT project boundary on DM prompts (topics are checked inside
	// authorizeChatSend via loadAuthorizedDecisionPrompt).
	if token != nil && strings.HasPrefix(chatKey, "dm:") {
		if !s.newSelfScopeCheck(ctx, token, permInboxWrite).allows(ds[0].ProjectID) {
			NotFound(w, "Decision prompt")
			return
		}
	}

	if !s.allowChatSend(w, user.ID(), chatSenderHuman) {
		return
	}

	byID := make(map[string]*store.Decision, len(ds))
	for _, d := range ds {
		byID[d.ID] = d
	}
	var cleanIDs []string
	if len(req.RowIDs) > 0 {
		cleanIDs = make([]string, 0, len(req.RowIDs))
		seenIDs := make(map[string]bool, len(req.RowIDs))
		for i, rawID := range req.RowIDs {
			field := fmt.Sprintf("decisionIds[%d]", i)
			rowID := strings.TrimSpace(rawID)
			d, exists := byID[rowID]
			if !exists {
				ValidationError(w, fmt.Sprintf("decision row %q is not a decision of this prompt", rowID),
					map[string]interface{}{"field": field})
				return
			}
			if seenIDs[rowID] {
				ValidationError(w, fmt.Sprintf("decision %s is listed twice", d.Key),
					map[string]interface{}{"field": field})
				return
			}
			seenIDs[rowID] = true
			cleanIDs = append(cleanIDs, rowID)
		}
	}

	asker, askerGone, ok := s.decisionAsker(w, ctx, ds[0].AskerAgentID)
	if !ok {
		return
	}
	if !askerGone {
		if allowed, reason, _ := s.authorizeAgentMessage(ctx, user, asker, false); !allowed {
			writeError(w, http.StatusForbidden, ErrCodeMessageDenied, "Message delivery denied", map[string]interface{}{
				"reason":        mapReasonToCode(reason),
				"senderMode":    "user",
				"recipientMode": asker.MessageMode,
			})
			return
		}
	}

	now := s.nowDecision().Truncate(time.Microsecond)
	responseMsg := s.decisionDismissResponseStoreMessage(ctx, user, ds[0], chatKey, asker, askerGone, now)
	who := userDisplay(user)

	minAge := s.decisionDismissMinAge()
	res, err := s.store.DismissDecisions(ctx, &store.DismissDecisionsInput{
		MessageID: msgID,
		RowIDs:    cleanIDs,
		User: store.DecisionAnswerer{
			UserID: user.ID(),
			Name:   user.DisplayName(),
			Email:  user.Email(),
		},
		Reason:          trimmedReason,
		Surface:         surface,
		IdempotencyKey:  req.IdempotencyKey,
		ResponseMessage: responseMsg,
		FormatMessage: func(dismissed []*store.Decision) string {
			return decisionDismissConversationLine(who, dismissed, trimmedReason)
		},
		Now:    func() time.Time { return s.nowDecision() },
		MinAge: minAge,
	})
	var tooRecent *store.DecisionTooRecentError
	var changed *store.DismissChangedError
	switch {
	case err == nil:
	case errors.Is(err, store.ErrIdempotencyMismatch):
		writeError(w, http.StatusUnprocessableEntity, ErrCodeIdempotencyMismatch,
			"the idempotency key was used for another dismissal or answer set", nil)
		return
	case errors.As(err, &tooRecent):
		msg := "decisions must be at least 5 minutes old before they can be dismissed"
		if minAge != store.DecisionDismissMinAge {
			msg = fmt.Sprintf("decisions must be at least %s old before they can be dismissed", minAge)
		}
		writeError(w, http.StatusConflict, ErrCodeDecisionTooRecent,
			msg,
			map[string]interface{}{"availableAt": tooRecent.AvailableAt.UTC()})
		return
	case errors.As(err, &changed):
		writeError(w, http.StatusConflict, ErrCodeDismissChanged,
			"the decisions to dismiss changed; refresh and try again",
			dismissChangedDetails(changed, s.promptDecisionViewsByID(ctx, msgID)))
		return
	case errors.Is(err, store.ErrInvalidInput):
		ValidationError(w, err.Error(), nil)
		return
	case errors.Is(err, store.ErrNotFound):
		NotFound(w, "Decision prompt")
		return
	default:
		writeErrorFromErr(w, err, "")
		return
	}

	dismissedViews := s.decisionViewsFor(ctx, res.Dismissed)
	alreadyViews := s.decisionViewsFor(ctx, res.AlreadyDismissed)

	if res.Replayed {
		respMsgID := ""
		if len(res.Dismissed) > 0 {
			if dsm := res.Dismissed[0].Dismissal(); dsm != nil {
				respMsgID = dsm.ResponseMessageID
			}
		} else if len(res.AlreadyDismissed) > 0 {
			if dsm := res.AlreadyDismissed[0].Dismissal(); dsm != nil {
				respMsgID = dsm.ResponseMessageID
			}
		}
		status := DismissDeliveryNone
		if len(res.Dismissed) > 0 {
			// A lookup failure reports failed: claiming delivery we cannot
			// confirm is the wrong default.
			var storedMsg *store.Message
			if !askerGone && respMsgID != "" {
				var merr error
				if storedMsg, merr = s.store.GetMessage(ctx, respMsgID); merr != nil {
					slog.Warn("decision dismiss replay: response message lookup failed",
						"response_message_id", respMsgID, "error", merr)
					storedMsg = nil
				}
			}
			status = dismissReplayDeliveryStatus(askerGone, storedMsg)
		}
		writeJSON(w, http.StatusOK, DismissDecisionPromptResponse{
			Dismissed:         dismissedViews,
			AlreadyDismissed:  alreadyViews,
			ResponseMessageID: respMsgID,
			Delivered:         dismissDeliveryAccepted(status),
			DeliveryStatus:    status,
		})
		return
	}

	if len(res.Dismissed) == 0 {
		respMsgID := ""
		if len(res.AlreadyDismissed) > 0 {
			if dsm := res.AlreadyDismissed[0].Dismissal(); dsm != nil {
				respMsgID = dsm.ResponseMessageID
			}
		}
		writeJSON(w, http.StatusOK, DismissDecisionPromptResponse{
			Dismissed:         dismissedViews,
			AlreadyDismissed:  alreadyViews,
			ResponseMessageID: respMsgID,
			Delivered:         false,
			DeliveryStatus:    DismissDeliveryNone,
		})
		return
	}

	answers := make([]*store.DecisionAnswer, 0, len(res.Dismissed))
	for _, d := range res.Dismissed {
		if dsm := d.Dismissal(); dsm != nil {
			answers = append(answers, dsm)
		}
	}
	setView := decisionResponseSetView(msgID, res.BatchID, ds, answers)
	s.publishDecisionResponseSet(ctx, responseMsg, setView)

	status := DismissDeliveryGone
	if !askerGone {
		status = s.deliverDecisionDismissResponse(ctx, asker, responseMsg, res.BatchID, res.Dismissed, answers)
	}
	for _, v := range dismissedViews {
		s.publishDecisionUpdatedEventAt(ds[0].ProjectID, msgID, chatKey, v)
	}
	s.enqueueDecisionBellDismiss(ds[0].ProjectID, chatKey, res.Dismissed)

	writeJSON(w, http.StatusOK, DismissDecisionPromptResponse{
		Dismissed:         dismissedViews,
		AlreadyDismissed:  alreadyViews,
		ResponseMessageID: responseMsg.ID,
		Delivered:         dismissDeliveryAccepted(status),
		DeliveryStatus:    status,
	})
}

// dismissChangedDetails builds the 409 dismiss_changed details: per-row
// reasons, the prompt's current views, and availableAt when a listed row is
// too recent (so the page can re-arm its unlock timer, as for too_recent).
func dismissChangedDetails(changed *store.DismissChangedError, views []DecisionView) map[string]interface{} {
	details := map[string]interface{}{
		"reasons":   changed.Reasons,
		"decisions": views,
	}
	if !changed.AvailableAt.IsZero() {
		details["availableAt"] = changed.AvailableAt.UTC()
	}
	return details
}

// decisionDismissResponseStoreMessage builds the decision-response message
// stored with a dismissal (design 8194, 4.5). Unlike an answer to a gone
// asker, a dismissal of a gone asker's prompt stays in DispatchState
// dispatched (not marked failed), is not delivered, and reports
// deliveryStatus gone. An unreachable asker that will not auto-wake
// (user-suspended, held, stopped, ...) has its response stored failed and
// reports deliveryStatus unreachable.
func (s *Server) decisionDismissResponseStoreMessage(ctx context.Context, user UserIdentity, first *store.Decision, chatKey string, asker *store.Agent, askerGone bool, now time.Time) *store.Message {
	m := &store.Message{
		ID:             api.NewUUID(),
		ProjectID:      first.ProjectID,
		Sender:         userSenderAddress(user),
		SenderID:       user.ID(),
		RecipientID:    first.AskerAgentID,
		Type:           messages.TypeDecisionResponse,
		AgentID:        first.AskerAgentID,
		Channel:        "web",
		ThreadID:       chatKey,
		ConversationID: first.ConversationID,
		DispatchState:  store.MessageDispatchDispatched,
		CreatedAt:      now,
	}
	if askerGone {
		m.Recipient = "agent:" + first.AskerAgentName
	} else {
		m.Recipient = "agent:" + asker.Slug
		if unreachable, why := s.isAgentUnreachable(asker); unreachable && !s.canAutoWakeDecisionAsker(ctx, asker) {
			reason := agentUnreachableReason(why)
			m.DispatchState = store.MessageDispatchFailed
			m.DispatchFailureReason = &reason
		}
	}
	return m
}

// decisionDismissConversationLine formats the plain-text conversation body of a
// dismissal decision-response (design 8194, 4.5).
func decisionDismissConversationLine(who string, dismissed []*store.Decision, reason string) string {
	var base string
	if len(dismissed) == 1 {
		base = fmt.Sprintf("%s dismissed %s", who, dismissed[0].Key)
	} else {
		keys := make([]string, len(dismissed))
		for i, d := range dismissed {
			keys[i] = d.Key
		}
		base = fmt.Sprintf("%s dismissed %d decisions: %s", who, len(dismissed), strings.Join(keys, ", "))
	}
	if r := plainLine(reason); r != "" {
		return base + " · " + r
	}
	return base
}

// decisionDismissAgentLine formats the hub-authored line delivered to the live
// asking agent (design 8194, 4.5):
// `alice dismissed D234 ("Ship v0.9 today?"), 17 ("Which approach?"). Reason: ...`
// For a dismissed row that had an agent-interpreted answer awaiting
// confirmation (D6), appends ` (your reading: <label>)`.
func decisionDismissAgentLine(dismissed []*store.Decision, answers []*store.DecisionAnswer) string {
	if len(answers) == 0 {
		return ""
	}
	interpByRow := make(map[string]string, len(dismissed))
	for _, d := range dismissed {
		if d == nil {
			continue
		}
		for _, ans := range d.Answers {
			if ans != nil && ans.Provenance == store.DecisionProvenanceAgentInterpreted {
				interpByRow[d.ID] = ans.ChoiceLabel
			}
		}
	}
	who := answererDisplayName(answers[0])
	if who == "" {
		who = answers[0].AnsweredByUserID
	}
	parts := make([]string, len(answers))
	for i, a := range answers {
		part := fmt.Sprintf("%s (%q)", a.Key, truncateRunes(plainLine(a.QuestionText), decisionResponseQuestionMax))
		if reading := plainLine(interpByRow[a.DecisionRowID]); reading != "" {
			part += fmt.Sprintf(" (your reading: %s)", reading)
		}
		parts[i] = part
	}
	line := fmt.Sprintf("%s dismissed %s.", who, strings.Join(parts, ", "))
	if r := plainLine(answers[0].DismissReason); r != "" {
		line += " Reason: " + r
	}
	return line
}

func decisionDismissMetadata(raw map[string]string, batchID string, answers []*store.DecisionAnswer) map[string]string {
	meta := messaging.StripReservedMetadata(raw)
	if meta == nil {
		meta = make(map[string]string, 7)
	}
	populateDecisionDismissMetadata(meta, batchID, answers)
	return meta
}

func populateDecisionDismissMetadata(meta map[string]string, batchID string, answers []*store.DecisionAnswer) {
	items := make([]decisionDismissAnswerMeta, len(answers))
	for i, a := range answers {
		items[i] = decisionDismissAnswerMeta{
			Key:        a.Key,
			Version:    a.Version,
			ChoiceID:   messages.ChoiceIDDismissed,
			Provenance: store.DecisionProvenanceDirect,
			Outcome:    store.DecisionOutcomeDismiss,
		}
	}
	if encoded, err := json.Marshal(items); err == nil {
		meta[messages.MetadataDecisionAnswers] = string(encoded)
	}
	if batchID != "" {
		meta[messages.MetadataDecisionBatch] = batchID
	}
	if len(answers) > 0 && answers[0].DismissReason != "" {
		meta[messages.MetadataDecisionDismissReason] = answers[0].DismissReason
	}
	if len(answers) == 1 {
		a := answers[0]
		meta[messages.MetadataDecisionKey] = a.Key
		meta[messages.MetadataDecisionVersion] = strconv.Itoa(a.Version)
		meta[messages.MetadataChoiceID] = messages.ChoiceIDDismissed
		meta[messages.MetadataDecisionProvenance] = store.DecisionProvenanceDirect
	}
}

// deliverDecisionDismissResponse dispatches a dismissal's decision-response to
// the asking agent and returns its DismissDelivery* status.
func (s *Server) deliverDecisionDismissResponse(ctx context.Context, asker *store.Agent, m *store.Message, batchID string, dismissed []*store.Decision, answers []*store.DecisionAnswer) string {
	if asker == nil || len(answers) == 0 {
		return DismissDeliveryFailed
	}
	if m.DispatchState != store.MessageDispatchDispatched {
		// Stored failed up front: the asker is unreachable.
		return DismissDeliveryUnreachable
	}
	if s.GetDispatcher() == nil {
		return DismissDeliveryFailed
	}
	text := decisionDismissAgentLine(dismissed, answers)
	sm := &messages.StructuredMessage{
		Version:        messages.Version,
		Timestamp:      m.CreatedAt.UTC().Format(time.RFC3339),
		Sender:         m.Sender,
		SenderID:       m.SenderID,
		Recipient:      m.Recipient,
		RecipientID:    m.RecipientID,
		Msg:            text,
		Type:           messages.TypeDecisionResponse,
		Channel:        m.Channel,
		ThreadID:       m.ThreadID,
		ConversationID: m.ConversationID,
	}
	held, err := s.dispatchDecisionResponse(ctx, asker, m, text, sm, func(meta map[string]string) {
		populateDecisionDismissMetadata(meta, batchID, answers)
	}, "dismiss decision-response")
	if err != nil {
		return DismissDeliveryFailed
	}
	if held {
		return DismissDeliveryHeld
	}
	return DismissDeliveryDelivered
}
