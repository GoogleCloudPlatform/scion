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
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Error codes of the decision endpoints (design choices v4, 4.10).
const (
	ErrCodeDecisionReplaced    = "replaced"
	ErrCodeDecisionAnswered    = "already_answered"
	ErrCodeNotRecordedAnswerer = "not_recorded_answerer"
)

// decisionsRoutePrefix is the subtree of the decision endpoints.
const decisionsRoutePrefix = "/api/v1/decisions/"

// choiceResponseQuestionMax caps the question quoted in the line delivered
// to the asking agent.
const choiceResponseQuestionMax = 200

// DecisionAnswerRequest is the body of POST /api/v1/decisions/{rowId}/answer.
// The client names the choice by id only; the label is always copied from
// the stored decision.
type DecisionAnswerRequest struct {
	ChoiceID       string `json:"choice_id"`
	IdempotencyKey string `json:"idempotency_key,omitempty"`
}

// DecisionAnswerResponse is the 200 answer of the answer endpoint.
type DecisionAnswerResponse struct {
	Decision          DecisionView `json:"decision"`
	ResponseMessageID string       `json:"responseMessageId"`
	Replayed          bool         `json:"replayed,omitempty"`
}

// handleDecisionRoutes serves the decision endpoints, 404 while
// web.chat_choices is off:
//
//	GET  /api/v1/decisions/{rowId}         the decision's view
//	POST /api/v1/decisions/{rowId}/answer  answer it (a user, never an agent)
func (s *Server) handleDecisionRoutes(w http.ResponseWriter, r *http.Request) {
	s.requireExperiment(experiments.ChatChoices, func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, decisionsRoutePrefix)
		rowID, sub, _ := strings.Cut(rest, "/")
		switch {
		case rowID == "":
			NotFound(w, "Decision")
		case sub == "":
			if r.Method != http.MethodGet {
				MethodNotAllowed(w, http.MethodGet)
				return
			}
			s.handleDecisionGet(w, r, rowID)
		case sub == "answer":
			if r.Method != http.MethodPost {
				MethodNotAllowed(w, http.MethodPost)
				return
			}
			s.handleDecisionAnswer(w, r, rowID)
		default:
			NotFound(w, "Decision")
		}
	})(w, r)
}

// decisionCaller returns the user calling a decision endpoint, or writes
// the refusal: agents (an agent never answers directly, design 5 item 2),
// visitors and user access tokens get 403. Phase 1 answers come from the
// web session only.
func decisionCaller(w http.ResponseWriter, r *http.Request) UserIdentity {
	ctx := r.Context()
	if GetAgentIdentityFromContext(ctx) != nil {
		writeError(w, http.StatusForbidden, ErrCodeForbidden, "agents cannot answer or read decisions here", nil)
		return nil
	}
	user := GetUserIdentityFromContext(ctx)
	if isNilIdentity(user) {
		Unauthorized(w)
		return nil
	}
	if isVisitorIdentity(user) || chatToken(ctx) != nil {
		writeError(w, http.StatusForbidden, ErrCodeForbidden, "decisions are answered from a web session", nil)
		return nil
	}
	return user
}

// loadAuthorizedDecision reads the decision and its prompt message and
// authorizes the caller on the prompt's conversation: authorizeChatSend to
// answer (for a DM prompt that admits only the user in the DM key, the
// addressee), authorizeChatSendRead to read. A missing row, a missing
// prompt message and a refused caller all answer 404, so a row id reveals
// nothing.
func (s *Server) loadAuthorizedDecision(w http.ResponseWriter, ctx context.Context, user UserIdentity, rowID string, answering bool) (*store.Decision, *store.Message, bool) {
	d, err := s.store.GetDecision(ctx, rowID)
	if err != nil || d == nil {
		NotFound(w, "Decision")
		return nil, nil, false
	}
	prompt, err := s.store.GetMessage(ctx, d.MessageID)
	if err != nil || prompt == nil || prompt.ThreadID == "" {
		NotFound(w, "Decision")
		return nil, nil, false
	}
	authorize := s.authorizeChatSendRead
	if answering {
		authorize = s.authorizeChatSend
	}
	if _, serr := authorize(ctx, user, prompt.ThreadID); serr != nil {
		if serr.Status == http.StatusNotFound || serr.accessRefused {
			NotFound(w, "Decision")
			return nil, nil, false
		}
		serr.write(w)
		return nil, nil, false
	}
	return d, prompt, true
}

// handleDecisionGet serves GET /api/v1/decisions/{rowId}.
func (s *Server) handleDecisionGet(w http.ResponseWriter, r *http.Request, rowID string) {
	user := decisionCaller(w, r)
	if user == nil {
		return
	}
	d, _, ok := s.loadAuthorizedDecision(w, r.Context(), user, rowID, false)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"decision": s.decisionViewFor(r.Context(), d)})
}

// handleDecisionAnswer serves POST /api/v1/decisions/{rowId}/answer (design
// 4.10 steps 1 to 6).
func (s *Server) handleDecisionAnswer(w http.ResponseWriter, r *http.Request, rowID string) {
	ctx := r.Context()
	user := decisionCaller(w, r)
	if user == nil {
		return
	}
	var req DecisionAnswerRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}
	req.ChoiceID = strings.TrimSpace(req.ChoiceID)
	if req.ChoiceID == "" {
		ValidationError(w, "choice_id is required", map[string]interface{}{"field": "choice_id"})
		return
	}
	if len(req.IdempotencyKey) > store.MaxDecisionIdempotencyKeyLen {
		ValidationError(w, fmt.Sprintf("idempotency_key is at most %d bytes", store.MaxDecisionIdempotencyKeyLen),
			map[string]interface{}{"field": "idempotency_key"})
		return
	}

	// Steps 1 and 2: the row, its conversation, the caller's send rights.
	d, prompt, ok := s.loadAuthorizedDecision(w, ctx, user, rowID, true)
	if !ok {
		return
	}
	// Step 4: the choice is checked against the stored row; the label is
	// never taken from the client.
	choice, ok := d.Choice(req.ChoiceID)
	if !ok {
		ValidationError(w, fmt.Sprintf("choice %q is not a choice of decision %s", req.ChoiceID, d.Key),
			map[string]interface{}{"field": "choice_id"})
		return
	}

	// Step 3a: a true replay. The answer recorded under this idempotency
	// key, if it is this user's answer with this choice, is answered again
	// (200, replayed) before any state check, so a retry after the version
	// was replaced or answered still gets its answer. The key used for any
	// other answer is a conflict. A key with no recorded answer is a fresh
	// answer and gets every check below.
	if req.IdempotencyKey != "" {
		if prior := answerWithIdempotencyKey(d, req.IdempotencyKey); prior != nil {
			if prior.ChoiceID != choice.ID || prior.AnsweredByUserID != user.ID() {
				s.writeDecisionAnswered(w, ctx, d)
				return
			}
			writeJSON(w, http.StatusOK, DecisionAnswerResponse{
				Decision: s.decisionViewFor(ctx, d), ResponseMessageID: prior.ResponseMessageID, Replayed: true,
			})
			return
		}
	}

	// Step 3b: state, for every answer that is not a replay.
	outcome := store.DecisionOutcomeAnswer
	var overridden *store.DecisionAnswer
	if d.Status == store.DecisionStatusReplaced {
		s.writeDecisionReplaced(w, ctx, d)
		return
	}
	if eff := d.EffectiveAnswer(); eff != nil {
		if eff.Provenance == store.DecisionProvenanceDirect {
			s.writeDecisionAnswered(w, ctx, d)
			return
		}
		// An interpreted answer: only the user the agent recorded as
		// the answerer may confirm or override it (Q2).
		if eff.AnsweredByUserID == "" || eff.AnsweredByUserID != user.ID() {
			s.writeNotRecordedAnswerer(w, ctx, d)
			return
		}
		outcome = store.DecisionOutcomeConfirm
		if eff.ChoiceID != choice.ID {
			outcome = store.DecisionOutcomeOverride
			overridden = eff
		}
	}

	// Per-user answer rate limit: the chat send limiter (design 5 item 7).
	if !s.allowChatSend(w, user.ID(), chatSenderHuman) {
		return
	}

	asker, _ := s.store.GetAgent(ctx, d.AskerAgentID)
	responseMsg := choiceResponseStoreMessage(user, d, prompt, choice, asker)

	// Step 5: record the answer and store the response, in one transaction.
	answer, replayed, err := s.store.RecordAnswer(ctx, &store.RecordAnswerInput{
		DecisionRowID:    d.ID,
		ChoiceID:         choice.ID,
		AnsweredByUserID: user.ID(),
		AnsweredByName:   user.DisplayName(),
		AnsweredByEmail:  user.Email(),
		Provenance:       store.DecisionProvenanceDirect,
		Outcome:          outcome,
		Surface:          store.DecisionSurfaceNative,
		ResponseMessage:  responseMsg,
		IdempotencyKey:   req.IdempotencyKey,
	})
	switch {
	case err == nil:
	case errors.Is(err, store.ErrDecisionReplaced):
		s.writeDecisionReplaced(w, ctx, d)
		return
	case errors.Is(err, store.ErrNotRecordedAnswerer):
		// An interpreted answer recorded after the state check above.
		if cur, gerr := s.store.GetDecision(ctx, d.ID); gerr == nil {
			d = cur
		}
		s.writeNotRecordedAnswerer(w, ctx, d)
		return
	case errors.Is(err, store.ErrDecisionAnswerConflict):
		// The loser of a race, or an idempotency key reused for another
		// answer: the current state wins.
		if cur, gerr := s.store.GetDecision(ctx, d.ID); gerr == nil {
			d = cur
		}
		s.writeDecisionAnswered(w, ctx, d)
		return
	case errors.Is(err, store.ErrInvalidInput):
		ValidationError(w, err.Error(), nil)
		return
	default:
		writeErrorFromErr(w, err, "")
		return
	}

	cur, gerr := s.store.GetDecision(ctx, d.ID)
	if gerr != nil {
		writeErrorFromErr(w, gerr, "")
		return
	}
	view := s.decisionViewFor(ctx, cur)
	if replayed {
		writeJSON(w, http.StatusOK, DecisionAnswerResponse{Decision: view, ResponseMessageID: answer.ResponseMessageID, Replayed: true})
		return
	}

	// Step 6: publish the stored response, deliver it to the asker (not a
	// confirmation: the agent already acted on that answer), and tell every
	// open card.
	s.publishChoiceResponse(ctx, responseMsg, cur, answer)
	if outcome != store.DecisionOutcomeConfirm {
		s.deliverChoiceResponse(ctx, asker, responseMsg, answer, overridden)
	}
	s.publishChoiceUpdated(ctx, prompt, view)
	writeJSON(w, http.StatusOK, DecisionAnswerResponse{Decision: view, ResponseMessageID: answer.ResponseMessageID})
}

// answerWithIdempotencyKey returns d's answer recorded under key, or nil.
func answerWithIdempotencyKey(d *store.Decision, key string) *store.DecisionAnswer {
	for _, a := range d.Answers {
		if a.IdempotencyKey == key {
			return a
		}
	}
	return nil
}

// writeNotRecordedAnswerer answers 403: only the user an interpreted answer
// records may confirm or change it.
func (s *Server) writeNotRecordedAnswerer(w http.ResponseWriter, ctx context.Context, d *store.Decision) {
	writeError(w, http.StatusForbidden, ErrCodeNotRecordedAnswerer,
		"only the user recorded as the answerer can confirm or change this answer",
		map[string]interface{}{"decision": s.decisionViewFor(ctx, d)})
}

// writeDecisionReplaced answers 409 replaced with the decision and the
// newer version's view.
func (s *Server) writeDecisionReplaced(w http.ResponseWriter, ctx context.Context, d *store.Decision) {
	details := map[string]interface{}{"decision": s.decisionViewFor(ctx, d)}
	if d.ReplacedByID != "" {
		if newer, err := s.store.GetDecision(ctx, d.ReplacedByID); err == nil {
			details["replacedBy"] = s.decisionViewFor(ctx, newer)
		}
	}
	writeError(w, http.StatusConflict, ErrCodeDecisionReplaced,
		fmt.Sprintf("decision %s v%d was replaced by a newer version", d.Key, d.Version), details)
}

// writeDecisionAnswered answers 409 already_answered with the effective
// answer.
func (s *Server) writeDecisionAnswered(w http.ResponseWriter, ctx context.Context, d *store.Decision) {
	writeError(w, http.StatusConflict, ErrCodeDecisionAnswered,
		fmt.Sprintf("decision %s v%d is already answered", d.Key, d.Version),
		map[string]interface{}{"decision": s.decisionViewFor(ctx, d)})
}

// choiceResponseStoreMessage builds the choice-response row stored with the
// answer: sender the answering user, recipient the asking agent, in the
// prompt's conversation. An asker that is gone or cannot take a message
// gets the row born failed, as a chat send to it would.
func choiceResponseStoreMessage(user UserIdentity, d *store.Decision, prompt *store.Message, choice messages.Choice, asker *store.Agent) *store.Message {
	m := &store.Message{
		ID:             api.NewUUID(),
		ProjectID:      d.ProjectID,
		Sender:         userSenderAddress(user),
		SenderID:       user.ID(),
		RecipientID:    d.AskerAgentID,
		Msg:            choiceResponseLine(userDisplay(user), d.Key, choice.Label, d.Question),
		Type:           messages.TypeChoiceResponse,
		AgentID:        d.AskerAgentID,
		Channel:        "web",
		ThreadID:       prompt.ThreadID,
		ConversationID: d.ConversationID,
		DispatchState:  store.MessageDispatchDispatched,
		CreatedAt:      time.Now().UTC(),
	}
	if asker != nil {
		m.Recipient = "agent:" + asker.Slug
	} else {
		m.Recipient = "agent:" + d.AskerAgentName
	}
	if asker == nil {
		reason := agentGoneReason
		m.DispatchState = store.MessageDispatchFailed
		m.DispatchFailureReason = &reason
	} else if unreachable, why := isAgentUnreachable(asker); unreachable {
		reason := agentUnreachableReason(why)
		m.DispatchState = store.MessageDispatchFailed
		m.DispatchFailureReason = &reason
	}
	return m
}

// userDisplay is how an answering user is named in the conversation.
func userDisplay(user UserIdentity) string {
	if n := strings.TrimSpace(user.DisplayName()); n != "" {
		return n
	}
	if e := user.Email(); e != "" {
		return e
	}
	return user.ID()
}

// choiceResponseLine is the stored body of a choice-response (design 4.4):
// `ptone chose "yes" for D234 "Ship the feature now?"`.
func choiceResponseLine(who, key, label, question string) string {
	return fmt.Sprintf("%s chose %q for %s %q", who, label, key, truncateRunes(plainLine(question), choiceResponseQuestionMax))
}

// choiceResponseAgentLine is the hub-formatted line delivered to the asking
// agent (design 4.4): the question is the agent's own text, truncated.
func choiceResponseAgentLine(a *store.DecisionAnswer, overridden *store.DecisionAnswer) string {
	who := a.AnsweredByEmail
	if who == "" {
		who = a.AnsweredByName
	}
	line := fmt.Sprintf("[choice-response] %s v%d (%q) answer=%s (choice %s) by %s",
		a.Key, a.Version, truncateRunes(plainLine(a.QuestionText), choiceResponseQuestionMax), a.ChoiceLabel, a.ChoiceID, who)
	if overridden != nil {
		line += ", overriding interpreted answer " + overridden.ChoiceLabel
	}
	return line
}

// plainLine collapses whitespace so a multi-line question fits one line.
func plainLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// truncateRunes cuts s to at most n runes, marking a cut with an ellipsis.
func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}

// publishChoiceResponse emits the chat events of the stored response,
// carrying its ChoiceResponseView.
func (s *Server) publishChoiceResponse(ctx context.Context, m *store.Message, d *store.Decision, a *store.DecisionAnswer) {
	view := &ChoiceResponseView{
		RowID:           d.ID,
		Ref:             d.Ref(),
		Key:             a.Key,
		Version:         a.Version,
		Question:        a.QuestionText,
		ChoiceID:        a.ChoiceID,
		Label:           a.ChoiceLabel,
		Provenance:      a.Provenance,
		PromptMessageID: d.MessageID,
		SourceMessageID: a.SourceMessageID,
	}
	pubCtx := context.WithoutCancel(ctx)
	opt := withChoiceEvent(&choiceEventPayload{Response: view})
	s.touchConversationActivity(pubCtx, m.ThreadID, m.ID)
	s.events.PublishUserMessage(pubCtx, m, nil, nil, opt)
	if !strings.HasPrefix(m.ThreadID, "dm:") {
		s.fanOutThreadMessageToMembersAsync(pubCtx, m, nil, nil, opt)
	}
}

// deliverChoiceResponse dispatches the stored response to the asking agent
// with the hub-formatted line and the hub-set choice metadata. A stopped or
// deleted asker leaves the row failed (undeliverable semantics); the answer
// stands either way.
func (s *Server) deliverChoiceResponse(ctx context.Context, asker *store.Agent, m *store.Message, a *store.DecisionAnswer, overridden *store.DecisionAnswer) {
	if asker == nil || m.DispatchState != store.MessageDispatchDispatched {
		return
	}
	dispatcher := s.GetDispatcher()
	if dispatcher == nil {
		return
	}
	line := choiceResponseAgentLine(a, overridden)
	sm := &messages.StructuredMessage{
		Version:        messages.Version,
		Timestamp:      m.CreatedAt.UTC().Format(time.RFC3339),
		Sender:         m.Sender,
		SenderID:       m.SenderID,
		Recipient:      m.Recipient,
		RecipientID:    m.RecipientID,
		Msg:            line,
		Type:           messages.TypeChoiceResponse,
		Channel:        m.Channel,
		ThreadID:       m.ThreadID,
		ConversationID: m.ConversationID,
		Metadata: map[string]string{
			messages.MetadataChoiceKey:        a.Key,
			messages.MetadataChoiceVersion:    strconv.Itoa(a.Version),
			messages.MetadataChoiceID:         a.ChoiceID,
			messages.MetadataChoiceProvenance: a.Provenance,
		},
	}
	dctx, cancel := context.WithTimeout(withDispatchMessageID(context.WithoutCancel(ctx), m.ID), 30*time.Second)
	defer cancel()
	if err := dispatchWithBrokerRetry(dctx, s.recordLog(), dispatcher, asker, line, false, sm); err != nil {
		s.messageLog.Error("choices: failed to deliver choice-response", "agent", asker.Slug, "message_id", m.ID, "error", err)
		_ = s.markFailed(context.WithoutCancel(ctx), m.ID, err.Error())
	}
}

// publishChoiceUpdated tells every open card of the decision about its new
// state: on project.<id>.choice.updated for a thread, and on
// user.<id>.choice.updated for each user of a DM, so a DM's decisions reach
// only its participants.
func (s *Server) publishChoiceUpdated(ctx context.Context, prompt *store.Message, view DecisionView) {
	evt := map[string]interface{}{"decision": view, "messageId": prompt.ID, "threadId": prompt.ThreadID}
	if strings.HasPrefix(prompt.ThreadID, "dm:") {
		parts := strings.Split(prompt.ThreadID, ":")
		if len(parts) >= 5 {
			for i, id := range []string{parts[2], parts[4]} {
				kind := parts[1]
				if i == 1 {
					kind = parts[3]
				}
				if kind == "user" && id != "" {
					s.events.PublishRaw("user."+id+".choice.updated", evt)
				}
			}
		}
		return
	}
	if prompt.ProjectID != "" {
		s.events.PublishRaw("project."+prompt.ProjectID+".choice.updated", evt)
	}
}
